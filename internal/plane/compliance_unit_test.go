// Package plane_test's compliance audit tests: the HTTP half of Phase 19, run
// without PostgreSQL.
//
// The endpoint's whole job at this layer is to choose a chain from a verified
// token, gate it on the token's compliance tier, hand a validated window to the
// auditor, put the stored traces back into the response as objects, and record
// every call before it answers -- and every one of those is a thing a fake
// auditor can prove or disprove without a database. What a fake cannot prove is
// that the SQL is right, which is internal/ledger's own test and, end to end,
// compliance_test.go's (build-tagged integration).
package plane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"synapse/internal/plane"
	"synapse/internal/tenant"
	"synapse/internal/trace"

	charmlog "github.com/charmbracelet/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// complianceAuditPath is this endpoint's path, written out here rather than
// shared with the package under test so a route registered at the wrong path
// fails a test instead of moving with it.
const complianceAuditPath = "/v2/compliance/audit"

// complianceRemoteAddr is the address every request in these tests claims, so the
// access record's digest can be checked as a value that is stable, is not the raw
// address, and is not empty.
const complianceRemoteAddr = "203.0.113.7:54321"

// fakeAuditor records what the handler asked for and answers with whatever the
// test scripted. The handler calls it from the test's own goroutine
// (httptest.ResponseRecorder spawns none), so plain fields need no
// synchronisation.
type fakeAuditor struct {
	page      plane.AuditPage
	pageErr   error
	records   []plane.AccessRecord
	recordErr error

	pageCalls int
	tenantID  string
	filter    plane.AuditFilter
}

// AuditPage implements plane.ComplianceAuditor.
func (f *fakeAuditor) AuditPage(_ context.Context, tenantID string, filter plane.AuditFilter) (plane.AuditPage, error) {
	f.pageCalls++
	f.tenantID = tenantID
	f.filter = filter

	return f.page, f.pageErr
}

// RecordAccess implements plane.ComplianceAuditor.
func (f *fakeAuditor) RecordAccess(_ context.Context, record plane.AccessRecord) error {
	f.records = append(f.records, record)

	return f.recordErr
}

// enterpriseToken mints a real token with the tenant layer, carrying the
// compliance tier this endpoint gates on, so these tests exercise the same
// issuer and the same verifier production runs rather than a stub that always
// agrees.
func enterpriseToken(t *testing.T, cfg *plane.PlaneConfig, tier string) string {
	t.Helper()

	token, err := tenant.IssueToken(cfg, tenant.TokenIdentity{
		TenantID: testTenantID,
		Slug:     testTenantSlug,
		Plan:     "enterprise",
		Tier:     tier,
	})
	require.NoError(t, err)

	return token
}

// newComplianceRouter builds the plane's routes with the tenant token middleware
// and the given auditor installed, and returns the captured log output so a test
// can assert what reached it.
func newComplianceRouter(t *testing.T, cfg *plane.PlaneConfig, auditor plane.ComplianceAuditor) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, nil, nil, auditor, tenant.JWTMiddleware(cfg), logger).Routes(), &logs
}

