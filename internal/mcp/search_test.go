package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"synapse/internal/config"
	"synapse/internal/store"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

const (
	// searchDims is the embedding width every backend stores.
	searchDims = 384

	// searchSessionID is the session the seeded memories live in. A search does
	// not require a session -- naming one is how a caller narrows it -- but the
	// standalone store's Search can only address one session, so the DoD test
	// names it rather than asserting against an empty result set.
	searchSessionID = "sess-phase24"

	// searchQuery is the question memory 2 answers. Its first byte picks the
	// basis vector the test's embedder returns for it, which is what makes
	// memory 2 the nearest of the three.
	searchQuery = "how many retry attempts does the order handler make"
)

// scoreKeys are the five score fields the brief requires on every result, plus
// the provenance fields a caller needs to interpret them. They are asserted as
// present keys rather than decoded struct fields on purpose: "score_s: 0" and
// "no score_s key at all" decode identically into a struct, and only the former
// is a 4-Factor breakdown.
var scoreKeys = []string{
	"id", "content", "memory_type", "agent_id", "cross_agent",
	"conflict_status", "created_at", "session_id", "visibility",
	"score_s", "score_r", "score_i", "score_t", "score_total",
}

// searchCallRequest builds a CallToolRequest the way an MCP client would.
func searchCallRequest(args any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Name = searchToolName
	req.Params.Arguments = args
	return req
}

// searchInProcessClient starts an in-process MCP client against srv and returns
// it. Everything except the transport is the production path.
func searchInProcessClient(t *testing.T, srv *Server) *client.Client {
	t.Helper()

	ctx := context.Background()
	c, err := client.NewInProcessClient(srv.mcp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, initializeRequest())
	require.NoError(t, err)

	return c
}

// searchResult is the decoded payload, with the memory objects kept as maps so
// the presence of every score key can be asserted.
type searchResult struct {
	TraceID  string           `json:"trace_id"`
	Memories []map[string]any `json:"memories"`
}

// decodeSearchResult decodes a successful search response.
func decodeSearchResult(t *testing.T, res *mcpgo.CallToolResult) searchResult {
	t.Helper()

	require.False(t, res.IsError, "a valid search must not be reported as an error")

	var got searchResult
	require.NoError(t, json.Unmarshal([]byte(toolText(t, res)), &got))
	return got
}

