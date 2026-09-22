// The audit-ledger chain verification endpoint: GET
// /v2/compliance/chain-integrity, with GET /v2/ledger/verify kept as its
// deprecated Phase 17 alias.
//
// Split out of handlers.go for the same reason the sync, search, and tenant
// provisioning endpoints live in their own files: handlers.go holds the server,
// the routes, and the shared HTTP helpers, and every file in this project is
// held to the 300-line ceiling. This endpoint is the smallest of the four -- one
// dependency, no request body, one result type.
//
// Phase 21 renamed the surface to match Phase 19's audit page and Phase 20's
// report -- all three answer questions about the same ledger, and a path under
// /v2/compliance/ is what says so -- and put the enterprise-tier gate the other
// two already had in front of it. The Phase 17 path is still routed, because it
// shipped to deployments and a client written against it must not break; both
// paths reach this one handler, so neither can answer differently, and the gate
// lives in the handler rather than on a route so neither can be gated
// differently either.
//
// That result type is declared *here*, in the HTTP package, rather than in
// internal/ledger, and the import graph is what decides that: internal/ledger
// imports internal/tenant (the schema name, the writer role, and the tenant
// secret store), and internal/tenant imports this package (the verified-claim
// accessors JWTMiddleware publishes), so plane -> ledger would be a cycle. The
// same arrangement already holds one endpoint over: plane.ProvisionResult is
// declared here and returned by internal/tenant's Provisioner. internal/ledger
// aliases this type -- `type ChainIntegrityResult = plane.ChainIntegrityResult`
// -- so the domain name and the wire type cannot drift into two shapes.
package plane

import (
	"context"
	"net/http"
	"time"
)

// complianceChainIntegrityRoute is the chain verification endpoint's canonical
// path, referenced by both the route registration and the handler's own
// documentation. It is also the endpoint name an access record for a refused
// call carries, so the route and the audit row cannot drift apart.
const complianceChainIntegrityRoute = "/v2/compliance/chain-integrity"

// ledgerVerifyRoute is the same endpoint's Phase 17 path, kept as a deprecated
// alias: a client written against it keeps working, and it is registered to the
// same handler as the canonical path so the two cannot disagree about what they
// answer or about which tier they require.
const ledgerVerifyRoute = "/v2/ledger/verify"

// ChainIntegrityResult is the answer to "is this tenant's audit ledger intact?":
// the body of GET /v2/compliance/chain-integrity on both a clean chain and a
// broken one.
//
// Both timestamps are emitted as RFC 3339 UTC. first_break_id is empty when the
// chain is valid, and first_break_at is then the zero time -- deliberately sent
// rather than omitted, because "there was no break" and "this field was not part
// of the answer" are different statements and only the first one is true. The
// zero time is the encoding of a field that has no value, not a timestamp a
// caller should parse as one; chain_valid is what says which case it is.
type ChainIntegrityResult struct {
	// EntriesChecked is how many entries the walk examined, the entry that
	// broke the chain included: a break at the fifth row of a ten-row chain
	// reports 5, not 4. The walk stops at the first break because nothing past
	// it is meaningful -- once one row is in doubt, every later link is.
	EntriesChecked int `json:"entries_checked"`
	// ChainValid is true only when every entry's signature recomputed to the
	// stored hash_value under the tenant's own secret and every entry chained
	// from the row before it (the tenant's first entry, from genesis). An empty
	// chain is valid: a tenant with nothing appended has nothing that could
	// have been tampered with.
	ChainValid bool `json:"chain_valid"`
	// FirstBreakID is the id of the earliest entry that failed, and is empty
	// when the chain is valid. It is a ledger row id of the caller's own
	// tenant, so it names nothing the caller could not already read.
	FirstBreakID string `json:"first_break_id,omitempty"`
	// FirstBreakAt is that entry's own created_at -- the database's timestamp
	// for the row as stored, not a time computed here -- and is the zero time
	// when the chain is valid.
	FirstBreakAt time.Time `json:"first_break_at"`
	// CheckedAt is when this walk ran, in UTC.
	CheckedAt time.Time `json:"checked_at"`
}

