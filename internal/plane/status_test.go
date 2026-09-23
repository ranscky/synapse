//go:build integration

// The tests Phase 28 is defined by: a suspended tenant is refused with 402 on a
// paid endpoint, a tenant in its grace period is served with a warning header, an
// active tenant is served silently, and a suspended tenant can still read
// /health.
//
// Everything they touch is real -- the route table, the chi middleware chain and
// its order, the JWT verification, and the row in synapse_global.tenants --
// because the claims are about what the platform does with a status, not about
// what a function returns. The fixtures (statusPool, provisionStatusTenant,
// setTenantStatus, statusRouter) live in status_setup_test.go; the two tests after
// the four above are the other half of the specification, the routes the gate must
// leave alone.
//
//	go test ./internal/plane/... -run TestTenantStatus -v -tags integration
package plane_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"synapse/internal/plane"
	"synapse/internal/store"
	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTenantStatusSuspendedTenantIsRefusedWith402 is the phase's first claim: a
// tenant whose subscription was deleted does not reach a paid endpoint, and the
// body tells the caller how to get back in.
//
// The searcher's counter is the assertion that matters most. A 402 that still ran
// the handler would be a gate that refused the response after doing the work --
// and on the write surfaces it would mean a suspended tenant's memories were
// stored anyway. Nothing about the handler is allowed to have happened.
func TestTenantStatusSuspendedTenantIsRefusedWith402(t *testing.T) {
	pool := statusPool(t)
	ctx := context.Background()
	t.Setenv(plane.EnvMasterKey, statusMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	suspended := provisionStatusTenant(t, ctx, provisioner)
	setTenantStatus(t, ctx, pool, suspended.tenantID, plane.TenantStatusSuspended, nil)

	searcher := &fakeSearcher{entries: []store.MemoryEntry{}}
	router := statusRouter(t, cfg, pool, provisioner, searcher, nil)

	rec := getStatusSearch(router, suspended.jwt)

	require.Equal(t, http.StatusPaymentRequired, rec.Code, "body: %s", rec.Body.String())

	// The published 402 shape, asserted as JSON: the machine-readable reason and
	// the URL that resolves it.
	assert.JSONEq(t, `{"error":"payment_required","upgrade_url":"`+statusUpgradeURL+`"}`, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Zero(t, searcher.calls, "the gate must not let a suspended tenant reach the handler")
	assert.Empty(t, rec.Header().Get(statusGraceHeader), "a suspended tenant has no grace period to warn about")
}

// TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader is the phase's
// second claim: a tenant whose payment failed is still served, and is told how
// much of its window is left.
//
// Two stamps, because presence alone would not distinguish a working countdown
// from a header that always says the same thing: one tenant entered the grace
// period just now and must be told the whole window, one entered it two days ago
// and must be told the remainder. Both are read back through a real request, so
// the header is the one the HTTP layer wrote.
func TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader(t *testing.T) {
	cases := []struct {
		name     string
		started  func() time.Time
		expected string
	}{
		{name: "just entered the grace period", started: time.Now, expected: "7days"},
		{name: "two days into it", started: func() time.Time { return time.Now().Add(-48 * time.Hour) }, expected: "5days"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := statusPool(t)
			ctx := context.Background()
			t.Setenv(plane.EnvMasterKey, statusMasterKeyHex)

			cfg := newConfig(adminToken)
			provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

			warned := provisionStatusTenant(t, ctx, provisioner)
			started := tc.started()
			setTenantStatus(t, ctx, pool, warned.tenantID, plane.TenantStatusGracePeriod, &started)

			searcher := &fakeSearcher{entries: []store.MemoryEntry{}}
			router := statusRouter(t, cfg, pool, provisioner, searcher, nil)

			rec := getStatusSearch(router, warned.jwt)

			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, tc.expected, rec.Header().Get(statusGraceHeader),
				"the header counts the %d-day window down", statusGraceDays)
			assert.JSONEq(t, `{"memories":[]}`, rec.Body.String())
			assert.Equal(t, 1, searcher.calls, "a tenant in its grace period is served normally")
		})
	}
}

