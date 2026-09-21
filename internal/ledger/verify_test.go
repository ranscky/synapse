//go:build integration

// Chain verification tests for the ledger's read path.
//
// The claim under test is this phase's definition of done: Ledger.Verify walks a
// tenant's whole signed chain, recomputes every signature under that tenant's own
// secret, checks every link, and names the first entry that does not hold up -- so
// tampering the append-only grants are meant to prevent shows up as a finding
// rather than as a missing row.
//
// What makes these tests evidence rather than a mirror of the implementation is
// that the tampering is done *to the database*, with SQL, through a connection
// whose privilege is asserted first (see tamper), and that no assertion here
// calls the package's own primitives to predict an outcome: signatures are
// recomputed in the test with crypto/hmac directly, exactly as TestAppend does.
//
// Build-tagged integration because a real PostgreSQL is required and
// testcontainers-go is not a dependency of this module, so the tests use the
// database the Phase 4 compose stack publishes (deploy/docker-compose.yml). The
// database is a precondition, not an option, for the same reason
// ledger_table_test.go's is: a skipped tamper test reports success without having
// checked anything. SYNAPSE_MASTER_KEY is set by the tests themselves, so the
// documented command exports nothing:
//
//	go test ./internal/ledger/... -run TestVerify -v -tags integration
package ledger

import (
	"context"
	"testing"
	"time"

	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tamperedSpan is the row every tampering test breaks: the fifth of ten, so
// there are rows on both sides of the break and a walk that stopped early, or
// that carried on and blamed the wrong entry, is visible in the counts.
const tamperedSpan = 4

// chainedEntries provisions a fresh tenant and appends appendCount entries for
// it, returning the pool, the tenant's id and secret, and the entries in chain
// order. Every test gets its own tenant, so one test's deliberate tampering
// cannot reach another test's chain.
func chainedEntries(t *testing.T) (*pgxpool.Pool, string, []byte, []LedgerEntry) {
	t.Helper()

	pool := ledgerPool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))
	setMasterKey(t)

	tenantID, secret := testChainTenant(t, pool)

	ledger := NewLedger(pool)

	entries := make([]LedgerEntry, 0, appendCount)
	for i := 0; i < appendCount; i++ {
		entry, err := ledger.Append(ctx, tenantID, uuid.NewString(), appendTrace(i), secret)
		require.NoError(t, err, "append %d", i)

		entries = append(entries, entry)
	}

	require.Len(t, entries, appendCount)

	return pool, tenantID, secret, entries
}

// tamper rewrites the ledger with SQL, through the only identity in this
// deployment that can: the application connection, which owns the table and is a
// superuser (documented at length in ledger_table_test.go -- the grants bind
// ledger_writer, and nothing else).
//
// That privilege is asserted rather than assumed. A deployment whose application
// user is *not* a superuser is a stronger setup, not a broken test, and this is
// where the test says so instead of failing confusingly on the UPDATE. The other
// half of the point is the threat model: what these tests simulate is exactly
// what the append-only grants cannot stop, which is why the chain has to be
// independently verifiable at all.
func tamper(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	var (
		superuser bool
		canUpdate bool
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&superuser))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT has_table_privilege(current_user, $1, 'UPDATE')`, ledgerTable).Scan(&canUpdate))

	require.True(t, superuser || canUpdate,
		"the tamper probe needs a connection that can rewrite %s: this deployment's application user is the table's owner and a superuser", ledgerTable)

	_, err := pool.Exec(ctx, sql, args...)
	require.NoError(t, err, "the tamper probe must be able to rewrite the ledger")
}

// TestVerify is the happy path: ten appends, nothing touched, and a verdict that
// says so -- a valid chain, no break id or break time, and a CheckedAt that
// belongs to this walk. Without it, a verifier that always answered
// chain_valid=false would pass every tampering test below.
//
// The second walk is part of the assertion: verifying is a pure read, so the same
// ledger must answer the same way twice and its entry count must not move.
func TestVerify(t *testing.T) {
	pool, tenantID, secret, entries := chainedEntries(t)
	ctx := context.Background()

	before := time.Now().UTC()

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	assert.True(t, result.ChainValid, "ten appends that nothing touched must verify")
	assert.Equal(t, appendCount, result.EntriesChecked, "every appended entry must be examined")
	assert.Empty(t, result.FirstBreakID, "a valid chain names no break")
	assert.True(t, result.FirstBreakAt.IsZero(), "a valid chain reports no break time")
	assert.False(t, result.CheckedAt.IsZero(), "CheckedAt must be this walk's own clock reading")
	assert.False(t, result.CheckedAt.Before(before), "CheckedAt must be this walk's own clock reading")
	assert.Len(t, entries, appendCount)

	again, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	assert.Equal(t, result.ChainValid, again.ChainValid)
	assert.Equal(t, result.EntriesChecked, again.EntriesChecked, "verifying must not change the ledger")
	assert.Empty(t, again.FirstBreakID)
}

// TestVerifyDetectsTamperedTrace is this phase's definition of done: ten entries,
// the fifth one's trace_json rewritten directly in the database, and a verdict
// that names exactly that row.
//
// The rewrite is the attack the chain is signed against. Changing the trace
// changes the message the signature covers, and whoever rewrote the row does not
// hold the tenant's secret, so the stored hash_value cannot be brought back into
// agreement with the row: the mismatch is the evidence.
func TestVerifyDetectsTamperedTrace(t *testing.T) {
	pool, tenantID, secret, entries := chainedEntries(t)
	ctx := context.Background()

	tamper(ctx, t, pool,
		`UPDATE `+ledgerTable+` SET trace_json = $1 WHERE id = $2`,
		"tampered", entries[tamperedSpan].ID)

	// The row really is different now; without this, the test could pass on a
	// statement that matched nothing.
	var stored string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT trace_json FROM `+ledgerTable+` WHERE id = $1`, entries[tamperedSpan].ID).Scan(&stored))
	require.Equal(t, "tampered", stored)

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	assert.False(t, result.ChainValid, "a rewritten trace must break the chain")
	assert.Equal(t, entries[tamperedSpan].ID, result.FirstBreakID,
		"the rewritten row must be named as the first break")
	assert.True(t, entries[tamperedSpan].CreatedAt.Equal(result.FirstBreakAt),
		"FirstBreakAt must be the broken row's own created_at")
	assert.Equal(t, tamperedSpan+1, result.EntriesChecked,
		"the walk examines the rows up to and including the break, and stops there")
}

