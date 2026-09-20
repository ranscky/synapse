package plane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testTenantID is a well-formed uuid the fake provisioner hands back.
const testTenantID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

// testAPIKey is a 64-character hex string shaped like the key
// tenant.GenerateAPIKey produces.
const testAPIKey = "6f1c0f0d5a2b4c8e9f3d7a1b6c2e4f8091a2b3c4d5e6f708192a3b4c5d6e7f80"

// newIssuingRouter returns a router whose provisioner mints a real token with
// tenant.IssueToken, so the response can be checked against the very middleware
// that will verify it in production.
func newIssuingRouter(t *testing.T, cfg *plane.PlaneConfig) (http.Handler, *fakeProvisioner, *bytes.Buffer) {
	t.Helper()

	provisioner := &fakeProvisioner{}
	provisioner.issue = func(_ context.Context, req plane.ProvisionRequest) (plane.ProvisionResult, error) {
		token, err := tenant.IssueToken(cfg, tenant.TokenIdentity{
			TenantID: testTenantID,
			Slug:     req.Slug,
			Plan:     req.Plan,
			Tier:     req.ComplianceTier,
			AgentID:  req.AgentID,
			TeamID:   req.TeamID,
		})
		if err != nil {
			return plane.ProvisionResult{}, err
		}

		return plane.ProvisionResult{TenantID: testTenantID, JWT: token, APIKey: testAPIKey}, nil
	}

	router, logs := newRouter(cfg, fakeDB{}, provisioner)

	return router, provisioner, logs
}

// assertTokenAcceptedByMiddleware runs token through the tenant JWT middleware
// and asserts every claim the verifier publishes.
func assertTokenAcceptedByMiddleware(t *testing.T, cfg *plane.PlaneConfig, token, slug, plan string) {
	t.Helper()

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
		gotTenantID = tenant.TenantIDFromCtx(ctx)
		gotSlug = tenant.TenantSlugFromCtx(ctx)
		gotPlan = tenant.PlanFromCtx(ctx)
		gotTier = tenant.ComplianceTierFromCtx(ctx)
		gotAdmin = tenant.IsAdminFromCtx(ctx)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	tenant.JWTMiddleware(cfg)(inner).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "the plane must mint tokens its own verifier accepts")
	require.True(t, called, "the middleware must reach the handler for a freshly issued token")
	assert.Equal(t, testTenantID, gotTenantID)
	assert.Equal(t, slug, gotSlug)
	assert.Equal(t, plan, gotPlan)
	assert.Equal(t, "team", gotTier)
	assert.False(t, gotAdmin, "provisioning must never mint an admin token")
}

func TestCreateTenantHappyPath(t *testing.T) {
	cfg := newConfig(adminToken)
	router, provisioner, logs := newIssuingRouter(t, cfg)

	rec := postTenant(router, adminToken, `{"slug":"my-team","plan":"team"}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.True(t, strings.HasPrefix(rec.Body.String(), `{"tenant_id":"`), "unexpected body shape: %s", rec.Body.String())

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body, 3, "the 201 body carries exactly tenant_id, jwt and api_key")

	assert.Equal(t, testTenantID, body["tenant_id"])
	assert.Equal(t, testAPIKey, body["api_key"])
	assert.NotEmpty(t, body["jwt"])

	assert.Equal(t, plane.ProvisionRequest{Slug: "my-team", Plan: "team", ComplianceTier: "team"}, provisioner.gotReq)

	assertTokenAcceptedByMiddleware(t, cfg, body["jwt"], "my-team", "team")

	// Nothing that came back to the client may appear in the log.
	assert.NotContains(t, logs.String(), body["api_key"])
	assert.NotContains(t, logs.String(), body["jwt"])
	assert.NotContains(t, logs.String(), cfg.JWTSecret)
	assert.NotContains(t, logs.String(), adminToken)
	assert.Contains(t, logs.String(), testTenantID, "the operator should still see which tenant was created")
}

func TestCreateTenantDefaultsPlanToOSS(t *testing.T) {
	bodies := map[string]string{
		"plan absent": `{"slug":"my-team"}`,
		"plan empty":  `{"slug":"my-team","plan":""}`,
		"plan null":   `{"slug":"my-team","plan":null}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			provisioner := &fakeProvisioner{}
			router, _ := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

			rec := postTenant(router, adminToken, body)

			require.Equal(t, http.StatusCreated, rec.Code)
			assert.Equal(t, "oss", provisioner.gotReq.Plan)
			assert.Equal(t, "team", provisioner.gotReq.ComplianceTier)
		})
	}
}

func TestCreateTenantDuplicateSlugReturnsConflict(t *testing.T) {
	errs := map[string]error{
		"sentinel": plane.ErrTenantExists,
		"wrapped":  fmt.Errorf("tenant: insert tenant: %w", plane.ErrTenantExists),
	}

	for name, provisionErr := range errs {
		t.Run(name, func(t *testing.T) {
			provisioner := &fakeProvisioner{err: provisionErr}
			router, _ := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

			rec := postTenant(router, adminToken, `{"slug":"my-team","plan":"team"}`)

			require.Equal(t, http.StatusConflict, rec.Code)
			assert.JSONEq(t, `{"error":"tenant_exists"}`, rec.Body.String())
		})
	}
}

func TestCreateTenantDatabaseErrorIsNotReflectedToTheClient(t *testing.T) {
	// Shaped like a real pgx connect error: it names the host and the user,
	// which is exactly what must not reach a client.
	const leaky = "failed to connect to host=127.0.0.1 user=synapse database=synapse: dial error"

	provisioner := &fakeProvisioner{err: errors.New(leaky)}
	router, logs := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

	rec := postTenant(router, adminToken, `{"slug":"my-team","plan":"team"}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "127.0.0.1")
	assert.NotContains(t, rec.Body.String(), "dial error")

	assert.Contains(t, logs.String(), "Tenant provisioning failed",
		"the operator still needs the real cause, server-side")
}
