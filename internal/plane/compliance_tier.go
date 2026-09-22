// The compliance tier gate every compliance surface shares: GET
// /v2/compliance/audit, GET /v2/compliance/chain-integrity, and GET
// /v2/compliance/report.
//
// It is one file rather than a block repeated in three handlers because the gate
// is now a property of a surface rather than of a single endpoint, and the three
// things it decides -- which claim it reads, what a refusal says, and whether
// the refusal is recorded -- must be the same three things everywhere or the
// surfaces are not gated uniformly. The 403 body is written here and nowhere
// else, so the three cannot answer differently.
//
// It is a handler-level helper and not chi middleware, and that is deliberate
// rather than a missed opportunity. Middleware runs before the handler, and the
// audit and report refusals are recorded in synapse_global.compliance_access_log
// *before* they are answered (see requireAccessRecord): a middleware would have
// no verified window to record, because the handler that parses it has not run
// yet. Phases 19 and 20 both promise that a refusal is recorded and their tests
// assert it. A helper called from inside the handler also gates every name a
// route is registered under: GET /v2/ledger/verify and GET
// /v2/compliance/chain-integrity are one handler reached through two paths, so a
// gate attached to a route rather than to the handler would leave whichever
// spelling was registered without it wide open.
package plane

import "net/http"

// requireComplianceTier reports whether the verified caller holds the compliance
// tier every compliance surface requires and, when it does not, refuses the call
// and returns false.
//
// A true answer means "the caller passed the gate". It is not a statement that
// the request is otherwise valid: every caller goes on to validate its own
// parameters and dependencies after it, which is why the order at each call site
// is identity, then dependency, then this gate, then the caller's window, then
// the read.
//
// The tier is the verified token's own compliance_tier claim -- the value
// internal/tenant's middleware published after it checked the signature -- and
// never the registry row and never anything a request carries. There is no
// query, path, body, or header parameter that could name a tier, exactly as
// there is none that could name a tenant, so no caller can raise its own tier by
// asking for one.
//
// endpoint and redacted are the access record's halves: the surface being asked
// for, and the caller's own window and paging as parsed. A refusal is recorded
// when this plane has an auditor wired, before the 403 is written, so a denied
// read of a compliance surface appears in the tenant's own access log carrying
// the code it was answered with -- the same treatment every other answer gets.
// The dependency is not required, though: a plane wired with a chain verifier
// and no auditor still refuses an unentitled caller, and only the record is
// missing.
//
// When the record cannot be written the call is refused with this package's
// generic 500 instead, for the reason requireAccessRecord documents: an
// unrecorded read of a compliance surface is precisely the event the log exists
// to rule out, and a 500 is still a refusal. The coupling is the same one the
// audit and report endpoints have always had.
//
// The body is complianceTierRequiredResponse rather than errorResponse because a
// caller is meant to act on it, and the one thing it can act on is where to buy
// the tier. Absent is not a tier either: a route wired without the JWT
// middleware leaves ComplianceTierFromCtx returning "", which is not
// complianceTierEnterprise, so such a route refuses as well.
func (s *Server) requireComplianceTier(w http.ResponseWriter, r *http.Request, endpoint, tenantID, redacted string) bool {
	if ComplianceTierFromCtx(r.Context()) == complianceTierEnterprise {
		return true
	}

	if s.auditor != nil {
		if !s.requireAccessRecord(w, r, endpoint, tenantID, redacted, http.StatusForbidden) {
			return false
		}
	}

	writeJSON(w, http.StatusForbidden, complianceTierRequiredResponse{
		Error:      "compliance_tier_required",
		UpgradeURL: complianceUpgradeURL,
	})

	return false
}
