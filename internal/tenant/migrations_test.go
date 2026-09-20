package tenant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
	"time"

	"synapse/internal/plane"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnvDatabaseDSN is the environment variable that points these tests at a
// real PostgreSQL instance, e.g.
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse@127.0.0.1:5432/synapse?sslmode=disable' go test ./internal/tenant/...
//
// The tests skip when it is unset, so the unit suite keeps running anywhere.
const TestEnvDatabaseDSN = "SYNAPSE_TEST_DB_DSN"

// uuidPattern matches the uuid shape Postgres' gen_random_uuid() returns.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// testPool connects to the database named by TestEnvDatabaseDSN, or skips the
// test when it is not configured.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(TestEnvDatabaseDSN)
	if dsn == "" {
		t.Skipf("set %s to run the database-backed tests", TestEnvDatabaseDSN)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", TestEnvDatabaseDSN)
	require.NoError(t, pool.Ping(ctx), "could not reach the database named by %s", TestEnvDatabaseDSN)

	t.Cleanup(pool.Close)

	return pool
}

// uniqueSlug returns a slug that satisfies the API's ^[a-z0-9-]{3,32}$ rule and
// will not collide with an earlier run's rows. Nothing is ever deleted here: the
// registry rows are cheap and several of these tables are append-only by design.
func uniqueSlug(t *testing.T) string {
	t.Helper()

	entropy, err := GenerateAPIKey()
	require.NoError(t, err)

	return "test-" + entropy[:12]
}

func TestRunMigrationsIsIdempotentAndCreatesEveryDocumentedTable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	require.NoError(t, RunMigrations(ctx, pool))
	require.NoError(t, RunMigrations(ctx, pool), "a second boot must be a no-op")

	tables := []string{"tenants", "tenant_keys", "tenant_secrets", "compliance_access_log", "usage_events"}

	for _, table := range tables {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2)`,
			SchemaName, table,
		).Scan(&exists))

		assert.True(t, exists, "%s.%s must exist after RunMigrations", SchemaName, table)
	}
}

// TestMigratedConstraintsAreWhatTheHandlerReliesOn checks the two database-level
// guarantees POST /v2/tenants depends on: a unique slug (what makes a duplicate
// a 409) and tenant_keys pointing back at the registry.
func TestMigratedConstraintsAreWhatTheHandlerReliesOn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	require.NoError(t, RunMigrations(ctx, pool))

	constraintCount := func(table, constraintType string) int {
		var count int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*)
			   FROM pg_constraint c
			   JOIN pg_class t ON t.oid = c.conrelid
			   JOIN pg_namespace n ON n.oid = t.relnamespace
			  WHERE n.nspname = $1 AND t.relname = $2 AND c.contype = $3`,
			SchemaName, table, constraintType,
		).Scan(&count))

		return count
	}

	assert.Equal(t, 1, constraintCount("tenants", "u"), "tenants.slug must be unique")
	assert.Equal(t, 1, constraintCount("tenant_keys", "f"), "tenant_keys.tenant_id must be a foreign key")
	assert.Equal(t, 1, constraintCount("tenant_secrets", "f"), "tenant_secrets.tenant_id must be a foreign key")

	defaults := map[string]string{}
	rows, err := pool.Query(ctx,
		`SELECT column_name, coalesce(column_default, '') FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'tenants'`,
		SchemaName)
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var column, defaultValue string
		require.NoError(t, rows.Scan(&column, &defaultValue))
		defaults[column] = defaultValue
	}
	require.NoError(t, rows.Err())

	assert.Contains(t, defaults["plan"], "'oss'")
	assert.Contains(t, defaults["compliance_tier"], "'team'")
	assert.Contains(t, defaults["status"], "'active'")
	assert.Contains(t, defaults["id"], "gen_random_uuid()")
	assert.Contains(t, defaults["created_at"], "now()")
}

// TestProvisionerStoresTheHashAndIssuesAVerifiableToken walks the whole
// provisioning path against a real database: key generation, bcrypt at rest, the
// registry row, the issued token, and the duplicate-slug answer.
func TestProvisionerStoresTheHashAndIssuesAVerifiableToken(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	require.NoError(t, RunMigrations(ctx, pool))

	cfg := &plane.PlaneConfig{JWTSecret: tokenTestSecret}
	provisioner := NewProvisioner(cfg, NewStore(pool))
	slug := uniqueSlug(t)

	result, err := provisioner.Provision(ctx, plane.ProvisionRequest{Slug: slug, Plan: "team", ComplianceTier: "team"})
	require.NoError(t, err)
	assert.Regexp(t, uuidPattern, result.TenantID, "the id must be the uuid the DDL default minted")
	assert.Regexp(t, hexKeyPattern, result.APIKey)

	// The stored key is a bcrypt hash of the key the caller was handed, and the
	// plaintext is nowhere in the row.
	var storedHash string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT k.key_hash
		   FROM synapse_global.tenant_keys k
		   JOIN synapse_global.tenants t ON t.id = k.tenant_id
		  WHERE t.slug = $1`, slug).Scan(&storedHash))

	assert.NotEqual(t, result.APIKey, storedHash)
	assert.NotContains(t, storedHash, result.APIKey)
	assert.True(t, VerifyAPIKey(storedHash, result.APIKey))

	var plan, tier string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT plan, compliance_tier FROM synapse_global.tenants WHERE slug = $1`, slug).Scan(&plan, &tier))
	assert.Equal(t, "team", plan)
	assert.Equal(t, "team", tier)

	// The token the tenant walks away with is one the plane will accept.
	var verifiedTenantID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifiedTenantID = TenantIDFromCtx(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+result.JWT)
	rec := httptest.NewRecorder()
	JWTMiddleware(cfg)(inner).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, result.TenantID, verifiedTenantID)

	// A second attempt on the same slug is a conflict, and it must not have left
	// a stray row behind. The plane-facing sentinel is what the HTTP layer maps
	// to 409; the store keeps its own, and the translation between them is
	// asserted directly.
	_, err = provisioner.Provision(ctx, plane.ProvisionRequest{Slug: slug, Plan: "oss", ComplianceTier: "team"})
	assert.ErrorIs(t, err, plane.ErrTenantExists)

	_, err = NewStore(pool).CreateTenant(ctx, CreateTenantParams{
		Slug:           slug,
		Plan:           "oss",
		ComplianceTier: "team",
		KeyHash:        "irrelevant-because-the-slug-is-taken",
	})
	assert.ErrorIs(t, err, ErrTenantExists)

	var tenants, keys int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM synapse_global.tenants WHERE slug = $1`, slug).Scan(&tenants))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM synapse_global.tenant_keys k JOIN synapse_global.tenants t ON t.id = k.tenant_id WHERE t.slug = $1`,
		slug).Scan(&keys))

	assert.Equal(t, 1, tenants, "a rejected duplicate must not create a second tenant")
	assert.Equal(t, 1, keys, "a rejected duplicate must not create a second key")
}
