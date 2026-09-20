// The tenant provisioning endpoint: POST /v2/tenants.
//
// Split out of handlers.go for the same reason the sync and search endpoints live
// in their own files: handlers.go holds the server, the routes, and the shared
// HTTP helpers, and every file is held to the 300-line ceiling this project
// applies. All three endpoints have the same shape -- validate, hand the work to
// an injected dependency, answer without leaking anything -- and each one's
// request and response types live beside it.
//
// Phase 10 added agent_id and team_id to the request, which is what lets an
// operator mint a token whose reader can be held to a memory scope.
package plane

import (
	"errors"
	"net/http"
	"regexp"
)

// This endpoint's own bounds and defaults, as opposed to the server's.
const (
	// maxTenantBodyBytes bounds a provisioning request body. The documented body
	// is four short strings; anything larger is a client bug or an attempt to
	// make the plane allocate.
	maxTenantBodyBytes = 4 << 10

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

// identityPattern is the accepted shape of an agent id or team id: 1 to 64
// characters of letters, digits, and the punctuation operator-chosen names
// actually use ("edge-01", "team.blue", "build_agent").
//
// It is deliberately not slugPattern. These are names a human types into a
// config file, not schema components, so mixed case and dots are legitimate --
// but nothing here may carry whitespace, a quote, or a control character into a
// JWT claim or a log line, and that is what the character class rules out.
var identityPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// createTenantRequest is the POST /v2/tenants body.
//
// agent_id and team_id are optional and, when present, are minted into the
// returned token as its agent_id and team_id claims -- that is what makes an
// agent-scoped token reachable end to end, and what lets the search endpoint
// enforce memory visibility from a verified identity rather than from a request
// body. Omitting them mints a tenant-level token, which is what every token
// issued before this field existed was.
type createTenantRequest struct {
	Slug    string `json:"slug"`
	Plan    string `json:"plan"`
	AgentID string `json:"agent_id"`
	TeamID  string `json:"team_id"`
}

// createTenantResponse is the 201 body: the tenant id, a signed JWT, and the API
// key, which is the one and only time the plaintext key is returned.
type createTenantResponse struct {
	TenantID string `json:"tenant_id"`
	JWT      string `json:"jwt"`
	APIKey   string `json:"api_key"`
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

	// Both are optional. When supplied they are minted into the returned token
	// and become the identity a memory's visibility is checked against, so they
	// are validated here rather than taken on trust: a value carrying whitespace
	// or quoting would end up inside a JWT claim and inside every log line that
	// names the caller.
	if req.AgentID != "" && !identityPattern.MatchString(req.AgentID) {
		writeError(w, http.StatusBadRequest, "invalid_agent")
		return
	}
	if req.TeamID != "" && !identityPattern.MatchString(req.TeamID) {
		writeError(w, http.StatusBadRequest, "invalid_team")
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
		AgentID:        req.AgentID,
		TeamID:         req.TeamID,
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