// TestVerifyDetectsRewrittenSignature covers the other half of what a row can
// have done to it: the trace is left alone and the stored hash is replaced. The
// message the row implies no longer produces the hash the row carries, which is
// the same mismatch a rewritten trace produces and the reason the comparison
// cannot be skipped in favour of the linkage check.
func TestVerifyDetectsRewrittenSignature(t *testing.T) {
	pool, tenantID, secret, entries := chainedEntries(t)
	ctx := context.Background()

	tamper(ctx, t, pool,
		`UPDATE `+ledgerTable+` SET hash_value = $1 WHERE id = $2`,
		ledgerPlaceholderHash, entries[tamperedSpan].ID)

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	assert.False(t, result.ChainValid, "a hash that is not the row's signature must break the chain")
	assert.Equal(t, entries[tamperedSpan].ID, result.FirstBreakID,
		"the row whose hash_value was replaced must be named")
	assert.Equal(t, tamperedSpan+1, result.EntriesChecked)
}

// TestVerifyDetectsRemovedRow is why the linkage check stands next to the
// signature check. The row after the hole was not touched at all, so its
// signature still recomputes to its stored hash -- the assertion below states
// that -- and only its prev_hash, which now names a hash no longer present in
// the ledger, gives the removal away.
func TestVerifyDetectsRemovedRow(t *testing.T) {
	pool, tenantID, secret, entries := chainedEntries(t)
	ctx := context.Background()

	tamper(ctx, t, pool, `DELETE FROM `+ledgerTable+` WHERE id = $1`, entries[tamperedSpan].ID)

	successor := entries[tamperedSpan+1]
	assert.Equal(t, successor.HashValue, independentSignature(secret, successor),
		"the row after the hole is untouched, which is what makes this case invisible to a signature-only walk")

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	assert.False(t, result.ChainValid, "a removed row must break the chain")
	assert.Equal(t, successor.ID, result.FirstBreakID,
		"the row after the hole chains from a hash that is no longer in the ledger")
	assert.Equal(t, tamperedSpan+1, result.EntriesChecked,
		"the survivors before the hole are examined, and then the row after it")
}

// TestVerifyDetectsTruncatedGenesis is the case a signature check cannot see at
// all: the tenant's first entry is removed, and every surviving entry -- the one
// that is now first included -- still carries a valid signature over its own
// fields. A walk that started at the oldest surviving row would call this chain
// valid; holding that row to the genesis link the write path gave it is what
// refuses to.
func TestVerifyDetectsTruncatedGenesis(t *testing.T) {
	pool, tenantID, secret, entries := chainedEntries(t)
	ctx := context.Background()

	tamper(ctx, t, pool, `DELETE FROM `+ledgerTable+` WHERE id = $1`, entries[0].ID)

	assert.Equal(t, entries[1].HashValue, independentSignature(secret, entries[1]),
		"the new first row's signature still verifies -- only its prev_hash is now wrong")

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	assert.False(t, result.ChainValid,
		"a tenant's chain starts at genesis, and the second row is not a first row")
	assert.Equal(t, entries[1].ID, result.FirstBreakID)
	assert.Equal(t, 1, result.EntriesChecked, "the walk stops at the first row it can see")
}

// TestVerifyRejectsAnotherSecret is the negative control. The ledger is
// untouched and only the key changes -- an all-zero key, which is not the 32
// random bytes the tenant store minted -- and no signature may reproduce under
// it. Without this, an implementation that compared a value with itself, or that
// ignored the secret, would still pass every tampering test above.
func TestVerifyRejectsAnotherSecret(t *testing.T) {
	pool, tenantID, _, entries := chainedEntries(t)
	ctx := context.Background()

	otherSecret := make([]byte, secretBytes)

	result, err := NewLedger(pool).Verify(ctx, tenantID, otherSecret)
	require.NoError(t, err)

	assert.False(t, result.ChainValid, "the signature must depend on the tenant's own secret")
	assert.Equal(t, entries[0].ID, result.FirstBreakID,
		"without the tenant's key the walk fails at the first entry")
	assert.Equal(t, 1, result.EntriesChecked, "and it stops there")
}
