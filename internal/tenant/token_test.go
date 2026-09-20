package tenant

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"synapse/internal/plane"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenTestSecret is the signing key these tests mint against. auth_test.go's
// testSecret is a different value on purpose: nothing here depends on it.
const tokenTestSecret = "tenant-token-test-secret-0123456789abcdef"

// testTokenTenantID is a well-formed uuid used as the subject of issued tokens.
const testTokenTenantID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

func TestIssueTokenRoundTripsThroughJWTMiddleware(t *testing.T) {
	cfg := &plane.PlaneConfig{JWTSecret: tokenTestSecret}

	token, err := IssueToken(cfg, testTokenTenantID, "my-team", "pro", "soc2")
	require.NoError(t, err)
	require.NotEmpty(t, token)

	var (
		gotTenantID string
		gotSlug     string
		gotPlan     string
		gotTier     string
		gotAdmin    bool
		called      bool
	)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true

		ctx := r.Context()
		gotTenantID = TenantIDFromCtx(ctx)
		gotSlug = TenantSlugFromCtx(ctx)
		gotPlan = PlanFromCtx(ctx)
		gotTier = ComplianceTierFromCtx(ctx)
		gotAdmin = IsAdminFromCtx(ctx)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	JWTMiddleware(cfg)(inner).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "the issuer and the verifier must agree")
	require.True(t, called)
	assert.Equal(t, testTokenTenantID, gotTenantID)
	assert.Equal(t, "my-team", gotSlug)
	assert.Equal(t, "pro", gotPlan)
	assert.Equal(t, "soc2", gotTier)
	assert.False(t, gotAdmin)
}

// TestIssueTokenClaimsAreTheDocumentedWireContract parses the token unverified
// and inspects the raw JSON claim names, so a renamed struct tag fails here
// instead of silently producing tokens nothing can verify.
func TestIssueTokenClaimsAreTheDocumentedWireContract(t *testing.T) {
	cfg := &plane.PlaneConfig{JWTSecret: tokenTestSecret}

	token, err := IssueToken(cfg, testTokenTenantID, "my-team", "pro", "soc2")
	require.NoError(t, err)

	parsed := jwt.MapClaims{}
	raw, _, err := jwt.NewParser().ParseUnverified(token, parsed)
	require.NoError(t, err)

	assert.Equal(t, "HS256", raw.Method.Alg())
	assert.Equal(t, testTokenTenantID, parsed["tenant_id"])
	assert.Equal(t, "my-team", parsed["tenant_slug"])
	assert.Equal(t, "pro", parsed["plan"])
	assert.Equal(t, "soc2", parsed["compliance_tier"])
	assert.Equal(t, false, parsed["admin"])

	exp, ok := parsed["exp"].(float64)
	require.True(t, ok, "an issued token must carry an exp claim")

	lifetime := time.Until(time.Unix(int64(exp), 0))
	assert.InDelta(t, TokenTTL.Hours(), lifetime.Hours(), 1, "issued tokens live for one year")
}

func TestIssueTokenFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *plane.PlaneConfig
		tenantID string
	}{
		{name: "nil config", cfg: nil, tenantID: testTokenTenantID},
		{name: "no signing secret", cfg: &plane.PlaneConfig{}, tenantID: testTokenTenantID},
		{name: "no tenant id", cfg: &plane.PlaneConfig{JWTSecret: tokenTestSecret}, tenantID: ""},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			token, err := IssueToken(tt.cfg, tt.tenantID, "my-team", "pro", "soc2")

			require.Error(t, err)
			assert.Empty(t, token)
			assert.NotContains(t, err.Error(), tokenTestSecret, "the signing key must never appear in an error")
		})
	}
}
