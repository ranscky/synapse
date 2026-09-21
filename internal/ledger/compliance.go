// The compliance audit endpoint's read path: one page of a tenant's signed
// ledger, and the access record for the read.
//
// It lives in this package rather than in internal/plane for the reason the
// whole ledger does: the plane's HTTP package holds interfaces, not database
// handles, and internal/ledger already owns the ledger table's name, its uuid
// canonicalization, and its read path (verify.go). It cannot live in cmd/plane
// either -- GET /v2/compliance/audit's integration test wires the real
// implementation, and a test binary cannot import package main -- so the package
// that owns the table is the only place left, and the right one.
//
// The two halves travel together in one type because they are one capability to
// their caller -- "read a page of a tenant's audit history and record that it
// happened" -- and because the endpoint refuses to serve the first without the
// second (see plane/compliance.go).
package ledger

import (
	"context"
	"fmt"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/jackc/pgx/v5/pgxpool"
)

// complianceAccessLogTable is the table every compliance read is recorded in.
// The schema name comes from internal/tenant, which owns the migration, so the
// name is written down once -- the same arrangement ledgerTable has.
const complianceAccessLogTable = tenant.SchemaName + ".compliance_access_log"

// auditPageQuery reads one page of a tenant's chain, newest first.
//
// The two bounds arrive as nullable timestamps and the predicate is written as
// "$n IS NULL OR ...", so "no window" is one code path in the database rather
// than two statement shapes that could drift apart. The ::timestamptz casts are
// what make a NULL unambiguous to the server: without them a bare IS NULL test
// on an untyped parameter has no type for the planner to infer.
//
// deleted_at IS NULL is the same filter the chain walk applies, and for the same
// reason: nothing in this project can soft-delete a ledger row (the writer role
// holds no UPDATE), so a row hidden that way must not appear here as though the
// chain had never contained it.
//
// The three ids are cast to text rather than left as uuid, so what leaves this
// query is the canonical lowercase rendering the signature covers -- the
// convention verifyQuery follows, for the same reason.
//
// ORDER BY created_at DESC is the order the caller asked for and the order the
// write path guarantees is meaningful: every append stamps its row strictly later
// than the head it chained from, so newest-first is also
// most-recently-chained-first.
const auditPageQuery = `SELECT id::text, tenant_id::text, request_id::text, trace_json, prev_hash, hash_value, created_at
	FROM ` + ledgerTable + `
	WHERE tenant_id = $1
	  AND deleted_at IS NULL
	  AND ($2::timestamptz IS NULL OR created_at >= $2)
	  AND ($3::timestamptz IS NULL OR created_at <= $3)
	ORDER BY created_at DESC
	LIMIT $4 OFFSET $5`

// auditTotalQuery counts what auditPageQuery would page through, with the same
// window predicates written the same way. Total answers "how many entries match"
// rather than "how many are on this page", and a second round trip is the price
// of saying so without a window function whose value disappears exactly when the
// page is empty -- which is when a client most wants to know the count.
const auditTotalQuery = `SELECT count(*)
	FROM ` + ledgerTable + `
	WHERE tenant_id = $1
	  AND deleted_at IS NULL
	  AND ($2::timestamptz IS NULL OR created_at >= $2)
	  AND ($3::timestamptz IS NULL OR created_at <= $3)`

// insertAccessLog appends one compliance access record.
//
// It runs as the pool's own role and not as tenant.LedgerWriterRole: that role
// exists to make the *ledger* append-only in the database and holds no privilege
// on this table at all. The access log has no such guarantee yet -- nothing
// revokes UPDATE or DELETE on it -- and PROGRESS.md's Phase 19 section records
// that as a finding rather than leaving it as an implication.
const insertAccessLog = `INSERT INTO ` + complianceAccessLogTable + `
	(tenant_id, endpoint, query_params_redacted, ip_hash, response_code)
	VALUES ($1, $2, $3, $4, $5)`

// Auditor implements plane.ComplianceAuditor against a live database: the audit
// ledger's paged read half, and the access log that records each read of it.
//
// It is a value type holding one pool, like Ledger. It holds no logger and never
// a tenant's signing secret -- a page of rows carries memory previews, and this
// type has nowhere to write one -- which is why nothing here can leak a payload
// by accident.
type Auditor struct {
	pool *pgxpool.Pool
}

