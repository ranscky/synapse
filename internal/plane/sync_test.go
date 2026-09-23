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

// testTenantSlug is the slug the tokens minted in these tests name, and the slug
// the handler must therefore use as the schema to write into.
const testTenantSlug = "my-team"

// fakeMemoryWriter records what the sync handler asked it to store.
type fakeMemoryWriter struct {
	calls     int
	slug      string
	entries   []store.MemoryEntry
	written   int
	sanitized int
	err       error
}

// WriteBatch implements plane.MemoryWriter. The handler calls it synchronously
// from the test's own goroutine (httptest.ResponseRecorder does not spawn one),
// so plain fields need no synchronisation.
func (f *fakeMemoryWriter) WriteBatch(_ context.Context, tenantSlug string, entries []store.MemoryEntry) (int, int, error) {
	f.calls++
	f.slug = tenantSlug
	f.entries = append([]store.MemoryEntry(nil), entries...)

	if f.err != nil {
		return 0, 0, f.err
	}

	return f.written, f.sanitized, nil
}

// newSyncRouter builds the plane's routes with the tenant token middleware and
// the given memory writer installed, and returns the captured log output.
func newSyncRouter(t *testing.T, cfg *plane.PlaneConfig, writer plane.MemoryWriter) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, writer, nil, nil, nil, tenant.JWTMiddleware(cfg), logger, nil).Routes(), &logs
}

// tenantToken mints a real token with the tenant layer, so these tests exercise
// the same verifier production runs rather than a stub that always agrees.
func tenantToken(t *testing.T, cfg *plane.PlaneConfig) string {
	t.Helper()

	token, err := tenant.IssueToken(cfg, tenant.TokenIdentity{
		TenantID: testTenantID,
		Slug:     testTenantSlug,
		Plan:     "team",
		Tier:     "team",
	})
	require.NoError(t, err)

	return token
}

// agentToken mints a real agent-scoped token: the same tenant as tenantToken,
// plus the agent_id and team_id claims Phase 10's memory visibility is evaluated
// against. It goes through tenant.IssueToken for the same reason tenantToken
// does -- these tests exercise the verifier production runs.
func agentToken(t *testing.T, cfg *plane.PlaneConfig, agentID, teamID string) string {
	t.Helper()

	token, err := tenant.IssueToken(cfg, tenant.TokenIdentity{
		TenantID: testTenantID,
		Slug:     testTenantSlug,
		Plan:     "team",
		Tier:     "team",
		AgentID:  agentID,
		TeamID:   teamID,
	})
	require.NoError(t, err)

	return token
}

// postSync sends body to POST /v2/sync/memories with the given Authorization
// header value, omitted entirely when it is empty.
func postSync(router http.Handler, authorization, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v2/sync/memories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

// syncPayload marshals a sync body the way the edge node sends it. It is built
// as a map rather than through the endpoint's own request type so the test
// states the wire format independently of the code that reads it.
func syncPayload(t *testing.T, sessionID, agentID string, memories []store.MemoryEntry) string {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"session_id": sessionID,
		"agent_id":   agentID,
		"memories":   memories,
	})
	require.NoError(t, err)

	return string(raw)
}

func TestSyncMemoriesRequiresAVerifiedTenantToken(t *testing.T) {
	cfg := newConfig(adminToken)
	writer := &fakeMemoryWriter{}
	router, _ := newSyncRouter(t, cfg, writer)
	valid := tenantToken(t, cfg)

	// A token this plane did not sign: same shape, same claims, different secret.
	otherPlane := &plane.PlaneConfig{
		ListenAddr: plane.DefaultListenAddr,
		JWTSecret:  strings.Repeat("x", 48),
		AdminToken: adminToken,
	}
	foreign, err := tenant.IssueToken(otherPlane, tenant.TokenIdentity{
		TenantID: testTenantID,
		Slug:     testTenantSlug,
		Plan:     "team",
		Tier:     "team",
	})
	require.NoError(t, err)

	body := syncPayload(t, "session-1", "edge-agent-1", []store.MemoryEntry{
		{ID: "req-1", Content: "a memory", MemoryType: "fact"},
	})

	headers := map[string]string{
		"no header":                           "",
		"empty bearer":                        "Bearer ",
		"token without the scheme":            valid,
		"a token signed with another secret":  "Bearer " + foreign,
		"a string that is not a token at all": "Bearer not-a-token",
	}

	for name, header := range headers {
		t.Run(name, func(t *testing.T) {
			rec := postSync(router, header, body)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
		})
	}

	assert.Zero(t, writer.calls, "an unverified request stores nothing")
}

func TestSyncMemoriesFailsClosedWithoutATokenMiddleware(t *testing.T) {
	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})
	writer := &fakeMemoryWriter{}

	cfg := newConfig(adminToken)
	router := plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, writer, nil, nil, nil, nil, logger, nil).Routes()

	rec := postSync(router, "Bearer "+tenantToken(t, cfg), syncPayload(t, "session-1", "edge-agent-1", nil))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
	assert.Zero(t, writer.calls)
}

