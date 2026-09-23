// The compliance audit endpoint's refusal and failure paths: the tier gate, the
// unverified request, the malformed window, and the four ways this endpoint says
// no to a caller it cannot serve.
//
// They are split from compliance_unit_test.go along the same line the package
// under test splits compliance.go from compliance_types.go: that file is about
// what a successful read returns, and every test here is about what a caller gets
// instead of one. The fake auditor they share is declared there, because these
// tests verify refusals as much as answers and the two sets have to agree about
// what never happened.
package plane_test

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"time"

	"synapse/internal/plane"

	charmlog "github.com/charmbracelet/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComplianceAuditDeniesANonEnterpriseTier is the upsell gate: a token whose
// compliance tier is anything but enterprise is refused, the refusal is the one
// body a client can act on, and -- the part that matters -- the ledger is never
// read, so a denied caller cannot learn from this route whether an entry exists.
func TestComplianceAuditDeniesANonEnterpriseTier(t *testing.T) {
	cfg := newConfig(adminToken)

	for name, token := range map[string]string{
		"team tier":   tenantToken(t, cfg),
		"no tier":     enterpriseToken(t, cfg, ""),
		"other tier":  enterpriseToken(t, cfg, "hipaa"),
		"capitalized": enterpriseToken(t, cfg, "Enterprise"),
	} {
		t.Run(name, func(t *testing.T) {
			auditor := &fakeAuditor{
				page: plane.AuditPage{Entries: []plane.AuditRow{auditRow(t, "entry-1", "req-1")}, Total: 1},
			}
			router, _ := newComplianceRouter(t, cfg, auditor)

			rec := getComplianceAudit(router, "Bearer "+token, "")

			require.Equal(t, http.StatusForbidden, rec.Code)
			assert.JSONEq(t, `{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}`,
				rec.Body.String())

			assert.Zero(t, auditor.pageCalls, "a refused caller must not reach the ledger")

			require.Len(t, auditor.records, 1, "the refusal itself is recorded")
			assert.Equal(t, http.StatusForbidden, auditor.records[0].ResponseCode)
			assert.Equal(t, complianceAuditPath, auditor.records[0].Endpoint)
			assert.Equal(t, testTenantID, auditor.records[0].TenantID)
		})
	}
}

// TestComplianceAuditRequiresAVerifiedTenant is the fail-closed case under the
// middleware: a route registered outside requireJWT hands the handler no verified
// tenant, and an empty tenant id must be a refusal rather than a read of an
// unnamed history. Nothing is recorded either, because an access row has to name
// the tenant whose history was read.
func TestComplianceAuditRequiresAVerifiedTenant(t *testing.T) {
	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{}

	router := plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, nil, nil, auditor, nil, logger, nil).Routes()

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
	assert.Zero(t, auditor.pageCalls, "a request with no verified tenant reads nothing")
	assert.Empty(t, auditor.records, "and there is no tenant for a record to name")
}

