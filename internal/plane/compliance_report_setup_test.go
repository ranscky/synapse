//go:build integration

// The compliance report integration test's setup: the tenants, the signed ledger entries
// the report is computed from, the metering rows that override part of it, and the router
// this endpoint is exercised through.
//
// It is split from compliance_report_test.go -- the test itself, which is the file the
// phase's definition-of-done command names -- for the 300-line ceiling every file in this
// project is held to. Both halves carry the integration tag, so neither runs without a
// database and neither is skipped silently in its absence. The pool, the provisioner, and
// the access-log reader it uses come from compliance_setup_test.go, which Phase 19 wrote
// and which already points at the same database:
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
//	  go test ./internal/plane/... -run TestComplianceReport -v -tags integration
package plane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"synapse/internal/ledger"
	"synapse/internal/plane"
	"synapse/internal/tenant"
	"synapse/internal/trace"

	charmlog "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// reportEntryCount is how many signed entries the report tenant's chain gets: one per
// fixture shape (a used memory, a cross-agent memory, a superseded memory, a contradiction
// pair), so every section of the report has something to say.
const reportEntryCount = 5

// reportFixture is one appended entry with the memories it recorded, so the test's
// expectations can be stated in terms of entries rather than indices.
type reportFixture struct {
	entry    ledger.LedgerEntry
	memories []trace.TraceMemory
}

// reportTraces returns the fixtures' trace shapes, one slice of memories per appended
// entry.
//
// They are the memories a real enterprise node would have written: a plain memory this
// node's agent used, memories written by two other agents (the Global Brain contribution),
// a memory a newer one replaced, and the two halves of a contradiction the write path
// recorded. run is a per-run suffix so two runs against one database cannot be confused by
// a shared memory id.
func reportTraces(run string) [][]trace.TraceMemory {
	return [][]trace.TraceMemory{
		{
			{ID: "mem-fact-" + run + "-1", MemoryType: "fact", Included: true},
			{ID: "mem-dec-" + run + "-1", MemoryType: "decision", Included: true, CrossAgent: true, AgentID: "agent-b"},
		},
		{
			{ID: "mem-fact-" + run + "-2", MemoryType: "fact", Included: true},
			{ID: "mem-sup-" + run, MemoryType: "fact", ExclusionReason: "superseded", SupersededBy: "mem-dec-" + run + "-1"},
		},
		{
			{ID: "mem-dec-" + run + "-2", MemoryType: "decision", Included: true, CrossAgent: true, AgentID: "agent-c"},
			{
				ID: "mem-conflict-" + run, MemoryType: "decision", Included: true,
				ConflictStatus: "conflict", ConflictWithID: "mem-old-" + run,
			},
		},
		{
			{ID: "mem-fact-" + run + "-3", MemoryType: "fact", Included: true},
		},
		{
			{ID: "mem-fact-" + run + "-4", MemoryType: "fact", Included: true},
			{
				ID: "mem-old-" + run, MemoryType: "decision",
				ConflictStatus: "superseded_candidate", ConflictWithID: "mem-conflict-" + run,
			},
		},
	}
}

// appendReportEntries appends the report tenant's fixture entries through the ledger's own
// write path, under that tenant's own signing secret, and returns them in append order.
//
// ReductionPct is 30 on every entry, deliberately: it makes the ledger-side fallback for
// the mean reduction observable, and it is not the value production writes (0, per Phase
// 18's fidelity finding) because a fixture set to 0 could not tell "the fallback ran and
// found zeros" from "no data was read at all".
func appendReportEntries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID string) []reportFixture {
	t.Helper()

	secret, err := tenant.GetSecret(ctx, pool, tenantID)
	require.NoError(t, err, "provisioning must have minted a signing secret")

	shapes := reportTraces(uuid.NewString()[:8])
	require.Len(t, shapes, reportEntryCount)

	fixtures := make([]reportFixture, 0, len(shapes))

	for i, memories := range shapes {
		requestID := uuid.NewString()

		encoded, err := json.Marshal(trace.TraceManifest{
			RequestID:        requestID,
			Timestamp:        time.Now().UTC(),
			DetectedIntent:   "debugging",
			IntentConfidence: 0.9,
			MemoriesCompiled: len(memories),
			TokensUsed:       100 + i,
			ReductionPct:     30,
			Memories:         memories,
		})
		require.NoError(t, err)

		entry, err := ledger.NewLedger(pool).Append(ctx, tenantID, requestID, string(encoded), secret)
		require.NoError(t, err, "append entry %d", i)

		fixtures = append(fixtures, reportFixture{entry: entry, memories: memories})
	}

	return fixtures
}

