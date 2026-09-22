// The compliance tier gate, asserted across every compliance surface at once:
// GET /v2/compliance/audit, GET /v2/compliance/chain-integrity, and GET
// /v2/compliance/report, each against an enterprise, a business, and a team
// tenant token.
//
// The per-surface suites next door -- compliance_audit_gate_test.go and
// compliance_report_gate_test.go -- prove each endpoint's own refusals in depth:
// the missing dependency, the malformed window, the unreadable trace, the
// renderer that is not installed. This file is the systematic half of Phase 21.
// One table, one router shape, one refusal body, so a surface wired without the
// gate cannot pass by being the case nobody tabulated; a change to the tier
// check, to the body, or to the order the checks happen in shows up three times
// here rather than once. The chain-integrity column was Phase 17's GET
// /v2/ledger/verify, which had no gate at all before this phase.
package plane_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// complianceChainIntegrityPath is the chain-integrity surface's canonical path,
// written out here rather than shared with the package under test so a route
// registered at the wrong path fails a test instead of moving with it. Phase
// 17's spelling is exercised by ledger_test.go, which still uses it, and by
// TestLedgerVerifyAliasIsGatedByComplianceTier below.
const complianceChainIntegrityPath = "/v2/compliance/chain-integrity"

// complianceTierRefusalBody is the one body every refusal answers with, spelled
// out rather than read from the package under test: a client keys off these two
// exact fields, so the test has to be able to fail when they change.
const complianceTierRefusalBody = `{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}`

// complianceTierToken mints a real token whose plan and compliance tier are the
// tier under test, so the matrix runs the issuer and the verifier production
// runs rather than a stub that always agrees.
//
// Plan and tier move together because that is what a real tenant looks like: a
// team tenant's plan is team and its tier is team. The distinction the gate
// actually rests on -- that it reads the compliance_tier claim and not the plan
// -- is pinned by TestLedgerVerifyAliasIsGatedByComplianceTier, whose
// enterprise-plan token carries a team tier and is refused.
func complianceTierToken(t *testing.T, cfg *plane.PlaneConfig, tier string) string {
	t.Helper()

	token, err := tenant.IssueToken(cfg, tenant.TokenIdentity{
		TenantID: testTenantID,
		Slug:     testTenantSlug,
		Plan:     tier,
		Tier:     tier,
	})
	require.NoError(t, err)

	return token
}

// getChainIntegrity sends GET /v2/compliance/chain-integrity with the given
// Authorization header, omitted entirely when it is empty.
func getChainIntegrity(router http.Handler, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, complianceChainIntegrityPath, nil)
	req.RemoteAddr = complianceRemoteAddr
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

