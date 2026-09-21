// The compliance audit query endpoint: GET /v2/compliance/audit.
//
// Split out of handlers.go for the same reason the sync, search, ledger, and
// tenant-provisioning endpoints live in their own files: handlers.go holds the
// server, the routes, and the shared HTTP helpers, and every file in this
// project is held to the 300-line ceiling. The query-parameter half of this
// endpoint is one file over, in compliance_params.go, and the names it shares
// with its implementation are in compliance_types.go.
//
// This is the read half of the audit ledger Phase 16 started writing. Phase 17's
// GET /v2/ledger/verify answers "is this chain intact?"; this answers "what does
// it say?" -- one page of a tenant's own signed entries, newest first, bounded
// by a window the caller chooses. The two are complementary and deliberately
// separate: a verification walk reads the whole chain to reach a verdict, while
// this reads a page of it, because the person asking is a compliance officer
// with a date range rather than a verifier with a yes/no question.
//
// Three properties are what this file is built around, and each one is the
// reason for a piece of the code below.
//
//   - The tenant is the verified token's and nothing else. There is no path,
//     query, body, or header parameter that names a tenant -- exactly as
//     GET /v2/ledger/verify has none -- so this route can never be made to read
//     another tenant's rows.
//   - The tier gate reads the verified compliance_tier claim rather than the
//     registry row. The claim is what the plane signed, and reading
//     synapse_global.tenants instead would give this endpoint a second source of
//     truth that could disagree with the token the caller is actually holding.
//     The cost is staleness -- a tenant whose tier was lowered keeps reading
//     until its token is replaced -- and that is stated as a limitation in
//     PROGRESS.md's Phase 19 section rather than left implicit.
//   - The access record is written before the answer is. A read of a tenant's
//     audit history that could not be recorded is refused, so every byte of
//     audit data that leaves this plane is preceded by a row saying who asked
//     for it. See requireAccessRecord.
package plane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"synapse/internal/trace"
)

// complianceAuditRoute is the compliance audit endpoint's path, referenced by
// both the route registration and the handler's own documentation. It is also
// the endpoint name every access record carries, so the two cannot drift.
const complianceAuditRoute = "/v2/compliance/audit"

const (
	// complianceTierEnterprise is the only compliance tier that may read a
	// tenant's audit history through this endpoint. It is compared against the
	// verified claim the JWT middleware published, never against anything a
	// request carries.
	//
	// compiler.EnterprisePlan is the same literal on the write side of the
	// ledger, and the two are separate constants on purpose: one gates a
	// billing plan and this one gates a compliance tier, and a deployment can
	// have either without the other. A tenant whose plan is enterprise and
	// whose tier is team writes ledger rows it may not read back here, which is
	// a configuration a later phase may want to change -- collapsing the two
	// strings into one constant today would hide that they are two questions.
	complianceTierEnterprise = "enterprise"

	// complianceUpgradeURL is the link a refusal carries. It is a constant
	// rather than configuration because the response's shape is part of this
	// endpoint's published contract and a client keys off the field, not the
	// value; a deployment that needs its own URL changes it here, in one place.
	complianceUpgradeURL = "https://synapse.ai/enterprise"

	// defaultCompliancePageLimit is the page size a request that names no limit
	// gets. It is small enough that one unqualified request cannot pull a
	// tenant's whole history into memory.
	defaultCompliancePageLimit = 50

	// maxCompliancePageLimit caps limit. An audit page carries whole trace
	// manifests, so a caller asking for an unbounded page is asking this plane
	// to serialize an entire chain in one response -- the same shape of request
	// maxSearchTopK refuses on the search endpoint.
	maxCompliancePageLimit = 200

	// complianceAccessLogTimeout bounds the access-record write. It is the
	// write's own ceiling rather than the request's: the row is written after
	// the read it records is over, and a database that has stopped answering
	// must cost this endpoint a bounded wait rather than a hung request.
	complianceAccessLogTimeout = 5 * time.Second
)

