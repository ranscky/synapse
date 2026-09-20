// Package plane_test exercises the control plane's HTTP surface through its
// public API: real chi routes, real JSON encoding, real middleware.
//
// It is an external test package because the provisioning tests verify the JWT
// the plane mints through internal/tenant's middleware, and an internal test
// could not import internal/tenant without closing the cycle
// plane -> tenant -> plane.
package plane_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapse/internal/plane"

	charmlog "github.com/charmbracelet/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adminToken is the value the test plane expects in the Authorization header.
const adminToken = "test-admin-token-that-must-not-be-logged"

// jwtSecret is longer than the 32-character minimum the config demands, so a
// token minted in these tests is one a real plane could have signed.
const jwtSecret = "test-signing-secret-0123456789abcdef-0123456789"

// fakeDB is the health probe's test double.
type fakeDB struct {
	err error
}

// Ping implements plane.Database.
func (f fakeDB) Ping(ctx context.Context) error { return f.err }

// fakeProvisioner records what the handler asked for and answers with whatever
// the test scripted.
type fakeProvisioner struct {
	result plane.ProvisionResult
	err    error

	// issue, when set, computes the answer instead of returning the scripted one.
	issue func(context.Context, plane.ProvisionRequest) (plane.ProvisionResult, error)

	calls  int
	gotReq plane.ProvisionRequest
}

// Provision implements plane.TenantProvisioner.
func (f *fakeProvisioner) Provision(ctx context.Context, req plane.ProvisionRequest) (plane.ProvisionResult, error) {
	f.calls++
	f.gotReq = req

	if f.issue != nil {
		return f.issue(ctx, req)
	}

	return f.result, f.err
}

// newConfig returns a config carrying only what the HTTP surface reads.
func newConfig(admin string) *plane.PlaneConfig {
	return &plane.PlaneConfig{
		ListenAddr: plane.DefaultListenAddr,
		JWTSecret:  jwtSecret,
		AdminToken: admin,
	}
}

// newRouter builds the plane's routes around the given doubles and returns the
// captured log output, so a test can assert on what was written.
func newRouter(cfg *plane.PlaneConfig, db plane.Database, tenants plane.TenantProvisioner) (http.Handler, *bytes.Buffer) {
	var logs bytes.Buffer

	logger := charmlog.NewWithOptions(&logs, charmlog.Options{
		Level:           charmlog.DebugLevel,
		ReportTimestamp: false,
	})

	return plane.NewServer(cfg, db, tenants, nil, nil, logger).Routes(), &logs
}

// postTenant sends body to POST /v2/tenants with the given Authorization header,
// omitted entirely when token is empty.
func postTenant(router http.Handler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v2/tenants", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

func TestHealthReportsConnectedDatabase(t *testing.T) {
	router, _ := newRouter(newConfig(adminToken), fakeDB{}, &fakeProvisioner{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"status":"ok","version":"2.0.0","db":"connected"}`, rec.Body.String())
}

// TestCreateTenantRequiresAdminToken pins the middleware's fail-closed behavior:
// only the exact configured token passes, and no attempt reaches the
// provisioner. The Bearer-prefixed case is a rejection on purpose -- the admin
// token is not a bearer credential, and accepting two spellings for one secret
// only widens the surface.
func TestCreateTenantRequiresAdminToken(t *testing.T) {
	rejected := []struct {
		name  string
		token string
	}{
		{name: "no authorization header", token: ""},
		{name: "wrong token", token: "not-the-admin-token"},
		{name: "bearer prefixed", token: "Bearer " + adminToken},
		{name: "trailing space", token: adminToken + " "},
		{name: "token as a prefix", token: adminToken[:10]},
	}

	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			provisioner := &fakeProvisioner{}
			router, logs := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

			rec := postTenant(router, tt.token, `{"slug":"my-team","plan":"team"}`)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
			assert.Zero(t, provisioner.calls, "a rejected request must not reach the provisioner")
			assert.NotContains(t, logs.String(), adminToken, "the admin token must never be logged")
		})
	}

	t.Run("exact token accepted", func(t *testing.T) {
		provisioner := &fakeProvisioner{}
		router, _ := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

		rec := postTenant(router, adminToken, `{"slug":"my-team","plan":"team"}`)

		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Equal(t, 1, provisioner.calls)
	})
}

// TestCreateTenantUnconfiguredAdminTokenFailsClosed covers a plane whose admin
// token was never set: the route must be unusable, never open.
func TestCreateTenantUnconfiguredAdminTokenFailsClosed(t *testing.T) {
	provisioner := &fakeProvisioner{}
	router, _ := newRouter(newConfig(""), fakeDB{}, provisioner)

	for _, token := range []string{"", adminToken, " "} {
		rec := postTenant(router, token, `{"slug":"my-team","plan":"team"}`)

		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
	}

	assert.Zero(t, provisioner.calls)
}

func TestCreateTenantRejectsInvalidSlug(t *testing.T) {
	invalid := []string{
		"",
		"ab",
		strings.Repeat("a", 33),
		"My-Team",
		"my_team",
		"my team",
		" my-team",
		"my-team ",
		"team!",
		"éam",
	}

	for _, slug := range invalid {
		t.Run(slug, func(t *testing.T) {
			provisioner := &fakeProvisioner{}
			router, _ := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

			rec := postTenant(router, adminToken, `{"slug":"`+slug+`","plan":"team"}`)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"invalid_slug"}`, rec.Body.String())
			assert.Zero(t, provisioner.calls)
		})
	}
}

func TestCreateTenantRejectsInvalidPlan(t *testing.T) {
	invalid := []string{"Team", "team plan", strings.Repeat("p", 33), "team/plan"}

	for _, plan := range invalid {
		t.Run(plan, func(t *testing.T) {
			provisioner := &fakeProvisioner{}
			router, _ := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

			rec := postTenant(router, adminToken, `{"slug":"my-team","plan":"`+plan+`"}`)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"invalid_plan"}`, rec.Body.String())
			assert.Zero(t, provisioner.calls)
		})
	}
}

func TestCreateTenantRejectsMalformedBody(t *testing.T) {
	bodies := map[string]string{
		"empty body":       "",
		"not json":         "slug=my-team",
		"unknown field":    `{"slug":"my-team","admin":true}`,
		"two json objects": `{"slug":"my-team"}{"slug":"other"}`,
		"wrong type":       `{"slug":42}`,
		"oversized":        `{"slug":"` + strings.Repeat("a", 5000) + `"}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			provisioner := &fakeProvisioner{}
			router, _ := newRouter(newConfig(adminToken), fakeDB{}, provisioner)

			rec := postTenant(router, adminToken, body)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.JSONEq(t, `{"error":"invalid_body"}`, rec.Body.String())
			assert.Zero(t, provisioner.calls)
		})
	}
}

func TestCreateTenantWithoutProvisionerAnswersInternally(t *testing.T) {
	router, _ := newRouter(newConfig(adminToken), fakeDB{}, nil)

	rec := postTenant(router, adminToken, `{"slug":"my-team","plan":"team"}`)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
}

func TestHealthReportsDisconnectedDatabase(t *testing.T) {
	tests := []struct {
		name string
		db   plane.Database
	}{
		{name: "probe fails", db: fakeDB{err: errors.New("connection refused")}},
		{name: "no database configured", db: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, _ := newRouter(newConfig(adminToken), tt.db, &fakeProvisioner{})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.JSONEq(t, `{"status":"degraded","version":"2.0.0","db":"disconnected"}`, rec.Body.String())
		})
	}
}
