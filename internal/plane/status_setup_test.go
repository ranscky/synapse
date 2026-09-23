//go:build integration

// Phase 28's integration scaffolding: the database connection, the tenants whose
// status the gate reads, the one statement that sets that status, and the router
// the requests go through.
//
// It is split from status_test.go -- the tests this phase is defined by -- along
// the same line internal/plane splits compliance_setup_test.go from
// compliance_test.go: every file in this project is held to the 300-line ceiling,
// and the fixtures every case needs are not the cases. Both halves carry the
// integration tag, so neither runs without a database and neither skips silently
// in its absence.
//
// The database is the one the Phase 4 compose stack publishes
// (deploy/docker-compose.yml), reached through SYNAPSE_TEST_DB_DSN or, when that
// is unset, host loopback with the compose credentials -- which is what makes
// this phase's definition-of-done command work with nothing exported:
//
//	cd deploy && docker compose up -d db
//	go test ./internal/plane/... -run TestTenantStatus -v -tags integration
package plane_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	charmlog "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const (
	// statusDSNEnv is the environment variable that points this test at a real
	// PostgreSQL, matching internal/tenant's, internal/store's, internal/ledger's,
	// internal/metering's, and internal/billing's convention.
	statusDSNEnv = "SYNAPSE_TEST_DB_DSN"

	// statusDefaultDSN is the database the Phase 4 compose stack publishes on
	// host loopback with the credentials in deploy/docker-compose.yml. It is the
	// fallback, so the command above works against a running db service.
	statusDefaultDSN = "postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable"

	// statusPoolTimeout bounds opening the pool and its confirming ping.
	statusPoolTimeout = 30 * time.Second

	// statusMasterKeyHex is 64 hex characters -- 32 bytes, the only shape
	// tenant.GenerateAndStoreSecret accepts. Test material: no deployment uses
	// it, and nothing prints it.
	statusMasterKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	// statusGraceHeader is the header a tenant in its grace period is warned
	// with. Spelled out rather than imported because internal/plane keeps it
	// unexported, and a client's view of a header name is what a test should pin.
	statusGraceHeader = "X-Synapse-Grace-Period"

	// statusUpgradeURL is the destination the 402 body names, also from the
	// outside.
	statusUpgradeURL = "https://synapse.ai/pricing"

	// statusGraceDays is the window a grace_period status buys, mirrored from the
	// gate's own window so a change to it fails a test here rather than quietly
	// changing what is asserted.
	statusGraceDays = 7

	// statusSearchPath, statusHealthPath, and statusTenantsPath are the
	// client-visible paths these tests exercise.
	statusSearchPath  = "/v2/memories/search"
	statusHealthPath  = "/health"
	statusTenantsPath = "/v2/tenants"

	// statusAgentID is the agent the search requests name. The endpoint requires
	// one; nothing here depends on its value.
	statusAgentID = "phase28-agent"
)

// statusPool returns a pool for the test database, migrating the global schema on
// the way in. SYNAPSE_TEST_DB_DSN wins when set; otherwise the Phase 4 compose
// database is used.
//
// The migration runs rather than being assumed: the status column and the grace
// period timestamp are created by that migration, and a suite that silently
// depended on a control plane having booted would fail on a fresh database for a
// reason that has nothing to do with this phase.
func statusPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(statusDSNEnv)
	if dsn == "" {
		dsn = statusDefaultDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), statusPoolTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", statusDSNEnv)
	require.NoError(t, pool.Ping(ctx), "could not reach the test database; is the Phase 4 compose db up?")
	require.NoError(t, tenant.RunMigrations(ctx, pool), "could not run the synapse_global migrations")

	t.Cleanup(pool.Close)

	return pool
}

// statusTenant is one tenant this test provisioned: the id its status is set on,
// and the token a caller holding it would present.
//
// The token is minted by tenant.IssueToken inside the provisioner -- the same
// issuer production runs -- so the tenant id the gate reads out of the request
// context is one that travelled through a real signature and a real verification,
// rather than a value this test assembled. The slug carries a uuid slice because
// the registry's slug is unique: a fixed one would make a second run of this test
// fail on ErrTenantExists instead of on anything it is checking.
type statusTenant struct {
	tenantID string
	jwt      string
}

// provisionStatusTenant creates one tenant through the plane's own provisioner.
func provisionStatusTenant(t *testing.T, ctx context.Context, provisioner *tenant.Provisioner) statusTenant {
	t.Helper()

	result, err := provisioner.Provision(ctx, plane.ProvisionRequest{
		Slug:           "status-" + uuid.NewString()[:12],
		Plan:           "team",
		ComplianceTier: "team",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.JWT, "provisioning must mint the token this test presents")

	return statusTenant{tenantID: result.TenantID, jwt: result.JWT}
}

// setTenantStatus writes the one column this phase enforces, directly.
//
// Direct is the point: whether the Stripe webhook moves that column is Phase 27's
// test, and what Phase 28 has to be able to read is the row as it stands, whoever
// wrote it. graceStarted is nil for every status that has no grace period -- which
// is also the shape a row predating the column has.
func setTenantStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, status string, graceStarted *time.Time) {
	t.Helper()

	tag, err := pool.Exec(ctx,
		`UPDATE `+tenant.SchemaName+`.tenants SET status = $2, grace_period_started_at = $3 WHERE id = $1::uuid`,
		tenantID, status, graceStarted)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected(), "the tenant this test provisioned must still be there")
}

// statusRouter builds the plane's routes the way cmd/plane does: the real
// provisioner, the real pool as both the health probe and the status gate's
// handle, the tenant JWT middleware production verifies with, and the given
// webhook handler.
//
// The searcher is a double with a call counter, because the strongest half of the
// suspended claim is not the status code -- it is that the handler behind the gate
// never ran.
func statusRouter(t *testing.T, cfg *plane.PlaneConfig, pool *pgxpool.Pool, provisioner plane.TenantProvisioner, searcher plane.MemorySearcher, webhook http.HandlerFunc) http.Handler {
	t.Helper()

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, pool, provisioner, nil, searcher, nil, nil,
		tenant.JWTMiddleware(cfg), logger, webhook, pool).Routes()
}

// getStatusSearch sends the documented search body to GET /v2/memories/search
// with the given tenant token.
func getStatusSearch(router http.Handler, jwt string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, statusSearchPath,
		strings.NewReader(`{"agent_id":"`+statusAgentID+`","top_k":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}
