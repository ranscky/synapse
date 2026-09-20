package plane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapse/internal/plane"
	"synapse/internal/store"
	"synapse/internal/tenant"

	charmlog "github.com/charmbracelet/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSearcher records what the search handler asked the tenant layer for.
type fakeSearcher struct {
	calls     int
	slug      string
	embedding []float32
	sessionID string
	topK      int
	entries   []store.MemoryEntry
	err       error
}

// Search implements plane.MemorySearcher. The handler calls it synchronously
// from the test's own goroutine (httptest.ResponseRecorder does not spawn one),
// so plain fields need no synchronisation.
func (f *fakeSearcher) Search(_ context.Context, tenantSlug string, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error) {
	f.calls++
	f.slug = tenantSlug
	f.embedding = queryEmbedding
	f.sessionID = sessionID
	f.topK = topK

	return f.entries, f.err
}

// newSearchRouter builds the plane's routes with the tenant token middleware and
// the given searcher installed, and returns the captured log output.
func newSearchRouter(t *testing.T, cfg *plane.PlaneConfig, searcher plane.MemorySearcher) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, searcher, tenant.JWTMiddleware(cfg), logger).Routes(), &logs
}

// searchPayload marshals a search body the way the edge node sends it. It is
// built as a map rather than through the endpoint's own request type so the test
// states the wire format independently of the code that reads it.
func searchPayload(t *testing.T, sessionID, agentID string, embedding []float32, topK int) string {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"query_embedding": embedding,
		"session_id":      sessionID,
		"agent_id":        agentID,
		"top_k":           topK,
	})
	require.NoError(t, err)

	return string(raw)
}

// getSearch sends body to GET /v2/memories/search with the given Authorization
// header value, omitted entirely when it is empty.
func getSearch(router http.Handler, authorization, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v2/memories/search", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

// vector builds the 384-dim embedding every real query is, with one
// recognisable non-zero element.
func vector(i int, v float32) []float32 {
	vec := make([]float32, store.EmbeddingDimensions)
	vec[i] = v

	return vec
}

func TestSearchMemoriesRequiresAVerifiedTenantToken(t *testing.T) {
	cfg := newConfig(adminToken)
	searcher := &fakeSearcher{}
	router, _ := newSearchRouter(t, cfg, searcher)
	valid := tenantToken(t, cfg)

	// A token this plane did not sign: same shape, same claims, other secret.
	otherPlane := &plane.PlaneConfig{
		ListenAddr: plane.DefaultListenAddr,
		JWTSecret:  strings.Repeat("x", 48),
		AdminToken: adminToken,
	}
	foreign, err := tenant.IssueToken(otherPlane, testTenantID, testTenantSlug, "team", "team")
	require.NoError(t, err)

	body := searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20)
	headers := map[string]string{
		"no header":                           "",
		"empty bearer":                        "Bearer ",
		"token without the scheme":            valid,
		"a token signed with another secret":  "Bearer " + foreign,
		"a string that is not a token at all": "Bearer not-a-token",
	}

	for name, header := range headers {
		t.Run(name, func(t *testing.T) {
			rec := getSearch(router, header, body)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
		})
	}

	assert.Zero(t, searcher.calls, "an unverified request reads nothing")
}

func TestSearchMemoriesFailsClosedWithoutATokenMiddleware(t *testing.T) {
	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})
	searcher := &fakeSearcher{}

	cfg := newConfig(adminToken)
	router := plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, searcher, nil, logger).Routes()

	rec := getSearch(router, "Bearer "+tenantToken(t, cfg), searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
	assert.Zero(t, searcher.calls)
}

