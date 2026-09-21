//go:build integration

// Verification's two boundary cases: a tenant with no chain at all, and a
// verifier that cannot run.
//
// Split out of verify_test.go, which carries the walk's tamper tests, because
// that file is already at the 300-line ceiling this project holds every file to.
// Both cases are about *not* answering: an empty chain must not be called
// suspect, and an unusable verifier must not answer at all -- the one shape of
// lie an audit endpoint can tell.
package ledger

import (
	"context"
	"testing"

	"synapse/internal/tenant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyEmptyChain: a tenant the plane has appended nothing for has nothing
// that could have been tampered with, so an empty chain is valid rather than an
// error -- and the answer still carries a CheckedAt, because the check did run.
// A freshly provisioned tenant must not be told its empty ledger is suspect.
func TestVerifyEmptyChain(t *testing.T) {
	pool := ledgerPool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))
	setMasterKey(t)

	tenantID, secret := testChainTenant(t, pool)

	result, err := NewLedger(pool).Verify(ctx, tenantID, secret)
	require.NoError(t, err)

	assert.True(t, result.ChainValid, "an empty chain is valid: there is nothing that could have been tampered with")
	assert.Zero(t, result.EntriesChecked)
	assert.Empty(t, result.FirstBreakID)
	assert.True(t, result.FirstBreakAt.IsZero())
	assert.False(t, result.CheckedAt.IsZero(), "the check ran, even though it examined nothing")
}

// TestVerifyRejectsUnusableInput: a verifier that cannot run must fail closed.
// Every case here would otherwise be a route to a chain_valid answer for a check
// that never happened, which is the worst answer an audit endpoint can give.
func TestVerifyRejectsUnusableInput(t *testing.T) {
	pool := ledgerPool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))
	setMasterKey(t)

	tenantID, secret := testChainTenant(t, pool)

	tests := []struct {
		name     string
		ledger   *Ledger
		tenantID string
		secret   []byte
	}{
		{name: "no ledger at all", ledger: nil, tenantID: tenantID, secret: secret},
		{name: "no database pool", ledger: NewLedger(nil), tenantID: tenantID, secret: secret},
		{name: "no signing secret", ledger: NewLedger(pool), tenantID: tenantID},
		{name: "tenant id is not a uuid", ledger: NewLedger(pool), tenantID: "tenant-" + tenantID, secret: secret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.ledger.Verify(ctx, tt.tenantID, tt.secret)

			require.Error(t, err, "an unusable verifier must fail, not answer")
			assert.Contains(t, err.Error(), "ledger:", "errors carry the package prefix")
			assert.False(t, result.ChainValid, "no verdict may be reported for a check that did not run")
			assert.Zero(t, result.EntriesChecked)
			assert.True(t, result.CheckedAt.IsZero(), "a failed check reports no time")
		})
	}
}
