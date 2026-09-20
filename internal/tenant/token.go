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

// IssueToken mints the tenant JWT handed back at provisioning time.
//
// The payload is the same unexported claims struct JWTMiddleware parses, so the
// wire contract has exactly one definition: a field renamed here changes what
// the middleware reads, and the tests fail instead of the two silently drifting
// apart. The claims are tenant_id, tenant_slug, plan, compliance_tier, and
// admin=false -- admin is never granted by provisioning, only by an operator.
//
// It fails closed: without a signing secret or without a tenant id there is no
// token, rather than an unsigned or tenant-less one. The secret is never logged,
// and the returned error text never contains it.
func IssueToken(cfg *plane.PlaneConfig, tenantID, slug, plan, tier string) (string, error) {
	if cfg == nil || cfg.JWTSecret == "" {
		return "", fmt.Errorf("tenant: cannot issue a token without a signing secret")
	}
	if tenantID == "" {
		return "", fmt.Errorf("tenant: cannot issue a token without a tenant id")
	}

	now := time.Now()
	payload := claims{
		TenantID:       tenantID,
		TenantSlug:     slug,
		Plan:           plan,
		ComplianceTier: tier,
		Admin:          false,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   tenantID,
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