func TestSearchMemoriesReadsTheTokensTenant(t *testing.T) {
	cfg := newConfig(adminToken)
	searcher := &fakeSearcher{entries: []store.MemoryEntry{
		{
			ID: "11111111-1111-1111-1111-111111111111", SessionID: "session-1",
			Content: "org memory", MemoryType: "fact", Importance: 0.9,
			SyncStatus: store.SyncStatusSynced, AgentID: "edge-agent-2",
			Embedding: vector(3, 1),
		},
	}}
	router, logs := newSearchRouter(t, cfg, searcher)

	rec := getSearch(router, "Bearer "+tenantToken(t, cfg), searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	// The response is the store's own entry serialization, so agent_id and the
	// embedding survive a real JSON round trip -- the edge needs both.
	var body struct {
		Memories []store.MemoryEntry `json:"memories"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Memories, 1)
	assert.Equal(t, "org memory", body.Memories[0].Content)
	assert.Equal(t, "edge-agent-2", body.Memories[0].AgentID)
	assert.Equal(t, store.SyncStatusSynced, body.Memories[0].SyncStatus)
	require.Len(t, body.Memories[0].Embedding, store.EmbeddingDimensions)

	// The tenant is the token's slug, and the query is what the caller sent.
	require.Equal(t, 1, searcher.calls)
	assert.Equal(t, testTenantSlug, searcher.slug, "the schema searched is the signed tenant, never anything in the body")
	assert.Equal(t, "session-1", searcher.sessionID)
	assert.Equal(t, 20, searcher.topK)
	require.Len(t, searcher.embedding, store.EmbeddingDimensions)
	assert.Equal(t, float32(1), searcher.embedding[1])

	// Logs name the tenant and the agent, never a memory's content or a token.
	assert.Contains(t, logs.String(), testTenantSlug)
	assert.NotContains(t, logs.String(), "org memory")
	assert.NotContains(t, logs.String(), jwtSecret)
}

func TestSearchMemoriesAnswersAnEmptyTenantWithAnEmptyArray(t *testing.T) {
	cfg := newConfig(adminToken)
	searcher := &fakeSearcher{} // no entries: the store returns a nil slice
	router, _ := newSearchRouter(t, cfg, searcher)

	rec := getSearch(router, "Bearer "+tenantToken(t, cfg), searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"memories":[]}`, rec.Body.String(), "a nil slice must not reach a client as null")
}

func TestSearchMemoriesRejectsMalformedRequests(t *testing.T) {
	cfg := newConfig(adminToken)

	cases := []struct {
		name       string
		body       string
		wantReason string
	}{
		{"not json", "not json at all", "invalid_body"},
		{"an unknown field", `{"agent_id":"a","top_k":1,"tenant_slug":"other"}`, "invalid_body"},
		{"no agent", searchPayload(t, "session-1", "", vector(1, 1), 20), "invalid_agent"},
		{"top_k beyond the cap", searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 501), "invalid_top_k"},
		{"an embedding the column cannot hold", searchPayload(t, "session-1", "edge-agent-1", []float32{1, 2, 3}, 20), "invalid_embedding"},
		{"an empty body", "", "invalid_body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			searcher := &fakeSearcher{}
			router, _ := newSearchRouter(t, cfg, searcher)

			rec := getSearch(router, "Bearer "+tenantToken(t, cfg), tc.body)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"`+tc.wantReason+`"}`, rec.Body.String())
			assert.Zero(t, searcher.calls, "a rejected request reads nothing")
		})
	}

	// The legal edge of the same shapes is accepted: no embedding (the store
	// falls back to recency) and a zero top_k (the store applies its default).
	searcher := &fakeSearcher{}
	router, _ := newSearchRouter(t, cfg, searcher)

	rec := getSearch(router, "Bearer "+tenantToken(t, cfg), searchPayload(t, "session-1", "edge-agent-1", nil, 0))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, searcher.calls)
	assert.Nil(t, searcher.embedding)
	assert.Zero(t, searcher.topK)
}

func TestSearchMemoriesWithoutASearcherAnswersInternal(t *testing.T) {
	cfg := newConfig(adminToken)
	router, _ := newSearchRouter(t, cfg, nil)

	rec := getSearch(router, "Bearer "+tenantToken(t, cfg), searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
}

func TestSearchMemoriesReportsAFailureWithoutLeakingIt(t *testing.T) {
	cfg := newConfig(adminToken)
	searcher := &fakeSearcher{err: errors.New("store: query memories: cannot reach postgres://synapse:synapse@127.0.0.1:5432/synapse")}
	router, logs := newSearchRouter(t, cfg, searcher)

	rec := getSearch(router, "Bearer "+tenantToken(t, cfg), searchPayload(t, "session-1", "edge-agent-1", vector(1, 1), 20))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())

	// The cause is kept server-side, and the response body never quotes it.
	assert.Contains(t, logs.String(), "Memory search failed")
	assert.NotContains(t, rec.Body.String(), "postgres://")
	assert.NotContains(t, rec.Body.String(), "127.0.0.1:5432")
}
