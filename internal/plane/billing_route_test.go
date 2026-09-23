// The billing webhook route's HTTP contract: that it is reachable without a token,
// that it delegates to the injected handler, and what a plane built without one
// answers.
//
// This is all this package can say about the route. Whether a delivery is authentic
// is answered inside internal/billing -- the Stripe-Signature header against the
// signing secret -- and those tests live with the code that answers them, because
// this package does not hold the secret or the signature library. What it does own
// is the path, the delegation, and the fact that no middleware stands in front of
// it, which is what these two tests pin.
package plane_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapse/internal/plane"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// billingWebhookPath is the route under test. Written out rather than imported
// because internal/plane keeps it unexported, and a client's view of the path is
// what a test should pin.
const billingWebhookPath = "/v2/billing/webhook"

// TestBillingWebhookRouteIsOpenAndDelegates asserts the two properties that make
// this route different from every other write surface in the package: it answers
// with no Authorization header at all, and what it answers is whatever the injected
// handler wrote.
func TestBillingWebhookRouteIsOpenAndDelegates(t *testing.T) {
	var delegations int

	handler := func(w http.ResponseWriter, r *http.Request) {
		delegations++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"received":true}`))
	}

	router := plane.NewServer(newConfig(""), fakeDB{}, &fakeProvisioner{}, nil, nil, nil, nil, nil, nil, handler, nil).Routes()

	// No Authorization and no x-api-key: Stripe sends neither, and a middleware that
	// demanded one would 401 every real delivery.
	req := httptest.NewRequest(http.MethodPost, billingWebhookPath, strings.NewReader(`{"type":"invoice.payment_failed"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.JSONEq(t, `{"received":true}`, rec.Body.String())
	assert.Equal(t, 1, delegations, "the route must hand the delivery to the injected handler")
}

// TestBillingWebhookRouteRefusesWithNoHandler covers the plane that was never wired
// for billing: the route exists and answers 503, rather than disappearing into a 404
// that would tell Stripe the endpoint is gone.
func TestBillingWebhookRouteRefusesWithNoHandler(t *testing.T) {
	router := plane.NewServer(newConfig(""), fakeDB{}, &fakeProvisioner{}, nil, nil, nil, nil, nil, nil, nil, nil).Routes()

	req := httptest.NewRequest(http.MethodPost, billingWebhookPath, strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	assert.JSONEq(t, `{"error":"billing_unavailable"}`, rec.Body.String())
}