// getComplianceAudit sends GET /v2/compliance/audit with the given Authorization
// header and raw query, each omitted entirely when empty.
func getComplianceAudit(router http.Handler, authorization, rawQuery string) *httptest.ResponseRecorder {
	path := complianceAuditPath
	if rawQuery != "" {
		path += "?" + rawQuery
	}

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = complianceRemoteAddr
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

// complianceBody is the 200 body decoded loosely enough to see the shape of every
// field, which is the point of decoding it this way: "the trace is an object" is
// a claim about JSON types, and a struct with a typed field would assert it by
// construction rather than by observation.
type complianceBody struct {
	Data   []map[string]any `json:"data"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// decodeComplianceBody unmarshals a 200 from the endpoint, reporting the body in
// the failure message so a shape change is readable from the test output.
func decodeComplianceBody(t *testing.T, rec *httptest.ResponseRecorder) complianceBody {
	t.Helper()

	var body complianceBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body: %s", rec.Body.String())

	return body
}

// storedTraceJSON renders one entry's trace the way the compiler does when it
// appends it: a marshalled TraceManifest, stored verbatim.
func storedTraceJSON(t *testing.T, requestID string) string {
	t.Helper()

	encoded, err := json.Marshal(trace.TraceManifest{
		RequestID:        requestID,
		Timestamp:        time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
		DetectedIntent:   "debugging",
		MemoriesCompiled: 1,
		Memories: []trace.TraceMemory{
			{ID: "mem-1", MemoryType: "fact", ContentPreview: "a preview", ScoreTotal: 0.9},
		},
	})
	require.NoError(t, err)

	return string(encoded)
}

// auditRow builds one stored row as an auditor would hand it over.
func auditRow(t *testing.T, id, requestID string) plane.AuditRow {
	t.Helper()

	return plane.AuditRow{
		ID:        id,
		TenantID:  testTenantID,
		RequestID: requestID,
		TraceJSON: storedTraceJSON(t, requestID),
		PrevHash:  "prev-hash",
		HashValue: "hash-value",
		CreatedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}
}

// TestComplianceAuditReturnsStoredTracesAsObjects is the response contract: the
// stored trace_json comes back as a JSON object rather than a string inside JSON,
// the page carries the window's own total, and the paging is echoed back.
func TestComplianceAuditReturnsStoredTracesAsObjects(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{
		page: plane.AuditPage{
			Entries: []plane.AuditRow{
				auditRow(t, "entry-2", "req-2"),
				auditRow(t, "entry-1", "req-1"),
			},
			Total: 7,
		},
	}
	router, _ := newComplianceRouter(t, cfg, auditor)

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	body := decodeComplianceBody(t, rec)
	require.Len(t, body.Data, 2)
	assert.Equal(t, 7, body.Total, "total is the window's size, not the page's")
	assert.Equal(t, 50, body.Limit, "an unqualified request gets the default page size")
	assert.Equal(t, 0, body.Offset)

	entry := body.Data[0]
	assert.Equal(t, "entry-2", entry["id"])
	assert.Equal(t, testTenantID, entry["tenant_id"])
	assert.Equal(t, "req-2", entry["request_id"])
	assert.Equal(t, "prev-hash", entry["prev_hash"])
	assert.Equal(t, "hash-value", entry["hash_value"])

	// The claim this test exists for: a JSON object, not the string the row
	// stores.
	stored, ok := entry["trace"].(map[string]any)
	require.True(t, ok, "trace must be a JSON object, got %T", entry["trace"])
	assert.Equal(t, "req-2", stored["request_id"], "the parsed trace is the one the row stored")
	assert.Equal(t, "debugging", stored["detected_intent"])
	assert.Len(t, stored["memories"], 1)

	assert.Equal(t, testTenantID, auditor.tenantID, "the verified token's own tenant chooses the chain")
	assert.Equal(t, 50, auditor.filter.Limit)
	assert.Nil(t, auditor.filter.Since)
	assert.Nil(t, auditor.filter.Until)

	require.Len(t, auditor.records, 1)
	assert.Equal(t, http.StatusOK, auditor.records[0].ResponseCode, "the record is written before the answer")
}

// TestComplianceAuditRendersAnEmptyHistoryAsAnArray: a tenant with no entries yet
// must get [] and never null, because the published contract is an array and a
// client that iterates should not have to special-case an empty audit history.
func TestComplianceAuditRendersAnEmptyHistoryAsAnArray(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{page: plane.AuditPage{Entries: []plane.AuditRow{}, Total: 0}}
	router, _ := newComplianceRouter(t, cfg, auditor)

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"data":[],"total":0,"limit":50,"offset":0}`, rec.Body.String())
}

// TestComplianceAuditPassesTheWindowThrough asserts the query string reaches the
// auditor as parsed values -- UTC, microsecond-exact -- and that the access record
// carries the same window in a canonical, whitelisted rendering.
func TestComplianceAuditPassesTheWindowThrough(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{page: plane.AuditPage{Entries: []plane.AuditRow{}}}
	router, _ := newComplianceRouter(t, cfg, auditor)

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"),
		"since=2026-09-21T12:00:00.123456Z&until=2026-09-21T13:00:00Z&limit=10&offset=20&content=must-not-be-recorded")

	require.Equal(t, http.StatusOK, rec.Code)

	require.NotNil(t, auditor.filter.Since)
	assert.Equal(t, "2026-09-21T12:00:00.123456Z", auditor.filter.Since.UTC().Format(time.RFC3339Nano),
		"a fractional second survives the round trip, so a caller can name an exact boundary")
	require.NotNil(t, auditor.filter.Until)
	assert.Equal(t, "2026-09-21T13:00:00Z", auditor.filter.Until.UTC().Format(time.RFC3339Nano))
	assert.Equal(t, 10, auditor.filter.Limit)
	assert.Equal(t, 20, auditor.filter.Offset)

	require.Len(t, auditor.records, 1)
	recorded := auditor.records[0].QueryParamsRedacted
	assert.Contains(t, recorded, "since=2026-09-21T12%3A00%3A00.123456Z")
	assert.Contains(t, recorded, "limit=10")
	assert.Contains(t, recorded, "offset=20")
	assert.NotContains(t, recorded, "must-not-be-recorded",
		"an unknown parameter is dropped, never copied into the audit table")

	assert.Len(t, auditor.records[0].IPHash, 64, "the address is recorded as a sha256 digest")
	assert.NotEqual(t, complianceRemoteAddr, auditor.records[0].IPHash, "never as the raw address")
}
