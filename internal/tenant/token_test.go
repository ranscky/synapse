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

// testTokenIdentity is the identity these tests mint for. AgentID and TeamID are
// set, so an issued token carries a full scope: the middleware must publish them
// and the wire contract must name them. Tests that care about a tenant-level
// token build their own identity with those two blank.
func testTokenIdentity() TokenIdentity {
	return TokenIdentity{
		TenantID: testTokenTenantID,
		Slug:     "my-team",
		Plan:     "pro",
		Tier:     "soc2",
		AgentID:  "edge-01",
		TeamID:   "team-blue",
	}
}

func TestIssueTokenRoundTripsThroughJWTMiddleware(t *testing.T) {
	cfg := &plane.PlaneConfig{JWTSecret: tokenTestSecret}

	token, err := IssueToken(cfg, testTokenIdentity())
	require.NoError(t, err)
	require.NotEmpty(t, token)

	var (
		gotTenantID string
		gotSlug     string
		gotPlan     string
		gotTier     string
		gotAgent    string
		gotTeam     string
		gotAdmin    bool
		called      bool
	)

	// The plane publishes the same claims through its own accessors, because its
	// endpoints cannot read this package's unexported keys. Both routes are read
	// here so a claim that only one of them carries is a failure rather than a
	// silent difference between the tenant layer and the plane.
	var (
		gotPlaneSlug  string
		gotPlaneAgent string
		gotPlaneTeam  string
	)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true

		ctx := r.Context()
		gotTenantID = TenantIDFromCtx(ctx)
		gotSlug = TenantSlugFromCtx(ctx)
		gotPlan = PlanFromCtx(ctx)
		gotTier = ComplianceTierFromCtx(ctx)
		gotAgent = AgentIDFromCtx(ctx)
		gotTeam = TeamIDFromCtx(ctx)
		gotAdmin = IsAdminFromCtx(ctx)

		gotPlaneSlug = plane.TenantSlugFromCtx(ctx)
		gotPlaneAgent = plane.AgentIDFromCtx(ctx)
		gotPlaneTeam = plane.TeamIDFromCtx(ctx)
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

	// The scope Phase 10 enforces memory visibility with, on both routes.
	assert.Equal(t, "edge-01", gotAgent)
	assert.Equal(t, "team-blue", gotTeam)
	assert.Equal(t, "my-team", gotPlaneSlug)
	assert.Equal(t, "edge-01", gotPlaneAgent, "the plane must be able to read the verified agent id")
	assert.Equal(t, "team-blue", gotPlaneTeam)
}

// TestIssueTokenClaimsAreTheDocumentedWireContract parses the token unverified
// and inspects the raw JSON claim names, so a renamed struct tag fails here
// instead of silently producing tokens nothing can verify.
func TestIssueTokenClaimsAreTheDocumentedWireContract(t *testing.T) {
	cfg := &plane.PlaneConfig{JWTSecret: tokenTestSecret}

	token, err := IssueToken(cfg, testTokenIdentity())
	require.NoError(t, err)

	parsed := jwt.MapClaims{}
	raw, _, err := jwt.NewParser().ParseUnverified(token, parsed)
	require.NoError(t, err)

	assert.Equal(t, "HS256", raw.Method.Alg())
	assert.Equal(t, testTokenTenantID, parsed["tenant_id"])
	assert.Equal(t, "my-team", parsed["tenant_slug"])
	assert.Equal(t, "pro", parsed["plan"])
	assert.Equal(t, "soc2", parsed["compliance_tier"])
	assert.Equal(t, "edge-01", parsed["agent_id"])
	assert.Equal(t, "team-blue", parsed["team_id"])
	assert.Equal(t, false, parsed["admin"])

	exp, ok := parsed["exp"].(float64)
	require.True(t, ok, "an issued token must carry an exp claim")

	lifetime := time.Until(time.Unix(int64(exp), 0))
	assert.InDelta(t, TokenTTL.Hours(), lifetime.Hours(), 1, "issued tokens live for one year")
}