// TestTenantStatusActiveTenantIsServedWithoutTheWarning is the third claim, and
// the one a gate that keyed off the wrong column would fail: the tenant's status
// is active while its grace_period_started_at still holds a timestamp from before
// the payment went through.
//
// internal/billing clears that stamp when it activates a tenant, so this state is
// deliberate rather than realistic -- it is what makes the assertion mean "the
// warning follows the status" instead of "the stamp happens to be null".
func TestTenantStatusActiveTenantIsServedWithoutTheWarning(t *testing.T) {
	pool := statusPool(t)
	ctx := context.Background()
	t.Setenv(plane.EnvMasterKey, statusMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	active := provisionStatusTenant(t, ctx, provisioner)
	leftover := time.Now().Add(-48 * time.Hour)
	setTenantStatus(t, ctx, pool, active.tenantID, plane.TenantStatusActive, &leftover)

	searcher := &fakeSearcher{entries: []store.MemoryEntry{}}
	router := statusRouter(t, cfg, pool, provisioner, searcher, nil)

	rec := getStatusSearch(router, active.jwt)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.JSONEq(t, `{"memories":[]}`, rec.Body.String())
	assert.Equal(t, 1, searcher.calls, "an active tenant is served normally")
	assert.Empty(t, rec.Header().Get(statusGraceHeader), "an active tenant is not warned")
}

// TestTenantStatusSuspendedTenantCanStillReadHealth is the fourth claim, and it
// pins the exemption an orchestrator depends on: a suspended tenant's plane must
// still report whether it is up, or a suspension would look like an outage and
// the tenant would be restarted into a 402 loop.
//
// The token is presented even though /health reads nothing from it, which is the
// point: the request is the suspended tenant's, and it is answered.
func TestTenantStatusSuspendedTenantCanStillReadHealth(t *testing.T) {
	pool := statusPool(t)
	ctx := context.Background()
	t.Setenv(plane.EnvMasterKey, statusMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	suspended := provisionStatusTenant(t, ctx, provisioner)
	setTenantStatus(t, ctx, pool, suspended.tenantID, plane.TenantStatusSuspended, nil)

	searcher := &fakeSearcher{}
	router := statusRouter(t, cfg, pool, provisioner, searcher, nil)

	req := httptest.NewRequest(http.MethodGet, statusHealthPath, nil)
	req.Header.Set("Authorization", "Bearer "+suspended.jwt)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.JSONEq(t, `{"status":"ok","version":"2.0.0","db":"connected"}`, rec.Body.String())
	assert.Empty(t, rec.Header().Get(statusGraceHeader))
	assert.Zero(t, searcher.calls)
}

// TestTenantStatusRoutesWithoutATenantStatusStayOpen is the other half of the
// specification: the surfaces a billing status has no bearing on. The two that a
// blanket /v2 gate would have swept up are exercised here.
//
// Stripe's webhook first, because a 402 on it would be self-defeating: the
// delivery that lifts a suspension is exactly the one a status-aware gate would
// refuse, so a suspended tenant could never pay its way back. The tenant token is
// sent along to show the gate is not consulting it either.
//
// The admin provisioning route second: its caller holds the admin token rather
// than a tenant token, so there is no tenant whose status could be checked.
func TestTenantStatusRoutesWithoutATenantStatusStayOpen(t *testing.T) {
	pool := statusPool(t)
	ctx := context.Background()
	t.Setenv(plane.EnvMasterKey, statusMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	suspended := provisionStatusTenant(t, ctx, provisioner)
	setTenantStatus(t, ctx, pool, suspended.tenantID, plane.TenantStatusSuspended, nil)

	var deliveries int
	webhook := func(w http.ResponseWriter, _ *http.Request) {
		deliveries++
		w.WriteHeader(http.StatusOK)
	}

	router := statusRouter(t, cfg, pool, provisioner, &fakeSearcher{}, webhook)

	req := httptest.NewRequest(http.MethodPost, billingWebhookPath,
		strings.NewReader(`{"type":"customer.subscription.deleted"}`))
	req.Header.Set("Authorization", "Bearer "+suspended.jwt)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, deliveries, "the delivery must reach the injected handler, not the gate")

	req = httptest.NewRequest(http.MethodPost, statusTenantsPath,
		strings.NewReader(`{"slug":"status-`+uuid.NewString()[:12]+`","plan":"team"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", adminToken)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestTenantStatusUnknownStatusIsServed documents the last branch of the switch:
// a value this phase does not know is not a suspension. A later phase that adds a
// status must not find its tenants locked out by an older gate that never heard
// of it.
func TestTenantStatusUnknownStatusIsServed(t *testing.T) {
	pool := statusPool(t)
	ctx := context.Background()
	t.Setenv(plane.EnvMasterKey, statusMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	future := provisionStatusTenant(t, ctx, provisioner)
	// A literal, deliberately: the point is a status outside this phase's
	// vocabulary, so naming a constant here would defeat the test.
	setTenantStatus(t, ctx, pool, future.tenantID, "past_due", nil)

	searcher := &fakeSearcher{entries: []store.MemoryEntry{}}
	router := statusRouter(t, cfg, pool, provisioner, searcher, nil)

	rec := getStatusSearch(router, future.jwt)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, searcher.calls)
	assert.Empty(t, rec.Header().Get(statusGraceHeader))
}
