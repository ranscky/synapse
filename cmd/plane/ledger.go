// The ledger verifier the plane's audit endpoint is wired to: the one place the
// control plane's HTTP contract and the ledger's read path meet.
//
// It exists because neither package can import the other. internal/plane cannot
// import internal/ledger (the ledger imports internal/tenant, which imports the
// plane for the verified-claim accessors), and internal/ledger cannot import
// internal/plane's Server. main can see both, which is the same reason the
// provisioner, the memory writer, and the JWT middleware above are constructed
// here rather than inside a package.
package main

import (
	"context"
	"fmt"

	"synapse/internal/ledger"
	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ledgerVerifier implements plane.LedgerVerifier: fetch the tenant's raw signing
// secret, hand it to the ledger's own walk, and return the verdict.
//
// The secret is fetched per call and never held. It is read from the tenant
// store, used to recompute signatures, and dropped when VerifyChain returns;
// no field of this type can hold it, so there is nothing here to log or leak,
// and the value reaches no return value and no error.
type ledgerVerifier struct {
	ledger *ledger.Ledger
	pool   *pgxpool.Pool
}

// newLedgerVerifier returns the verifier that GET /v2/ledger/verify answers
// through. The pool is not owned: cmd/plane closes it, exactly as the migrator
// and the tenant store leave it.
func newLedgerVerifier(pool *pgxpool.Pool) ledgerVerifier {
	return ledgerVerifier{ledger: ledger.NewLedger(pool), pool: pool}
}

// VerifyChain implements plane.LedgerVerifier.
//
// tenantID comes from the verified token, so this can only ever walk the
// caller's own chain -- the same property the sync and search endpoints rely on,
// and what keeps the endpoint from being a way to read another tenant's ledger
// row ids and timestamps.
//
// A tenant that exists but has no stored secret surfaces as
// tenant.ErrSecretNotFound, which the plane answers with its generic 500. That
// is a real gap rather than a nicety: nothing mints a tenant secret at
// provisioning time yet (Phase 16's first finding), so in a running deployment
// this endpoint cannot verify anything until a phase wires that in. It is called
// out in PROGRESS.md rather than papered over with a "everything is fine"
// verdict, which is what an unverifiable chain must never be reported as.
func (v ledgerVerifier) VerifyChain(ctx context.Context, tenantID string) (plane.ChainIntegrityResult, error) {
	secret, err := tenant.GetSecret(ctx, v.pool, tenantID)
	if err != nil {
		return plane.ChainIntegrityResult{}, fmt.Errorf("ledger: fetch tenant secret: %w", err)
	}

	return v.ledger.Verify(ctx, tenantID, secret)
}
