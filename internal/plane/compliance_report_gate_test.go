// The compliance report endpoint's refusal and failure paths: the tier gate, the
// unverified request, the malformed parameters, the missing dependencies, and the
// ways this endpoint says no to a caller it cannot serve.
//
// They are split from compliance_report_unit_test.go along the same line the package
// under test splits compliance_report.go from compliance_report_pdf.go: that file is
// about what a successful JSON read returns, this one is about what a caller gets
// instead of one -- and the PDF file next door is about the one external program this
// endpoint depends on.
package plane_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"synapse/internal/plane"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComplianceReportDeniesANonEnterpriseTier is the upsell gate Phase 19
// established, asserted here because Phase 20 reuses it: a token whose compliance
// tier is anything but enterprise is refused, the refusal is the one body a client
// can act on, and the window is never read -- so a denied caller cannot learn from
// this route whether a period holds anything.
func TestComplianceReportDeniesANonEnterpriseTier(t *testing.T) {
	cfg := newConfig(adminToken)

	for name, token := range map[string]string{
		"team tier":   tenantToken(t, cfg),
		"no tier":     enterpriseToken(t, cfg, ""),
		"other tier":  enterpriseToken(t, cfg, "hipaa"),
		"capitalized": enterpriseToken(t, cfg, "Enterprise"),
	} {
		t.Run(name, func(t *testing.T) {
			auditor := &fakeAuditor{facts: reportFacts(t)}
			verifier := &fakeVerifier{result: plane.ChainIntegrityResult{ChainValid: true}}
			router, _ := newComplianceReportRouter(t, cfg, auditor, verifier)

			rec := getComplianceReport(router, "Bearer "+token, "format=json")

			require.Equal(t, http.StatusForbidden, rec.Code)
			assert.JSONEq(t, `{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}`,
				rec.Body.String())

			assert.Zero(t, auditor.factsCalls, "a refused caller must not reach the ledger")
			assert.Zero(t, verifier.calls, "nor walk the chain")

			require.Len(t, auditor.records, 1, "the refusal itself is recorded")
			assert.Equal(t, http.StatusForbidden, auditor.records[0].ResponseCode)
			assert.Equal(t, complianceReportPath, auditor.records[0].Endpoint)
			assert.Equal(t, testTenantID, auditor.records[0].TenantID)
		})
	}
}

// TestComplianceReportRequiresAVerifiedTenant is the fail-closed case under the
// middleware: a route registered outside requireJWT hands the handler no verified
// tenant, and an empty tenant id must be a refusal rather than a report on an
// unnamed tenant. Nothing is recorded either, because an access row has to name the
// tenant whose report was read.
func TestComplianceReportRequiresAVerifiedTenant(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{facts: reportFacts(t)}
	verifier := &fakeVerifier{}

	// No auth middleware: a verified tenant can never appear.
	router := plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, nil, verifier, auditor, nil, nil, nil).Routes()

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
	assert.Zero(t, auditor.factsCalls, "a request with no verified tenant reads nothing")
	assert.Empty(t, auditor.records, "and there is no tenant for a record to name")
}