// handleComplianceAudit serves GET /v2/compliance/audit: one page of the calling
// tenant's signed ledger, newest first.
//
// The order of the checks below is the endpoint's authorization story and it is
// deliberate: identity, then dependency, then the tier gate, then the caller's
// parameters, then the read. A caller that is not enterprise is refused before
// its window is validated, so this route cannot be used to probe what a valid
// query looks like, and a plane started without an auditor refuses outright
// rather than answering from a dependency it does not have.
//
// Every answer but the 401 is preceded by its own access record. See
// requireAccessRecord for why a record that cannot be written refuses the read
// rather than failing open, and recordAccess for what the record holds.
func (s *Server) handleComplianceAudit(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantIDFromCtx(r.Context())
	if tenantID == "" {
		// Unreachable behind requireJWT, which rejects a token that names no
		// tenant. Checked anyway so a rewired middleware cannot turn this into
		// a read of an unnamed tenant's history -- and this is the one refusal
		// that gets no access record, because there is no verified tenant for
		// the row to name.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if s.auditor == nil {
		// A plane wired without an auditor can neither read a page nor record
		// one. Refusing here is the fail-closed reading of "no dependency":
		// answering 200 from nothing is the only alternative, and it is the
		// wrong answer for an audit endpoint.
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	params, reason := parseAuditParams(r)

	if ComplianceTierFromCtx(r.Context()) != complianceTierEnterprise {
		if !s.requireAccessRecord(w, r, complianceAuditRoute, tenantID, params.redacted, http.StatusForbidden) {
			return
		}

		writeJSON(w, http.StatusForbidden, complianceTierRequiredResponse{
			Error:      "compliance_tier_required",
			UpgradeURL: complianceUpgradeURL,
		})
		return
	}

	if reason != "" {
		if !s.requireAccessRecord(w, r, complianceAuditRoute, tenantID, params.redacted, http.StatusBadRequest) {
			return
		}

		writeError(w, http.StatusBadRequest, reason)
		return
	}

	page, err := s.auditor.AuditPage(r.Context(), tenantID, params.filter)
	if err != nil {
		if s.logger != nil {
			// Identifiers and the error only. A pgx error can quote the
			// connection target, so it is reported server-side and never
			// reflected to a client.
			s.logger.Error("Compliance audit read failed",
				"tenant_id", tenantID, "error", err)
		}

		// The answer is a 500 either way, so a failed record changes nothing
		// the caller sees: requireAccessRecord has already written the 500 when
		// it could not record, and this writes the same body when it could.
		if s.requireAccessRecord(w, r, complianceAuditRoute, tenantID, params.redacted, http.StatusInternalServerError) {
			writeError(w, http.StatusInternalServerError, "internal")
		}
		return
	}

	// Built up front and always non-nil: an empty page has to marshal as [] and
	// never as null, which is the same rule the search endpoint's memory list
	// follows, and a client that iterates should not have to special-case an
	// empty history.
	data := make([]auditEntryResponse, 0, len(page.Entries))

	for _, entry := range page.Entries {
		manifest, err := parseAuditTrace(entry)
		if err != nil {
			if s.logger != nil {
				// The row's id, never its payload: a trace carries memory
				// previews, and this is a log line.
				s.logger.Error("Compliance audit entry is unreadable",
					"tenant_id", tenantID, "entry_id", entry.ID, "error", err)
			}

			if s.requireAccessRecord(w, r, complianceAuditRoute, tenantID, params.redacted, http.StatusInternalServerError) {
				writeError(w, http.StatusInternalServerError, "internal")
			}
			return
		}

		data = append(data, auditEntryResponse{
			ID:        entry.ID,
			TenantID:  entry.TenantID,
			RequestID: entry.RequestID,
			Trace:     manifest,
			PrevHash:  entry.PrevHash,
			HashValue: entry.HashValue,
			CreatedAt: entry.CreatedAt,
		})
	}

	if !s.requireAccessRecord(w, r, complianceAuditRoute, tenantID, params.redacted, http.StatusOK) {
		return
	}

	// Counts, an identifier, and the paging: no trace field, no window value,
	// no memory preview, and never the presented token. The window lives in the
	// access table, where it belongs.
	if s.logger != nil {
		s.logger.Info("Compliance audit read",
			"tenant_slug", TenantSlugFromCtx(r.Context()), "tenant_id", tenantID,
			"entries", len(data), "total", page.Total,
			"limit", params.filter.Limit, "offset", params.filter.Offset)
	}

	writeJSON(w, http.StatusOK, auditResponse{
		Data:   data,
		Total:  page.Total,
		Limit:  params.filter.Limit,
		Offset: params.filter.Offset,
	})
}

// parseAuditTrace turns one stored row's trace_json back into the manifest it
// was signed as, so the response carries a trace object rather than a JSON
// string inside JSON.
//
// A row that will not parse is an error and not an empty manifest, because the
// two are different statements: one says the ledger holds something this plane
// cannot read (corruption, or a schema this build predates), and the other says
// the entry had no trace. Reporting the second when the first is true would be
// the audit equivalent of a silent pass -- the failure mode Phase 17's walk
// exists to refuse.
func parseAuditTrace(entry AuditRow) (trace.TraceManifest, error) {
	var manifest trace.TraceManifest
	if err := json.Unmarshal([]byte(entry.TraceJSON), &manifest); err != nil {
		return trace.TraceManifest{}, fmt.Errorf("compliance: parse stored trace: %w", err)
	}

	return manifest, nil
}

// requireAccessRecord and recordAccess -- the access record both compliance
// endpoints write before they answer -- live in compliance_access.go. They are
// shared by GET /v2/compliance/audit and GET /v2/compliance/report, and this file
// is already at the 300-line ceiling this project holds every file to, which is
// the second reason they are their own file rather than a section here.
