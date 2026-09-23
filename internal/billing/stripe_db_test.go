// The fixtures the database-backed webhook tests share: the pool, the tenant
// rows, and the two reads that are the assertions.
//
// Split from stripe_test.go -- the four tests this phase is defined by -- for the
// 300-line ceiling every file in this project is held to, along the same line
// internal/store splits pgtest_test.go along: one file is what the endpoint does,
// the other is the scaffolding every case in it needs.
//
// The database is a precondition rather than an option, but it is also not a
// requirement of the suite: these tests skip when SYNAPSE_TEST_DB_DSN is unset,
// which is internal/tenant's and internal/store's documented convention, and the
// reason CI -- `go test ./...` on three operating systems, with no database at
// all -- stays green. The unit half of this package (stripe_unit_test.go) covers
// the paths that need no database, so a skipped run here is not a silent one.
//
// To run them:
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
//	  go test ./internal/billing/... -v
package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

const (
	// billingDSNEnv points these tests at a real PostgreSQL instance, matching
	// internal/tenant's, internal/store's, internal/ledger's, and
	// internal/metering's convention.
	billingDSNEnv = "SYNAPSE_TEST_DB_DSN"

	// billingPoolTimeout bounds opening the pool, its confirming ping, and the
	// migration.
	billingPoolTimeout = 30 * time.Second
)

// billingPool returns a pool for the database named by billingDSNEnv, or skips the
// test when it is not configured.
//
// It runs tenant.RunMigrations rather than assuming someone else did: the columns
// these tests write are created by that migration and by nothing else, so a suite
// that silently depended on a control plane having booted would fail on a fresh
// database for a reason that has nothing to do with billing. The call needs a role
// that can create the ledger's writer role -- true for the compose user, and the
// same caveat internal/metering documents.
func billingPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(billingDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the database-backed billing tests", billingDSNEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), billingPoolTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", billingDSNEnv)
	require.NoError(t, pool.Ping(ctx), "could not reach the database named by %s", billingDSNEnv)

	t.Cleanup(pool.Close)

	require.NoError(t, tenant.RunMigrations(ctx, pool), "could not run the synapse_global migrations")

	return pool
}

// uniqueCustomerID returns a Stripe customer id no earlier run used, so repeated
// runs cannot see each other's tenants. The prefix is the one Stripe really uses,
// which keeps a failure message recognizable.
func uniqueCustomerID(t *testing.T) string {
	t.Helper()

	return "cus_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// billingTenant inserts one tenant row linked to customerID and returns its id.
//
// Nothing is deleted afterwards: this is the registry that provisioning also
// writes to and that other phases' rows reference, the status column is what the
// tests assert on, and every test is scoped to its own customer id -- which the
// unique index on that column makes impossible to collide with.
func billingTenant(t *testing.T, pool *pgxpool.Pool, customerID string) string {
	t.Helper()

	slug := "billing-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]

	var id string
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO `+tenant.SchemaName+`.tenants (slug, stripe_customer_id) VALUES ($1, $2) RETURNING id::text`,
		slug, customerID).Scan(&id), "could not seed a tenant for %s", customerID)

	return id
}

// billingStatus reads the two columns a webhook delivery can change: the status
// and, if there is one, the grace period's start time.
//
// Reading the row back is the assertion these tests are made of. A double could
// prove the handler chose the right statement; only the database can prove the
// statement is the one that moves the row -- the column is text with a DEFAULT and
// the timestamp is timestamptz, which is where a wrong literal or a wrong cast
// would show up.
func billingStatus(t *testing.T, pool *pgxpool.Pool, tenantID string) (string, *time.Time) {
	t.Helper()

	var (
		status string
		grace  *time.Time
	)

	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status, grace_period_started_at FROM `+tenant.SchemaName+`.tenants WHERE id = $1::uuid`,
		tenantID).Scan(&status, &grace), "could not read back the status of tenant %s", tenantID)

	return status, grace
}
