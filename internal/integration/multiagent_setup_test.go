//go:build integration

// Phase 29's multi-agent fixture: the database, the enterprise tenant, and the
// control plane's own HTTP surface.
//
// Split from multiagent_test.go -- the scenario this phase is defined by -- along
// the same line internal/plane splits compliance_setup_test.go from
// compliance_test.go: every file in this project is held to the 300-line ceiling,
// and the fixtures every step needs are not the steps. Both halves (and the ledger,
// upstream, database, and edge files beside them) carry the integration tag, so
// none runs without a database and none skips silently in its absence.
//
// Everything here is real. The database is the one the Phase 4 compose stack
// publishes (deploy/docker-compose.yml), reached through SYNAPSE_TEST_DB_DSN or,
// when that is unset, host loopback with the compose credentials -- which is what
// makes this phase's definition-of-done command work with nothing exported:
//
//	cd deploy && docker compose up -d db
//	go test ./internal/integration/... -run TestMultiAgent -v -tags integration
//
// The plane is the real internal/plane router over that database, listening on
// 127.0.0.1:9090 (SYNAPSE_TEST_PLANE_ADDR may move it) so the edge nodes'
// control-plane-url is the address a deployment would use. The tenant is
// provisioned by the real tenant.Provisioner -- the same issuer production runs
// -- with an enterprise plan *and* an enterprise compliance tier, which is the
// pair the ledger sink and the three compliance surfaces gate on.
package integration

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"synapse/internal/compiler"
	"synapse/internal/conflict"
	"synapse/internal/ledger"
	"synapse/internal/plane"
	"synapse/internal/tenant"

	charmlog "github.com/charmbracelet/log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const (
	// multiagentDSNEnv is the environment variable that points this test at a real
	// PostgreSQL, matching internal/tenant's, internal/store's, internal/ledger's,
	// internal/metering's, internal/billing's, and internal/plane's convention.
	multiagentDSNEnv = "SYNAPSE_TEST_DB_DSN"

	// multiagentDefaultDSN is the database the Phase 4 compose stack publishes on
	// host loopback with the credentials in deploy/docker-compose.yml. It is the
	// fallback, so the command above works against a running db service.
	multiagentDefaultDSN = "postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable"

	// multiagentPlaneAddrEnv and multiagentPlaneAddrDefault are the control
	// plane's listen address. The default is the one the brief names and the one
	// a deployment uses; the environment variable exists because a test that
	// cannot choose its port cannot run next to a development plane.
	multiagentPlaneAddrEnv     = "SYNAPSE_TEST_PLANE_ADDR"
	multiagentPlaneAddrDefault = "127.0.0.1:9090"

	// multiagentPoolTimeout bounds opening the pool and its confirming ping.
	multiagentPoolTimeout = 30 * time.Second

	// multiagentMasterKeyHex is 64 hex characters -- 32 bytes, the only shape
	// tenant.GenerateAndStoreSecret accepts. Test material: no deployment uses it,
	// and nothing prints it.
	multiagentMasterKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	// multiagentJWTSecret signs the tokens this test presents. It is longer than
	// plane.MinJWTSecretLen and, like the master key, exists only here.
	multiagentJWTSecret = "phase29-multiagent-integration-jwt-secret-0123456789"

	// multiagentAdminToken guards POST /v2/tenants. This test provisions through
	// the provisioner directly, so the value is only there to keep Validate happy.
	multiagentAdminToken = "phase29-multiagent-admin-token"

	// The two agents, and the two decisions they disagree about. The decisions are
	// the brief's own wording, and they are also a fixture of internal/conflict's
	// detector: "decided" is a known predicate and the two sentences give it
	// different objects ("migrate" / "keep"), which is the value-swap signal a
	// contradiction is recognized by.
	multiagentAgentA = "agent_a"
	multiagentAgentB = "agent_b"

	multiagentPostgresDecision = "We decided to migrate the auth service to Postgres."
	multiagentMySQLDecision    = "We decided to keep the auth service on MySQL."
	multiagentAuthQuestion     = "what did we decide about the auth service database?"

	// The sessions each write lands in. They are distinct on purpose: a
	// cross-agent contradiction is between memories that never shared a session,
	// which is the case internal/supersession cannot see.
	multiagentSessionA = "sess-auth-agent-a"
	multiagentSessionB = "sess-auth-agent-b"

	// Polling. A push happens on the flusher's own tick, an audit append happens
	// in a goroutine, and neither is something this test may assume is
	// instantaneous; the deadline is generous because a failure here should mean a
	// broken product rather than a slow machine.
	multiagentWaitTimeout  = 30 * time.Second
	multiagentPollInterval = 50 * time.Millisecond
)

// multiagentTenantInfo is one provisioned tenant: the id the ledger and the
// schema are keyed by, the slug that names its schema, the token the edges
// present, and the API key shown exactly once at provisioning.
//
// Nothing here is ever logged by this test. The JWT and the API key are the two
// secrets the header assertion is about.
type multiagentTenantInfo struct {
	tenantID string
	slug     string
	jwt      string
	apiKey   string
}

