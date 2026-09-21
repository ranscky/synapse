//go:build integration

// Package plane_test's compliance report integration test: GET /v2/compliance/report over a
// real PostgreSQL, from real signed ledger entries to the numbers in the report.
//
// What this is evidence for, and why a test double will not do it: the SQL that reads the
// window, the arithmetic applied to traces as they were actually stored and marshalled, the
// metering table's override of the summary's two metering-facing numbers, the chain walk
// behind chain_integrity, and the access-log rows every call leaves behind. Every one of
// those lives in internal/ledger's implementation or in the tables' own DDL, so this test
// wires the real ledger.Auditor and the real verifier -- the untagged tests next to it cover
// what a fake can, which is this endpoint's own HTTP logic.
//
// Build-tagged integration because a real Postgres is required and testcontainers-go is not a
// dependency of this module. The database is a precondition, not an option: an unreachable
// one fails this test instead of skipping it, because a skipped compliance test reports
// success without having read a single row. SYNAPSE_MASTER_KEY is set by the test itself, so
// the documented command exports nothing else.
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeReport parses a 200 from the endpoint into the report struct, reporting the body in
// the failure message so a shape change is readable from the test output.
func decodeReport(t *testing.T, rec *httptest.ResponseRecorder) plane.ComplianceReport {
	t.Helper()

	var report plane.ComplianceReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report), "body: %s", rec.Body.String())

	return report
}

