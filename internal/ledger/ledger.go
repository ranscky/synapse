package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ledgerTable is the one table this package writes, schema-qualified. The schema
// name and the role the write runs as both come from internal/tenant, which owns
// the migration, so neither name is repeated here.
const ledgerTable = tenant.SchemaName + ".ledger"

// insertEntry writes one signed, already-chained row.
//
// created_at is stamped here rather than taking the column's now() default, and
// both halves of that stamp are load-bearing.
//
// clock_timestamp() rather than now(), because now() is the transaction's *start*
// time: two appends whose transactions began in one order but whose tenant locks
// were acquired in the other would store created_at in the opposite order to the
// chain. The newest row by created_at is the chain head (see chainHead), so the
// append after them would chain from the wrong row and fork the tenant's ledger.
//
// GREATEST(..., head + 1 microsecond) rather than clock_timestamp() alone,
// because two rows must never share a created_at: a tie makes "newest" ambiguous
// and the head read could pick either row. $7 is the current chain head's
// created_at -- NULL for a tenant's first entry, which GREATEST ignores -- so
// under the tenant lock every row is stamped strictly later than the row it
// chains from whatever the system clock does. A coarse clock, a clock that steps
// backwards, or two appends landing in one tick cannot tie or reorder a chain.
//
// There is deliberately no RETURNING: it requires SELECT on the returned columns
// and the writer role holds no SELECT at all. The row's created_at is read back
// afterwards instead.
const insertEntry = `INSERT INTO ` + ledgerTable + `
	(id, tenant_id, request_id, trace_json, prev_hash, hash_value, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, GREATEST(clock_timestamp(), $7::timestamptz + interval '1 microsecond'))`

// LedgerEntry is one row of the audit ledger, as it was written: every field
// records what the database holds, and nothing is recomputed on the way out.
//
// A later verification phase rebuilds one of these from a row and checks the
// signature against the tenant's secret; the field comments say what a verifier
// has to be able to reproduce.
type LedgerEntry struct {
	// ID is the row's uuid. It is minted in this process rather than left to the
	// DDL default because it is part of the signed message and therefore has to
	// exist before the INSERT.
	ID string
	// TenantID is the tenant whose chain this entry belongs to, in the canonical
	// uuid form Postgres stores -- the form the message is signed over, so a
	// verifier reading tenant_id back out of the row recomputes the same hash.
	TenantID string
	// RequestID identifies the request the trace came from. No foreign key
	// references it, deliberately: the ledger must be able to record a request
	// whose tenant row has since been frozen or removed.
	RequestID string
	// TraceJSON is the trace exactly as it was signed and stored. Nothing here
	// reformats it: any normalization would change the hash and break the chain.
	TraceJSON string
	// PrevHash is the previous entry's HashValue for this tenant, or the genesis
	// hash for the tenant's first entry.
	PrevHash string
	// HashValue is hex(HMAC-SHA256(secret, ID+TenantID+RequestID+PrevHash+TraceJSON)).
	HashValue string
	// CreatedAt is the database's own insertion timestamp, read back from the
	// committed row rather than taken from this process's clock.
	CreatedAt time.Time
}

// Ledger is the append-only writer for synapse_global.ledger.
//
// It holds no logger, on purpose: a ledger write handles HMAC keys and full trace
// payloads, and the structural guarantee that neither can be logged is that this
// type has nowhere to log them. A caller that wants a line about an append has
// the returned LedgerEntry's id, which is safe to print.
type Ledger struct {
	pool *pgxpool.Pool
}

// NewLedger returns a Ledger that appends through pool. The pool is not owned:
// the caller closes it, exactly as internal/tenant's Store leaves it.
func NewLedger(pool *pgxpool.Pool) *Ledger {
	return &Ledger{pool: pool}
}

