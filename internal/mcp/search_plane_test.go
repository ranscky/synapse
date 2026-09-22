package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"synapse/internal/store"

	"github.com/stretchr/testify/require"
)

// stubPlane stands in for the control plane candidate source an edge node
// configures (the same *sync.Syncer the proxy and the compile path hold).
type stubPlane struct {
	memories []store.MemoryEntry
	err      error
	calls    int
}

// PullCandidates implements retrieval.PlaneCandidates.
func (p *stubPlane) PullCandidates(_ context.Context, _ []float32, _ string, _ int) ([]store.MemoryEntry, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return p.memories, nil
}

// TestSearchMemoriesConsultsThePlaneBeforeTheLocalStore is the search half of
// what Phase 9 did for the compile path: an edge node holds its own database and
// the org's memories live on the plane, so a search that read only the local
// store would surface a different half of the brain than the compile it is
// explaining. The local store here is the assertion as much as the plane is: it
// must not be searched at all when the plane answered.
func TestSearchMemoriesConsultsThePlaneBeforeTheLocalStore(t *testing.T) {
	const orgMemory = "the shared order service retries three times before it gives up"

	plane := &stubPlane{memories: []store.MemoryEntry{{
		ID:         "org-mem-1",
		SessionID:  "sess-someone-else",
		Content:    orgMemory,
		MemoryType: "decision",
		Timestamp:  time.Now().UTC(),
		AgentID:    "agent-b",
		Visibility: store.VisibilityOrg,
		// The query's own axis, so the semantic factor is a real 1.0 rather
		// than an artifact of a degenerate vector.
		Embedding: basisVector(searchDims, int(searchQuery[0])%searchDims),
	}}}

	rec := &recordingStore{}
	srv := NewServer(rec, *searchConfig(), nil, basisEmbedder(searchDims))
	srv.SetPlaneCandidates(plane)

	res, err := srv.handleSearchMemories(context.Background(), searchCallRequest(map[string]any{"query": searchQuery}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	require.Equal(t, 1, plane.calls, "the plane must be asked first")
	require.Empty(t, rec.searches, "an answered plane is the candidate set; the local store is not merged in")

	got := decodeSearchResult(t, res)
	require.Len(t, got.Memories, 1)
	require.Equal(t, "org-mem-1", got.Memories[0]["id"])
	require.Equal(t, orgMemory, got.Memories[0]["content"])
	require.Equal(t, 1.0, got.Memories[0]["score_s"], "the plane's memory is scored by the same 4-Factor model")
	require.Equal(t, "agent-b", got.Memories[0]["agent_id"])
	require.Equal(t, true, got.Memories[0]["cross_agent"], "another agent's memory is cross-agent")
}

// TestSearchMemoriesFallsBackToTheLocalStoreWhenThePlaneFails covers the other
// half of the same policy: an unreachable plane costs one bounded ceiling and
// never a failed search -- which is exactly what retrieval.Candidates promises
// the compile path.
func TestSearchMemoriesFallsBackToTheLocalStoreWhenThePlaneFails(t *testing.T) {
	plane := &stubPlane{err: errors.New("plane: request timed out after 200ms")}
	rec := &recordingStore{}

	srv := NewServer(rec, *searchConfig(), nil, basisEmbedder(searchDims))
	srv.SetPlaneCandidates(plane)

	res, err := srv.handleSearchMemories(context.Background(), searchCallRequest(map[string]any{"query": searchQuery}))
	require.NoError(t, err)
	require.False(t, res.IsError, "a down plane must not fail a search")

	require.Equal(t, 1, plane.calls)
	require.Len(t, rec.searches, 1, "the local store answers when the plane cannot")

	got := decodeSearchResult(t, res)
	require.Empty(t, got.Memories)
}
