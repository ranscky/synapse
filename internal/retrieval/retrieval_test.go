// Tests for the shared candidate pipeline: which source answers, and what
// happens when the one that was asked does not.
package retrieval

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"synapse/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is the local memory store: it records how it was called and answers
// with whatever the test scripted.
type fakeStore struct {
	entries    []store.MemoryEntry
	err        error
	calls      int
	gotEmbed   []float32
	gotSession string
	gotTopK    int
}

func (f *fakeStore) Search(_ context.Context, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error) {
	f.calls++
	f.gotEmbed = queryEmbedding
	f.gotSession = sessionID
	f.gotTopK = topK

	return f.entries, f.err
}

// fakeEmbedder stands in for the ONNX embedder: one fixed vector, no model.
type fakeEmbedder struct {
	embedding []float32
	err       error
	calls     int
}

func (f *fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	f.calls++

	return f.embedding, f.err
}

// fakePlane is the control plane candidate source.
type fakePlane struct {
	entries      []store.MemoryEntry
	err          error
	calls        int
	gotEmbedding []float32
	gotSession   string
	gotTopK      int
}

func (f *fakePlane) PullCandidates(_ context.Context, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error) {
	f.calls++
	f.gotEmbedding = queryEmbedding
	f.gotSession = sessionID
	f.gotTopK = topK

	return f.entries, f.err
}

// candidate builds one entry a source can return.
func candidate(id, content string) store.MemoryEntry {
	return store.MemoryEntry{
		ID: id, SessionID: "session-1", Content: content, MemoryType: "fact",
	}
}

// embeddingOf builds a small recognisable vector; nothing here is scored, so
// the width only has to be distinguishable from another call's.
func embeddingOf(v float32) []float32 { return []float32{v, v, v} }

// captureLogs redirects the default logger into a buffer for the duration of a
// test. Same shape as the helper internal/sync uses: the fallback is observed in
// production through a WARN line, so a test has to be able to read one.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()

	var captured strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &captured
}

func TestCandidatesUsesThePlaneAndSkipsTheLocalStore(t *testing.T) {
	logs := captureLogs(t)
	local := &fakeStore{entries: []store.MemoryEntry{candidate("local-1", "local memory")}}
	plane := &fakePlane{entries: []store.MemoryEntry{candidate("plane-1", "org memory")}}
	emb := &fakeEmbedder{embedding: embeddingOf(1)}

	result, err := Candidates(context.Background(), local, emb, plane, "session-1", "what did we decide?", 50)
	require.NoError(t, err)
	require.Len(t, result.Candidates, 1)
	assert.Equal(t, "plane-1", result.Candidates[0].ID)

	assert.Zero(t, local.calls, "a plane answer is the candidate set; the local store is not consulted")
	assert.Equal(t, 1, plane.calls)
	assert.Equal(t, "session-1", plane.gotSession)
	assert.Equal(t, 50, plane.gotTopK)
	assert.Equal(t, emb.embedding, plane.gotEmbedding, "the plane is asked with the query embedding, not the query text")
	assert.Equal(t, emb.embedding, result.QueryEmbedding, "callers downstream still need the embedding")
	assert.NotContains(t, logs.String(), "plane_unavailable")
}

// TestCandidatesFallsBackToTheLocalStoreWhenThePlaneFails is the phase's central
// fallback assertion: every way a pull can fail -- the 200ms timeout, a
// connection that is refused, a non-2xx status -- has to end with a successful
// compile served from local memory and a WARN an operator can grep for.
func TestCandidatesFallsBackToTheLocalStoreWhenThePlaneFails(t *testing.T) {
	failures := []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"connection refused", errors.New("sync: search request: dial tcp 127.0.0.1:9090: connect: connection refused")},
		{"non-2xx", errors.New("sync: search failed status=503")},
	}

	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			logs := captureLogs(t)
			local := &fakeStore{entries: []store.MemoryEntry{candidate("local-1", "local memory")}}
			plane := &fakePlane{err: failure.err}
			emb := &fakeEmbedder{embedding: embeddingOf(2)}

			result, err := Candidates(context.Background(), local, emb, plane, "session-1", "what did we decide?", 50)
			require.NoError(t, err, "a plane failure is never a compile failure")
			require.Len(t, result.Candidates, 1)
			assert.Equal(t, "local-1", result.Candidates[0].ID)

			assert.Equal(t, 1, plane.calls, "the plane is asked first")
			assert.Equal(t, 1, local.calls, "the local store answers when the plane does not")
			assert.Equal(t, "session-1", local.gotSession)
			assert.Equal(t, 50, local.gotTopK)
			assert.Equal(t, emb.embedding, local.gotEmbed)

			assert.Contains(t, logs.String(), "plane_unavailable=true")
			assert.Contains(t, logs.String(), "fallback=local")
		})
	}
}

func TestCandidatesWithoutAPlaneSearchesLocally(t *testing.T) {
	logs := captureLogs(t)
	local := &fakeStore{entries: []store.MemoryEntry{candidate("local-1", "local memory")}}
	emb := &fakeEmbedder{embedding: embeddingOf(3)}

	result, err := Candidates(context.Background(), local, emb, nil, "session-1", "what did we decide?", 50)
	require.NoError(t, err)
	require.Len(t, result.Candidates, 1)
	assert.Equal(t, "local-1", result.Candidates[0].ID)

	assert.Equal(t, 1, local.calls)
	assert.Equal(t, "session-1", local.gotSession)
	assert.Equal(t, 50, local.gotTopK)
	assert.Equal(t, emb.embedding, result.QueryEmbedding)
	assert.Empty(t, logs.String(), "a standalone node has no plane to log about")
}

func TestCandidatesTreatsAnEmptyPlaneAnswerAsTheAnswer(t *testing.T) {
	local := &fakeStore{entries: []store.MemoryEntry{candidate("local-1", "local memory")}}
	plane := &fakePlane{}
	emb := &fakeEmbedder{embedding: embeddingOf(4)}

	result, err := Candidates(context.Background(), local, emb, plane, "session-1", "anything", 50)
	require.NoError(t, err)
	assert.Empty(t, result.Candidates)

	assert.Equal(t, 1, plane.calls)
	assert.Zero(t, local.calls, `a plane that answers "nothing yet" is an answer, not a failure`)
}

func TestCandidatesWithoutAQueryStillAsksThePlane(t *testing.T) {
	plane := &fakePlane{entries: []store.MemoryEntry{candidate("plane-1", "org memory")}}

	// A nil embedder and an empty query is the "nothing to embed" case, which
	// both backends answer with plain recency ordering. It must reach the plane
	// rather than short-circuit into an empty candidate set.
	result, err := Candidates(context.Background(), nil, nil, plane, "session-1", "", 50)
	require.NoError(t, err)
	require.Len(t, result.Candidates, 1)
	assert.Equal(t, 1, plane.calls)
	assert.Nil(t, plane.gotEmbedding)
}

func TestCandidatesFailsWhenTheQueryCannotBeEmbedded(t *testing.T) {
	plane := &fakePlane{}
	local := &fakeStore{}
	emb := &fakeEmbedder{err: errors.New("model missing")}

	result, err := Candidates(context.Background(), local, emb, plane, "session-1", "anything", 50)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to generate query embedding")
	assert.Zero(t, plane.calls, "nothing is asked of a plane without a query embedding")
	assert.Zero(t, local.calls)
	assert.Empty(t, result.Candidates)
}