// multiagentPool returns a pool for the test database.
func multiagentPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(multiagentDSNEnv)
	if dsn == "" {
		dsn = multiagentDefaultDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), multiagentPoolTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", multiagentDSNEnv)
	require.NoError(t, pool.Ping(ctx), "could not reach the test database; is the Phase 4 compose db up? (cd deploy && docker compose up -d db)")

	t.Cleanup(pool.Close)

	return pool
}

// multiagentPlaneConfig returns the plane configuration this test serves with.
// The secrets are the test's own; Validate still runs, so a config that could not
// boot a real plane fails here rather than at the first request.
func multiagentPlaneConfig(t *testing.T, dsn string) *plane.PlaneConfig {
	t.Helper()

	cfg := &plane.PlaneConfig{
		ListenAddr:          multiagentPlaneAddrDefault,
		DatabaseDSN:         dsn,
		JWTSecret:           multiagentJWTSecret,
		AdminToken:          multiagentAdminToken,
		MasterKey:           multiagentMasterKeyHex,
		LogLevel:            plane.DefaultLogLevel,
		LedgerRetentionDays: plane.DefaultLedgerRetentionDays,
	}
	require.NoError(t, cfg.Validate())

	return cfg
}

// multiagentProvision creates the enterprise tenant this scenario is about.
//
// Both halves of "enterprise" are set, and the brief's single word covers two
// switches that are deliberately not one: Plan is what the node's ledger sink is
// gated on (compiler.EnterprisePlan, read from the token's own claim), and
// ComplianceTier is what GET /v2/compliance/audit and
// GET /v2/compliance/chain-integrity are gated on. POST /v2/tenants mints the
// tier as "team" (plane/tenants.go: defaultComplianceTier), so a tenant created
// through the public route can never read the compliance surfaces this phase
// asserts -- which is why this test provisions through the provisioner, exactly
// as internal/plane's own compliance tests do.
//
// The slug carries a uuid slice because the registry's slug is unique: a fixed
// one would make a second run fail on ErrTenantExists instead of on anything it
// is checking.
func multiagentProvision(t *testing.T, ctx context.Context, cfg *plane.PlaneConfig, pool *pgxpool.Pool) multiagentTenantInfo {
	t.Helper()

	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	slug := "ma-" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	result, err := provisioner.Provision(ctx, plane.ProvisionRequest{
		Slug:           slug,
		Plan:           compiler.EnterprisePlan,
		ComplianceTier: "enterprise",
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.JWT, "provisioning must mint the token the edges present")
	require.NotEmpty(t, result.APIKey)

	return multiagentTenantInfo{tenantID: result.TenantID, slug: slug, jwt: result.JWT, apiKey: result.APIKey}
}

// multiagentTable is a tenant's memories table, named the way NewPGStore maps a
// slug (hyphens folded to underscores, schema "tenant_" + slug). It is built here
// rather than asked of the store because the assertions about it read the
// database directly -- a projection reported by the store would not be evidence
// that the rows are there.
func multiagentTable(slug string) string {
	return "tenant_" + strings.ReplaceAll(slug, "-", "_") + ".memories"
}

// multiagentStartPlane serves the real control-plane router on the configured
// loopback address, and returns the running server, its base URL, and the log
// output it captured.
//
// It is wired the way cmd/plane wires it, which is the point: the same
// provisioner, the same tenant memory writer with the same contradiction detector
// installed (that detector is what turns two agents' decisions into a conflict),
// the real ledger.Auditor for the compliance surfaces, the real chain verifier,
// and the tenant JWT middleware production verifies with.
//
// The listener is built by hand rather than left to httptest because the address
// is part of the scenario: the edge nodes are configured with the plane's URL, and
// the brief names 127.0.0.1:9090. A port already in use is reported with what to
// do about it instead of a bare bind error.
func multiagentStartPlane(t *testing.T, cfg *plane.PlaneConfig, pool *pgxpool.Pool, provisioner plane.TenantProvisioner) (*httptest.Server, string, *bytes.Buffer) {
	t.Helper()

	writer := tenant.NewMemoryWriter(pool)
	writer.SetConflictDetector(conflict.NewContradictionDetector(conflict.DefaultJaccardThreshold))

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	router := plane.NewServer(
		cfg,
		pool,
		provisioner,
		writer,
		writer,
		multiagentLedgerVerifier{chain: ledger.NewLedger(pool), pool: pool},
		ledger.NewAuditor(pool),
		tenant.JWTMiddleware(cfg),
		logger,
		nil,
		pool,
	).Routes()

	addr := os.Getenv(multiagentPlaneAddrEnv)
	if addr == "" {
		addr = multiagentPlaneAddrDefault
	}

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err, "could not listen on %s -- a development plane may already hold it; set %s to another loopback address", addr, multiagentPlaneAddrEnv)

	server := httptest.NewUnstartedServer(router)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return server, "http://" + listener.Addr().String(), &logs
}