// TestComplianceGate is the systematic half of Phase 21: every compliance
// surface, against every tier, checking the two things a refusal must never do
// -- answer with a different body, or reach the dependency underneath.
//
// Every case builds its own router and its own doubles, so the read counters
// asserted below start at zero and no case can pass on state another case left
// behind.
func TestComplianceGate(t *testing.T) {
	cfg := newConfig(adminToken)

	surfaces := []struct {
		name string
		// request is the surface's own client, so every case goes through the
		// route and the method the published contract names.
		request func(router http.Handler, authorization string) *httptest.ResponseRecorder
		// reads counts what the surface's dependencies were asked. A refused
		// caller must leave them all at zero: a denied caller must not learn
		// whether an entry exists, what a period holds, or where a chain broke.
		reads func(auditor *fakeAuditor, verifier *fakeVerifier) int
		// allowed is how many reads the surface makes when it answers: one page
		// for the audit, one walk for the chain verdict, and both for a report.
		allowed int
		// recordedPath is the endpoint name the refusal's access record carries.
		recordedPath string
	}{
		{
			name: "compliance audit",
			request: func(router http.Handler, authorization string) *httptest.ResponseRecorder {
				return getComplianceAudit(router, authorization, "")
			},
			reads:        func(auditor *fakeAuditor, _ *fakeVerifier) int { return auditor.pageCalls },
			allowed:      1,
			recordedPath: complianceAuditPath,
		},
		{
			name: "compliance chain integrity",
			request: func(router http.Handler, authorization string) *httptest.ResponseRecorder {
				return getChainIntegrity(router, authorization)
			},
			reads:        func(_ *fakeAuditor, verifier *fakeVerifier) int { return verifier.calls },
			allowed:      1,
			recordedPath: complianceChainIntegrityPath,
		},
		{
			name: "compliance report",
			request: func(router http.Handler, authorization string) *httptest.ResponseRecorder {
				return getComplianceReport(router, authorization, "format=json")
			},
			reads: func(auditor *fakeAuditor, verifier *fakeVerifier) int {
				return auditor.factsCalls + verifier.calls
			},
			allowed:      2,
			recordedPath: complianceReportPath,
		},
	}

	tiers := []struct {
		name    string
		tier    string
		allowed bool
	}{
		{name: "enterprise", tier: "enterprise", allowed: true},
		{name: "business", tier: "business"},
		{name: "team", tier: "team"},
	}

	for _, surface := range surfaces {
		for _, tier := range tiers {
			t.Run(surface.name+"/"+tier.name, func(t *testing.T) {
				auditor := &fakeAuditor{
					page:  plane.AuditPage{Entries: []plane.AuditRow{}},
					facts: reportFacts(t),
				}
				verifier := &fakeVerifier{result: plane.ChainIntegrityResult{ChainValid: true}}
				router, _ := newComplianceReportRouter(t, cfg, auditor, verifier)

				rec := surface.request(router, "Bearer "+complianceTierToken(t, cfg, tier.tier))

				if tier.allowed {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					assert.Equal(t, surface.allowed, surface.reads(auditor, verifier),
						"an entitled caller reaches the dependency its surface is built on")
					return
				}

				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.JSONEq(t, complianceTierRefusalBody, rec.Body.String())
				assert.Zero(t, surface.reads(auditor, verifier),
					"a refused caller must not reach the ledger or walk the chain")

				require.Len(t, auditor.records, 1, "the refusal itself is recorded")
				assert.Equal(t, http.StatusForbidden, auditor.records[0].ResponseCode)
				assert.Equal(t, surface.recordedPath, auditor.records[0].Endpoint)
				assert.Equal(t, testTenantID, auditor.records[0].TenantID)
			})
		}
	}
}

// TestLedgerVerifyAliasIsGatedByComplianceTier is the anti-bypass case for Phase
// 17's spelling of the chain-integrity surface. GET /v2/ledger/verify and GET
// /v2/compliance/chain-integrity are one handler reached two ways, so a caller
// that knows the old name must not be able to reach a surface the new name
// refuses -- which is why the gate is in the handler rather than on a route.
//
// The second case is also the one that pins *which* claim the gate reads: that
// token's plan is enterprise and its compliance tier is team, so a gate written
// against the plan, the slug, or anything else a request carries would let it
// through.
func TestLedgerVerifyAliasIsGatedByComplianceTier(t *testing.T) {
	cfg := newConfig(adminToken)

	cases := []struct {
		name   string
		token  string
		code   int
		walked int
	}{
		{
			name:   "enterprise tier",
			token:  complianceTierToken(t, cfg, "enterprise"),
			code:   http.StatusOK,
			walked: 1,
		},
		{
			name:  "enterprise plan, team tier",
			token: enterpriseToken(t, cfg, "team"),
			code:  http.StatusForbidden,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			auditor := &fakeAuditor{}
			verifier := &fakeVerifier{result: plane.ChainIntegrityResult{ChainValid: true}}
			router, _ := newComplianceReportRouter(t, cfg, auditor, verifier)

			rec := getLedgerVerify(router, "Bearer "+tt.token)

			require.Equal(t, tt.code, rec.Code, rec.Body.String())
			assert.Equal(t, tt.walked, verifier.calls)

			if tt.code != http.StatusForbidden {
				return
			}

			assert.JSONEq(t, complianceTierRefusalBody, rec.Body.String())

			require.Len(t, auditor.records, 1,
				"the refusal is recorded against the surface, not the spelling")
			assert.Equal(t, complianceChainIntegrityPath, auditor.records[0].Endpoint)
			assert.Equal(t, http.StatusForbidden, auditor.records[0].ResponseCode)
		})
	}
}
