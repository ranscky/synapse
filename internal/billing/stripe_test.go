// The billing webhook's four tests: the two events that take access away, the one
// that gives it back, and the rejection that must change nothing.
//
// What they are evidence for: a signed Stripe delivery moves exactly the tenant
// row whose stripe_customer_id the event names, and an unsigned or mis-signed one
// moves nothing. The assertions read the tenant row back out of Postgres rather
// than counting calls, because the claim is about a column and a timestamp in that
// table -- `status text NOT NULL DEFAULT 'active'` and
// `grace_period_started_at timestamptz` -- and a double can see neither of them.
//
// Every accepted case is signed through the real signing path
// (webhook.GenerateTestSignedPayload) and validated by the real validating path
// (webhook.ConstructEventWithOptions, inside the handler), so the signature is not
// stubbed out of the test: the only synthetic part is the payload's contents.
//
// The database is required; this file's tests skip when SYNAPSE_TEST_DB_DSN is
// unset. stripe_db_test.go documents that convention, and stripe_unit_test.go
// covers the paths that need no database at all.
package billing

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"synapse/internal/plane"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v76/webhook"
)

const (
	// webhookPath is the route the handler is registered on. It is written here
	// rather than imported because the constant that owns it is unexported in
	// internal/plane: this is the client's view of the path, and a client's view
	// is exactly what a test should pin.
	webhookPath = "/v2/billing/webhook"

	// testWebhookSecret is the signing secret these payloads are signed with.
	// Test material: no deployment uses it, and no test prints it -- the handler's
	// own promise is that nothing about a secret or a signature reaches a log,
	// which is why the rejection cases assert on the response body rather than on
	// log output.
	testWebhookSecret = "whsec_test_2f4a9c1e7b3d5a8f0c6e9b2d4a7c1e3f"

	// testOtherSecret is a second endpoint's secret: the credential an attacker
	// would have to hold to forge a delivery, used here to sign one.
	testOtherSecret = "whsec_test_9d1b7e3a5c8f2a4e6b0d3c9f1a7b5e2c"
)

// testConfig returns the configuration the handler under test reads. Only the
// signing secret matters to this package; every other field is left at its zero
// value because nothing in internal/billing reads one.
func testConfig(secret string) *plane.PlaneConfig {
	return &plane.PlaneConfig{StripeWebhookSecret: secret}
}

// eventBody wraps one resource in the envelope Stripe posts: an event id, the
// type, and data.object. The api_version every real delivery carries is
// deliberately absent, which also documents that the handler does not require it
// -- see its IgnoreAPIVersionMismatch comment.
func eventBody(eventType, object string) []byte {
	return []byte(`{"id":"evt_test","object":"event","type":"` + eventType + `","data":{"object":` + object + `}}`)
}

// invoiceObject is the part of an invoice event this package reads: the customer
// id. Stripe sends that field as a bare id string on an unexpanded object, which
// is what a webhook always carries.
func invoiceObject(customerID string) string {
	return `{"id":"in_test","object":"invoice","customer":"` + customerID + `"}`
}

// subscriptionObject is the same field on the other object type. One field name
// across both is what lets the handler read them through a single struct.
func subscriptionObject(customerID string) string {
	return `{"id":"sub_test","object":"subscription","customer":"` + customerID + `"}`
}

// webhookRequest returns a POST to the webhook route carrying body signed with
// secret. Nothing here constructs a signature header by hand: a test that did
// could pass against a handler that accepted a forged one.
func webhookRequest(body []byte, secret string) *http.Request {
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   body,
		Secret:    secret,
		Timestamp: time.Now(),
	})

	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", signed.Header)

	return req
}

// deliver sends req to handler and returns the recorded response.
func deliver(handler http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler(rec, req)

	return rec
}

// requireReceived asserts the one success shape this endpoint has: 200 and
// {"received":true}, which is what stops Stripe redelivering.
func requireReceived(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.JSONEq(t, `{"received":true}`, rec.Body.String())
}

// TestWebhookPaymentFailedSetsGracePeriod is the phase's first named case: a
// failed invoice puts the tenant into grace_period and stamps when it started.
//
// The timestamp is asserted to be non-nil rather than to any value: what the
// column exists for is aging a grace period, and a status set without a start time
// would be a grace period nothing could ever end.
func TestWebhookPaymentFailedSetsGracePeriod(t *testing.T) {
	pool := billingPool(t)
	customerID := uniqueCustomerID(t)
	tenantID := billingTenant(t, pool, customerID)

	status, grace := billingStatus(t, pool, tenantID)
	require.Equal(t, StatusActive, status, "a freshly provisioned tenant starts active")
	require.Nil(t, grace, "a new tenant is not in a grace period")

	rec := deliver(WebhookHandler(testConfig(testWebhookSecret), pool),
		webhookRequest(eventBody("invoice.payment_failed", invoiceObject(customerID)), testWebhookSecret))
	requireReceived(t, rec)

	status, grace = billingStatus(t, pool, tenantID)
	assert.Equal(t, StatusGracePeriod, status)
	require.NotNil(t, grace, "a grace period with no start time cannot be aged")
}

