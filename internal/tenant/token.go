package tenant

import (
	"fmt"
	"time"

	"synapse/internal/plane"

	"github.com/golang-jwt/jwt/v5"
)

// TokenTTL is how long an issued tenant JWT stays valid: 365 days, matching the
// provision-once-then-reissue-at-renewal model the API-key flow assumes.
const TokenTTL = 365 * 24 * time.Hour

// tokenSigningMethod is fixed at HS256 -- the narrowest algorithm the verifier
// in auth.go accepts. Signing and verification therefore cannot disagree about
// what a valid token looks like.
var tokenSigningMethod jwt.SigningMethod = jwt.SigningMethodHS256

// TokenIdentity is the identity a tenant token is minted for: which tenant it
// belongs to, and -- optionally -- which of that tenant's agents it speaks for.
//
// It is a struct rather than a longer list of positional parameters because the
// two halves are not interchangeable. TenantID/Slug/Plan/Tier describe the
// tenant; AgentID/TeamID describe the caller inside it and are what memory
// visibility is evaluated against. Both are strings, so passing one where the
// other belonged would compile and quietly mint a token that names the wrong
// thing.
type TokenIdentity struct {
	// TenantID is the tenant's uuid, and the token's subject. Required.
	TenantID string
	// Slug is the tenant's slug. It is the schema the plane searches, so it is
	// the one claim the plane trusts to pick a tenant's data.
	Slug string
	// Plan is the tenant's billing plan.
	Plan string
	// Tier is the tenant's compliance tier.
	Tier string
	// AgentID names the agent this token speaks for, and is the identity a
	// private memory's visibility is checked against. Optional: an empty one
	// mints a tenant-level token, which reads org-scoped memories only -- the
	// fail-closed default, and what every token issued before agent-scoped
	// tokens existed is.
	AgentID string
	// TeamID is the team that agent belongs to, matched against a team-scoped
	// memory's team_id. Optional, and only meaningful alongside AgentID.
	TeamID string
}

// IssueToken mints the tenant JWT handed back at provisioning time.
//
// The payload is the same unexported claims struct JWTMiddleware parses, so the
// wire contract has exactly one definition: a field renamed here changes what
// the middleware reads, and the tests fail instead of the two silently drifting
// apart. The claims are tenant_id, tenant_slug, plan, compliance_tier,
// admin=false -- admin is never granted by provisioning, only by an operator --
// plus agent_id and team_id when id names an agent.
//
// agent_id and team_id are omitted from the payload entirely when empty rather
// than sent as "", so a tenant-level token is byte-identical to the tokens this
// plane issued before agent scoping existed and nothing downstream has to
// distinguish "" from absent.
//
// The claim shapes themselves are not validated here: the HTTP layer that
// accepts a provisioning request is where an operator-supplied agent id is
// checked, and by the time a value reaches this function it has already passed
// that gate. What this function does enforce is the part only it can: no token
// without a signing secret, and none without a tenant.
//
// It fails closed: without a signing secret or without a tenant id there is no
// token, rather than an unsigned or tenant-less one. The secret is never logged,
// and the returned error text never contains it.
func IssueToken(cfg *plane.PlaneConfig, id TokenIdentity) (string, error) {
	if cfg == nil || cfg.JWTSecret == "" {
		return "", fmt.Errorf("tenant: cannot issue a token without a signing secret")
	}
	if id.TenantID == "" {
		return "", fmt.Errorf("tenant: cannot issue a token without a tenant id")
	}

	now := time.Now()
	payload := claims{
		TenantID:       id.TenantID,
		TenantSlug:     id.Slug,
		Plan:           id.Plan,
		ComplianceTier: id.Tier,
		AgentID:        id.AgentID,
		TeamID:         id.TeamID,
		Admin:          false,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.TenantID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(TokenTTL)),
		},
	}

	signed, err := jwt.NewWithClaims(tokenSigningMethod, payload).SignedString([]byte(cfg.JWTSecret))
	if err != nil {
		return "", fmt.Errorf("tenant: sign tenant token: %w", err)
	}

	return signed, nil
}
