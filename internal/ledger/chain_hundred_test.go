//go:build integration

// The chain-length case and the entry-47 case: the two claims verify_test.go makes
// over ten entries, exercised over a hundred.
//
// Length is its own file rather than more cases in that fixture because what is
// under test here is a scale and a position, not a rule verify_test.go does not
// already cover. At a hundred entries an implementation whose walk stopped early,
// whose count meant something other than "rows examined", or whose `first_break_id`
// was off by one has ninety-nine chances to say so instead of nine.
//
// The database is a precondition, not an option (see ledger_table_test.go): the
// documented commands export nothing but the tag.
//
//	go test ./internal/ledger/... -run TestVerifyHundredEntryChain -v -tags integration
//	go test ./internal/ledger/... -run TestVerifyNamesEntry47 -v -tags integration
package ledger

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// hundredEntries is the chain length the QA checklist names: long enough that a
	// single batch-shaped read cannot pass for a walk, and that the walk's own
	// position bookkeeping has to be right a hundred times rather than ten.
	hundredEntries = 100

	// tamperedOrdinal is the entry the tamper test breaks, numbered the way a reader
	// counts: "entry 47" is the forty-seventh appended entry, at index 46.
	tamperedOrdinal = 47

	// tamperedIndex is that entry's position in the slice the appends returned.
	tamperedIndex = tamperedOrdinal - 1
)

// chainedHundredEntries provisions a fresh tenant and appends hundredEntries
// entries for it, returning the pool, the tenant's id and secret, and the entries
// in chain order.
//
// It mirrors verify_test.go's chainedEntries rather than generalizing it: that
// helper's call sites assert counts as part of their own claims ("ten appends that
// nothing touched must verify"), and folding a length parameter into it would make
// those call sites say strictly less than they do now. The shared parts -- the
// pool, the migrations, the master key, the tenant, the trace shape -- are the same
// code either way.
func chainedHundredEntries(t *testing.T) (*pgxpool.Pool, string, []byte, []LedgerEntry) {
	t.Helper()

	pool := ledgerPool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))
	setMasterKey(t)

	tenantID, secret := testChainTenant(t, pool)

	ledger := NewLedger(pool)

	entries := make([]LedgerEntry, 0, hundredEntries)
	for i := 0; i < hundredEntries; i++ {
		entry, err := ledger.Append(ctx, tenantID, uuid.NewString(), appendTrace(i), secret)
		require.NoError(t, err, "append %d of %d", i+1, hundredEntries)

		entries = append(entries, entry)
	}

	require.Len(t, entries, hundredEntries)

	return pool, tenantID, secret, entries
}

// logVerdict prints a verdict as the JSON a caller of the chain-integrity endpoint
// would read, rather than as a Go struct dump.
//
// It exists so the checklist's "paste Verify() output" is the endpoint's own answer:
// ChainIntegrityResult is the body of GET /v2/compliance/chain-integrity (it is
// plane.ChainIntegrityResult, aliased), so the marshaled bytes here and the bytes a
// tenant receives are produced by one set of json tags.
func logVerdict(t *testing.T, label string, result ChainIntegrityResult) {
	t.Helper()

	encoded, err := json.MarshalIndent(result, "", "  ")
	require.NoError(t, err, "a verdict must be marshalable: it is an endpoint body")

	t.Logf("%s\n%s", label, encoded)
}

// TestVerifyHundredEntryChain is the length case: a hundred signed appends, nothing
// touched, and a verdict that says so with every entry examined.
//
// The count is checked against the table as well as against the walk, so a verifier
// that reported hundredEntries without reading that many rows would be contradicted
// by the ledger itself.
func TestVerifyHundredEntryChain(t *testing.T) {
	pool, tenantID, secret, entries := chainedHundredEntries(t)
	ctx := context.Background()

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	logVerdict(t, "Verify() over a 100-entry chain:", result)

	assert.True(t, result.ChainValid, "a hundred appends that nothing touched must verify")
	assert.Equal(t, hundredEntries, result.EntriesChecked,
		"the walk must reach the hundredth entry rather than stop at an earlier link")
	assert.Empty(t, result.FirstBreakID, "a valid chain names no break")
	assert.True(t, result.FirstBreakAt.IsZero(), "a valid chain reports no break time")
	assert.False(t, result.CheckedAt.IsZero(), "CheckedAt must be this walk's own clock reading")

	var stored int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM `+ledgerTable+` WHERE tenant_id = $1`, tenantID).Scan(&stored))
	assert.Equal(t, hundredEntries, stored, "the tenant's ledger really holds a hundred rows")

	// The last link in particular: entry 100 chains from entry 99's signature. A walk
	// that stopped at entry 99 and counted from zero would pass the count above only by
	// accident, and would never look at this link.
	assert.Equal(t, entries[hundredEntries-2].HashValue, entries[hundredEntries-1].PrevHash,
		"entry 100 must chain from entry 99")
}

// TestVerifyNamesEntry47WhenEntry47IsTampered is the position case: entry 47 of a
// hundred has its trace rewritten directly in the database, and the verdict has to
// name exactly that row -- not the row before it, not the row after it, and not the
// last row the walk happened to reach.
//
// Both halves of the break are asserted. The signature check is what catches this
// rewrite, because the row's own hash_value column is untouched; the entries around it
// keep the links they were written with, which is what makes the verdict a finding
// about one entry rather than a doubt about the whole chain.
func TestVerifyNamesEntry47WhenEntry47IsTampered(t *testing.T) {
	pool, tenantID, secret, entries := chainedHundredEntries(t)
	ctx := context.Background()

	tampered := entries[tamperedIndex]

	tamper(ctx, t, pool,
		`UPDATE `+ledgerTable+` SET trace_json = $1 WHERE id = $2`,
		"tampered", tampered.ID)

	// The row really is different now. Without this, the test could pass on a
	// statement that matched nothing.
	var stored string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT trace_json FROM `+ledgerTable+` WHERE id = $1`, tampered.ID).Scan(&stored))
	require.Equal(t, "tampered", stored)

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	t.Logf("tampered entry %d of %d: id=%s created_at=%s",
		tamperedOrdinal, hundredEntries, tampered.ID, tampered.CreatedAt.Format(time.RFC3339Nano))

	logVerdict(t, "Verify() after tampering entry 47:", result)

	assert.False(t, result.ChainValid,
		"entry %d's rewritten trace must break the chain", tamperedOrdinal)
	assert.Equal(t, tampered.ID, result.FirstBreakID,
		"first_break_id must be entry %d's own id", tamperedOrdinal)
	assert.Equal(t, tamperedOrdinal, result.EntriesChecked,
		"the walk examines entries 1..%d and stops there", tamperedOrdinal)
	assert.True(t, tampered.CreatedAt.Equal(result.FirstBreakAt),
		"first_break_at must be entry %d's own created_at", tamperedOrdinal)

	// Entry 47's link from entry 46 is still intact, and entry 48's link from entry
	// 47's stored hash is too: the rewrite changed neither prev_hash nor hash_value,
	// which is precisely why only the signature check can see it.
	assert.Equal(t, entries[tamperedIndex-1].HashValue, tampered.PrevHash,
		"entry %d still chains from entry %d", tamperedOrdinal, tamperedOrdinal-1)
	assert.Equal(t, entries[tamperedIndex+1].PrevHash, tampered.HashValue,
		"entry %d still chains from entry %d's stored signature", tamperedOrdinal+1, tamperedOrdinal)
}
