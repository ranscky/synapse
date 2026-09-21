// The compliance report endpoint's read path: one window's worth of raw report
// material, as stored.
//
// It lives in this package rather than in internal/plane for the reason the whole
// ledger does: the plane's HTTP package holds interfaces, not database handles,
// and this package already owns the ledger table's name, its window predicates
// (compliance.go), and the usage_events table's schema-qualified name via
// internal/tenant. What it deliberately does *not* do is interpret what it reads:
// the traces come back as stored and the report's arithmetic happens in
// internal/plane, where it is a pure function and therefore testable without a
// database (see compliance_report_build.go).
package ledger

import (
	"context"
	"fmt"

	"synapse/internal/plane"
	"synapse/internal/tenant"
)

// usageEventsTable is the metering table the report's two metering-facing numbers
// come from. The schema name comes from internal/tenant, which owns the migration,
// so the name is written down once -- the same arrangement ledgerTable and
// complianceAccessLogTable have.
const usageEventsTable = tenant.SchemaName + ".usage_events"

// reportWindowQuery reads one window of a tenant's chain, newest first.
//
// It is auditPageQuery without the paging: a report covers the period it names,
// so there is no LIMIT to hand it and no page size to justify, and the same
// (tenant_id, created_at) index answers both. The predicates are written the same
// way -- "$n IS NULL OR ..." casts included -- so "no window" is one code path in
// the database rather than a second statement shape that could drift from the
// paged one.
//
// deleted_at IS NULL is the walk's own filter, for the reason the paged read
// states: nothing in this project can soft-delete a ledger row, so a row hidden
// that way must not vanish from a report that claims to cover a period.
const reportWindowQuery = `SELECT id::text, tenant_id::text, request_id::text, trace_json, prev_hash, hash_value, created_at
	FROM ` + ledgerTable + `
	WHERE tenant_id = $1
	  AND deleted_at IS NULL
	  AND ($2::timestamptz IS NULL OR created_at >= $2)
	  AND ($3::timestamptz IS NULL OR created_at <= $3)
	ORDER BY created_at DESC`

// usageTotalsQuery reads one tenant's metering totals for the same window shape.
//
// COALESCE(..., 0)::float8 rather than a nullable scan: "no rows in this window"
// is the ordinary state today -- nothing writes usage_events until the metering
// phase lands -- and the report distinguishes it by the count, not by a sentinel
// mean. The cast is what makes the zero unambiguous to the server, the same reason
// the window predicates carry their own.
const usageTotalsQuery = `SELECT count(*), COALESCE(avg(reduction_pct), 0)::float8
	FROM ` + usageEventsTable + `
	WHERE tenant_id = $1
	  AND ($2::timestamptz IS NULL OR created_at >= $2)
	  AND ($3::timestamptz IS NULL OR created_at <= $3)`

// ReportFacts implements plane.ComplianceAuditor: every ledger row the window
// holds, plus the metering totals it holds.
//
// The rows are streamed into a slice rather than aggregated here, because the
// report's numbers are this package's caller's contract: a store that summed
// traces would be a second place for the arithmetic to live, and the one that
// cannot be tested in CI. What this method answers is exactly "what does the
// window contain".
//
// The tenant id is canonicalized exactly as the write path canonicalizes it, so
// the predicate matches the form the rows were signed over -- the same rule
// AuditPage and Verify follow. Rows is always a non-nil slice, so an empty window
// cannot make a caller iterate a nil.
//
// The two queries are not in a transaction with each other, deliberately: a row
// appended, or a metering row written, between them makes one total stale by one,
// which is the same instant-level guarantee every other listing in this project
// offers and not worth holding a transaction open for.
func (a Auditor) ReportFacts(ctx context.Context, tenantID string, window plane.ReportWindow) (plane.ReportFacts, error) {
	if a.pool == nil {
		return plane.ReportFacts{}, fmt.Errorf("ledger: compliance report needs a database pool")
	}

	id, err := canonicalID(tenantID)
	if err != nil {
		return plane.ReportFacts{}, fmt.Errorf("ledger: tenant id: %w", err)
	}

	facts := plane.ReportFacts{Rows: make([]plane.AuditRow, 0)}

	rows, err := a.pool.Query(ctx, reportWindowQuery, id, window.Since, window.Until)
	if err != nil {
		return plane.ReportFacts{}, fmt.Errorf("ledger: read report window: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entry plane.AuditRow
		if err := rows.Scan(&entry.ID, &entry.TenantID, &entry.RequestID,
			&entry.TraceJSON, &entry.PrevHash, &entry.HashValue, &entry.CreatedAt); err != nil {
			return plane.ReportFacts{}, fmt.Errorf("ledger: scan report entry: %w", err)
		}

		facts.Rows = append(facts.Rows, entry)
	}

	if err := rows.Err(); err != nil {
		return plane.ReportFacts{}, fmt.Errorf("ledger: read report window: %w", err)
	}

	var (
		compilations int
		avgReduction float64
	)

	if err := a.pool.QueryRow(ctx, usageTotalsQuery, id, window.Since, window.Until).
		Scan(&compilations, &avgReduction); err != nil {
		return plane.ReportFacts{}, fmt.Errorf("ledger: read usage totals: %w", err)
	}

	facts.Usage = plane.UsageTotals{
		Compilations:    compilations,
		AvgReductionPct: avgReduction,
		HasRows:         compilations > 0,
	}

	return facts, nil
}