// TestComplianceAuditRejectsBadQueryParameters: every parameter is validated
// before the ledger is read, so a malformed window costs a database round trip
// nothing -- and the attempt is still recorded, which is what makes a run of
// failed queries visible in the tenant's own audit table.
func TestComplianceAuditRejectsBadQueryParameters(t *testing.T) {
	cfg := newConfig(adminToken)

	cases := map[string]struct {
		query  string
		reason string
	}{
		"since is not a time":    {"since=yesterday", "invalid_since"},
		"since is a date only":   {"since=2026-09-21", "invalid_since"},
		"until is not a time":    {"until=soon", "invalid_until"},
		"limit is zero":          {"limit=0", "invalid_limit"},
		"limit is negative":      {"limit=-1", "invalid_limit"},
		"limit is not a number":  {"limit=all", "invalid_limit"},
		"limit exceeds the cap":  {"limit=201", "invalid_limit"},
		"offset is negative":     {"offset=-1", "invalid_offset"},
		"offset is not a number": {"offset=next", "invalid_offset"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			auditor := &fakeAuditor{page: plane.AuditPage{Entries: []plane.AuditRow{auditRow(t, "entry-1", "req-1")}, Total: 1}}
			router, _ := newComplianceRouter(t, cfg, auditor)

			rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), tc.query)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"`+tc.reason+`"}`, rec.Body.String())
			assert.Zero(t, auditor.pageCalls, "a malformed window is refused before the ledger is read")

			require.Len(t, auditor.records, 1)
			assert.Equal(t, http.StatusBadRequest, auditor.records[0].ResponseCode)
		})
	}
}

// TestComplianceAuditRefusesToAnswerWhenTheAccessRecordFails is the fail-closed
// policy, asserted rather than described: the page was read, the record could not
// be written, and the caller gets an error instead of the tenant's audit history.
// An audit read that cannot be recorded must not be handed over.
func TestComplianceAuditRefusesToAnswerWhenTheAccessRecordFails(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{
		page:      plane.AuditPage{Entries: []plane.AuditRow{auditRow(t, "entry-1", "req-1")}, Total: 1},
		recordErr: errors.New("ledger: insert compliance access log: connection refused"),
	}
	router, logs := newComplianceRouter(t, cfg, auditor)

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "entry-1", "the record's failure withholds the data too")

	assert.Equal(t, 1, auditor.pageCalls)
	assert.Contains(t, logs.String(), "Compliance access log write failed")
	assert.NotContains(t, logs.String(), "a preview", "the failure path logs no content")
}

// TestComplianceAuditReportsAReadFailureAsInternal: an auditor that could not run
// is a 500 with this package's one error body, and the underlying error is not
// reflected -- a pgx error can quote the connection target, and the DSN carries a
// password.
func TestComplianceAuditReportsAReadFailureAsInternal(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{pageErr: errors.New("failed to connect to postgres://synapse:hunter2@db:5432/synapse")}
	router, logs := newComplianceRouter(t, cfg, auditor)

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "postgres://")

	assert.Contains(t, logs.String(), "Compliance audit read failed")
	require.Len(t, auditor.records, 1, "a call that was answered 500 is still recorded")
	assert.Equal(t, http.StatusInternalServerError, auditor.records[0].ResponseCode)
}

// TestComplianceAuditRefusesAnUnreadableTrace: a row whose trace_json will not
// parse is a 500 rather than an entry with an empty trace, because "the ledger
// holds something this plane cannot read" and "the entry had no trace" are
// different statements and only one of them can be true. The row's id is logged;
// its payload is not.
func TestComplianceAuditRefusesAnUnreadableTrace(t *testing.T) {
	cfg := newConfig(adminToken)
	auditor := &fakeAuditor{
		page: plane.AuditPage{
			Entries: []plane.AuditRow{{
				ID: "entry-1", TenantID: testTenantID, RequestID: "req-1",
				// Truncated mid-value, with a marker that must not reach the log.
				TraceJSON: `{"detected_intent":"leak-me`,
				PrevHash:  "prev-hash", HashValue: "hash-value",
				CreatedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			}},
			Total: 1,
		},
	}
	router, logs := newComplianceRouter(t, cfg, auditor)

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())

	assert.Contains(t, logs.String(), "Compliance audit entry is unreadable")
	assert.Contains(t, logs.String(), "entry-1", "the row that failed is named by id")
	assert.NotContains(t, logs.String(), "leak-me", "its payload is not")

	require.Len(t, auditor.records, 1)
	assert.Equal(t, http.StatusInternalServerError, auditor.records[0].ResponseCode)
}

// TestComplianceAuditFailsClosedWithoutAnAuditor: a plane started without the
// dependency refuses the route rather than reporting an empty history it has no
// way to read -- the same fail-closed shape the other endpoints use, and the
// reason the nil check sits before the tier gate.
func TestComplianceAuditFailsClosedWithoutAnAuditor(t *testing.T) {
	cfg := newConfig(adminToken)
	router, _ := newComplianceRouter(t, cfg, nil)

	rec := getComplianceAudit(router, "Bearer "+enterpriseToken(t, cfg, "enterprise"), "")

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
}

// TestComplianceAuditRefusesAnUnverifiedRequestWithoutAnAuditor pins the order of
// the two fail-closed checks: an unverified request is a 401 even when no auditor
// is wired, because "who are you" comes before "can this plane answer".
func TestComplianceAuditRefusesAnUnverifiedRequestWithoutAnAuditor(t *testing.T) {
	cfg := newConfig(adminToken)
	router, _ := newComplianceRouter(t, cfg, nil)

	rec := getComplianceAudit(router, "", "")

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
}
