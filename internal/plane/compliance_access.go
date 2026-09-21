// The compliance access log both compliance endpoints write before they answer.
//
// Split out of compliance.go for the 300-line ceiling this project holds every
// file to, and along a line that is real: the paged audit read and the period
// report are two different questions with two different response shapes, and the
// one thing they share is this -- who read what, and what they were answered --
// which is exactly the thing that must not be duplicated per endpoint.
package plane

import (
	"context"
	"net/http"
)

// requireAccessRecord writes this call's compliance_access_log row and reports
// whether the caller's answer may be sent. When the record cannot be written it
// logs the failure, writes this package's generic 500, and returns false, so the
// caller's own answer -- audit data included -- is never sent.
//
// endpoint is the route being answered (complianceAuditRoute or
// complianceReportRoute), passed in rather than assumed, because the access table
// is the one place that says which of a tenant's compliance surfaces was read, and
// a row that named the wrong one would be worse than no row.
//
// Fail-closed rather than best-effort is the deliberate choice, and the reason is
// what these endpoints are for. An audit read that goes unrecorded is precisely
// the event a compliance officer has to be able to rule out, and a plane that
// answered anyway would be handing over a tenant's audit history with no evidence
// that anyone asked -- the failure mode the ledger itself exists to prevent. The
// cost is stated rather than hidden: the access table and the ledger are in one
// database, so a long run of reads that worked means the next one is
// overwhelmingly likely to write its record too, and the case where it does not
// is a database that has stopped answering -- at which point the read being
// recorded would have failed as well.
//
// The alternative, answering and logging the failure server-side, was rejected:
// it makes "log every call" a best effort, which is a property no auditor can
// rely on.
func (s *Server) requireAccessRecord(w http.ResponseWriter, r *http.Request, endpoint, tenantID, redacted string, code int) bool {
	if err := s.recordAccess(r, endpoint, tenantID, redacted, code); err != nil {
		if s.logger != nil {
			s.logger.Error("Compliance access log write failed",
				"endpoint", endpoint, "tenant_id", tenantID, "response_code", code, "error", err)
		}

		writeError(w, http.StatusInternalServerError, "internal")
		return false
	}

	return true
}

// recordAccess writes one row of the tenant's compliance access log: the verified
// tenant, the endpoint that was called, the caller's window and paging as they
// were parsed, the caller's address as a digest, and the code about to be written.
//
// The request's own context is detached before the write. The read this records is
// already over by the time the row is written, and a client that hung up in the
// meantime must not take the record of its own read with it -- which is exactly the
// case an auditor would ask about, since a caller who disconnects mid-response is
// still a caller who asked. WithoutCancel keeps the request's values and drops its
// cancellation; the ceiling below replaces it.
func (s *Server) recordAccess(r *http.Request, endpoint, tenantID, redacted string, code int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), complianceAccessLogTimeout)
	defer cancel()

	return s.auditor.RecordAccess(ctx, AccessRecord{
		TenantID:            tenantID,
		Endpoint:            endpoint,
		QueryParamsRedacted: redacted,
		IPHash:              ipHash(r.RemoteAddr),
		ResponseCode:        code,
	})
}