// newSearchStore writes three memories into a fresh SQLite store, all of the
// same type and timestamp so that only the semantic factor can separate them.
//
// Memory 2 is stored with the query's own embedding (cosine similarity 1.0) and
// memories 1 and 3 with distinct basis vectors (0.0), which is what the DoD
// assertion -- memory 2 ranked first, with a strictly higher score_total -- is
// measuring. The write goes through the real store, so the memories a search
// reads back have been through the same sanitization pass the REST write path
// applies.
func newSearchStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.NewStore(filepath.Join(t.TempDir(), "mcp-search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	queryAxis := int(searchQuery[0]) % searchDims
	contents := []string{
		"the staging database is rebuilt every night",
		"the retry budget for the order handler is three attempts",
		"the marketing site is deployed from its own repository",
	}
	// axes[1] is the query's own axis; the others sit two steps away from it.
	axes := []int{(queryAxis + 1) % searchDims, queryAxis, (queryAxis + 2) % searchDims}

	// One instant for all three: equal recency, equal importance (same memory
	// type), equal task alignment (same intent and type), so score_total
	// differs by exactly the 0.4-weighted semantic factor.
	at := time.Now().UTC().Truncate(time.Millisecond)

	ctx := context.Background()
	for i, content := range contents {
		entry := store.MemoryEntry{
			ID:         fmt.Sprintf("mem-%d", i+1),
			SessionID:  searchSessionID,
			Content:    content,
			MemoryType: "context",
			Timestamp:  at,
			Importance: 0.5,
			Embedding:  basisVector(searchDims, axes[i]),
		}
		require.NoError(t, st.Write(ctx, entry))
	}

	return st
}

// TestSearchMemoriesRanksNearestMemoryFirstWithItsScoreBreakdown is this phase's
// DoD test: three memories in a real store, one query, and the memory the query
// is close to coming back first -- carrying all five score fields, because the
// point of the tool is the explanation, not only the rank.
func TestSearchMemoriesRanksNearestMemoryFirstWithItsScoreBreakdown(t *testing.T) {
	st := newSearchStore(t)

	// No compile pipeline: a search must not depend on one.
	srv := NewServer(st, *config.DefaultConfig(), nil, basisEmbedder(searchDims))
	c := searchInProcessClient(t, srv)

	res, err := c.CallTool(context.Background(), searchCallRequest(map[string]any{
		"query":      searchQuery,
		"session_id": searchSessionID,
	}))
	require.NoError(t, err)

	got := decodeSearchResult(t, res)
	require.NotEmpty(t, got.TraceID, "every answer carries the trace id that ties it to one log line")
	require.Len(t, got.Memories, 3, "the default top_k must not truncate a three-memory brain")

	// The DoD ranking assertion.
	require.Equal(t, "mem-2", got.Memories[0]["id"], "the memory the query is close to must rank first")

	// Every result carries every field, scores included, zero or not.
	for i, memory := range got.Memories {
		for _, key := range scoreKeys {
			require.Contains(t, memory, key, "memories[%d] is missing %q -- it must be present even when zero", i, key)
		}
		_, isFloat := memory["score_total"].(float64)
		require.True(t, isFloat, "memories[%d].score_total must be a JSON number", i)
	}

	// A zero must be a zero and not a missing key: the two memories the query is
	// not close to score exactly 0.0 on the semantic factor.
	require.Equal(t, 1.0, got.Memories[0]["score_s"])
	require.Equal(t, 0.0, got.Memories[1]["score_s"])
	require.Equal(t, 0.0, got.Memories[2]["score_s"])

	// The DoD score assertion: the ranked-first memory scores strictly higher
	// than both others.
	first := got.Memories[0]["score_total"].(float64)
	for _, other := range got.Memories[1:] {
		require.Greater(t, first, other["score_total"].(float64),
			"the top result's score_total must beat %v's", other["id"])
	}

	// Provenance: nothing here is cross-agent, nothing is in conflict, and the
	// local backend's blank visibility reads as the column's own default.
	require.Equal(t, false, got.Memories[0]["cross_agent"])
	require.Equal(t, store.ConflictStatusNone, got.Memories[0]["conflict_status"])
	require.Equal(t, store.VisibilityOrg, got.Memories[0]["visibility"])
	require.Equal(t, searchSessionID, got.Memories[0]["session_id"])
}

// TestSearchMemoriesHonorsTopK covers top_k's contract: it caps the answer, and
// the cap is applied to the 4-Factor ranking.
func TestSearchMemoriesHonorsTopK(t *testing.T) {
	st := newSearchStore(t)
	srv := NewServer(st, *config.DefaultConfig(), nil, basisEmbedder(searchDims))
	c := searchInProcessClient(t, srv)

	res, err := c.CallTool(context.Background(), searchCallRequest(map[string]any{
		"query":      searchQuery,
		"top_k":      2,
		"session_id": searchSessionID,
	}))
	require.NoError(t, err)

	got := decodeSearchResult(t, res)
	require.Len(t, got.Memories, 2)
	require.Equal(t, "mem-2", got.Memories[0]["id"], "the cap must keep the best-ranked memory")
}

// TestSearchMemoriesToolIsRegisteredAndAdvertisesItsSchema drives the real
// protocol through mcp-go's in-process client: a tool that is registered but not
// callable (or vice versa) fails here rather than in an editor.
func TestSearchMemoriesToolIsRegisteredAndAdvertisesItsSchema(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	c := searchInProcessClient(t, srv)

	tools, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	require.NoError(t, err)

	var found *mcpgo.Tool
	for i := range tools.Tools {
		if tools.Tools[i].Name == searchToolName {
			found = &tools.Tools[i]
			break
		}
	}
	require.NotNil(t, found, "tools/list must advertise synapse_search_memories")
	require.Equal(t, searchToolDescription, found.Description)
	require.Contains(t, found.InputSchema.Required, "query")
	for _, optional := range []string{"top_k", "visibility", "session_id"} {
		require.NotContains(t, found.InputSchema.Required, optional, "%s is optional", optional)
		require.Contains(t, found.InputSchema.Properties, optional)
	}

	// The tool reads and never writes, and it says so: mcp-go's defaults mark
	// every tool destructive, which is right for synapse_compile and wrong here.
	require.NotNil(t, found.Annotations.ReadOnlyHint)
	require.True(t, *found.Annotations.ReadOnlyHint, "a search must advertise itself as read-only")
	require.NotNil(t, found.Annotations.DestructiveHint)
	require.False(t, *found.Annotations.DestructiveHint, "a search must not advertise itself as destructive")

	// Listed and callable are two different maps inside mcp-go; only a call
	// proves both are populated. This server has no store, so the answer is the
	// typed failure it must be -- not a panic and not silence.
	res, err := c.CallTool(ctx, searchCallRequest(map[string]any{"query": "anything"}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, toolText(t, res), errorTypeSearchFailed)
}