// TestComplianceReport is Phase 20's definition of done against a real PostgreSQL: an
// enterprise tenant's own signed chain summarised into every section of the report, the
// metering table preferred when it has rows for the window, a bounded window narrowing the
// numbers, a team tenant refused with the upsell body, a PDF rendered from the same data, and
// every one of those calls recorded in the access log.
func TestComplianceReport(t *testing.T) {
	pool := compliancePool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))

	// The signing secret is wrapped under this master key, so the ledger's write path and the
	// chain verifier can fetch and unwrap it.
	t.Setenv(plane.EnvMasterKey, complianceMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	enterprise := provisionReportTenant(t, ctx, provisioner, "enterprise", "enterprise")

	fixtures := appendReportEntries(t, ctx, pool, enterprise.tenantID)
	require.Len(t, fixtures, reportEntryCount)

	router, logs := newReportRouter(t, cfg, pool, provisioner)

	// --- the JSON report, computed from the signed ledger ----------------------
	rec := reportRequest(router, enterprise.jwt, "format=json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	report := decodeReport(t, rec)
	assert.Equal(t, enterprise.tenantID, report.Header.TenantID)
	assert.Equal(t, plane.Version, report.Header.SynapseVersion)
	assert.False(t, report.Header.GeneratedAt.IsZero())
	assert.Nil(t, report.Header.Period.Since, "no since means the whole ledger, echoed as null")
	assert.Nil(t, report.Header.Period.Until)
	assert.Equal(t, plane.Article50Statement, report.Article50Statement)

	// No metering rows yet, so the ledger is the source: one compilation per entry, the
	// memories the traces themselves claim, and the reduction the traces themselves carry.
	assert.Equal(t, reportEntryCount, report.Summary.TotalCompilations)
	assert.Equal(t, 9, report.Summary.TotalMemoriesUsed, "2+2+2+1+2, the traces' own memories_compiled")
	assert.Equal(t, 30.0, report.Summary.AvgReductionPct, "the mean of the five recorded traces' 30%")

	// Seven used memories: four facts and three decisions. The superseded memory and the
	// older half of the contradiction were not used, so they are not in the denominator.
	assert.Equal(t, map[string]float64{"fact": 57.14, "decision": 42.86}, report.MemoryTypeBreakdown)

	assert.Equal(t, 2, report.GlobalBrain.CrossAgentRetrievals)
	assert.Equal(t, 2, report.GlobalBrain.UniqueContributingAgents, "agent-b and agent-c")

	assert.Equal(t, 1, report.ConflictResolution.ContradictionsDetected,
		"the pair is one detection: the older half is not counted again")
	assert.Equal(t, 1, report.Supersession.MemoriesSuperseded)

	assert.True(t, report.ChainIntegrity.Valid, "the chain just written must verify")
	assert.Equal(t, reportEntryCount, report.ChainIntegrity.EntriesInPeriod)
	assert.False(t, report.ChainIntegrity.LastVerifiedAt.IsZero())

	// --- a bounded window narrows the numbers ---------------------------------
	// The newest entry's own created_at, sent to the microsecond, is an inclusive lower bound
	// that lands on that entry rather than near it.
	boundary := fixtures[reportEntryCount-1].entry.CreatedAt.UTC().Format(time.RFC3339Nano)
	windowed := decodeReport(t, reportRequest(router, enterprise.jwt, "since="+boundary))
	assert.Equal(t, 1, windowed.ChainIntegrity.EntriesInPeriod)
	assert.Equal(t, 1, windowed.Summary.TotalCompilations)
	assert.Equal(t, 2, windowed.Summary.TotalMemoriesUsed)
	assert.Equal(t, map[string]float64{"fact": 100}, windowed.MemoryTypeBreakdown)
	assert.True(t, windowed.ChainIntegrity.Valid, "the verdict covers the whole chain, not the window")
	require.NotNil(t, windowed.Header.Period.Since)
	assert.Equal(t, boundary, windowed.Header.Period.Since.UTC().Format(time.RFC3339Nano))

	// --- both reads are in the access log, against this endpoint --------------
	accessLog := readAccessLog(t, ctx, pool, enterprise.tenantID)
	require.Len(t, accessLog, 2, "the unqualified read and the windowed one")

	for i, row := range accessLog {
		assert.Equal(t, complianceReportPath, row.Endpoint, "row %d", i)
		assert.Equal(t, http.StatusOK, row.Code, "row %d", i)
		assert.Len(t, row.IPHash, 64, "row %d: the address is recorded as a sha256 digest", i)
	}

	assert.Contains(t, accessLog[0].Params, "format=json")
	assert.Contains(t, accessLog[1].Params, "since=", "the window the caller asked for is recorded")

	assert.NotContains(t, logs.String(), enterprise.jwt, "the presented token is never logged")
	assert.NotContains(t, logs.String(), jwtSecret, "nor the key that signs them")
}

// TestComplianceReportPrefersMeteringAndRefusesATeamTenant continues the phase's
// definition of done over the same fixtures: the metering table wins for the two numbers it
// owns, the ledger still answers for the rest, the PDF is the same report rendered, a team
// tenant is refused, and every call is in the access log.
//
// It is a second function rather than a longer one because the first half is the report's
// arithmetic and this half is the report's two other outputs -- and because the metering
// rows it inserts would otherwise change the numbers the first half asserts.
func TestComplianceReportPrefersMeteringAndRefusesATeamTenant(t *testing.T) {
	pool := compliancePool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))
	t.Setenv(plane.EnvMasterKey, complianceMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	enterprise := provisionReportTenant(t, ctx, provisioner, "enterprise", "enterprise")
	team := provisionReportTenant(t, ctx, provisioner, "team", "team")

	require.Len(t, appendReportEntries(t, ctx, pool, enterprise.tenantID), reportEntryCount)

	// Two metering rows with reductions of 10% and 20%: the report's compilations and mean
	// reduction become the metering table's, while the chain coverage and the memories used
	// stay the ledger's -- they are different quantities, and the report says so by reporting
	// different numbers.
	insertUsageEvents(t, ctx, pool, enterprise.tenantID, 10, 20)

	router, logs := newReportRouter(t, cfg, pool, provisioner)

	metered := decodeReport(t, reportRequest(router, enterprise.jwt, "format=json"))
	assert.Equal(t, 2, metered.Summary.TotalCompilations, "the metering count, not the five ledger entries")
	assert.Equal(t, 15.0, metered.Summary.AvgReductionPct, "the metering mean")
	assert.Equal(t, reportEntryCount, metered.ChainIntegrity.EntriesInPeriod, "the ledger still says five")
	assert.Equal(t, 9, metered.Summary.TotalMemoriesUsed, "memories used is always the traces' own count")

	// --- a team tenant is refused, and the refusal is recorded ----------------
	refused := reportRequest(router, team.jwt, "format=json")
	require.Equal(t, http.StatusForbidden, refused.Code)
	assert.JSONEq(t, `{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}`,
		refused.Body.String())
	assert.NotContains(t, refused.Body.String(), enterprise.tenantID,
		"a refused caller is told nothing about another tenant")

	// --- the PDF is the same report, rendered ---------------------------------
	pdf := reportRequest(router, enterprise.jwt, "format=pdf")

	switch pdf.Code {
	case http.StatusServiceUnavailable:
		// No HTML-to-PDF renderer on this host: the documented answer, and the assertion is
		// that it is the documented body rather than a generic 500.
		assert.JSONEq(t,
			`{"error":"pdf_tool_unavailable","message":"install wkhtmltopdf to enable PDF reports"}`,
			pdf.Body.String())
		t.Log("no HTML-to-PDF renderer installed: asserted the documented 503 instead of a PDF")
	default:
		require.Equal(t, http.StatusOK, pdf.Code, pdf.Body.String())
		assert.Equal(t, "application/pdf", pdf.Header().Get("Content-Type"))
		assert.Contains(t, pdf.Header().Get("Content-Disposition"), "compliance-report")
		require.Greater(t, pdf.Body.Len(), 0, "a PDF with no bytes is not a PDF")
		assert.True(t, bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF-")),
			"the body must be a PDF: got %q", pdf.Body.String()[:min(8, pdf.Body.Len())])
	}

	// --- every call above is in the access log --------------------------------
	accessLog := readAccessLog(t, ctx, pool, enterprise.tenantID)
	require.Len(t, accessLog, 2, "the JSON report and the PDF request")

	for i, row := range accessLog {
		assert.Equal(t, complianceReportPath, row.Endpoint, "row %d", i)
		assert.Len(t, row.IPHash, 64, "row %d: the address is recorded as a sha256 digest", i)
	}

	assert.Equal(t, http.StatusOK, accessLog[0].Code)
	assert.Contains(t, accessLog[0].Params, "format=json", "the format actually used is recorded")
	assert.Contains(t, accessLog[1].Params, "format=pdf")
	require.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, accessLog[1].Code,
		"the PDF call's own outcome is what its record says")

	teamLog := readAccessLog(t, ctx, pool, team.tenantID)
	require.Len(t, teamLog, 1, "a refused call is recorded too: the attempt is the audit fact")
	assert.Equal(t, http.StatusForbidden, teamLog[0].Code)
	assert.Equal(t, complianceReportPath, teamLog[0].Endpoint)

	// --- and nothing secret or content-shaped reached the process log ----------
	assert.NotContains(t, logs.String(), enterprise.jwt, "the presented token is never logged")
	assert.NotContains(t, logs.String(), team.jwt)
	assert.NotContains(t, logs.String(), jwtSecret, "nor the key that signs them")
	assert.NotContains(t, logs.String(), "mem-dec-", "nor a memory id")
	assert.NotContains(t, logs.String(), plane.Article50Statement, "nor the statement as a log line")
}