// TestIssueTokenOmitsAnAbsentScope pins the other half of the wire contract: a
// tenant-level identity carries no agent_id or team_id key at all, rather than
// sending them as empty strings. Every token this plane issued before agent
// scoping existed looked exactly like that, so a reader of the payload has
// nothing new to distinguish -- and the fail-closed meaning of an absent agent
// claim is the middleware's, not the wire's.
func TestIssueTokenOmitsAnAbsentScope(t *testing.T) {
	cfg := &plane.PlaneConfig{JWTSecret: tokenTestSecret}

	token, err := IssueToken(cfg, TokenIdentity{
		TenantID: testTokenTenantID,
		Slug:     "my-team",
		Plan:     "pro",
		Tier:     "soc2",
	})
	require.NoError(t, err)

	parsed := jwt.MapClaims{}
	_, _, err = jwt.NewParser().ParseUnverified(token, parsed)
	require.NoError(t, err)

	assert.NotContains(t, parsed, "agent_id")
	assert.NotContains(t, parsed, "team_id")

	// And a tenant-level token is still a working token: it reads org-scoped
	// memories, which is the whole point of leaving it valid.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	JWTMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, AgentIDFromCtx(r.Context()), "a token with no agent claim means an org-only reader")
		assert.Empty(t, plane.AgentIDFromCtx(r.Context()))
	})).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
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
			token, err := IssueToken(tt.cfg, TokenIdentity{
				TenantID: tt.tenantID,
				Slug:     "my-team",
				Plan:     "pro",
				Tier:     "soc2",
			})

			require.Error(t, err)
			assert.Empty(t, token)
			assert.NotContains(t, err.Error(), tokenTestSecret, "the signing key must never appear in an error")
		})
	}
}

// TestParseTokenClaimsReadsOwnCredential is the edge node's contract: the token
// this node holds as its own credential yields exactly the identity provisioning
// minted it for, so the ledger can be wired to the right tenant and the right
// plan.
func TestParseTokenClaimsReadsOwnCredential(t *testing.T) {
	token, err := IssueToken(&plane.PlaneConfig{JWTSecret: tokenTestSecret}, testTokenIdentity())
	require.NoError(t, err)

	got, err := ParseTokenClaims(token)
	require.NoError(t, err)

	assert.Equal(t, testTokenIdentity(), got)
}

// TestParseTokenClaimsDoesNotVerifyTheSignature is the documented cost of this
// function rather than a bug: it is a claim reader, not an authenticator. The
// test exists so that nobody adds a signature check here under the impression
// that the edge can perform one -- it cannot, because it holds no JWT secret.
func TestParseTokenClaimsDoesNotVerifyTheSignature(t *testing.T) {
	token, err := IssueToken(&plane.PlaneConfig{JWTSecret: "a-secret-this-node-does-not-have-0123456789"}, testTokenIdentity())
	require.NoError(t, err)

	got, err := ParseTokenClaims(token)
	require.NoError(t, err)
	assert.Equal(t, testTokenTenantID, got.TenantID)
}

// TestParseTokenClaimsFailsClosed covers the two shapes a caller must handle:
// nothing to read, and something that is not a token. A well-formed token that
// simply carries no tenant is not an error -- the identity is empty, and a
// caller that needs a tenant id is the one that has to check.
func TestParseTokenClaimsFailsClosed(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := ParseTokenClaims("")
		require.Error(t, err)
		assert.Empty(t, got)
	})

	t.Run("not a token", func(t *testing.T) {
		got, err := ParseTokenClaims("not-a-jwt")
		require.Error(t, err)
		assert.Empty(t, got)
		assert.NotContains(t, err.Error(), "not-a-jwt", "the value is not echoed into an error")
	})

	t.Run("no tenant claims", func(t *testing.T) {
		bare, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "nobody"}).
			SignedString([]byte(tokenTestSecret))
		require.NoError(t, err)

		got, err := ParseTokenClaims(bare)
		require.NoError(t, err)
		assert.Empty(t, got.TenantID, "a token with no tenant claim yields an empty identity, not a guess")
	})
}
