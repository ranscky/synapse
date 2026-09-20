// Phase 10's provisioning tests: an agent-scoped token can really be minted, and
// a malformed agent identity is refused rather than signed into a token.
//
// This is the half of the visibility feature that makes the other half reachable
// in production: internal/store enforces the scope, and the plane's search
// endpoint reads it from a verified token -- but a token only carries a scope if
// provisioning put one there. Split out of provision_test.go to stay inside the
// 300-line ceiling.
package plane_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimsFromToken runs token through the tenant middleware and reports the scope
// claims it publishes. The token's payload is not inspected directly: what
// matters is what the plane can actually read back out of a token its own
// verifier accepted.
func claimsFromToken(t *testing.T, cfg *plane.PlaneConfig, token string) (agentID, teamID, slug string) {
	t.Helper()

	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		agentID = tenant.AgentIDFromCtx(ctx)
		teamID = tenant.TeamIDFromCtx(ctx)
		slug = tenant.TenantSlugFromCtx(ctx)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	tenant.JWTMiddleware(cfg)(inner).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "the plane must mint tokens its own verifier accepts")

	return agentID, teamID, slug
}

func TestCreateTenantMintsAnAgentScopedToken(t *testing.T) {
	cfg := newConfig(adminToken)
	router, provisioner, logs := newIssuingRouter(t, cfg)

	rec := postTenant(router, adminToken,
		`{"slug":"my-team","plan":"team","agent_id":"edge-01","team_id":"team-blue"}`)

	require.Equal(t, http.StatusCreated, rec.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	// The operator's request reaches the provisioner unchanged...
	assert.Equal(t, plane.ProvisionRequest{
		Slug: "my-team", Plan: "team", ComplianceTier: "team",
		AgentID: "edge-01", TeamID: "team-blue",
	}, provisioner.gotReq)

	// ...and the token that comes back carries the scope, as read by the very
	// middleware that will enforce it.
	agentID, teamID, slug := claimsFromToken(t, cfg, body["jwt"])
	assert.Equal(t, "edge-01", agentID)
	assert.Equal(t, "team-blue", teamID)
	assert.Equal(t, "my-team", slug)

	// The identity is not a credential: it is safe in a log line, unlike the key
	// and the token, which still never appear.
	assert.Contains(t, logs.String(), "my-team")
	assert.NotContains(t, logs.String(), body["api_key"])
	assert.NotContains(t, logs.String(), body["jwt"])
}

func TestCreateTenantWithoutAnAgentMintsATenantToken(t *testing.T) {
	cfg := newConfig(adminToken)
	router, provisioner, _ := newIssuingRouter(t, cfg)

	rec := postTenant(router, adminToken, `{"slug":"my-team","plan":"team"}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Empty(t, provisioner.gotReq.AgentID)
	assert.Empty(t, provisioner.gotReq.TeamID)

	// A tenant-level token: valid, and an org-only reader. That is the
	// fail-closed default, not a broken token.
	agentID, teamID, slug := claimsFromToken(t, cfg, body["jwt"])
	assert.Empty(t, agentID)
	assert.Empty(t, teamID)
	assert.Equal(t, "my-team", slug)
}

func TestCreateTenantRejectsAMalformedIdentity(t *testing.T) {
	cases := map[string]struct {
		body       string
		wantReason string
	}{
		"an agent id with a space":   {`{"slug":"my-team","agent_id":"edge 01"}`, "invalid_agent"},
		"an agent id that is a path": {`{"slug":"my-team","agent_id":"edge/01"}`, "invalid_agent"},
		"an over-long agent id":      {`{"slug":"my-team","agent_id":"` + strings.Repeat("a", 65) + `"}`, "invalid_agent"},
		"a team id with a space":     {`{"slug":"my-team","team_id":"team blue"}`, "invalid_team"},
		"an over-long team id":       {`{"slug":"my-team","team_id":"` + strings.Repeat("t", 65) + `"}`, "invalid_team"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			provisioner := &fakeProvisioner{}
			router, _ := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

			rec := postTenant(router, adminToken, tc.body)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"`+tc.wantReason+`"}`, rec.Body.String())
			assert.Zero(t, provisioner.calls, "a refused request provisions nothing")
		})
	}
}
