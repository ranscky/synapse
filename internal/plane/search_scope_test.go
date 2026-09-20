// Phase 10's read-path visibility tests: the scope a search is served with comes
// from the verified token and from nowhere else.
//
// Split out of search_test.go to stay inside the 300-line ceiling, and because
// this is a security property rather than a description of the endpoint: what a
// caller may read is decided by what its token says, never by what its body
// claims. The plane's half of that contract is here; the SQL half is in
// internal/store's visibility_test.go.
package plane_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSearchMemoriesTakesTheScopeFromTheToken asserts the handler hands the
// tenant layer exactly the identity its token carries: the agent and team claims
// Phase 10 evaluates memory visibility against, plus the tenant from the slug and
// the session from the request.
func TestSearchMemoriesTakesTheScopeFromTheToken(t *testing.T) {
	cfg := newConfig(adminToken)
	searcher := &fakeSearcher{}
	router, _ := newSearchRouter(t, cfg, searcher)

	rec := getSearch(router, "Bearer "+agentToken(t, cfg, "edge-agent-1", "team-blue"),
		searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, searcher.calls)
	assert.Equal(t, testTenantSlug, searcher.slug)
	assert.Equal(t, "edge-agent-1", searcher.agentID, "the agent is the token's claim")
	assert.Equal(t, "team-blue", searcher.teamID, "the team is the token's claim")
	assert.Equal(t, "session-1", searcher.sessionID, "the session is what the request is allowed to name")
}

// TestSearchMemoriesIgnoresTheBodyScope is the test the whole scope model rests
// on: a body cannot widen a search.
//
// The token here is tenant-level, so it names no agent, and the body claims to be
// a specific agent in a specific team. Nothing about the body reaches the data
// layer as a filter -- if it did, any node could read any other agent's private
// memories by typing that agent's name into a request.
func TestSearchMemoriesIgnoresTheBodyScope(t *testing.T) {
	cfg := newConfig(adminToken)
	searcher := &fakeSearcher{}
	router, _ := newSearchRouter(t, cfg, searcher)

	raw, err := json.Marshal(map[string]any{
		"query_embedding": vector(1, 1),
		"session_id":      "session-1",
		"agent_id":        "agent_a",
		"team_id":         "team-blue",
		"top_k":           20,
	})
	require.NoError(t, err)

	rec := getSearch(router, "Bearer "+tenantToken(t, cfg), string(raw))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, searcher.calls)
	assert.Empty(t, searcher.agentID,
		"the body's agent_id is attribution; a request must not be able to name the agent it is filtered by")
	assert.Empty(t, searcher.teamID, "the same for the body's team_id")
	assert.Equal(t, "session-1", searcher.sessionID)
}

// TestSearchMemoriesRefusesABodyThatNamesAnotherAgent covers the other side of
// the same rule: an agent-scoped token is that agent's credential, so a request
// claiming to be a different agent is refused rather than served as the token's
// owner -- a node configured with the wrong agent-id finds out instead of
// quietly reading the wrong scope. A tenant-level token is not agent-scoped, so
// it may legitimately be presented by any of a tenant's nodes, each of which is
// then an org-only reader.
func TestSearchMemoriesRefusesABodyThatNamesAnotherAgent(t *testing.T) {
	cfg := newConfig(adminToken)
	searcher := &fakeSearcher{}
	router, _ := newSearchRouter(t, cfg, searcher)
	token := agentToken(t, cfg, "edge-agent-1", "")

	rec := getSearch(router, "Bearer "+token, searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20))
	require.Equal(t, http.StatusOK, rec.Code, "the token's own agent is the one name that is accepted")
	require.Equal(t, 1, searcher.calls)

	rec = getSearch(router, "Bearer "+token, searchPayload(t, "session-1", "edge-agent-2", vector(1, 1), 20))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"invalid_agent"}`, rec.Body.String())
	assert.Equal(t, 1, searcher.calls, "a refused request reads nothing")

	rec = getSearch(router, "Bearer "+tenantToken(t, cfg), searchPayload(t, "session-1", "edge-agent-2", vector(1, 1), 20))
	require.Equal(t, http.StatusOK, rec.Code, "a tenant-level token is not agent-scoped, so any node may present it")
	assert.Equal(t, 2, searcher.calls)
	assert.Empty(t, searcher.agentID, "and it reads as an org-only reader")
}