func TestSyncMemoriesStoresTheBatchInTheTokensTenant(t *testing.T) {
	cfg := newConfig(adminToken)
	writer := &fakeMemoryWriter{written: 2, sanitized: 1}
	router, logs := newSyncRouter(t, cfg, writer)
	token := tenantToken(t, cfg)

	// The first memory has no session of its own and the second is still marked
	// pending locally: the honest shape of a batch the edge sends.
	body := syncPayload(t, "batch-session", "edge-agent-1", []store.MemoryEntry{
		{ID: "req-1", Content: "first memory", MemoryType: "fact"},
		{ID: "req-2", Content: "second memory", MemoryType: "fact", SyncStatus: store.SyncStatusSyncPending},
	})

	rec := postSync(router, "Bearer "+token, body)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"written":2,"sanitized":1}`, rec.Body.String())

	require.Equal(t, 1, writer.calls)
	assert.Equal(t, testTenantSlug, writer.slug, "the schema comes from the verified token, never from the body")
	require.Len(t, writer.entries, 2)
	assert.Equal(t, "batch-session", writer.entries[0].SessionID, "a memory with no session of its own takes the batch's")
	assert.Equal(t, "req-1", writer.entries[0].ID, "ids travel untouched; the writer maps them onto the uuid column")
	assert.Equal(t, store.SyncStatusSynced, writer.entries[0].SyncStatus)
	assert.Equal(t, store.SyncStatusSynced, writer.entries[1].SyncStatus, "the plane decides its own copy is synced")

	assert.NotContains(t, logs.String(), "first memory", "memory content is never logged")
	assert.NotContains(t, logs.String(), token, "the presented token is never logged")
	assert.NotContains(t, logs.String(), jwtSecret)
}

func TestSyncMemoriesAcceptsAnEmptyBatch(t *testing.T) {
	cfg := newConfig(adminToken)
	writer := &fakeMemoryWriter{}
	router, _ := newSyncRouter(t, cfg, writer)

	rec := postSync(router, "Bearer "+tenantToken(t, cfg), syncPayload(t, "", "edge-agent-1", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"written":0,"sanitized":0}`, rec.Body.String())
	assert.Equal(t, 1, writer.calls, "an idle batch is a no-op the plane has to acknowledge")
}

func TestSyncMemoriesRejectsBadInput(t *testing.T) {
	cfg := newConfig(adminToken)

	cases := map[string]struct {
		body   string
		reason string
	}{
		"no agent id":                   {`{"agent_id":"","memories":[]}`, "invalid_agent"},
		"missing agent id":              {`{"session_id":"s"}`, "invalid_agent"},
		"memory with no session at all": {`{"agent_id":"a","memories":[{"id":"req-1","content":"x"}]}`, "invalid_memory"},
		"memory with no id":             {`{"agent_id":"a","session_id":"s","memories":[{"content":"x"}]}`, "invalid_memory"},
		"not json":                      {`agent=1`, "invalid_body"},
		"unknown field":                 {`{"agent_id":"a","tenant":"someone-else"}`, "invalid_body"},
		"two json objects":              {`{"agent_id":"a"}{"agent_id":"b"}`, "invalid_body"},
		"wrong type":                    {`{"agent_id":42}`, "invalid_body"},
		"oversized body":                {`{"agent_id":"a","session_id":"s","content":"` + strings.Repeat("a", 1<<20) + `"}`, "invalid_body"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			writer := &fakeMemoryWriter{}
			router, _ := newSyncRouter(t, cfg, writer)

			rec := postSync(router, "Bearer "+tenantToken(t, cfg), tc.body)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"`+tc.reason+`"}`, rec.Body.String())
			assert.Zero(t, writer.calls)
		})
	}
}

func TestSyncMemoriesReportsAStorageFailureAsInternal(t *testing.T) {
	cfg := newConfig(adminToken)
	// Shaped like a pgx error, which quotes the connection target and can carry
	// the password: it must reach the log and never the client.
	writer := &fakeMemoryWriter{err: errors.New(`failed to connect to postgres://synapse:hunter2@db:5432/synapse`)}
	router, logs := newSyncRouter(t, cfg, writer)

	body := syncPayload(t, "session-1", "edge-agent-1", []store.MemoryEntry{
		{ID: "req-1", Content: "a memory", MemoryType: "fact"},
	})

	rec := postSync(router, "Bearer "+tenantToken(t, cfg), body)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())

	assert.Contains(t, logs.String(), "Memory sync write failed")
	assert.NotContains(t, rec.Body.String(), "postgres://")
	assert.NotContains(t, logs.String(), "a memory", "the failure path logs counts, not content")
}

func TestSyncMemoriesWithoutAWriterAnswersInternally(t *testing.T) {
	cfg := newConfig(adminToken)
	router, _ := newSyncRouter(t, cfg, nil)

	body := syncPayload(t, "session-1", "edge-agent-1", []store.MemoryEntry{
		{ID: "req-1", Content: "a memory", MemoryType: "fact"},
	})

	rec := postSync(router, "Bearer "+tenantToken(t, cfg), body)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
}
