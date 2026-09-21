//go:build integration

// The compliance audit integration test's setup: the database connection, the
// two provisioned tenants, the signed ledger entries, and the router this
// endpoint is exercised through.
//
// It is split from compliance_test.go -- the test itself, which is the file the
// phase's definition-of-done command names -- for the 300-line ceiling every file
// in this project is held to. Both halves carry the integration tag, so neither
// runs without a database and neither is skipped silently in its absence.
//
// The database is the one the Phase 4 compose stack publishes
// (deploy/docker-compose.yml), reached through SYNAPSE_TEST_DB_DSN or, when that
// is unset, host loopback with the compose credentials:
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
//	  go test ./internal/plane/... -run TestComplianceAudit -v -tags integration
package plane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

const (
	// complianceDSNEnv is the environment variable that points this test at a
	// real PostgreSQL, matching internal/tenant's, internal/store's, and
	// internal/ledger's convention.
	complianceDSNEnv = "SYNAPSE_TEST_DB_DSN"

	// complianceDefaultDSN is the database the Phase 4 compose stack publishes on
	// host loopback with the credentials in deploy/docker-compose.yml. It is the
	// fallback, so the command above works against the running
	// `cd deploy && docker compose up -d db` with nothing exported.
	complianceDefaultDSN = "postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable"

	// compliancePoolTimeout bounds opening the pool and its confirming ping.
	compliancePoolTimeout = 30 * time.Second

	// complianceMasterKeyHex is 64 hex characters -- 32 bytes, the only shape
	// tenant.GenerateAndStoreSecret accepts. Test material: no deployment uses it,
	// and nothing prints it.
	complianceMasterKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	// complianceAppendCount is how many signed entries the enterprise tenant's
	// chain gets: enough for a full first page, a filter that narrows it to two,
	// and an offset that lands in the middle of it.
	complianceAppendCount = 5
)

// compliancePool returns a pool for the test database. SYNAPSE_TEST_DB_DSN wins
// when set; otherwise the Phase 4 compose database is used.
func compliancePool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(complianceDSNEnv)
	if dsn == "" {
		dsn = complianceDefaultDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), compliancePoolTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", complianceDSNEnv)
	require.NoError(t, pool.Ping(ctx), "could not reach the test database; is the Phase 4 compose db up?")

	t.Cleanup(pool.Close)

	return pool
}

// complianceTenant is one tenant this test provisioned: its id (the ledger's own
// key), its slug, and the token the plane would have handed the caller.
type complianceTenant struct {
	tenantID string
	slug     string
	jwt      string
	tier     string
}

// provisionComplianceTenant creates a tenant through the plane's own provisioner,
// so the token this test presents is minted and signed by production code and
// carries the compliance tier the endpoint gates on -- rather than a token this
// test assembled, which would prove the endpoint reads a claim it was handed.
//
// The slug carries a uuid slice because the registry's slug is unique: a fixed
// one would make the second run of this test fail on ErrTenantExists rather than
// on anything it is checking.
func provisionComplianceTenant(t *testing.T, ctx context.Context, provisioner *tenant.Provisioner, plan, tier string) complianceTenant {
	t.Helper()

	slug := "compliance-" + uuid.NewString()[:12]

	result, err := provisioner.Provision(ctx, plane.ProvisionRequest{
		Slug:           slug,
		Plan:           plan,
		ComplianceTier: tier,
	})
	require.NoError(t, err)

	require.NotEmpty(t, result.JWT)

	return complianceTenant{tenantID: result.TenantID, slug: slug, jwt: result.JWT, tier: tier}
}

// appendComplianceEntries appends count signed entries to tenantID's chain the
// way an enterprise edge node does: through the ledger's own write path, under
// that tenant's stored signing secret, with a marshalled TraceManifest as the
// trace.
//
// The entries come back in append order, and each one's CreatedAt is the
// timestamp the database stored rather than one this test predicted -- which is
// what lets the since-filter assertion below name an entry's own timestamp as an
// exact boundary instead of a value chosen to be approximately right.
func appendComplianceEntries(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID string, count int) []ledger.LedgerEntry {
	t.Helper()

	// The secret is fetched once and never printed; Phase 18's provisioner is
	// what minted it, which this call also confirms: an unwritable chain would
	// make every assertion below vacuous.
	secret, err := tenant.GetSecret(ctx, pool, tenantID)
	require.NoError(t, err, "provisioning must have minted a signing secret")

	entries := make([]ledger.LedgerEntry, 0, count)

	for i := 0; i < count; i++ {
		encoded, err := json.Marshal(trace.TraceManifest{
			RequestID:        uuid.NewString(),
			Timestamp:        time.Now().UTC(),
			DetectedIntent:   "debugging",
			IntentConfidence: 0.9,
			MemoriesCompiled: i + 1,
			TokensUsed:       100 + i,
			Memories: []trace.TraceMemory{
				{
					ID:             uuid.NewString(),
					MemoryType:     "fact",
					ContentPreview: fmt.Sprintf("entry %d", i),
					ScoreTotal:     0.5,
				},
			},
		})
		require.NoError(t, err)

		entry, err := ledger.NewLedger(pool).Append(ctx, tenantID, uuid.NewString(), string(encoded), secret)
		require.NoError(t, err, "append %d", i)

		entries = append(entries, entry)
	}

	return entries
}

// complianceRouter builds the plane's routes the way cmd/plane does: the real
// ledger.Auditor over the real pool, and the tenant JWT middleware production
// verifies with. It returns the captured log output so the test can assert that
// no token and no trace content reached it.
func complianceRouter(t *testing.T, cfg *plane.PlaneConfig, pool *pgxpool.Pool, provisioner plane.TenantProvisioner) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, pool, provisioner, nil, nil, nil, ledger.NewAuditor(pool), tenant.JWTMiddleware(cfg), logger).Routes(), &logs
}

// complianceRequest sends GET /v2/compliance/audit with the given token and raw
// query, from a fixed address so the access record's digest can be checked as a
// value that is stable and is not the address itself.
func complianceRequest(router http.Handler, token, rawQuery string) *httptest.ResponseRecorder {
	path := complianceAuditPath
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

// complianceAccessRow is one row of synapse_global.compliance_access_log, as this
// test reads it back.
type complianceAccessRow struct {
	Endpoint string
	Params   string
	IPHash   string
	Code     int
}

// readAccessLog returns one tenant's access-log rows, oldest first, read straight
// from the table rather than through any endpoint: the claim being checked is that
// the rows exist in the database, and an endpoint that reported its own writes
// could not be evidence for that.
//
// id breaks created_at ties, so two rows written in the same microsecond still
// come back in a deterministic order.
func readAccessLog(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID string) []complianceAccessRow {
	t.Helper()

	rows, err := pool.Query(ctx,
		`SELECT endpoint, query_params_redacted, ip_hash, response_code
		 FROM `+tenant.SchemaName+`.compliance_access_log
		 WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	require.NoError(t, err)
	defer rows.Close()

	logged := make([]complianceAccessRow, 0)

	for rows.Next() {
		var row complianceAccessRow
		require.NoError(t, rows.Scan(&row.Endpoint, &row.Params, &row.IPHash, &row.Code))

		logged = append(logged, row)
	}

	require.NoError(t, rows.Err())

	return logged
}
