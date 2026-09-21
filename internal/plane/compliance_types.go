// The compliance audit endpoint's contract: the interface its implementation
// satisfies, the query and record types that cross that boundary, and the wire
// shapes a client sees.
//
// Split from compliance.go, which holds the endpoint itself, for the 300-line
// ceiling this project holds every file to -- and along a line that is real
// rather than convenient: everything here is a name two packages have to agree
// on, while compliance.go is the one place that decides what happens to a
// request.
package plane

import (
	"context"
	"time"

	"synapse/internal/trace"
)

// ComplianceAuditor is everything the two compliance endpoints need from
// synapse_global: one page of a tenant's signed ledger entries, one window's worth
// of raw report material, and the record that either was read.
//
// Like MemoryWriter, MemorySearcher, and LedgerVerifier it is declared on the
// consumer side, so this package depends on a behaviour rather than on a
// database handle and the endpoints are testable without PostgreSQL. The
// implementation lives in internal/ledger (the package that already owns the
// ledger table's name and read path) and is bound in cmd/plane, the one place
// that can see both packages.
//
// The three capabilities travel in one interface because they are one thing to a
// caller -- "read a tenant's audit history and record that it happened" -- and
// because both readers need the record before they will answer at all: the record
// is written first, so an implementation that cannot write one cannot serve a read
// either. The report's own arithmetic deliberately does *not* live here: it is a
// pure function of the facts (see compliance_report_build.go), which is what keeps
// it testable without a database.
type ComplianceAuditor interface {
	// AuditPage returns one page of tenantID's ledger entries, newest first,
	// restricted to the filter's inclusive time window, together with the total
	// number of entries that window holds -- which is the count the page is a
	// slice of, not the size of the page.
	//
	// tenantID is the verified token's own claim and never a request value, so
	// a caller can only ever page through its own chain. Soft-deleted rows are
	// excluded, the same way the chain walk excludes them.
	AuditPage(ctx context.Context, tenantID string, filter AuditFilter) (AuditPage, error)
	// ReportFacts returns the window's raw material: every ledger entry it
	// holds, newest first, plus the metering totals it holds. It is the whole
	// window rather than a page, because a report covers the period it names --
	// a page of it would be a report about a page.
	//
	// The rows carry trace_json as stored, unparsed: what the traces *mean* is
	// the HTTP layer's contract (see buildComplianceReport), and a store that
	// interpreted them would be a second place for the report's numbers to be
	// computed. An empty window is a zero-length slice and a UsageTotals with
	// HasRows false, never an error.
	ReportFacts(ctx context.Context, tenantID string, window ReportWindow) (ReportFacts, error)
	// RecordAccess writes one row of synapse_global.compliance_access_log: who
	// read what, with what window, from which hashed address, and what the
	// endpoint answered. It is called before the answer is written, and its
	// failure is what refuses the read.
	RecordAccess(ctx context.Context, record AccessRecord) error
}

// AuditFilter is one page request: an inclusive time window, either half of
// which may be open, a page size, and how far into the result to start.
//
// The two times are pointers so that "no since" and "since the zero time" are
// different requests -- the first pages a whole chain, the second pages the
// entries a zero timestamp does not follow -- and the zero time is a legitimate
// value a caller can ask for. The distinction matters because the filter is
// handed to a query that has to treat "no bound" as no predicate at all.
type AuditFilter struct {
	// Since, when set, is the inclusive lower bound on created_at.
	Since *time.Time
	// Until, when set, is the inclusive upper bound on created_at.
	Until *time.Time
	// Limit is the page size. The handler always sets
	// defaultCompliancePageLimit or a validated caller value, so an
	// implementation may pass it straight to LIMIT.
	Limit int
	// Offset is how many matching entries to skip, newest first.
	Offset int
}

// AuditRow is one ledger row as it is stored: every field is the database's own
// value, and nothing is recomputed or reformatted on the way out. The field
// names mirror internal/ledger's LedgerEntry, which is what lets that package's
// implementation copy a row across without a translation that could lose a
// byte -- and a byte lost here would be a byte the chain's signature covers.
//
// TraceJSON is the trace exactly as it was signed, still a string at this
// layer: parsing it is the HTTP layer's job (parseAuditTrace), because the shape
// a client sees is this endpoint's contract and not the store's.
type AuditRow struct {
	ID        string
	TenantID  string
	RequestID string
	TraceJSON string
	PrevHash  string
	HashValue string
	CreatedAt time.Time
}

