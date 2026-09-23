//go:build integration

// Permission tests for the audit ledger table itself.
//
// The claim under test is the one .clinerules states as a hard rule -- never
// update, never delete a ledger row -- and the point of this phase is that the
// database enforces it, not the Go code. These tests therefore go through the
// server's own privilege checks (SQLSTATE 42501) instead of through any wrapper
// this project could have written and could have got wrong.
//
// Build-tagged integration because a real Postgres is required and
// testcontainers-go is not a dependency of this module, so the test uses the
// database the Phase 4 compose stack publishes (deploy/docker-compose.yml):
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
//	  go test ./internal/ledger/... -run TestLedgerTablePermissions -v -tags integration
//
// The database is a precondition, not an option: an unreachable one fails this
// test instead of skipping it, because a skipped permission test reports success
// without having checked anything.
//
// One caveat, documented at length in PROGRESS.md: a Postgres superuser bypasses
// every privilege check, and the compose application user is both a superuser and
// the ledger table's owner, so no grant can bind it. The probes below therefore
// run inside SET LOCAL ROLE ledger_writer -- the only identity the grants do bind,
// and the shape the production writer has to adopt.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// ledgerDSNEnv is the environment variable that points this test at a real
	// PostgreSQL instance, matching internal/tenant's and internal/store's
	// convention.
	ledgerDSNEnv = "SYNAPSE_TEST_DB_DSN"

	// ledgerDefaultDSN is the database the Phase 4 compose stack publishes on
	// host loopback with the credentials in deploy/docker-compose.yml. It is the
	// fallback, so the command above works against the running
	// `cd deploy && docker compose up -d db` with nothing exported.
	ledgerDefaultDSN = "postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable"

	// ledgerTable is the table under test, schema-qualified. It is no longer
	// declared here: the write path (ledger.go) owns that constant now, so the
	// permission probes and the writer cannot disagree about which table they
	// mean. The value is unchanged -- tenant.SchemaName + ".ledger".

	// ledgerIndex is the index the migration creates on (tenant_id, created_at).
	ledgerIndex = "ledger_tenant_created_idx"

	// ledgerPlaceholderHash is written into prev_hash for the probe row: 64 zeros,
	// the width of a SHA-256 digest. Signing is a later phase and the column is
	// NOT NULL, so the probe has to supply something.
	ledgerPlaceholderHash = "0000000000000000000000000000000000000000000000000000000000000000"

	// insufficientPrivilege is PostgreSQL's SQLSTATE for a permission denial
	// (42501). Asserting the code, not only the message, is what stops a syntax
	// error, a missing table, or a failed role switch from being mistaken for an
	// enforcement pass.
	insufficientPrivilege = "42501"

	// ledgerPoolTimeout bounds opening the pool and its confirming ping.
	ledgerPoolTimeout = 30 * time.Second
)

// ledgerPool returns a pool for the test database. SYNAPSE_TEST_DB_DSN wins when
// set; otherwise the Phase 4 compose database is used.
//
// Unlike the untagged Postgres tests, an unusable database is fatal here rather
// than a skip: under the integration tag the database is a precondition of the
// claim being tested.
func ledgerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(ledgerDSNEnv)
	if dsn == "" {
		dsn = ledgerDefaultDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), ledgerPoolTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", ledgerDSNEnv)
	require.NoError(t, pool.Ping(ctx), "could not reach the test database; is the Phase 4 compose db up?")

	t.Cleanup(pool.Close)

	return pool
}