// LedgerVerifier is everything GET /v2/compliance/chain-integrity needs from the
// ledger's read path: verify one tenant's hash chain and report what the walk
// found.
//
// Like MemoryWriter and MemorySearcher it is declared on the consumer side,
// because internal/ledger cannot be imported here (see the file header), so this
// package depends on a behaviour rather than on a database handle and the
// endpoint is testable without PostgreSQL. The implementation lives in
// internal/ledger and is bound in cmd/plane, the one place that can see both
// packages.
type LedgerVerifier interface {
	// VerifyChain walks tenantID's ledger entries in created_at order -- which
	// the write path guarantees is chain order -- recomputes each signature
	// under that tenant's own secret, checks each link, and reports the first
	// break. A chain that verifies is not an error, and neither is one that
	// does not: only a check that could not run is.
	//
	// tenantID is the verified token's own claim and never a request value, so
	// a caller can only ever verify its own chain; the implementation fetches
	// the tenant's secret itself and holds it for the length of the call.
	VerifyChain(ctx context.Context, tenantID string) (ChainIntegrityResult, error)
}

// handleVerifyLedger serves GET /v2/compliance/chain-integrity -- and, through
// the deprecated alias, GET /v2/ledger/verify: verify the caller's chain, answer
// with what the walk found.
//
// The chain is chosen by the verified token and nothing else: there is no path,
// query, body, or header parameter naming a tenant, which is what keeps this
// route from becoming a way to read another tenant's row ids and timestamps. A
// request without a verified tenant id is refused rather than answered for an
// empty one -- the middleware rejects a token that names no tenant, so an empty
// id here means the route was wired without it, and a fail-closed 401 is the
// only safe reading of that.
//
// Phase 21 put the compliance tier gate the audit and report surfaces already
// had in front of this one, so a token that is not enterprise is refused before
// the chain is walked. The order is the one those endpoints document -- identity,
// then dependency, then the gate, then the read -- which is why the gate sits
// after the nil-verifier check: "this plane cannot answer" comes before "this
// caller may not ask". The refusal carries the same body as its siblings' and,
// when this plane has an auditor wired, the same access-log record; the window
// half of that record is empty because this route has no query parameters to
// record. There is no read here to withhold from a refused caller -- the walk
// itself is the thing being gated -- so the gate's only job is to keep the
// verdict and the chain's row ids out of an unentitled caller's hands.
//
// A broken chain is a 200, because the check itself succeeded. An error status
// would collapse "your ledger no longer verifies" into "the check could not
// run", which is precisely the distinction an audit endpoint exists to draw; the
// body's chain_valid carries the finding. A check that could not run at all --
// no verifier wired, database unreachable, no stored secret for the tenant -- is
// a 500 with the one error body this package writes, and the underlying error is
// logged server-side and never reflected, because a pgx error can quote the
// connection target.
func (s *Server) handleVerifyLedger(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantIDFromCtx(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if s.ledger == nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	// The gate sits here, after the two checks above and before the walk: a
	// caller this plane cannot identify or cannot answer at all is told so
	// first, and a caller that may not ask never reaches the walk. The
	// canonical route name is recorded rather than the path that was
	// requested, because the record names the compliance surface that was
	// asked for and not the spelling the caller happened to use -- and because
	// a route constant is not caller input.
	if !s.requireComplianceTier(w, r, complianceChainIntegrityRoute, tenantID, "") {
		return
	}

	result, err := s.ledger.VerifyChain(r.Context(), tenantID)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("Ledger chain verification failed", "error", err)
		}

		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	// Counts and a boolean: no trace, no hash, no secret, no token. The slug
	// names the caller's own tenant, which is registry metadata rather than a
	// credential.
	if s.logger != nil {
		s.logger.Info("Ledger chain verified",
			"tenant_slug", TenantSlugFromCtx(r.Context()),
			"entries_checked", result.EntriesChecked, "chain_valid", result.ChainValid)
	}

	writeJSON(w, http.StatusOK, result)
}
