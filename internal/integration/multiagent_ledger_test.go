//go:build integration

// Phase 29's two ledger adapters: the one plane answers GET
// /v2/compliance/chain-integrity through, and the one an edge node's compilation
// path appends through.
//
// Both exist because the code they mirror lives in a package main. cmd/plane's
// ledgerVerifier and cmd/synapse's enterpriseLedger cannot be imported by any
// test, and neither package can be reached by internal/plane or internal/ledger
// either (the verifier's own doc comment explains the import cycle). What each
// one does is two calls to the production implementation -- fetch the tenant's
// secret, hand it to internal/ledger -- so restating them here is restating the
// wiring rather than the behaviour under test.
//
// Split from multiagent_setup_test.go on the same seam every file in this project
// is split on: the 300-line ceiling, and one subject per file.
package integration

import (
	"context"
	"fmt"

	"synapse/internal/compiler"
	"synapse/internal/ledger"
	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// multiagentLedgerVerifier implements plane.LedgerVerifier: fetch the tenant's
// raw signing secret, hand it to the ledger's own walk, return the verdict.
//
// The secret is fetched per call, used to recompute signatures, and held by
// nobody -- there is no field here that could hold it, which is the same
// structural guarantee cmd/plane's verifier documents.
type multiagentLedgerVerifier struct {
	chain *ledger.Ledger
	pool  *pgxpool.Pool
}

// VerifyChain implements plane.LedgerVerifier.
func (v multiagentLedgerVerifier) VerifyChain(ctx context.Context, tenantID string) (plane.ChainIntegrityResult, error) {
	secret, err := tenant.GetSecret(ctx, v.pool, tenantID)
	if err != nil {
		return plane.ChainIntegrityResult{}, fmt.Errorf("ledger: fetch tenant secret: %w", err)
	}

	return v.chain.Verify(ctx, tenantID, secret)
}

// multiagentLedgerSink implements compiler.LedgerSink for one tenant: sign the
// trace and append it to that tenant's chain. It is cmd/synapse's
// enterpriseLedger again.
//
// This is the one piece of Phase 29's scenario a test has to restate, because the
// sink an edge node uses is installed by cmd/synapse at boot and an in-process
// edge server has no boot. The pool is the test's own rather than a second one:
// cmd/synapse opens a dedicated pool, and doing the same here would double this
// test's connections for no property it asserts.
type multiagentLedgerSink struct {
	chain    *ledger.Ledger
	pool     *pgxpool.Pool
	tenantID string
}

// AppendTrace implements compiler.LedgerSink. The request id is mapped to a uuid
// because the ledger's column is one and v1 mints ids like
// req-1758301000123456789 -- the same translation cmd/synapse/ledger.go performs,
// under the same namespace scheme.
func (s multiagentLedgerSink) AppendTrace(ctx context.Context, requestID, traceJSON string) error {
	secret, err := tenant.GetSecret(ctx, s.pool, s.tenantID)
	if err != nil {
		return fmt.Errorf("ledger: fetch tenant secret: %w", err)
	}

	_, err = s.chain.Append(ctx, s.tenantID, multiagentRequestUUID(requestID), traceJSON, secret)

	return err
}

// multiagentRequestNamespace is the fixed uuid namespace edge request ids are
// hashed under, mirroring cmd/synapse's ledgerRequestNamespace. It only has to be
// stable within this process: it exists so a request id that is not a uuid can be
// appended at all.
var multiagentRequestNamespace = uuid.MustParse("6f9c1e5a-3b7d-4c21-9a5e-8d0f4b2c7e31")

// multiagentRequestUUID maps a v1 request id onto the uuid the ledger stores.
func multiagentRequestUUID(requestID string) string {
	if _, err := uuid.Parse(requestID); err == nil {
		return requestID
	}

	return uuid.NewSHA1(multiagentRequestNamespace, []byte(requestID)).String()
}

// The adapters have to satisfy the same interfaces the production wiring does, so
// a signature drift is a build failure here rather than a 500 at request time.
var (
	_ plane.LedgerVerifier = multiagentLedgerVerifier{}
	_ compiler.LedgerSink  = multiagentLedgerSink{}
)