// Append signs traceJSON with secret and appends it to tenantID's hash chain,
// returning the row it wrote.
//
// One transaction, two identities, and that split is the design. The chain head
// is read as whatever role the pool connects as -- the table's owner, which holds
// SELECT -- and the INSERT runs as tenant.LedgerWriterRole, which holds INSERT on
// that one table and nothing else. Reading the head as the writer is impossible
// (it is granted no SELECT), and writing as the owner would make the append-only
// grant decorative, which is the trade Phase 15's second finding said not to
// make. SET LOCAL ROLE rather than SET ROLE, so a pooled connection is never
// handed back still wearing the writer's identity.
//
// A per-tenant advisory lock serializes appends for the rest of the transaction,
// so two concurrent requests cannot both read the same head and chain from it.
// Together with the strictly-increasing created_at stamp in insertEntry, a
// tenant's entries form one linear chain however many writers race.
//
// secret is passed in rather than fetched: the caller already holds the tenant's
// raw secret for the request (tenant.GetSecret), and this package cannot decrypt
// one, so a signing key cannot leak through code that never sees the store. The
// secret appears in no return value other than the signature it produces, and in
// no error.
//
// Fail-closed: a nil pool, an empty secret, or a tenant or request id that is not
// a uuid is an error with no row written -- never an unsigned or unchained entry.
func (l *Ledger) Append(ctx context.Context, tenantID, requestID, traceJSON string, secret []byte) (LedgerEntry, error) {
	if l == nil || l.pool == nil {
		return LedgerEntry{}, fmt.Errorf("ledger: append needs a database pool")
	}

	if len(secret) == 0 {
		return LedgerEntry{}, fmt.Errorf("ledger: append needs a signing secret")
	}

	entry, err := newEntry(tenantID, requestID, traceJSON)
	if err != nil {
		return LedgerEntry{}, err
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: begin append: %w", err)
	}
	// A committed transaction makes this a no-op; it only ever fires on the
	// failure paths below.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockChain(ctx, tx, entry.TenantID); err != nil {
		return LedgerEntry{}, err
	}

	// The head's created_at comes back with its hash: the INSERT stamps the new row
	// strictly after it, which is what keeps "newest by created_at" unambiguous.
	headHash, headCreatedAt, err := chainHead(ctx, tx, entry.TenantID)
	if err != nil {
		return LedgerEntry{}, err
	}

	entry.PrevHash = headHash
	entry.HashValue = signature(secret, entry)

	// The only statement in this package that runs as the writer role, and the
	// only reason that role and its membership exist.
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+tenant.LedgerWriterRole); err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: assume role %s: %w", tenant.LedgerWriterRole, err)
	}

	tag, err := tx.Exec(ctx, insertEntry,
		entry.ID, entry.TenantID, entry.RequestID, entry.TraceJSON, entry.PrevHash, entry.HashValue, headCreatedAt)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: insert ledger entry: %w", err)
	}

	// INSERT either writes one row or fails outright, so anything else means the
	// row this entry claims to be is not what landed.
	if tag.RowsAffected() != 1 {
		return LedgerEntry{}, fmt.Errorf("ledger: insert ledger entry wrote %d rows, expected 1", tag.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: commit append: %w", err)
	}

	// Read back through the pool as the application role, for the same reason the
	// head was read that way: the writer holds no SELECT.
	if err := l.readCreatedAt(ctx, entry.ID, &entry.CreatedAt); err != nil {
		return LedgerEntry{}, err
	}

	return entry, nil
}

// newEntry validates the caller's ids and mints the row id. Both ids are
// canonicalized before they are hashed, so a verifier that reads the row back
// -- where Postgres renders a uuid in its canonical lowercase form -- recomputes
// the very same message.
func newEntry(tenantID, requestID, traceJSON string) (LedgerEntry, error) {
	tenantUUID, err := canonicalID(tenantID)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: tenant id: %w", err)
	}

	requestUUID, err := canonicalID(requestID)
	if err != nil {
		return LedgerEntry{}, fmt.Errorf("ledger: request id: %w", err)
	}

	return LedgerEntry{
		ID:        uuid.NewString(),
		TenantID:  tenantUUID,
		RequestID: requestUUID,
		TraceJSON: traceJSON,
	}, nil
}

// canonicalID renders a uuid the way Postgres stores it, or fails. The input is
// never echoed: an id is not a secret, but an error message is a log line.
func canonicalID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", errors.New("must be a uuid")
	}

	return parsed.String(), nil
}

// lockChain serializes this tenant's appends for the rest of the transaction.
//
// Without it, two concurrent appends read the same chain head and both chain from
// it, which leaves the tenant's ledger with two entries pointing at one
// predecessor -- a fork, and precisely the structure the signature chain exists
// to make impossible.
//
// pg_advisory_xact_lock is taken for the transaction, so it is released by the
// COMMIT or the ROLLBACK without a matching unlock to forget. hashtext maps a
// tenant into a 32-bit key, so two tenants can occasionally share one lock; that
// costs throughput, never correctness, because an over-broad lock can only
// exclude.
func lockChain(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, tenantID); err != nil {
		return fmt.Errorf("ledger: lock tenant chain: %w", err)
	}

	return nil
}

// chainHead returns the hash of the tenant's most recent entry and that entry's
// created_at, or the genesis hash and a nil timestamp when the tenant has none
// yet (insertEntry's GREATEST ignores a NULL, which is how a first entry gets the
// clock's own time).
//
// "Most recent by created_at" is exactly right because every append holds the
// tenant's lock while it inserts and stamps its row strictly later than the head
// it just read (see insertEntry): created_at order is chain order, so the newest
// row is always the one the next entry has to chain from.
//
// This read runs as the connection's own role, before Append switches to
// ledger_writer: the writer is granted no SELECT, and granting it one would
// weaken the append-only guarantee Phase 15 established.
func chainHead(ctx context.Context, tx pgx.Tx, tenantID string) (string, *time.Time, error) {
	var (
		head      string
		createdAt time.Time
	)

	err := tx.QueryRow(ctx,
		`SELECT hash_value, created_at FROM `+ledgerTable+` WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 1`,
		tenantID,
	).Scan(&head, &createdAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return genesisHash(), nil, nil
	case err != nil:
		return "", nil, fmt.Errorf("ledger: read chain head: %w", err)
	}

	return head, &createdAt, nil
}

// readCreatedAt fills in the timestamp the database actually stored, so the
// returned entry is the row rather than a prediction of it.
//
// It runs after the COMMIT and as the pool's own role, because the writer role
// cannot read the table it just wrote to. A failure here leaves a committed row
// whose timestamp could not be read back; Append reports that as an error instead
// of inventing a value, so the caller never treats an unverified entry as a
// written one.
func (l *Ledger) readCreatedAt(ctx context.Context, id string, into *time.Time) error {
	if err := l.pool.QueryRow(ctx, `SELECT created_at FROM `+ledgerTable+` WHERE id = $1`, id).Scan(into); err != nil {
		return fmt.Errorf("ledger: read back created_at: %w", err)
	}

	return nil
}
