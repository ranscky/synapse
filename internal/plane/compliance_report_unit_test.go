// Package plane_test's compliance report tests: the HTTP half of Phase 20, run
// without PostgreSQL.
//
// The endpoint's job at this layer is to choose a window from a verified token,
// gate it on the token's compliance tier, hand the window to the auditor, merge
// the chain verdict the verifier returns, put the Article 50 statement on the
// report, and render it as JSON or as PDF -- every one of which a fake auditor and
// a fake verifier can prove or disprove without a database. What a fake cannot
// prove is that the SQL is right (internal/ledger's own tests) or that real stored
// traces add up to these numbers (compliance_report_integration_test.go, build tag
// integration).
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

	charmlog "github.com/charmbracelet/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// complianceReportPath is this endpoint's path, written out here rather than
// shared with the package under test so a route registered at the wrong path fails
// a test instead of moving with it.
const complianceReportPath = "/v2/compliance/report"

// fakeVerifier is the report endpoint's chain-verification double: it answers with
// whatever verdict the test scripted and records what it was asked about, so the
// endpoint's merge of the verdict into chain_integrity can be asserted without a
// tenant signing secret.
type fakeVerifier struct {
	result   plane.ChainIntegrityResult
	err      error
	calls    int
	tenantID string
}

// VerifyChain implements plane.LedgerVerifier.
func (f *fakeVerifier) VerifyChain(_ context.Context, tenantID string) (plane.ChainIntegrityResult, error) {
	f.calls++
	f.tenantID = tenantID

	return f.result, f.err
}

// ReportFacts implements plane.ComplianceAuditor, on the fake declared next to
// Phase 19's tests.
//
// It records the window it was handed rather than merely answering, because the
// endpoint's window handling is a claim about what was read: the integration test
// proves the SQL honours it, and this proves the handler passed it on.
func (f *fakeAuditor) ReportFacts(_ context.Context, tenantID string, window plane.ReportWindow) (plane.ReportFacts, error) {
	f.factsCalls++
	f.factsTenant = tenantID
	f.window = window

	return f.facts, f.factsErr
}

// newComplianceReportRouter builds the plane's routes with the tenant token
// middleware, the given auditor, and the given chain verifier installed, and
// returns the captured log output so a test can assert what reached it.
func newComplianceReportRouter(t *testing.T, cfg *plane.PlaneConfig, auditor plane.ComplianceAuditor, verifier plane.LedgerVerifier) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, nil, verifier, auditor, tenant.JWTMiddleware(cfg), logger, nil).Routes(), &logs
}

// getComplianceReport sends GET /v2/compliance/report with the given Authorization
// header and raw query, each omitted entirely when empty.
func getComplianceReport(router http.Handler, authorization, rawQuery string) *httptest.ResponseRecorder {
	path := complianceReportPath
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

// reportFacts builds the raw material one report request is answered from: one
// stored trace, plus metering totals -- so the metering-first summary rule is
// observable without a database.
func reportFacts(t *testing.T) plane.ReportFacts {
	t.Helper()

	return plane.ReportFacts{
		Rows: []plane.AuditRow{auditRow(t, "entry-1", "req-1")},
		Usage: plane.UsageTotals{
			Compilations:    3,
			AvgReductionPct: 41.5,
			HasRows:         true,
		},
	}
}

// TestComplianceReportAnswersEverySectionWithTheArticle50Statement is the phase's
// JSON contract: one enterprise request answered with every section the brief
// names -- header, summary, breakdown, global brain, conflicts, supersession, chain
// integrity -- and a non-empty Article 50 statement carrying the real document.
//
// The body is decoded loosely (`map[string]any`) rather than into the package's own
// struct, because "all the required fields are present" is a claim about the JSON a
// client receives: decoding into the struct that produced it would assert the tags
// by construction.
func TestComplianceReportAnswersEverySectionWithTheArticle50Statement(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{facts: reportFacts(t)}
	verifier := &fakeVerifier{result: plane.ChainIntegrityResult{
		EntriesChecked: 7,
		ChainValid:     true,
		CheckedAt:      time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC),
	}}
	router, _ := newComplianceReportRouter(t, cfg, auditor, verifier)

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "format=json")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body: %s", rec.Body.String())

	// Every section the report is defined to carry, by name.
	for _, section := range []string{
		"header", "summary", "memory_type_breakdown", "global_brain",
		"conflict_resolution", "supersession", "chain_integrity", "article_50_statement",
	} {
		assert.Contains(t, body, section, "the report must carry a %q section", section)
	}

	header, ok := body["header"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, testTenantID, header["tenant_id"], "the verified token's own tenant")
	assert.Equal(t, plane.Version, header["synapse_version"])
	assert.NotEmpty(t, header["generated_at"])
	assert.Contains(t, header, "period")

	summary, ok := body["summary"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(3), summary["total_compilations"], "the metering count wins when the window has rows")
	assert.Equal(t, float64(1), summary["total_memories_used"], "the trace's own memories_compiled")
	assert.Equal(t, 41.5, summary["avg_reduction_pct"])

	// The breakdown is an object, never null: an empty period has an empty one.
	_, ok = body["memory_type_breakdown"].(map[string]any)
	assert.True(t, ok, "memory_type_breakdown must be a JSON object, got %T", body["memory_type_breakdown"])

	statement, ok := body["article_50_statement"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, statement)
	assert.Equal(t, plane.Article50Statement, statement,
		"the statement is this build's constant, verbatim")
	assert.Contains(t, statement, "does not constitute legal advice",
		"the disclaimer is the part a reader must not lose")
	assert.Contains(t, statement, "2024/1689")

	chain, ok := body["chain_integrity"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, chain["valid"], "the walk's verdict")
	assert.Equal(t, float64(1), chain["entries_in_period"], "the window's own count, not the chain's length")
	assert.Equal(t, "2026-09-21T13:00:00Z", chain["last_verified_at"])

	assert.Equal(t, 1, verifier.calls)
	assert.Equal(t, testTenantID, verifier.tenantID, "the verified token's own chain is walked")

	assert.Equal(t, 1, auditor.factsCalls)
	assert.Equal(t, testTenantID, auditor.factsTenant)
	assert.Nil(t, auditor.window.Since, "no since means the whole ledger, not a default window")
	assert.Nil(t, auditor.window.Until)

	require.Len(t, auditor.records, 1, "the read is recorded before it is answered")
	assert.Equal(t, http.StatusOK, auditor.records[0].ResponseCode)
	assert.Equal(t, complianceReportPath, auditor.records[0].Endpoint)
	assert.Contains(t, auditor.records[0].QueryParamsRedacted, "format=json")
}