// TestWebhookSubscriptionDeletedSuspendsTenant is the second named case: the
// subscription is gone, so access is.
//
// It goes through the grace period first, which is the order these events really
// arrive in, and then asserts that suspension keeps the start time: the column
// records when the tenant was first in trouble, and the report that asks how long
// they were given reads it after the fact.
func TestWebhookSubscriptionDeletedSuspendsTenant(t *testing.T) {
	pool := billingPool(t)
	customerID := uniqueCustomerID(t)
	tenantID := billingTenant(t, pool, customerID)
	handler := WebhookHandler(testConfig(testWebhookSecret), pool)

	requireReceived(t, deliver(handler,
		webhookRequest(eventBody("invoice.payment_failed", invoiceObject(customerID)), testWebhookSecret)))

	rec := deliver(handler,
		webhookRequest(eventBody("customer.subscription.deleted", subscriptionObject(customerID)), testWebhookSecret))
	requireReceived(t, rec)

	status, grace := billingStatus(t, pool, tenantID)
	assert.Equal(t, StatusSuspended, status)
	require.NotNil(t, grace, "suspension records the end state and leaves the grace period's start time alone")
}

// TestWebhookRejectsInvalidSignature is the third named case, and the phase's
// security property: a delivery the endpoint cannot authenticate is refused with
// 400 and reaches no tenant at all.
//
// Three ways of failing are covered, and the third matters most: a body signed for
// one customer and then edited to name another -- the whole point of an attacker
// holding a captured delivery. Both seeded tenants are checked afterwards, so "no
// DB change" is asserted against the row the forgery was aimed at as well as the
// one it was signed for.
func TestWebhookRejectsInvalidSignature(t *testing.T) {
	pool := billingPool(t)
	signedFor := uniqueCustomerID(t)
	signedForID := billingTenant(t, pool, signedFor)
	targeted := uniqueCustomerID(t)
	targetedID := billingTenant(t, pool, targeted)

	handler := WebhookHandler(testConfig(testWebhookSecret), pool)

	cases := []struct {
		name string
		req  *http.Request
	}{
		{
			// The classic forgery: a perfectly shaped payload signed with a secret
			// this endpoint does not hold.
			name: "signed with another endpoint's secret",
			req: webhookRequest(eventBody("invoice.payment_failed", invoiceObject(targeted)),
				testOtherSecret),
		},
		{
			// A genuine delivery whose body was replaced after it was signed: the
			// signature is real and covers something else.
			name: "body replaced after signing",
			req: func() *http.Request {
				req := webhookRequest(eventBody("invoice.payment_failed", invoiceObject(signedFor)),
					testWebhookSecret)
				req.Body = httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(
					eventBody("invoice.payment_failed", invoiceObject(targeted)))).Body
				return req
			}(),
		},
		{
			// No credential at all, which is what a scanner or a curious client
			// sends.
			name: "no signature header",
			req: httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(
				eventBody("invoice.payment_failed", invoiceObject(targeted)))),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := deliver(handler, tc.req)

			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			require.JSONEq(t, `{"error":"invalid_signature"}`, rec.Body.String())
		})
	}

	for _, id := range []string{signedForID, targetedID} {
		status, grace := billingStatus(t, pool, id)
		assert.Equal(t, StatusActive, status, "a rejected delivery must not move a tenant out of active")
		assert.Nil(t, grace, "a rejected delivery must not start a grace period")
	}
}

// TestWebhookPaymentSucceededClearsGracePeriod is the fourth named case: the retry
// succeeds and access is restored.
//
// It asserts the clearing as well as the status, because the two can drift: a
// status set back to active while grace_period_started_at keeps its old value
// would make every later read of that column a lie about a tenant who is not in a
// grace period at all.
func TestWebhookPaymentSucceededClearsGracePeriod(t *testing.T) {
	pool := billingPool(t)
	customerID := uniqueCustomerID(t)
	tenantID := billingTenant(t, pool, customerID)
	handler := WebhookHandler(testConfig(testWebhookSecret), pool)

	// Down through the same route production uses, rather than by writing the
	// grace period into the row directly: the state this test recovers from is the
	// state the first test proves the endpoint creates.
	requireReceived(t, deliver(handler,
		webhookRequest(eventBody("invoice.payment_failed", invoiceObject(customerID)), testWebhookSecret)))

	status, grace := billingStatus(t, pool, tenantID)
	require.Equal(t, StatusGracePeriod, status)
	require.NotNil(t, grace)

	rec := deliver(handler,
		webhookRequest(eventBody("invoice.payment_succeeded", invoiceObject(customerID)), testWebhookSecret))
	requireReceived(t, rec)

	status, grace = billingStatus(t, pool, tenantID)
	assert.Equal(t, StatusActive, status)
	assert.Nil(t, grace, "recovery must clear the grace period, not leave a stale start time")
}
