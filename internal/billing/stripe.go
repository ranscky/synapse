// The webhook endpoint itself: the handler, the three status statements it
// chooses between, and the request parsing around them. The package comment --
// including why this route carries no token, what is never logged, and why an
// unhandled event is acknowledged -- lives in doc.go.
//
// The two response shapes this handler writes are in respond.go, so that this
// file stays inside the 300-line ceiling with the reasoning its decisions need.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/webhook"
)

const (
	// MaxWebhookBodyBytes is the request-body ceiling. It is the value Stripe's
	// own webhook example uses, which is the point: the largest event shape this
	// handler must survive is whatever Stripe actually sends, and a smaller
	// guess would reject real invoices. It is exported because it is also the
	// contract a deployment's reverse proxy has to allow through.
	MaxWebhookBodyBytes = 65536

	// stripeSignatureHeader is the header stripe-go reads. Named here so the one
	// place that could misspell it sits next to the call that consumes it.
	stripeSignatureHeader = "Stripe-Signature"

	// tenantsTable is the one table this package writes, schema-qualified. The
	// schema name comes from internal/tenant, which owns the migration, so it is
	// written down once -- the same arrangement internal/ledger and
	// internal/metering have for their own tables.
	tenantsTable = tenant.SchemaName + ".tenants"

	// updateTimeout bounds one status UPDATE. It is short on purpose: the work is
	// one indexed single-row update, and a webhook that hangs is one Stripe
	// redelivers anyway, so a timeout costs a retry rather than a stuck delivery.
	updateTimeout = 5 * time.Second
)

// Tenant status values, as stored in synapse_global.tenants.status. Exported
// because they are the vocabulary a later phase reads: this package is where the
// values are written, and whoever enforces them should not be spelling those
// strings a second time.
const (
	// StatusActive is a tenant in good standing: nothing to enforce.
	StatusActive = "active"
	// StatusGracePeriod is a tenant whose payment failed but whose access has not
	// been cut off yet.
	StatusGracePeriod = "grace_period"
	// StatusSuspended is a tenant whose subscription was deleted.
	StatusSuspended = "suspended"
)

// The three statements this package runs, one per event type. Each is keyed by
// the Stripe customer id -- which the unique index on that column makes a
// single-row predicate -- and each returns the tenant it touched, so a delivery
// is logged against a tenant rather than against a customer id nobody looks up.
// id is cast to text for the same reason internal/tenant's insert does it: the
// column is uuid and the value travels as a string.
const (
	// activateTenant clears the grace period as it sets the status, so
	// "grace_period_started_at is set" always means "this tenant is in the grace
	// period right now" rather than "was, once".
	activateTenant = `UPDATE ` + tenantsTable + `
	SET status = '` + StatusActive + `', grace_period_started_at = NULL
	WHERE stripe_customer_id = $1
	RETURNING id::text`

	// startGracePeriod stamps the start time on every transition, including a
	// repeated failure: the timestamp means "this grace period began now", and a
	// second failed invoice is a new beginning rather than a continuation of the
	// first.
	startGracePeriod = `UPDATE ` + tenantsTable + `
	SET status = '` + StatusGracePeriod + `', grace_period_started_at = now()
	WHERE stripe_customer_id = $1
	RETURNING id::text`

	// suspendTenant deliberately leaves grace_period_started_at alone: it is the
	// record of when the grace period began, which a report about how long a
	// tenant was given would read, and suspension does not unsay it.
	suspendTenant = `UPDATE ` + tenantsTable + `
	SET status = '` + StatusSuspended + `'
	WHERE stripe_customer_id = $1
	RETURNING id::text`
)

// rowQuerier is the one database capability this package needs: run a statement
// and read the single row it returns. *pgxpool.Pool satisfies it, and so does a
// test double, which is what makes the paths that must not touch the database
// provable without one.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WebhookHandler returns the handler for POST /v2/billing/webhook: the route
// Stripe calls directly, with no token in front of it.
//
// cfg carries the signing secret and may be nil; a nil cfg, or one whose secret
// is empty, can validate nothing and therefore trusts nothing. pool may be nil,
// in which case a correctly signed event is answered 500 rather than dropped:
// the event is the only copy of the fact, so a delivery that cannot be applied
// must be retried rather than acknowledged.
//
// Nothing about the secret, the signature, or the body is logged; see the
// package comment.
func WebhookHandler(cfg *plane.PlaneConfig, pool *pgxpool.Pool) http.HandlerFunc {
	// A nil pool becomes a nil interface rather than a non-nil interface holding
	// a nil pointer, which would panic on the first call.
	var db rowQuerier
	if pool != nil {
		db = pool
	}

	return newHandler(cfg, db)
}

