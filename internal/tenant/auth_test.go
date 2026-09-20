package tenant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"synapse/internal/plane"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// testSecret is a full-length HMAC key (>= plane.MinJWTSecretLen).
	testSecret = "test-signing-secret-0123456789abcdef"
	// otherSecret is a different, equally long key used to forge a token.
	otherSecret = "wrong-signing-secret-0123456789abcdef"
)

// testPayload builds the wire form of a tenant token. The test deliberately
// uses literal JSON keys through jwt.MapClaims instead of the package's own
// claims struct, so a typo in a struct tag fails here rather than passing
// silently against itself.
func testPayload() jwt.MapClaims {
	return jwt.MapClaims{
		"tenant_id":       "tenant-123",
		"tenant_slug":     "acme",
		"plan":            "pro",
		"compliance_tier": "soc2",
		"admin":           true,
		"exp":             time.Now().Add(time.Hour).Unix(),
		"iat":             time.Now().Unix(),
	}
}

// signToken signs payload with secret using HS256, the algorithm the control
// plane issues tokens with.
func signToken(t *testing.T, secret string, payload jwt.MapClaims) string {
	t.Helper()

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, payload).SignedString([]byte(secret))
	require.NoError(t, err)

	return signed
}

// probe records what the protected handler observed. ServeHTTP runs the
// handler synchronously, so a test can read these fields straight afterwards
// without synchronisation.
type probe struct {
	called         bool
	tenantID       string
	tenantSlug     string
	plan           string
	complianceTier string
	admin          bool
}

// protectedRouter wires the middleware the way the control plane will: through
// chi's Use, with the claims read back inside an ordinary handler.
func protectedRouter(cfg *plane.PlaneConfig, seen *probe) *chi.Mux {
	router := chi.NewRouter()
	router.Use(JWTMiddleware(cfg))

	router.Get("/protected", func(w http.ResponseWriter, r *http.Request) {
		seen.called = true
		seen.tenantID = TenantIDFromCtx(r.Context())
		seen.tenantSlug = TenantSlugFromCtx(r.Context())
		seen.plan = PlanFromCtx(r.Context())
		seen.complianceTier = ComplianceTierFromCtx(r.Context())
		seen.admin = IsAdminFromCtx(r.Context())

		w.WriteHeader(http.StatusOK)
	})

	return router
}

// testConfig returns a plane config carrying the test signing key.
func testConfig() *plane.PlaneConfig {
	return &plane.PlaneConfig{JWTSecret: testSecret}
}

// do sends one request through router with the given Authorization header
// value; an empty value omits the header entirely.
func do(router http.Handler, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

// Test 1: a token signed by the plane's key reaches the handler, and every
// helper reads back the verified claim.
func TestJWTMiddlewareValidTokenAttachesClaims(t *testing.T) {
	var seen probe
	rec := do(protectedRouter(testConfig(), &seen), "Bearer "+signToken(t, testSecret, testPayload()))

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, seen.called, "a valid token must reach the next handler")

	// These five fields are exactly what the helpers returned inside the
	// handler: TenantIDFromCtx, TenantSlugFromCtx, PlanFromCtx,
	// ComplianceTierFromCtx, IsAdminFromCtx.
	assert.Equal(t, "tenant-123", seen.tenantID)
	assert.Equal(t, "acme", seen.tenantSlug)
	assert.Equal(t, "pro", seen.plan)
	assert.Equal(t, "soc2", seen.complianceTier)
	assert.True(t, seen.admin)
}

func TestJWTMiddlewareLowercaseSchemeAccepted(t *testing.T) {
	var seen probe
	rec := do(protectedRouter(testConfig(), &seen), "bearer "+signToken(t, testSecret, testPayload()))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, seen.called)
	assert.Equal(t, "tenant-123", seen.tenantID)
}

func TestJWTMiddlewareAbsentAdminClaimDefaultsFalse(t *testing.T) {
	payload := testPayload()
	delete(payload, "admin")

	var seen probe
	rec := do(protectedRouter(testConfig(), &seen), "Bearer "+signToken(t, testSecret, payload))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, seen.called)
	assert.False(t, seen.admin, "an absent admin claim must default to false")
}

// Test 2: an expired token is refused and never reaches the handler.
func TestJWTMiddlewareExpiredTokenRejected(t *testing.T) {
	payload := testPayload()
	payload["exp"] = time.Now().Add(-time.Minute).Unix()

	var seen probe
	rec := do(protectedRouter(testConfig(), &seen), "Bearer "+signToken(t, testSecret, payload))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, seen.called, "an expired token must not reach the next handler")
	assert.Equal(t, unauthorizedBody, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
}

