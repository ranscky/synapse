package plane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	charmlog "github.com/charmbracelet/log"
	"github.com/go-chi/chi/v5"
)

// Version is reported by GET /health and in the control plane's startup log
// line. Both read this one constant so they can never disagree.
const Version = "2.0.0"

const (
	// healthPingTimeout bounds the database probe behind GET /health, so a hung
	// database cannot turn the health endpoint into a hanging request.
	healthPingTimeout = 2 * time.Second
)

// Database is the health probe's view of the database. *pgxpool.Pool satisfies
// it, and a test double can too, which keeps the health path testable without a
// server.
type Database interface {
	Ping(ctx context.Context) error
}

// Server is the control plane's HTTP surface.
//
// Every external dependency arrives through the constructor -- a database probe,
// the tenant provisioner, the tenant memory writer and searcher, the audit
// ledger verifier, the token middleware, the config holding the secrets, and the
// logger -- so there is no global state and no route reaches for one.
//
// auth is a plain constructor-injected func rather than an import of
// internal/tenant for a concrete reason: internal/tenant already imports this
// package (it needs PlaneConfig to sign and verify tokens), so the reverse
// import would be a cycle. The middleware itself is chi-compatible and knows
// nothing about this package, which is why the dependency can point this way.
type Server struct {
	cfg      *PlaneConfig
	db       Database
	tenants  TenantProvisioner
	memories MemoryWriter
	searcher MemorySearcher
	ledger   LedgerVerifier
	auditor  ComplianceAuditor
	auth     func(http.Handler) http.Handler
	logger   *charmlog.Logger
}

// NewServer returns a Server serving the control plane routes. db, tenants,
// memories, searcher, ledger, auditor, and auth may be nil: the health endpoint
// then reports a disconnected database, the provisioning endpoint answers 500,
// and the sync, search, ledger, and compliance endpoints refuse every request
// instead of panicking or trusting an unverified tenant.
//
// memories and searcher are separate parameters rather than one wider interface
// because they are two different capabilities: a plane can be wired to accept
// pushes without serving reads, and each endpoint then degrades on its own.
// cmd/plane passes the same tenant-backed implementation for both.
//
// ledger is a third capability again -- walking an audit chain with a tenant's
// signing key -- and it is a separate parameter for the same reason: a plane
// wired to serve memories has not thereby agreed to expose audit verification,
// and the ledger's read path needs a database identity the tenant data layer
// does not (the writer role holds no SELECT on that table).
//
// auditor is the fourth, and it is the audit ledger's read half: one page of a
// tenant's chain, plus the access-log row that records the read. It is separate
// from ledger rather than folded into it because the two answer different
// questions with different privileges -- verification recomputes signatures under
// the tenant's own secret, while a page read hands back the rows as stored -- and
// because an operator may reasonably want one wired without the other. What it
// shares with ledger is that a plane started without it refuses the route
// outright rather than answering from a dependency it does not have.
//
// logger may be nil, in which case this package logs nothing.
func NewServer(
	cfg *PlaneConfig,
	db Database,
	tenants TenantProvisioner,
	memories MemoryWriter,
	searcher MemorySearcher,
	ledger LedgerVerifier,
	auditor ComplianceAuditor,
	auth func(http.Handler) http.Handler,
	logger *charmlog.Logger,
) *Server {
	return &Server{
		cfg:      cfg,
		db:       db,
		tenants:  tenants,
		memories: memories,
		searcher: searcher,
		ledger:   ledger,
		auditor:  auditor,
		auth:     auth,
		logger:   logger,
	}
}

// Routes returns the plane's router: GET /health is open, POST /v2/tenants is
// behind the admin token, and POST /v2/sync/memories, GET /v2/memories/search,
// GET /v2/ledger/verify, GET /v2/compliance/audit, and GET
// /v2/compliance/report are behind a tenant token.
//
// The ledger route is tenant-scoped rather than admin-guarded on purpose: the
// chain it verifies is the caller's own (the tenant id comes from the verified
// token), so verification is the tenant's own audit of its own records rather
// than an operator reading someone else's. The two compliance routes are
// tenant-scoped for the same reason and add one gate of their own -- the token's
// compliance tier -- which the handlers apply, because it is a property of the
// caller rather than of the route.
func (s *Server) Routes() http.Handler {
	router := chi.NewRouter()
	router.Get("/health", s.handleHealth)
	router.With(s.requireAdmin).Post("/v2/tenants", s.handleCreateTenant)
	router.With(s.requireJWT).Post(syncRoute, s.handleSyncMemories)
	router.With(s.requireJWT).Get(searchRoute, s.handleSearchMemories)
	router.With(s.requireJWT).Get(ledgerVerifyRoute, s.handleVerifyLedger)
	router.With(s.requireJWT).Get(complianceAuditRoute, s.handleComplianceAudit)
	router.With(s.requireJWT).Get(complianceReportRoute, s.handleComplianceReport)

	return router
}

// healthResponse is the GET /health body, in wire order.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DB      string `json:"db"`
}

// errorResponse is the only error body shape this package writes: a machine
// readable reason and nothing else. Database errors, DSNs, tokens, and keys are
// never reflected to a client.
type errorResponse struct {
	Error string `json:"error"`
}

// handleHealth serves GET /health. It probes the database on every request and
// reports "connected" only when the probe succeeds, so the endpoint means
// something to an orchestrator: 200 plus db=connected, or 503 plus
// db=disconnected.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthPingTimeout)
	defer cancel()

	body := healthResponse{Status: "ok", Version: Version, DB: "connected"}
	code := http.StatusOK

	if s.db == nil || s.db.Ping(ctx) != nil {
		body.Status = "degraded"
		body.DB = "disconnected"
		code = http.StatusServiceUnavailable
	}

	writeJSON(w, code, body)
}

// decodeJSON reads a bounded, strict JSON body into dst. It writes a 400 and
// returns false on any malformed input -- unknown fields and trailing values
// included, so a client cannot smuggle extra keys past validation.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSONLimit(w, r, dst, maxTenantBodyBytes)
}

// decodeJSONLimit is decodeJSON with the body ceiling made explicit, because
// the two JSON endpoints this package serves do not carry comparable payloads:
// a provisioning body is two short strings, while one sync batch carries whole
// memories -- embeddings included. One shared limit would either reject a
// legitimate batch or let a provisioning request allocate megabytes.
func decodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	body := http.MaxBytesReader(w, r.Body, maxBytes)

	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return false
	}

	// A second value in the stream means the body was not one JSON object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return false
	}

	return true
}

// writeError writes the single error body shape this package uses.
func writeError(w http.ResponseWriter, code int, reason string) {
	writeJSON(w, code, errorResponse{Error: reason})
}

// writeJSON marshals body and writes it with the given status code. A marshal
// failure cannot happen for these fixed shapes, but it is still handled rather
// than ignored, so a client always gets valid JSON.
func writeJSON(w http.ResponseWriter, code int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	// A failed write means the client went away; there is nothing useful to do
	// about it on either the success or the rejection path.
	_, _ = w.Write(payload)
}