// newHandler is WebhookHandler's testing seam: the same handler over the narrow
// interface, so a test can assert that a rejected request issued no statement at
// all.
func newHandler(cfg *plane.PlaneConfig, db rowQuerier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := readBody(w, r)
		if !ok {
			return
		}

		secret := ""
		if cfg != nil {
			secret = cfg.StripeWebhookSecret
		}
		if secret == "" {
			// Nothing can be validated, so nothing is trusted. 503 rather than
			// 400 because it is retryable: Stripe redelivers a non-2xx for up to
			// three days, which is the window an operator has to configure the
			// secret without the events being lost -- and "this endpoint cannot
			// serve right now" is the honest answer, where "your signature is
			// wrong" would be a lie. Nothing about the value is logged beyond
			// this one line saying it is absent.
			slog.Warn("stripe webhook refused: no signing secret configured")
			writeError(w, http.StatusServiceUnavailable, "billing_unavailable")
			return
		}

		// IgnoreAPIVersionMismatch is deliberate. ConstructEvent's default also
		// refuses an event whose api_version differs from stripe-go's own pinned
		// version (2023-10-16 in v76), which would 400 every legitimately signed
		// delivery from any account pinned to a different version -- a failure no
		// signing secret could fix. The signature and the timestamp tolerance are
		// still fully enforced, and the one field this package reads
		// (data.object.customer) is stable across those versions.
		event, err := webhook.ConstructEventWithOptions(body, r.Header.Get(stripeSignatureHeader), secret,
			webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
		if err != nil {
			// 400 immediately, before any SQL. Neither the header, nor the body,
			// nor the secret, nor stripe-go's error text is logged: that error
			// string is not a contract about what it quotes.
			slog.Warn("stripe webhook rejected: signature validation failed")
			writeError(w, http.StatusBadRequest, "invalid_signature")
			return
		}

		customerID, ok := customerOf(event)
		if !ok {
			// A correctly signed event this package cannot act on. Acknowledged,
			// because no redelivery changes its shape, and logged with its type
			// only.
			slog.Warn("stripe webhook ignored: event carries no customer", "event_type", string(event.Type))
			writeJSON(w, http.StatusOK, receivedResponse{Received: true})
			return
		}

		switch event.Type {
		case stripe.EventTypeInvoicePaymentSucceeded:
			applyStatus(w, r, db, activateTenant, customerID, "payment_succeeded")
		case stripe.EventTypeInvoicePaymentFailed:
			applyStatus(w, r, db, startGracePeriod, customerID, "payment_failed")
		case stripe.EventTypeCustomerSubscriptionDeleted:
			applyStatus(w, r, db, suspendTenant, customerID, "subscription_deleted")
		default:
			// Every other event type, including the many Stripe sends to an
			// endpoint configured for more than these three. 200 is what stops
			// Stripe retrying a delivery nothing here would ever act on.
			slog.Info("stripe webhook ignored: unhandled event type", "event_type", string(event.Type))
			writeJSON(w, http.StatusOK, receivedResponse{Received: true})
		}
	}
}

// readBody reads at most MaxWebhookBodyBytes from the request. It answers 400 and
// returns false when the body is over the ceiling or cannot be read, and it never
// logs any part of it: an oversized delivery is a deployment-shaped problem (a
// proxy limit, a non-webhook caller), not something the body would explain.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBodyBytes))
	if err != nil {
		slog.Warn("stripe webhook rejected: unreadable body")
		writeError(w, http.StatusBadRequest, "invalid_body")
		return nil, false
	}

	return body, true
}

// customerOf returns the Stripe customer id the event is about, or false when the
// event carries none.
//
// The object is decoded through stripe.Customer rather than read out of the
// event's map form: a webhook sends an expandable field as a bare id string, an
// expanded one sends a whole object, and stripe.Customer's own UnmarshalJSON
// accepts both. The map-based accessor would stringify the second shape into
// something that is not an id at all.
//
// One local struct covers all three event types because invoice and subscription
// objects both name this field "customer", which is what keeps this function from
// having to know which type the event is.
func customerOf(event stripe.Event) (string, bool) {
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return "", false
	}

	var object struct {
		Customer *stripe.Customer `json:"customer"`
	}
	if err := json.Unmarshal(event.Data.Raw, &object); err != nil {
		return "", false
	}

	if object.Customer == nil || object.Customer.ID == "" {
		return "", false
	}

	return object.Customer.ID, true
}

// applyStatus runs one status UPDATE, logs what it did, and writes the response.
// event is the word the log line carries, so the log vocabulary and the switch in
// newHandler stay the same three labels.
//
// The failure split is the point of the function: a database error is a 500 so
// Stripe redelivers it, while a customer no tenant claims is a 200 with a warning,
// because no redelivery would create the row.
func applyStatus(w http.ResponseWriter, r *http.Request, db rowQuerier, statement, customerID, event string) {
	if db == nil {
		slog.Error("stripe webhook has no database", "event", event)
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), updateTimeout)
	defer cancel()

	tenantID := ""
	err := db.QueryRow(ctx, statement, customerID).Scan(&tenantID)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// A valid event for a customer no tenant claims: a test-mode delivery, or
		// one that arrived before provisioning wrote the id. The customer id is
		// logged because it is an identifier rather than a credential -- unlike
		// the signing secret, the signature header, or a tenant API key -- and it
		// is the only handle an operator has for finding which account is
		// unlinked.
		slog.Warn("stripe webhook matched no tenant", "event", event, "stripe_customer_id", customerID)

	case err != nil:
		// The one failure a retry can fix, so it is reported as 500: the event is
		// the only copy of the fact. A pgx error can quote the connection target,
		// so it is logged server-side and never returned to the client.
		slog.Error("stripe webhook status update failed", "event", event, "error", err)
		writeError(w, http.StatusInternalServerError, "internal")
		return

	case event == "payment_failed":
		// The one status change that means a human may have to act, which is why
		// it is the one that warns rather than informs.
		slog.Warn("payment_failed", "tenant_id", tenantID)

	default:
		slog.Info(event, "tenant_id", tenantID)
	}

	writeJSON(w, http.StatusOK, receivedResponse{Received: true})
}