// AuditPage is one page of a tenant's ledger plus the size of the window it was
// taken from. Total counts every entry the filter matches, not the entries in
// Entries, which is what lets a client render "showing 50 of 12,431" and decide
// whether to ask for the next page.
type AuditPage struct {
	// Entries is the page, newest first. Always non-nil, so an implementation
	// cannot make an empty page marshal as null.
	Entries []AuditRow
	// Total is how many entries the filter matched.
	Total int
}

// AccessRecord is one row of synapse_global.compliance_access_log: the record
// that a tenant's audit history was read, and by whom.
//
// Nothing here is memory content or a credential, and QueryParamsRedacted is
// built (by parseAuditParams) rather than copied: it can only ever carry the
// four parameters this endpoint accepts, re-rendered from their parsed values,
// so a caller cannot use the audit table as a place to park arbitrary text.
type AccessRecord struct {
	// TenantID is the verified token's tenant, and the column the row is keyed
	// by.
	TenantID string
	// Endpoint is the path that was called -- complianceAuditRoute or
	// complianceReportRoute.
	Endpoint string
	// QueryParamsRedacted is the caller's window and paging, re-rendered. See
	// parseAuditParams for what is and is not in it.
	QueryParamsRedacted string
	// IPHash is hex(sha256(r.RemoteAddr)): a stable pseudonym for the caller's
	// address, never the address itself. See ipHash.
	IPHash string
	// ResponseCode is what this endpoint answered with. It is written with the
	// record rather than after it, because a record whose status arrived later
	// would be a record of a call that might never have been answered.
	ResponseCode int
}

// auditEntryResponse is one entry of the 200 body, in wire order. The trace is a
// parsed manifest rather than the stored string -- a client reading an audit
// record should not have to decode a second time a value this plane already
// holds as JSON -- and prev_hash and hash_value travel with it so a reader can
// check the entry against the chain's own evidence.
type auditEntryResponse struct {
	ID        string              `json:"id"`
	TenantID  string              `json:"tenant_id"`
	RequestID string              `json:"request_id"`
	Trace     trace.TraceManifest `json:"trace"`
	PrevHash  string              `json:"prev_hash"`
	HashValue string              `json:"hash_value"`
	CreatedAt time.Time           `json:"created_at"`
}

// auditResponse is the 200 body: the page, the size of the window it came from,
// and the paging that produced it, echoed back so a client can advance without
// having to remember what it asked for.
type auditResponse struct {
	Data   []auditEntryResponse `json:"data"`
	Total  int                  `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

// complianceTierRequiredResponse is the 403 body, and it is the one error shape
// in this package that is not errorResponse: a refusal a caller is meant to act
// on carries the one thing it can act on -- where to buy the tier -- and putting
// that in the generic shape would add a field every other endpoint's client has
// to ignore.
type complianceTierRequiredResponse struct {
	Error      string `json:"error"`
	UpgradeURL string `json:"upgrade_url"`
}

// WithComplianceTier returns a copy of ctx carrying a verified compliance tier.
//
// Same arrangement as WithTenantID: internal/tenant's JWT middleware calls this
// with a claim it has already verified, because the claims it reads back through
// its own accessors are parked under unexported keys in that package and this
// one cannot read them.
//
// The value decides which endpoints a caller may reach, so trusting anything
// other than the signed claim would turn a self-declared string into an
// entitlement. Absent is not a value: the reader below returns "" for it, which
// is not complianceTierEnterprise, so a route wired without the middleware
// refuses.
func WithComplianceTier(ctx context.Context, tier string) context.Context {
	return context.WithValue(ctx, complianceTierKey, tier)
}

// ComplianceTierFromCtx returns the verified compliance tier attached by
// WithComplianceTier, or "" when the request never passed through the
// middleware.
func ComplianceTierFromCtx(ctx context.Context) string {
	tier, _ := ctx.Value(complianceTierKey).(string)

	return tier
}