// insertUsageEvents writes one metering row per reduction given, straight into
// synapse_global.usage_events.
//
// Nothing in this build writes that table yet (the metering phase is unbuilt), which is
// exactly why the test inserts rows by hand: it is the only way to assert that the report
// prefers them when they exist, and the choice between the two sources is a decision this
// phase made rather than an accident of which table happens to be populated.
func insertUsageEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID string, reductions ...float64) {
	t.Helper()

	for _, reduction := range reductions {
		_, err := pool.Exec(ctx,
			`INSERT INTO `+tenant.SchemaName+`.usage_events
				(tenant_id, agent_id, raw_tokens, compiled_tokens, reduction_pct)
			 VALUES ($1, $2, $3, $4, $5)`,
			tenantID, "agent-b", 1000, 500, reduction)
		require.NoError(t, err)
	}
}

// reportChainVerifier is the test's adapter between the plane's LedgerVerifier contract and
// the ledger package's own walk: it fetches the tenant's raw signing secret, hands it to
// Verify, and returns the verdict. It is the same shape as cmd/plane's ledgerVerifier, which
// cannot be imported here because it lives in package main.
type reportChainVerifier struct {
	pool *pgxpool.Pool
}

// VerifyChain implements plane.LedgerVerifier.
func (v reportChainVerifier) VerifyChain(ctx context.Context, tenantID string) (plane.ChainIntegrityResult, error) {
	secret, err := tenant.GetSecret(ctx, v.pool, tenantID)
	if err != nil {
		return plane.ChainIntegrityResult{}, fmt.Errorf("fetch tenant secret: %w", err)
	}

	return ledger.NewLedger(v.pool).Verify(ctx, tenantID, secret)
}

// newReportRouter builds the plane's routes the way cmd/plane does: the real
// ledger.Auditor over the real pool, the real chain verifier, the real middleware, and the
// shipped template -- so a PDF request in this test renders the document a deployment
// ships. It returns the captured log output so the test can assert that no token and no
// trace content reached it.
func newReportRouter(t *testing.T, cfg *plane.PlaneConfig, pool *pgxpool.Pool, provisioner plane.TenantProvisioner) (http.Handler, *bytes.Buffer) {
	t.Helper()

	cfg.ReportTemplatePath = reportTemplatePath

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, pool, provisioner, nil, nil, reportChainVerifier{pool: pool},
		ledger.NewAuditor(pool), tenant.JWTMiddleware(cfg), logger, nil, pool).Routes(), &logs
}

// reportRequest sends GET /v2/compliance/report with the given token and raw query, from
// the fixed address the access-log assertions expect.
func reportRequest(router http.Handler, token, rawQuery string) *httptest.ResponseRecorder {
	path := complianceReportPath
	if rawQuery != "" {
		path += "?" + rawQuery
	}

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = complianceRemoteAddr
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

// provisionReportTenant creates a tenant through the plane's own provisioner -- the same
// issuer production runs -- with the given plan and compliance tier, and mints its ledger
// signing secret as a side effect. The token it returns is therefore the token a deployment
// would have handed that tenant, which is what makes the tier gate in these tests a test of
// the gate rather than of a claim a test assembled.
func provisionReportTenant(t *testing.T, ctx context.Context, provisioner *tenant.Provisioner, plan, tier string) complianceTenant {
	t.Helper()

	return provisionComplianceTenant(t, ctx, provisioner, plan, tier)
}