// auditorIsCompliant is the compile-time assertion that this package's
// implementation still satisfies the contract internal/plane declares. It is a
// variable rather than a comment because a signature that drifted would then fail
// the build here, in the package that owns the SQL, instead of at whichever call
// site someone next touched.
var auditorIsCompliant plane.ComplianceAuditor = Auditor{}

// NewAuditor returns an Auditor that reads and records through pool. The pool is
// not owned: cmd/plane closes it, exactly as the migrator, the tenant store, and
// the ledger's write path leave it.
func NewAuditor(pool *pgxpool.Pool) Auditor {
	return Auditor{pool: pool}
}

// AuditPage implements plane.ComplianceAuditor.
//
// Both queries run on the pool's own connection, never as tenant.LedgerWriterRole:
// the writer holds INSERT and no SELECT at all, which is what makes the ledger
// append-only in the database, and is why any reader of this table needs the
// owner's identity -- the same constraint the write path's chain-head read and
// Phase 17's walk live under.
//
// The count is read before the page, and neither is in a transaction with the
// other. That is deliberate: a row appended between the two statements makes the
// count stale by one, and holding a transaction open to prevent that would take
// the tenant's appends behind this read for no benefit. A caller sees a page and
// a count that were true at slightly different instants, which is the same
// guarantee every other paginated listing in this project offers.
//
// An empty page is a zero-length slice and not nil, so an implementation cannot
// make a client's iterating code special-case an empty history.
func (a Auditor) AuditPage(ctx context.Context, tenantID string, filter plane.AuditFilter) (plane.AuditPage, error) {
	if a.pool == nil {
		return plane.AuditPage{}, fmt.Errorf("ledger: audit page needs a database pool")
	}

	// Canonicalized exactly as the write path canonicalizes it, so the tenant id
	// this predicate matches on is the form the rows were signed over.
	id, err := canonicalID(tenantID)
	if err != nil {
		return plane.AuditPage{}, fmt.Errorf("ledger: tenant id: %w", err)
	}

	page := plane.AuditPage{Entries: make([]plane.AuditRow, 0)}

	if err := a.pool.QueryRow(ctx, auditTotalQuery, id, filter.Since, filter.Until).Scan(&page.Total); err != nil {
		return plane.AuditPage{}, fmt.Errorf("ledger: count audit entries: %w", err)
	}

	rows, err := a.pool.Query(ctx, auditPageQuery, id, filter.Since, filter.Until, filter.Limit, filter.Offset)
	if err != nil {
		return plane.AuditPage{}, fmt.Errorf("ledger: read audit page: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entry plane.AuditRow
		if err := rows.Scan(&entry.ID, &entry.TenantID, &entry.RequestID,
			&entry.TraceJSON, &entry.PrevHash, &entry.HashValue, &entry.CreatedAt); err != nil {
			return plane.AuditPage{}, fmt.Errorf("ledger: scan audit entry: %w", err)
		}

		page.Entries = append(page.Entries, entry)
	}

	if err := rows.Err(); err != nil {
		return plane.AuditPage{}, fmt.Errorf("ledger: read audit page: %w", err)
	}

	return page, nil
}

// RecordAccess implements plane.ComplianceAuditor: one INSERT, keyed by the
// verified tenant, into synapse_global.compliance_access_log.
//
// The tenant id is canonicalized here for the same reason it is in AuditPage --
// the column is a uuid and the value came from a token, not from this package --
// and a tenant id that is not a uuid is an error rather than a row nobody could
// find later.
//
// The row's own id and created_at are the database's defaults, so the record
// carries when the database wrote it rather than when this process thought it
// did. Nothing is returned: the endpoint's own response code is what the record
// says, and a caller that needed the row back would be a caller doing something
// this endpoint does not offer.
func (a Auditor) RecordAccess(ctx context.Context, record plane.AccessRecord) error {
	if a.pool == nil {
		return fmt.Errorf("ledger: compliance access log needs a database pool")
	}

	id, err := canonicalID(record.TenantID)
	if err != nil {
		return fmt.Errorf("ledger: tenant id: %w", err)
	}

	if _, err := a.pool.Exec(ctx, insertAccessLog,
		id, record.Endpoint, record.QueryParamsRedacted, record.IPHash, record.ResponseCode,
	); err != nil {
		return fmt.Errorf("ledger: record compliance access: %w", err)
	}

	return nil
}
