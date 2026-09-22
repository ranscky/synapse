package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"synapse/internal/config"
	"synapse/internal/store"

	"github.com/stretchr/testify/require"
)

// The seam a search reads through is the project's own backend contract, not the
// concrete SQLite store the ranking tests happen to use. The assertion lives in
// a test so a signature drift fails here rather than in the package main imports.
var _ Store = (*recordingStore)(nil)

// recordedSearch is one Search call a recordingStore saw: the reader scope the
// tool built, and the pool size it asked for.
type recordedSearch struct {
	agentID   string
	teamID    string
	sessionID string
	topK      int
}

// recordingStore stands in for storage and records what it was asked, which is
// what lets the tests below assert the reader scope each visibility builds and
// that invalid input never reaches storage at all. It is a Backend, so it also
// proves the search tool depends on no more than the four methods every backend
// already has.
type recordingStore struct {
	searches []recordedSearch
	err      error
}

// Write implements store.Backend. A search never calls it.
func (r *recordingStore) Write(context.Context, store.MemoryEntry) error { return nil }

// Search implements store.Backend.
func (r *recordingStore) Search(_ context.Context, _ []float32, agentID, teamID, sessionID string, topK int) ([]store.MemoryEntry, error) {
	r.searches = append(r.searches, recordedSearch{agentID: agentID, teamID: teamID, sessionID: sessionID, topK: topK})
	if r.err != nil {
		return nil, r.err
	}
	return []store.MemoryEntry{}, nil
}

// GetRecent implements store.Backend. A search never calls it: candidates come
// from semantic search, not from recency.
func (r *recordingStore) GetRecent(context.Context, string, int) ([]store.MemoryEntry, error) {
	return nil, nil
}

// MarkSuperseded implements store.Backend. A search never writes.
func (r *recordingStore) MarkSuperseded(context.Context, string, string) error { return nil }

// searchConfig returns a config that names this node as an agent on a team, so
// the reader scopes below have an identity to be built from.
func searchConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.AgentID = "agent-a"
	cfg.TeamID = "team-t"
	return cfg
}

// TestSearchMemoriesReadsAtTheVisibilityScopeItWasGiven pins the mapping from
// visibility to reader scope: org is the fail-closed default (an org-only
// reader), team adds this node's configured team, and private adds this agent's
// own memories from the session the caller named. The agent and team always come
// from config, so no argument can widen what a search reads.
func TestSearchMemoriesReadsAtTheVisibilityScopeItWasGiven(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want recordedSearch
	}{
		{
			name: "visibility defaults to org, which names no agent and no team",
			args: map[string]any{"query": "retry budget", "session_id": "sess-1"},
			want: recordedSearch{sessionID: "sess-1"},
		},
		{
			name: "org names no agent and no team either",
			args: map[string]any{"query": "retry budget", "visibility": "org", "session_id": "sess-1"},
			want: recordedSearch{sessionID: "sess-1"},
		},
		{
			name: "org with no session searches the cross-session bucket",
			args: map[string]any{"query": "retry budget", "visibility": "org"},
			want: recordedSearch{},
		},
		{
			name: "team names this node's team and still no agent",
			args: map[string]any{"query": "retry budget", "visibility": "team", "session_id": "sess-1"},
			want: recordedSearch{teamID: "team-t", sessionID: "sess-1"},
		},
		{
			name: "private names this agent and the session it asked for",
			args: map[string]any{"query": "retry budget", "visibility": "private", "session_id": "sess-1"},
			want: recordedSearch{agentID: "agent-a", teamID: "team-t", sessionID: "sess-1"},
		},
		{
			name: "private without a session reaches no private memory, which is fail-closed",
			args: map[string]any{"query": "retry budget", "visibility": "private"},
			want: recordedSearch{agentID: "agent-a", teamID: "team-t"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingStore{}
			srv := NewServer(rec, *searchConfig(), nil, basisEmbedder(searchDims))

			res, err := srv.handleSearchMemories(context.Background(), searchCallRequest(tt.args))
			require.NoError(t, err)
			require.False(t, res.IsError, "a valid search must not be reported as an error")

			require.Len(t, rec.searches, 1, "a valid search reaches storage exactly once")
			got := rec.searches[0]
			require.Equal(t, tt.want.agentID, got.agentID)
			require.Equal(t, tt.want.teamID, got.teamID)
			require.Equal(t, tt.want.sessionID, got.sessionID)

			// Nothing matched, so the answer is an empty array -- never null,
			// which would break a client that iterates it.
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(toolText(t, res)), &raw))
			require.Equal(t, "[]", string(raw["memories"]))
		})
	}
}