// TestComplianceReportRejectsBadParameters: every parameter is validated before the
// ledger is read, so a malformed request costs a database round trip nothing -- and
// the attempt is still recorded, which is what makes a run of failed queries visible
// in the tenant's own audit table.
//
// The format cases are the security-relevant ones. The value selects one of two code
// paths, and the PDF path ends in an exec; a value carrying whitespace, a leading
// dash, or a path is refused here, so it can never be anywhere near an argument list.
func TestComplianceReportRejectsBadParameters(t *testing.T) {
	cfg := newConfig(adminToken)

	cases := map[string]struct {
		query  string
		reason string
	}{
		"format is xml":                   {"format=xml", "invalid_format"},
		"format is capitalized":           {"format=PDF", "invalid_format"},
		"format carries a flag":           {"format=pdf%20--no-sandbox", "invalid_format"},
		"format carries a shell metachar": {"format=pdf%3Brm%20-rf%20/", "invalid_format"},
		"format carries a newline":        {"format=pdf%0A--no-sandbox", "invalid_format"},
		"format carries a path":           {"format=../../etc/passwd", "invalid_format"},
		"since is not a time":             {"since=yesterday", "invalid_since"},
		"since is a date only":            {"since=2026-09-21", "invalid_since"},
		"until is not a time":             {"until=soon", "invalid_until"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			auditor := &fakeAuditor{facts: reportFacts(t)}
			router, _ := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

			rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), tc.query)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"`+tc.reason+`"}`, rec.Body.String())
			assert.Zero(t, auditor.factsCalls, "a malformed request is refused before the ledger is read")

			require.Len(t, auditor.records, 1)
			assert.Equal(t, http.StatusBadRequest, auditor.records[0].ResponseCode)

			// The refused value is never copied into the audit table either: the
			// record holds the whitelisted rendering, which for a refused format is
			// no format at all -- the request never got far enough to have one.
			assert.NotContains(t, auditor.records[0].QueryParamsRedacted, "rm%20")
			assert.NotContains(t, auditor.records[0].QueryParamsRedacted, "passwd")
			assert.NotContains(t, auditor.records[0].QueryParamsRedacted, "--no-sandbox")
		})
	}
}

// TestComplianceReportFallsBackToJSONWhenTheQueryIsNotParseable documents the one
// case where an unrecognized format does *not* reach this package: net/url rejects a
// query pair containing a raw semicolon (`invalid semicolon separator in query`), so
// `?format=pdf;rm -rf /` never becomes a parameter at all. The request is then an
// ordinary unqualified one -- JSON, the documented default -- which is asserted here
// rather than left as an untested assumption about the standard library.
//
// The encoded form of the same value (`%3B`) does parse, and is refused as
// invalid_format; see TestComplianceReportRejectsBadParameters.
func TestComplianceReportFallsBackToJSONWhenTheQueryIsNotParseable(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{facts: reportFacts(t)}
	router, _ := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "format=pdf;rm%20-rf%20/")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, 1, auditor.factsCalls)

	require.Len(t, auditor.records, 1)
	assert.Contains(t, auditor.records[0].QueryParamsRedacted, "format=json")
	assert.NotContains(t, auditor.records[0].QueryParamsRedacted, "rm%20")
}

// TestComplianceReportPassesTheWindowThrough asserts the query string reaches the
// auditor as parsed values -- UTC, microsecond-exact -- and that the access record
// carries the same window in a canonical, whitelisted rendering.
func TestComplianceReportPassesTheWindowThrough(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{facts: reportFacts(t)}
	router, _ := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"),
		"since=2026-09-21T12:00:00.123456Z&until=2026-09-21T13:00:00Z&content=must-not-be-recorded")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.NotNil(t, auditor.window.Since)
	assert.Equal(t, "2026-09-21T12:00:00.123456Z", auditor.window.Since.UTC().Format(time.RFC3339Nano),
		"a fractional second survives the round trip, so a caller can name an exact boundary")
	require.NotNil(t, auditor.window.Until)
	assert.Equal(t, "2026-09-21T13:00:00Z", auditor.window.Until.UTC().Format(time.RFC3339Nano))

	require.Len(t, auditor.records, 1)
	recorded := auditor.records[0].QueryParamsRedacted
	assert.Contains(t, recorded, "since=2026-09-21T12%3A00%3A00.123456Z")
	assert.Contains(t, recorded, "until=2026-09-21T13%3A00%3A00Z")
	assert.Contains(t, recorded, "format=json", "the default format is recorded too")
	assert.NotContains(t, recorded, "must-not-be-recorded",
		"an unknown parameter is dropped, never copied into the audit table")

	assert.Len(t, auditor.records[0].IPHash, 64, "the address is recorded as a sha256 digest")
	assert.NotEqual(t, complianceRemoteAddr, auditor.records[0].IPHash, "never as the raw address")
}

// TestComplianceReportFailsClosedWithoutItsDependencies: the report needs two -- the
// window's facts and the chain verdict -- and a plane wired with fewer answers 500
// rather than a report it could not stand behind. A report that certified an unwalked
// chain is the failure this refuses.
func TestComplianceReportFailsClosedWithoutItsDependencies(t *testing.T) {
	cfg := newConfig(adminToken)
	token := "Bearer " + enterpriseToken(t, cfg, "enterprise")

	t.Run("no auditor", func(t *testing.T) {
		router, _ := newComplianceReportRouter(t, cfg, nil, &fakeVerifier{})

		rec := getComplianceReport(router, token, "format=json")

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	})

	t.Run("no verifier", func(t *testing.T) {
		auditor := &fakeAuditor{facts: reportFacts(t)}
		router, _ := newComplianceReportRouter(t, cfg, auditor, nil)

		rec := getComplianceReport(router, token, "format=json")

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
		assert.Zero(t, auditor.factsCalls, "and the window is not read for a report that cannot be certified")
	})
}

// TestComplianceReportReportsFailuresAsInternal: an auditor or a verifier that could
// not run is a 500 with this package's one error body, and the underlying error is
// never reflected -- a pgx error can quote the connection target, and the DSN carries
// a password.
func TestComplianceReportReportsFailuresAsInternal(t *testing.T) {
	cfg := newConfig(adminToken)
	dsn := "failed to connect to postgres://synapse:hunter2@db:5432/synapse"

	t.Run("read failure", func(t *testing.T) {
		auditor := &fakeAuditor{factsErr: errors.New(dsn)}
		router, logs := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

		rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "postgres://")
		assert.Contains(t, logs.String(), "Compliance report read failed")
		require.Len(t, auditor.records, 1, "a call that was answered 500 is still recorded")
		assert.Equal(t, http.StatusInternalServerError, auditor.records[0].ResponseCode)
	})

	t.Run("chain walk failure", func(t *testing.T) {
		auditor := &fakeAuditor{facts: reportFacts(t)}
		verifier := &fakeVerifier{err: errors.New(dsn)}
		router, logs := newComplianceReportRouter(t, cfg, auditor, verifier)

		rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "postgres://")
		assert.Contains(t, logs.String(), "Compliance report chain verification failed")
	})

	t.Run("unreadable trace", func(t *testing.T) {
		auditor := &fakeAuditor{facts: plane.ReportFacts{
			Rows: []plane.AuditRow{{
				ID: "entry-1", TenantID: testTenantID, RequestID: "req-1",
				TraceJSON: `{"detected_intent":"leak-me`,
			}},
		}}
		router, logs := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

		rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
		assert.Contains(t, logs.String(), "Compliance report build failed")
		assert.NotContains(t, logs.String(), "leak-me", "the row is named by id, never by payload")
		assert.Contains(t, logs.String(), "entry-1")
	})
}

// TestComplianceReportRefusesToAnswerWhenTheAccessRecordFails is the fail-closed
// policy Phase 19 established, asserted for the report too: the report was built, the
// record could not be written, and the caller gets an error instead of a document
// certifying the tenant's chain. A report that cannot be recorded must not be handed
// over -- it is the most quotable artifact this plane produces.
func TestComplianceReportRefusesToAnswerWhenTheAccessRecordFails(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{
		facts:     reportFacts(t),
		recordErr: errors.New("ledger: insert compliance access log: connection refused"),
	}
	router, logs := newComplianceReportRouter(t, cfg, auditor, &fakeVerifier{})

	rec := getComplianceReport(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "article_50_statement",
		"the record's failure withholds the report too")

	assert.Equal(t, 1, auditor.factsCalls, "the read happened; only the answer was withheld")
	assert.Contains(t, logs.String(), "Compliance access log write failed")
	assert.NotContains(t, logs.String(), "a preview", "the failure path logs no content")
}
