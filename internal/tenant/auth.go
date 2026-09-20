// Package tenant owns Synapse v2's tenant identity layer.
//
// Phase 2 scope is deliberately narrow: one chi-compatible middleware that
// verifies a tenant JWT, plus the accessors handlers use to read the verified
// claims back out. There is no database access, no key wrapping, no metering,
// and no route registration in this package yet.
package tenant

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"synapse/internal/plane"

	"github.com/golang-jwt/jwt/v5"
)

// signingAlgorithms is the allow-list of HMAC algorithms the middleware
// accepts. Anything else -- asymmetric algorithms and "none" alike -- is
// rejected before signature verification begins, and hmacKeyFunc re-checks the
// method type, which together close the classic "alg" confusion attacks.
var signingAlgorithms = []string{"HS256", "HS384", "HS512"}

// unauthorizedBody is the only 401 body this package ever writes. Every
// rejection reason -- absent header, unknown scheme, malformed token, bad
// signature, expired token, missing exp, empty tenant_id, unconfigured secret
// -- returns exactly this, so the response cannot be used as an oracle to
// learn which check failed.
const unauthorizedBody = `{"error":"unauthorized"}`

// ctxKey is the unexported type behind every context key this package stores,
// so no unrelated package can read or overwrite a claim by reusing a string
// key that happens to match.
type ctxKey int

// Context keys, one per verified claim.
const (
	tenantIDKey ctxKey = iota
	tenantSlugKey
	planKey
	complianceTierKey
	adminKey
)

// claims is the tenant JWT payload the control plane depends on. The JSON
// names are the wire contract and must stay in sync with the tokens the plane
// issues; RegisteredClaims carries exp/iat/nbf so expiry is enforced by the
// parser rather than by hand.
type claims struct {
	TenantID       string `json:"tenant_id"`
	TenantSlug     string `json:"tenant_slug"`
	Plan           string `json:"plan"`
	ComplianceTier string `json:"compliance_tier"`
	Admin          bool   `json:"admin"`

	jwt.RegisteredClaims
}

// JWTMiddleware returns middleware that verifies the request's Bearer token
// against cfg.JWTSecret and, when it is valid, attaches the verified claims to
// the request context for TenantIDFromCtx and its siblings. It is compatible
// with chi's Router.Use.
//
// The signing key is read once, here, and captured by the returned closure:
// the middleware never consults cfg again, so mutating the config after
// construction cannot change how a token is verified mid-flight.
//
// Verification is strict and fail-closed. A request is answered with 401 and
// unauthorizedBody when the header is absent, the scheme is not Bearer, the
// token is blank or malformed, the algorithm is outside signingAlgorithms, the
// signature does not match, the token has expired, the token carries no exp,
// the verified token names no tenant, or the middleware was built without a
// secret. Nothing about the token, the secret, the claims, or the client is
// ever logged: this package imports no logger at all.
func JWTMiddleware(cfg *plane.PlaneConfig) func(http.Handler) http.Handler {
	var secret []byte
	if cfg != nil {
		secret = []byte(cfg.JWTSecret)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fail closed: a missing key must never degrade into "skip
			// verification and trust the payload".
			if len(secret) == 0 {
				writeUnauthorized(w)
				return
			}

			raw, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				writeUnauthorized(w)
				return
			}

			var parsed claims
			_, err := jwt.ParseWithClaims(raw, &parsed, hmacKeyFunc(secret),
				jwt.WithValidMethods(signingAlgorithms),
				jwt.WithExpirationRequired(),
			)
			if err != nil {
				writeUnauthorized(w)
				return
			}

			// A signature-valid token that names no tenant would hand the
			// handler an empty isolation key. Under schema-per-tenant that is
			// never acceptable, so it is a rejection, not a zero value.
			if parsed.TenantID == "" {
				writeUnauthorized(w)
				return
			}

			next.ServeHTTP(w, r.WithContext(withClaims(r.Context(), &parsed)))
		})
	}
}

// TenantIDFromCtx returns the verified tenant id attached by JWTMiddleware, or
// "" when the request never passed through it.
func TenantIDFromCtx(ctx context.Context) string {
	return stringFromCtx(ctx, tenantIDKey)
}

// TenantSlugFromCtx returns the verified tenant slug attached by
// JWTMiddleware, or "" when absent.
func TenantSlugFromCtx(ctx context.Context) string {
	return stringFromCtx(ctx, tenantSlugKey)
}

// PlanFromCtx returns the verified plan claim attached by JWTMiddleware, or ""
// when absent.
func PlanFromCtx(ctx context.Context) string {
	return stringFromCtx(ctx, planKey)
}

// ComplianceTierFromCtx returns the verified compliance_tier claim attached by
// JWTMiddleware, or "" when absent.
func ComplianceTierFromCtx(ctx context.Context) string {
	return stringFromCtx(ctx, complianceTierKey)
}

// IsAdminFromCtx reports the verified admin claim attached by JWTMiddleware.
// It is false when the claim is absent, which is the safe default.
func IsAdminFromCtx(ctx context.Context) bool {
	admin, _ := ctx.Value(adminKey).(bool)

	return admin
}

// hmacKeyFunc returns the key function the parser calls with the token under
// verification. It inspects the method's concrete type rather than its name,
// so a token declaring an asymmetric or "none" algorithm can never be verified
// with the shared HMAC secret.
func hmacKeyFunc(secret []byte) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("tenant: unexpected signing method %q", token.Method.Alg())
		}

		return secret, nil
	}
}

// bearerToken extracts the token from an Authorization header value. The
// scheme comparison is case-insensitive because RFC 7235 defines auth schemes
// that way. The token is trimmed and returned; it is never logged.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}

	return token, true
}

// withClaims returns a copy of ctx carrying every verified claim.
func withClaims(ctx context.Context, c *claims) context.Context {
	ctx = context.WithValue(ctx, tenantIDKey, c.TenantID)
	ctx = context.WithValue(ctx, tenantSlugKey, c.TenantSlug)
	ctx = context.WithValue(ctx, planKey, c.Plan)
	ctx = context.WithValue(ctx, complianceTierKey, c.ComplianceTier)
	ctx = context.WithValue(ctx, adminKey, c.Admin)

	// The slug is also published through internal/plane's own accessor, because
	// the keys above are unexported here and the plane's sync endpoint -- which
	// cannot import this package -- needs the slug to pick the tenant's schema.
	// Both accessors carry the same verified value; nothing is re-derived and
	// the claim is never taken from anywhere but this signed token.
	return plane.WithTenantSlug(ctx, c.TenantSlug)
}

// stringFromCtx reads a string claim, returning "" when the key is absent or
// holds something other than a string.
func stringFromCtx(ctx context.Context, key ctxKey) string {
	value, _ := ctx.Value(key).(string)

	return value
}

// writeUnauthorized emits the single 401 shape this package uses. Only the
// constant body is written -- no parser error, claim value, or header content
// is ever reflected back to the client.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	// RFC 6750 section 3: a 401 from a token-protected resource names the
	// scheme it expects.
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)

	// A failed write means the client went away; there is nothing useful to
	// do about it on the rejection path.
	_, _ = w.Write([]byte(unauthorizedBody))
}
