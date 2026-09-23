// The write half of v2 usage metering: one row per successful compilation,
// appended off the request path.
//
// What this package owns is exactly one table -- synapse_global.usage_events --
// and the two facts a row needs to be useful: which tenant and agent the
// compilation belonged to, and how many tokens it saved. Those numbers are what
// the control plane's compliance report reads back (internal/ledger/report.go
// supplies count(*) and avg(reduction_pct) for one tenant's window, and
// internal/plane turns them into the report's average context reduction), which
// is why reduction_pct is stored in the same 0-100 form the edge node already
// computes for its trace rather than as a fraction: the report renders it as a
// percentage, so a unit mismatch here would be published as a fact about the
// tenant.
//
// Nothing in this package interprets what it writes, and nothing here reads it
// back. There is no query in this file but the INSERT and no aggregation: the
// report's arithmetic lives in internal/plane, where it is a pure function and
// therefore testable without a database.
//
// Two properties are deliberately structural rather than conventional:
//
//   - Record never blocks a caller and never returns an error. It is called
//     from the compilation path, so a database that is slow, down, or refusing
//     writes must cost one goroutine and one warning rather than a slow or
//     failed compilation.
//   - session_id is stored and never logged. It is the one field of a usage
//     event a caller might consider private -- a session id is a conversation
//     handle -- so the failure path below logs the error and nothing else: not
//     the session, not the tenant, not the agent.
package metering

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// usageEventsTable is the one table this package writes, schema-qualified.
	// The schema name comes from internal/tenant, which owns the migration, so
	// it is written down once -- the same arrangement internal/ledger has for
	// .ledger and .compliance_access_log.
	usageEventsTable = tenant.SchemaName + ".usage_events"

	// recordTimeout bounds one insert. It is the write's own ceiling rather
	// than the request's: the request is over by the time this runs, so a
	// database that cannot accept one row in three seconds costs one abandoned
	// goroutine rather than a queue of them.
	recordTimeout = 3 * time.Second
)

// insertEvent writes one usage row. Every column the event carries is named
// explicitly, and id is not among them: this table's id has a
// gen_random_uuid() default and is not part of any signed message, unlike the
// ledger's, so the database can mint it. created_at is passed in from the event
// (see UsageEvent.CreatedAt) rather than left to now(), so the row records when
// the compilation finished rather than when the goroutine got a connection --
// the two differ exactly when the database is busy, which is when a usage
// reconciliation would care.
const insertEvent = `INSERT INTO ` + usageEventsTable + `
	(tenant_id, agent_id, session_id, raw_tokens, compiled_tokens, reduction_pct, model, created_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// UsageEvent is one finished compilation's token accounting.
//
// It is carried by value into the writing goroutine, so a caller may reuse its
// own event after Record returns without racing the write.
//
// TenantID and AgentID are attribution only: nothing in this package reads them
// and no row here grants access to anything. SessionID is stored and never
// logged nor returned by any API. RawTokens is the retrieved candidate pool's
// size (what the context would have cost) and CompiledTokens is what the sieve
// actually emitted; ReductionPct is the percentage between them, in the same
// 0-100 form the trace records. Model is the operator's label for the upstream
// model (config.UpstreamModel) or "" when the node does not name one -- stored
// as NULL in that case, because "not named" is not "the empty model".
type UsageEvent struct {
	// TenantID is the tenant the compilation belongs to, as a uuid: the
	// column is uuid, so a value that is not one is refused by canonicalID
	// below instead of failing every insert.
	TenantID string
	// AgentID is the compiling node's own agent-id (config.AgentID), the same
	// value the trace attributes its memories to.
	AgentID string
	// SessionID is the session the conversation belonged to.
	SessionID string
	// RawTokens is the token count of the retrieved candidate pool -- the
	// baseline the reduction is measured against.
	RawTokens int
	// CompiledTokens is the token count of the context the sieve emitted.
	CompiledTokens int
	// ReductionPct is (raw - compiled) / raw * 100, or 0 when the pool was
	// empty and there was nothing to reduce.
	ReductionPct float64
	// Model labels the upstream the savings are attributed to.
	Model string
	// CreatedAt is when the compilation finished. A zero value is stamped with
	// time.Now() at the moment Record is called, so a caller that does not care
	// does not have to pass one.
	CreatedAt time.Time
}

// Meter writes usage events to Postgres.
//
// It holds one pool and no logger. The pool is not owned: whoever constructed
// the meter (cmd/synapse) closes it, exactly as the ledger's writer, the
// auditor, and the tenant store leave their pools to their caller. A meter with
// a nil pool is inert rather than a panic, so a node that could not reach a
// database can hold one without every compilation having to ask whether it did.
type Meter struct {
	pool *pgxpool.Pool
}

// NewMeter returns a Meter that writes through pool.
func NewMeter(pool *pgxpool.Pool) *Meter {
	return &Meter{pool: pool}
}

// Record writes one usage event and returns immediately: the INSERT happens in
// its own goroutine, under its own three-second timeout, and nothing about it
// reaches the caller.
//
// Failures are logged and dropped, never retried. A retry would have to either
// buffer the event (this process would then hold usage data it has no store
// for) or block a caller, and the next compilation writes its own row anyway --
// the same reasoning internal/compiler's ledger sink documents for an append
// that fails. The warning names the error and nothing else: a usage event
// carries a session id and a tenant id, and neither belongs in a log line.
//
// A nil meter, a nil pool, or a tenant id that is not a uuid does nothing -- a
// warning in the last case only. This method is called from a live request
// path, so it must not be able to panic or block there under any input.
func (m *Meter) Record(event UsageEvent) {
	if m == nil || m.pool == nil {
		return
	}

	id, err := canonicalID(event.TenantID)
	if err != nil {
		slog.Warn("metering: record failed", "err", err)
		return
	}

	// The event is copied into the goroutine below: the caller's struct is
	// never read by the write, so a caller that reuses or mutates its own
	// event cannot race it.
	event.TenantID = id
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}

	go func(event UsageEvent) {
		ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
		defer cancel()

		if _, err := m.pool.Exec(ctx, insertEvent,
			event.TenantID,
			event.AgentID,
			event.SessionID,
			event.RawTokens,
			event.CompiledTokens,
			event.ReductionPct,
			nullableText(event.Model),
			event.CreatedAt,
		); err != nil {
			slog.Warn("metering: record failed", "err", err)
		}
	}(event)
}

// canonicalID returns id in the canonical uuid form Postgres stores, or an
// error naming the field rather than the value it was given. The tenant id
// arrives from the node's own credential, and a malformed one has to be caught
// here: the column is uuid, so an insert that carried it would fail every
// single time, once per compilation, for as long as the node ran.
func canonicalID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", errors.New("metering: tenant id must be a uuid")
	}

	return parsed.String(), nil
}

// nullableText maps "" to NULL. usage_events.model is nullable, and "the
// operator did not name the upstream model" is a different fact from "the model
// is the empty string" -- a distinction that costs one line now and a data
// cleanup later, once something groups usage by model.
func nullableText(value string) any {
	if value == "" {
		return nil
	}

	return value
}