// Test 3: no Authorization header at all is refused.
func TestJWTMiddlewareMissingAuthorizationHeaderRejected(t *testing.T) {
	var seen probe
	rec := do(protectedRouter(testConfig(), &seen), "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, seen.called)
	assert.Equal(t, unauthorizedBody, rec.Body.String())
	assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"), "401 must name the expected scheme")
}

// Test 4: a well-formed token signed with somebody else's key is refused.
func TestJWTMiddlewareWrongSecretRejected(t *testing.T) {
	var seen probe
	rec := do(protectedRouter(testConfig(), &seen), "Bearer "+signToken(t, otherSecret, testPayload()))

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, seen.called, "a token signed with another key must not reach the next handler")
	assert.Equal(t, unauthorizedBody, rec.Body.String())
}

// Strict mode: a token that verifies but is unusable -- no expiry, no tenant
// -- is a rejection, and so is anything the parser cannot accept.
func TestJWTMiddlewareRejectsUnusableCredentials(t *testing.T) {
	expired := testPayload()
	expired["exp"] = time.Now().Add(-time.Minute).Unix()

	noExpiry := testPayload()
	delete(noExpiry, "exp")

	emptyTenantID := testPayload()
	emptyTenantID["tenant_id"] = ""

	missingTenantID := testPayload()
	delete(missingTenantID, "tenant_id")

	// The classic bypass attempt: a valid JSON payload with no signature.
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, testPayload()).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	tests := []struct {
		name          string
		authorization string
	}{
		{name: "non-bearer scheme", authorization: "Basic dXNlcjpwYXNz"},
		{name: "scheme with no separator", authorization: "Bearer"},
		{name: "bearer with empty token", authorization: "Bearer "},
		{name: "not a jwt", authorization: "Bearer not-a-jwt"},
		{name: "expired", authorization: "Bearer " + signToken(t, testSecret, expired)},
		{name: "no exp claim", authorization: "Bearer " + signToken(t, testSecret, noExpiry)},
		{name: "empty tenant id", authorization: "Bearer " + signToken(t, testSecret, emptyTenantID)},
		{name: "missing tenant id", authorization: "Bearer " + signToken(t, testSecret, missingTenantID)},
		{name: "alg none", authorization: "Bearer " + unsigned},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen probe
			rec := do(protectedRouter(testConfig(), &seen), tt.authorization)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.False(t, seen.called, "a rejected request must not reach the next handler")
			assert.Equal(t, unauthorizedBody, rec.Body.String())
		})
	}
}

func TestJWTMiddlewareFailsClosedWithoutASecret(t *testing.T) {
	tests := []struct {
		name string
		cfg  *plane.PlaneConfig
	}{
		{name: "nil config", cfg: nil},
		{name: "empty secret", cfg: &plane.PlaneConfig{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen probe
			// Signed with the real test key and still refused: with no
			// configured key there is nothing to verify against, and an
			// unconfigured plane must never fall through to "trust it".
			rec := do(protectedRouter(tt.cfg, &seen), "Bearer "+signToken(t, testSecret, testPayload()))

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.False(t, seen.called)
			assert.Equal(t, unauthorizedBody, rec.Body.String())
		})
	}
}

func TestJWTMiddlewareRejectionsLeakNoCredential(t *testing.T) {
	token := signToken(t, otherSecret, testPayload())

	var seen probe
	rec := do(protectedRouter(testConfig(), &seen), "Bearer "+token)

	body := rec.Body.String()
	assert.Equal(t, unauthorizedBody, body)
	assert.NotContains(t, body, token)
	assert.NotContains(t, body, testSecret)
	assert.NotContains(t, body, "tenant-123")
	assert.NotContains(t, body, "acme")
}

func TestClaimHelpersOnEmptyContext(t *testing.T) {
	ctx := context.Background()

	assert.Empty(t, TenantIDFromCtx(ctx))
	assert.Empty(t, TenantSlugFromCtx(ctx))
	assert.Empty(t, PlanFromCtx(ctx))
	assert.Empty(t, ComplianceTierFromCtx(ctx))
	assert.False(t, IsAdminFromCtx(ctx))
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
		wantOK bool
	}{
		{name: "canonical", header: "Bearer abc.def.ghi", want: "abc.def.ghi", wantOK: true},
		{name: "lowercase scheme", header: "bearer abc", want: "abc", wantOK: true},
		{name: "extra spacing", header: "Bearer    abc   ", want: "abc", wantOK: true},
		{name: "empty header", header: "", wantOK: false},
		{name: "scheme with no separator", header: "Bearer", wantOK: false},
		{name: "empty token", header: "Bearer ", wantOK: false},
		{name: "other scheme", header: "Token abc", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := bearerToken(tt.header)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantOK, ok)
		})
	}
}
