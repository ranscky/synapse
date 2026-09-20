package plane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	charmlog "github.com/charmbracelet/log"
	"github.com/go-chi/chi/v5"
)

// Version is reported by GET /health and in the control plane's startup log
// line. Both read this one constant so they can never disagree.
const Version = "2.0.0"

const (
	// maxTenantBodyBytes bounds a provisioning request body. The documented body
	// is two short strings; anything larger is a client bug or an attempt to make
	// the plane allocate.
	maxTenantBodyBytes = 4 << 10

	// healthPingTimeout bounds the database probe behind GET /health, so a hung
	// database cannot turn the health endpoint into a hanging request.
	healthPingTimeout = 2 * time.Second

	// defaultPlan is the plan a tenant gets when the request omits one. It
	// matches the DDL default on synapse_global.tenants.plan.
	defaultPlan = "oss"

	// defaultComplianceTier is the tier assigned by this phase. The registry
	// column and the token claim are both written from it.
	defaultComplianceTier = "team"
)

// slugPattern is the accepted tenant slug: lower-case alphanumerics and hyphens,
// 3 to 32 characters. Nothing else reaches the database, so a slug can always be
// used as a schema-name component later.
var slugPattern = regexp.MustCompile(`^[a-z0-9-]{3,32}$`)

// planPattern keeps a plan identifier to the same conservative shape as a slug,
// so it can be used in log lines and comparisons without quoting surprises.
var planPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Database is the health probe's view of the database. *pgxpool.Pool satisfies
// it, and a test double can too, which keeps the health path testable without a
// server.
type Database interface {
	Ping(ctx context.Context) error
}

// Server is the control plane's HTTP surface.
//
// Every external dependency arrives through the constructor -- a database probe,
// the tenant provisioner, the tenant memory writer, the token middleware, the
// config holding the secrets, and the logger -- so there is no global state and
// no route reaches for one.
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
	auth     func(http.Handler) http.Handler
	logger   *charmlog.Logger
}

// NewServer returns a Server serving the control plane routes. db, tenants,
// memories, searcher, and auth may be nil: the health endpoint then reports a
// disconnected database, the provisioning endpoint answers 500, and the sync
// and search endpoints refuse every request instead of panicking or trusting an
// unverified tenant.
//
// memories and searcher are separate parameters rather than one wider interface
// because they are two different capabilities: a plane can be wired to accept
// pushes without serving reads, and each endpoint then degrades on its own.
// cmd/plane passes the same tenant-backed implementation for both.
//
// logger may be nil, in which case this package logs nothing.
func NewServer(
	cfg *PlaneConfig,
	db Database,
	tenants TenantProvisioner,
	memories MemoryWriter,
	searcher MemorySearcher,
	auth func(http.Handler) http.Handler,
	logger *charmlog.Logger,
) *Server {
	return &Server{cfg: cfg, db: db, tenants: tenants, memories: memories, searcher: searcher, auth: auth, logger: logger}
}

// Routes returns the plane's router: GET /health is open, POST /v2/tenants is
// behind the admin token, and POST /v2/sync/memories plus
// GET /v2/memories/search are behind a tenant token.
func (s *Server) Routes() http.Handler {
	router := chi.NewRouter()
	router.Get("/health", s.handleHealth)
	router.With(s.requireAdmin).Post("/v2/tenants", s.handleCreateTenant)
	router.With(s.requireJWT).Post(syncRoute, s.handleSyncMemories)
	router.With(s.requireJWT).Get(searchRoute, s.handleSearchMemories)

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

// createTenantRequest is the POST /v2/tenants body.
type createTenantRequest struct {
	Slug string `json:"slug"`
	Plan string `json:"plan"`
}

// createTenantResponse is the 201 body: the tenant id, a signed JWT, and the API
// key, which is the one and only time the plaintext key is returned.
type createTenantResponse struct {
	TenantID string `json:"tenant_id"`
	JWT      string `json:"jwt"`
	APIKey   string `json:"api_key"`
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

// handleCreateTenant serves POST /v2/tenants: validate, provision, answer.
//
// The handler never logs the api_key, the jwt, or the admin token, and never
// returns a database error to the client -- a pgx error can quote the connection
// target, so it is reported server-side and the client sees {"error":"internal"}.
func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	// The slug is validated exactly as sent: it becomes a schema-name component
	// later, so a padded or mixed-case value is a client error, not something to
	// quietly normalize.
	if !slugPattern.MatchString(req.Slug) {
		writeError(w, http.StatusBadRequest, "invalid_slug")
		return
	}

	plan := req.Plan
	if plan == "" {
		plan = defaultPlan
	}
	if !planPattern.MatchString(plan) {
		writeError(w, http.StatusBadRequest, "invalid_plan")
		return
	}

	if s.tenants == nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	result, err := s.tenants.Provision(r.Context(), ProvisionRequest{
		Slug:           req.Slug,
		Plan:           plan,
		ComplianceTier: defaultComplianceTier,
	})
	switch {
	case errors.Is(err, ErrTenantExists):
		writeError(w, http.StatusConflict, "tenant_exists")
		return
	case err != nil:
		if s.logger != nil {
			s.logger.Error("Tenant provisioning failed", "slug", req.Slug, "plan", plan, "error", err)
		}
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	// The slug and plan are registry metadata, not credentials; the returned
	// api_key and jwt are deliberately absent from this line.
	if s.logger != nil {
		s.logger.Info("Tenant provisioned", "tenant_id", result.TenantID, "slug", req.Slug, "plan", plan)
	}

	writeJSON(w, http.StatusCreated, createTenantResponse{
		TenantID: result.TenantID,
		JWT:      result.JWT,
		APIKey:   result.APIKey,
	})
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
