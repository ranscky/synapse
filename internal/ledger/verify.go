// Chain verification for the audit ledger: the read half of the write path
// ledger.go holds.
//
// The claim this file implements is the one the whole table exists for: every
// entry in a tenant's chain can be checked, by anyone who holds that tenant's
// secret and can read the rows, for both properties the write path signed --
// that the entry is the entry that was written, and that it sits where it says
// it sits in the chain.
package ledger

import (
	"context"
	"crypto/hmac"
	"fmt"
	"time"

	"synapse/internal/plane"
)

// ChainIntegrityResult is the answer to "is this tenant's ledger intact?".
//
// It is an alias, not a second declaration. The type is defined in
// internal/plane because that is where its only consumer is -- the 200 body of
// GET /v2/ledger/verify -- and because this package cannot be imported from
// there: internal/ledger imports internal/tenant (SchemaName, LedgerWriterRole,
// and now GetSecret) and internal/tenant imports internal/plane (the
// verified-claim accessors JWTMiddleware publishes), so plane -> ledger is a
// cycle. Declaring the five fields twice, once per package, is exactly the drift
// this alias prevents, and the arrangement already holds for
// plane.ProvisionResult, which internal/tenant's Provisioner returns.
type ChainIntegrityResult = plane.ChainIntegrityResult

// verifyQuery reads one tenant's whole chain in chain order, which the write
// path guarantees is created_at order: every append stamps its row strictly
// later than the head it chained from, under the tenant's advisory lock
// (insertEntry's GREATEST(clock_timestamp(), head + 1 microsecond)).
//
// deleted_at IS NULL is part of the walk rather than an optimization: nothing in
// this project can soft-delete a ledger row (the writer role holds no UPDATE),
// so the filter is what would make a row hidden that way show up as a broken
// link at its successor instead of as a silently shortened chain.
//
// The three ids are cast to text rather than left as uuid. What the signature
// covers is the canonical lowercase rendering, which uuid::text is exactly, and
// reading a uuid back as text is what tenant.Store.CreateTenant already does --
// one convention for one value.
//
// This runs on the pool's own connection, never as tenant.LedgerWriterRole:
// that role holds INSERT and no SELECT at all, which is what makes the ledger
// append-only in the database (Phase 15) and is why a verifier needs the owner's
// identity, exactly as the write path's chain-head read does (Phase 16).
const verifyQuery = `SELECT id::text, tenant_id::text, request_id::text, trace_json, prev_hash, hash_value, created_at
	FROM ` + ledgerTable + `
	WHERE tenant_id = $1 AND deleted_at IS NULL
	ORDER BY created_at ASC`

// Verify walks tenantID's ledger in chain order and reports whether every entry
// is still the entry that was signed.
//
// Two independent properties are checked per row, and both are needed because
// neither one implies the other:
//
//	signature  hex(HMAC-SHA256(secret, id+tenant_id+request_id+prev_hash+trace_json))
//	           must equal the stored hash_value. This is what detects a row whose
//	           trace, request id, or id was *rewritten*: the writer role cannot
//	           do that, but nothing stops a superuser, which is the threat model
//	           an audit ledger has to survive. Without the tenant's secret the
//	           rewritten row cannot be re-signed, so the stored hash stops
//	           matching what the row now says.
//	linkage    each row's prev_hash must equal its predecessor's hash_value, and
//	           the tenant's first row must chain from genesis. This is what
//	           detects a *removed* row: the entry after the hole still carries a
//	           valid signature over its own fields -- nothing about it changed --
//	           but its prev_hash now names a hash that is not in the chain.
//
// The walk stops at the first failure and reports it: nothing past a break is
// meaningful, because once one row is unaccounted for every later link is
// unverifiable with it. EntriesChecked counts the row that broke the chain.
//
// Comparison is hmac.Equal rather than !=. Both sides are 64-character hex
// digests, so the lengths always match and the comparison is constant time; a
// verifier has no reason to offer a timing side channel about how far into a
// digest two values agree.
//
// Fail-closed: no pool, no secret, or a tenant id that is not a uuid is an error
// with no partial result -- never a "valid" verdict for a check that did not
// run. secret is passed in, never fetched here: the caller already holds the
// tenant's raw key for the request (tenant.GetSecret) and this package cannot
// decrypt one, so a signing key appears in no return value and no error. An
// empty chain is valid, vacuously: a tenant with nothing appended has nothing
// that could have been tampered with, and a freshly provisioned tenant should
// not be told its empty ledger is suspect.
func (l *Ledger) Verify(ctx context.Context, tenantID string, secret []byte) (ChainIntegrityResult, error) {
	if l == nil || l.pool == nil {
		return ChainIntegrityResult{}, fmt.Errorf("ledger: verify needs a database pool")
	}

	if len(secret) == 0 {
		return ChainIntegrityResult{}, fmt.Errorf("ledger: verify needs a signing secret")
	}

	// Canonicalized exactly as Append canonicalizes it, so the tenant id inside
	// every recomputed message is the one the row was signed over.
	id, err := canonicalID(tenantID)
	if err != nil {
		return ChainIntegrityResult{}, fmt.Errorf("ledger: tenant id: %w", err)
	}

	result := ChainIntegrityResult{ChainValid: true, CheckedAt: time.Now().UTC()}

	rows, err := l.pool.Query(ctx, verifyQuery, id)
	if err != nil {
		return ChainIntegrityResult{}, fmt.Errorf("ledger: read entries to verify: %w", err)
	}
	defer rows.Close()

	// prev starts at the genesis hash, which holds the first surviving row to
	// the rule the write path applied when it wrote it: a tenant's first entry
	// chains from hex(sha256("genesis")). Without that, a chain whose earliest
	// rows were removed would start its walk at the first row still present and
	// report valid -- the one tamper a signature check alone cannot see, because
	// every surviving row's signature is untouched.
	prev := genesisHash()

	// Streamed, not collected: answering "where is the first break" does not
	// require one tenant's whole ledger in memory, and a verifier that needed it
	// would be the slowest way to answer the cheapest question about a chain.
	for rows.Next() {
		var entry LedgerEntry
		if err := rows.Scan(&entry.ID, &entry.TenantID, &entry.RequestID,
			&entry.TraceJSON, &entry.PrevHash, &entry.HashValue, &entry.CreatedAt); err != nil {
			return ChainIntegrityResult{}, fmt.Errorf("ledger: scan ledger entry: %w", err)
		}

		result.EntriesChecked++

		// signature(secret, entry) is the write path's own primitive, so the
		// message this verifier rebuilds cannot drift from the one Append
		// signed. That the primitive itself is right is what the package's
		// integration tests check, by recomputing the same digest from the
		// brief's definition with crypto/hmac directly and never calling it.
		if entry.PrevHash != prev || !hmac.Equal([]byte(signature(secret, entry)), []byte(entry.HashValue)) {
			result.ChainValid = false
			result.FirstBreakID = entry.ID
			result.FirstBreakAt = entry.CreatedAt

			return result, nil
		}

		prev = entry.HashValue
	}

	if err := rows.Err(); err != nil {
		return ChainIntegrityResult{}, fmt.Errorf("ledger: read entries to verify: %w", err)
	}

	return result, nil
}