// TestSearchMemoriesWidensThePoolForALargeTopK pins the other half of the pool
// rule: the store is asked for this node's configured retrieval width, but never
// for less than the caller's top_k -- otherwise the store's similarity order,
// not the 4-Factor ranking, would decide which memories come back.
func TestSearchMemoriesWidensThePoolForALargeTopK(t *testing.T) {
	cfg := searchConfig()
	cfg.RetrievalCandidateK = 1

	rec := &recordingStore{}
	srv := NewServer(rec, *cfg, nil, basisEmbedder(searchDims))

	res, err := srv.handleSearchMemories(context.Background(), searchCallRequest(map[string]any{
		"query": "retry budget",
		"top_k": 5,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)

	require.Len(t, rec.searches, 1)
	require.Equal(t, 5, rec.searches[0].topK)
}

// TestSearchMemoriesRejectsInvalidParams covers the input the tool must refuse
// before storage is reached. The recording store is the assertion: a rejected
// call is one it never saw.
func TestSearchMemoriesRejectsInvalidParams(t *testing.T) {
	tests := []struct {
		name string
		args any
	}{
		{"missing query", map[string]any{}},
		{"blank query", map[string]any{"query": "   "}},
		{"null byte in query", map[string]any{"query": "a\x00b"}},
		{"negative top_k", map[string]any{"query": "retry budget", "top_k": -1}},
		{"top_k above the ceiling", map[string]any{"query": "retry budget", "top_k": maxSearchTopK + 1}},
		{"unknown visibility", map[string]any{"query": "retry budget", "visibility": "world"}},
		{"illegal session_id", map[string]any{"query": "retry budget", "session_id": "not a session id"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingStore{}
			srv := NewServer(rec, *searchConfig(), nil, basisEmbedder(searchDims))

			res, err := srv.handleSearchMemories(context.Background(), searchCallRequest(tt.args))
			require.NoError(t, err, "a caller mistake is a typed tool error, not a transport failure")
			require.True(t, res.IsError, "invalid input must be reported as an error result")

			text := toolText(t, res)
			require.Contains(t, text, errorTypeInvalidParams)

			var payload errorEnvelope
			require.NoError(t, json.Unmarshal([]byte(text), &payload))
			require.Equal(t, errorTypeInvalidParams, payload.Error.Type)
			require.NotEmpty(t, payload.Error.Message)

			require.Empty(t, rec.searches, "storage must not be reached with invalid input")
		})
	}
}

// TestSearchMemoriesReportsAStoreFailureAsToolError covers the other failure
// kind: a search that ran and failed. The cause is logged, not returned, because
// a store error can name a database path.
func TestSearchMemoriesReportsAStoreFailureAsToolError(t *testing.T) {
	rec := &recordingStore{err: errors.New("failed to search memories: /var/lib/synapse/secret.db is locked")}
	srv := NewServer(rec, *searchConfig(), nil, basisEmbedder(searchDims))

	res, err := srv.handleSearchMemories(context.Background(), searchCallRequest(map[string]any{"query": "retry budget"}))
	require.NoError(t, err)
	require.True(t, res.IsError)

	text := toolText(t, res)
	require.Contains(t, text, errorTypeSearchFailed)
	require.NotContains(t, text, "secret.db", "the store's internal cause must not reach the caller")
}

// TestSearchMemoriesWithoutItsDependenciesFailsSafelyNotPanics covers a server
// built without the two things a search needs: it has to answer, because the
// alternative is a process that starts and then dies inside a tool call.
func TestSearchMemoriesWithoutItsDependenciesFailsSafelyNotPanics(t *testing.T) {
	tests := []struct {
		name string
		srv  *Server
	}{
		{"no store and no embedder", NewServer(nil, *searchConfig(), nil, nil)},
		{"no embedder", NewServer(&recordingStore{}, *searchConfig(), nil, nil)},
		{"no store", NewServer(nil, *searchConfig(), nil, basisEmbedder(searchDims))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := tt.srv.handleSearchMemories(context.Background(), searchCallRequest(map[string]any{"query": "retry budget"}))
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Contains(t, toolText(t, res), errorTypeSearchFailed)
		})
	}
}