// hasPrivilege reports whether the writer role holds privilege on the ledger
// table according to the server. It is stronger evidence than reading the ACL
// text: it is the same check the INSERT, UPDATE, and DELETE below go through.
func hasPrivilege(t *testing.T, pool *pgxpool.Pool, privilege string) bool {
	t.Helper()

	var granted bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT has_table_privilege($1, $2, $3)`,
		tenant.LedgerWriterRole, ledgerTable, privilege,
	).Scan(&granted))

	return granted
}

// runAsLedgerWriter runs one statement as tenant.LedgerWriterRole and returns its
// command tag.
//
// The role switch is SET LOCAL inside an explicit transaction rather than SET
// ROLE: it cannot outlive the transaction, so a pooled connection is never handed
// back still wearing the writer's identity, and an aborted statement leaves
// nothing behind for the next caller.
//
// commit decides whether the statement survives. A committed INSERT is the only
// way to prove the write path works end to end; the UPDATE and DELETE probes are
// never committed, so an unexpected success still leaves the append-only ledger
// exactly as it was -- and still fails the test.
func runAsLedgerWriter(ctx context.Context, t *testing.T, pool *pgxpool.Pool, commit bool, sql string, args ...any) (pgconn.CommandTag, error) {
	t.Helper()

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err, "could not acquire a connection for the ledger probe")
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	// Nothing below can run without this membership. The migration grants
	// ledger_writer to the application's own role precisely so the writer can
	// assume it per transaction.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+tenant.LedgerWriterRole); err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("set local role %s: %w", tenant.LedgerWriterRole, err)
	}

	// The assertion that keeps this test from passing vacuously: the statements
	// below are only evidence about the writer role if the connection really is
	// acting as it. A superuser would sail straight through the UPDATE and the
	// DELETE.
	var currentUser string
	require.NoError(t, tx.QueryRow(ctx, `SELECT current_user`).Scan(&currentUser))
	require.Equal(t, tenant.LedgerWriterRole, currentUser,
		"the probe must run as %s -- the grants bind that role and nothing else", tenant.LedgerWriterRole)

	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return tag, err
	}

	if commit {
		require.NoError(t, tx.Commit(ctx))
	}

	return tag, nil
}

// requirePermissionDenied runs sql as the writer role in a transaction that is
// always rolled back, and asserts the server refused it for lack of privilege.
func requirePermissionDenied(ctx context.Context, t *testing.T, pool *pgxpool.Pool, operation, sql string, args ...any) {
	t.Helper()

	_, err := runAsLedgerWriter(ctx, t, pool, false, sql, args...)
	require.Error(t, err, "%s must be refused for %s", operation, tenant.LedgerWriterRole)

	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "%s must fail with a Postgres error, got %v", operation, err)

	assert.Equal(t, insufficientPrivilege, pgErr.Code,
		"%s must fail with insufficient_privilege (42501), got %s: %s", operation, pgErr.Code, pgErr.Message)
	assert.Contains(t, pgErr.Message, "permission denied",
		"%s must be refused for lack of privilege, got %q", operation, pgErr.Message)

	// Logged as well as asserted. The refusal's own text is the artifact a reviewer of
	// this guarantee asks to see, and an all-green suite would otherwise report only
	// that some statement was expected to fail.
	t.Logf("%s refused: SQLSTATE %s: %s", operation, pgErr.Code, pgErr.Message)
}

// TestLedgerTablePermissions is the Phase 15 guarantee checked against a real
// PostgreSQL: the ledger table and its index exist after RunMigrations, the
// writer role holds INSERT and nothing else, an INSERT through that role lands
// and is readable afterwards, and the same role's UPDATE and DELETE are refused
// by the server.
func TestLedgerTablePermissions(t *testing.T) {
	pool := ledgerPool(t)
	ctx := context.Background()

	// Every statement the migration adds is re-runnable, so a second boot must be
	// a no-op -- including the role, which CREATE ROLE cannot express idempotently
	// on its own.
	require.NoError(t, tenant.RunMigrations(ctx, pool))
	require.NoError(t, tenant.RunMigrations(ctx, pool), "a second boot must be a no-op")

	// --- the objects the migration promises -----------------------------------
	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'ledger')`,
		tenant.SchemaName).Scan(&exists))
	assert.True(t, exists, "%s must exist after RunMigrations", ledgerTable)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND indexname = $2)`,
		tenant.SchemaName, ledgerIndex).Scan(&exists))
	assert.True(t, exists, "%s must exist after RunMigrations", ledgerIndex)

	// --- the writer role is exactly one privilege on one table ----------------
	var superuser, canLogin bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rolsuper, rolcanlogin FROM pg_roles WHERE rolname = $1`,
		tenant.LedgerWriterRole).Scan(&superuser, &canLogin))
	assert.False(t, superuser, "%s must not be a superuser: a superuser bypasses every grant", tenant.LedgerWriterRole)
	assert.False(t, canLogin, "%s must not be able to log in", tenant.LedgerWriterRole)

	assert.True(t, hasPrivilege(t, pool, "INSERT"), "%s must hold INSERT on %s", tenant.LedgerWriterRole, ledgerTable)
	for _, privilege := range []string{"UPDATE", "DELETE", "SELECT"} {
		assert.False(t, hasPrivilege(t, pool, privilege),
			"%s must never hold %s on %s", tenant.LedgerWriterRole, privilege, ledgerTable)
	}

	// --- INSERT through the writer role succeeds ------------------------------
	requestID := uuid.NewString()
	tag, err := runAsLedgerWriter(ctx, t, pool, true,
		`INSERT INTO `+ledgerTable+` (tenant_id, request_id, trace_json, prev_hash, hash_value)
		 VALUES ($1, $2, $3, $4, $5)`,
		uuid.NewString(), requestID, `{"phase":15,"probe":"ledger-table-permissions"}`,
		ledgerPlaceholderHash, "phase15-unsigned-probe",
	)
	require.NoError(t, err, "%s must be able to INSERT into %s", tenant.LedgerWriterRole, ledgerTable)
	assert.Equal(t, int64(1), tag.RowsAffected(), "the INSERT must have written exactly one row")

	// Read back through the application connection, which is the only identity
	// that can SELECT: the write has to be real, not merely unrefused.
	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM `+ledgerTable+` WHERE request_id = $1`, requestID).Scan(&rows))
	assert.Equal(t, 1, rows, "the committed INSERT must be visible in the ledger")

	// --- UPDATE that row is refused by the server -----------------------------
	requirePermissionDenied(ctx, t, pool, "UPDATE",
		`UPDATE `+ledgerTable+` SET trace_json = $1 WHERE request_id = $2`,
		`{"phase":15,"probe":"tampered"}`, requestID)

	// --- DELETE that row is refused by the server -----------------------------
	requirePermissionDenied(ctx, t, pool, "DELETE",
		`DELETE FROM `+ledgerTable+` WHERE request_id = $1`, requestID)

	// Neither refusal may have touched the row: the ledger is append-only, and a
	// refused statement that still changed something would be the worst possible
	// outcome for this phase.
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM `+ledgerTable+` WHERE request_id = $1`, requestID).Scan(&rows))
	assert.Equal(t, 1, rows, "neither the refused UPDATE nor the refused DELETE may remove the row")
}
