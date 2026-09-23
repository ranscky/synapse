// The billing webhook's database-free half: what a rejected delivery does (nothing
// at all), which statement each event type chooses, and how the failures a retry
// can and cannot fix are answered.
//
// These run everywhere, including CI, which runs `go test ./...` on three operating
// systems with no database on any of them.
//
// What they claim, and what they deliberately do not: a recording double proves
// which statement the handler asked for and with which argument, which is the whole
// of "a rejected delivery issues no SQL" and the first half of "each event type maps
// to its own status". It cannot prove the statement is valid SQL or that the status
// literal it carries is the one the column stores -- only Postgres can, which is
// what stripe_test.go's database-backed cases are for. Both layers exist because
// each is blind to what the other sees.
package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"synapse/internal/plane"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRow is the one row a recordingDB hands back: it copies a tenant id the way
// pgxpool would, or reports the error the test asked for (including pgx.ErrNoRows,
// which is how an unmatched customer arrives).
type stubRow struct {
	tenantID string
	err      error
}

// Scan implements pgx.Row. It accepts exactly the single *string the handler scans
// into and fails loudly on anything else, so a handler that started reading a
// different column would break a test rather than silently receive an empty id.
func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}

	if len(dest) != 1 {
		return fmt.Errorf("stubRow: handler scanned %d destinations, want 1", len(dest))
	}

	target, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("stubRow: handler scanned into %T, want *string", dest[0])
	}

	*target = r.tenantID

	return nil
}

// recordingDB is a rowQuerier that records what it was asked to run instead of
// running it.
type recordingDB struct {
	row     pgx.Row
	queries []string
	args    [][]any
}

// QueryRow implements rowQuerier.
func (db *recordingDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	db.queries = append(db.queries, sql)
	db.args = append(db.args, args)

	return db.row
}

// unitTenantID is the id the double hands back for a delivery that did match a
// tenant. A uuid, like the column, so a failure message looks like what a real
// read would produce.
const unitTenantID = "3f1c0b2e-5a4d-4c7b-9e8f-0d6a1b2c3d4e"

// TestWebhookIssuesNoSQLForARejectedDelivery is this package's "the signature
// protects the database" claim, made structural: every way a delivery can fail to
// authenticate, asserted to reach the double zero times.
//
// The oversized case is included on purpose, because it is the one rejection that
// is not about a signature at all: the body is refused at the read, before
// MaxWebhookBodyBytes is exceeded and before stripe-go ever sees it.
func TestWebhookIssuesNoSQLForARejectedDelivery(t *testing.T) {
	customerID := "cus_unit_rejected"
	body := eventBody("invoice.payment_failed", invoiceObject(customerID))

	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{
			name: "signed with another endpoint's secret",
			req:  webhookRequest(body, testOtherSecret),
			want: "invalid_signature",
		},
		{
			name: "body replaced after signing",
			req: func() *http.Request {
				req := webhookRequest(body, testWebhookSecret)
				req.Body = httptest.NewRequest(http.MethodPost, webhookPath,
					bytes.NewReader(eventBody("invoice.payment_failed", invoiceObject("cus_unit_other")))).Body
				return req
			}(),
			want: "invalid_signature",
		},
		{
			name: "no signature header",
			req:  httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body)),
			want: "invalid_signature",
		},
		{
			name: "body over the documented ceiling",
			req:  webhookRequest(bytes.Repeat([]byte("a"), MaxWebhookBodyBytes+1), testWebhookSecret),
			want: "invalid_body",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &recordingDB{row: stubRow{tenantID: unitTenantID}}

			rec := deliver(newHandler(testConfig(testWebhookSecret), db), tc.req)

			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			require.JSONEq(t, `{"error":"`+tc.want+`"}`, rec.Body.String())
			assert.Empty(t, db.queries, "a rejected delivery must not reach the database")
		})
	}
}

// TestWebhookWithoutASecretRefusesEveryDelivery covers the deployment that never
// configured billing: a nil config and an empty secret are the same answer, and it
// is neither a signature error nor a success -- the delivery is retryable, so the
// operator has the length of Stripe's retry window to fix the configuration.
func TestWebhookWithoutASecretRefusesEveryDelivery(t *testing.T) {
	body := eventBody("invoice.payment_failed", invoiceObject("cus_unit_unconfigured"))

	for _, cfg := range []*plane.PlaneConfig{nil, testConfig("")} {
		db := &recordingDB{row: stubRow{tenantID: unitTenantID}}

		rec := deliver(newHandler(cfg, db), webhookRequest(body, testWebhookSecret))

		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
		require.JSONEq(t, `{"error":"billing_unavailable"}`, rec.Body.String())
		assert.Empty(t, db.queries, "an unconfigured plane must not reach the database either")
	}
}

// TestWebhookDispatchesEachEventTypeToItsOwnStatement pins the mapping the phase
// is named for: three event types, three statements, one of them each.
//
// The assertion is that the statement carries the status the event implies and the
// customer id the event names as its only argument -- which is what keeps a
// delivery from moving a tenant other than the one the event is about. Whether the
// statement is valid SQL, and whether 'grace_period' is what the column will end up
// holding, is stripe_test.go's question, not this double's.
func TestWebhookDispatchesEachEventTypeToItsOwnStatement(t *testing.T) {
	cases := []struct {
		eventType  string
		object     func(customerID string) string
		wantStatus string
	}{
		{"invoice.payment_succeeded", invoiceObject, StatusActive},
		{"invoice.payment_failed", invoiceObject, StatusGracePeriod},
		{"customer.subscription.deleted", subscriptionObject, StatusSuspended},
	}

	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			customerID := "cus_unit_" + strings.ReplaceAll(tc.eventType, ".", "_")
			db := &recordingDB{row: stubRow{tenantID: unitTenantID}}

			rec := deliver(newHandler(testConfig(testWebhookSecret), db),
				webhookRequest(eventBody(tc.eventType, tc.object(customerID)), testWebhookSecret))

			requireReceived(t, rec)
			require.Len(t, db.queries, 1, "one delivery must run one statement")

			assert.Contains(t, db.queries[0], tenantsTable, "the status lives on the tenant registry")
			assert.Contains(t, db.queries[0], "status = '"+tc.wantStatus+"'")

			require.Len(t, db.args[0], 1, "the customer id is the only predicate")
			assert.Equal(t, customerID, db.args[0][0])
		})
	}
}

// TestWebhookAcknowledgesWhatItCannotActOn covers the two shapes of "valid signature,
// nothing to do": an event type this phase does not handle, and an event whose object
// carries no customer at all.
//
// Both are answered 200, which is what stops Stripe redelivering something no
// redelivery could change, and neither reaches the database.
func TestWebhookAcknowledgesWhatItCannotActOn(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "an event type this phase does not handle",
			body: eventBody("customer.created", `{"id":"cus_unit_created","object":"customer"}`),
		},
		{
			name: "a signed event whose object names no customer",
			body: eventBody("invoice.payment_failed", `{"id":"in_unit","object":"invoice"}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &recordingDB{row: stubRow{tenantID: unitTenantID}}

			rec := deliver(newHandler(testConfig(testWebhookSecret), db),
				webhookRequest(tc.body, testWebhookSecret))

			requireReceived(t, rec)
			assert.Empty(t, db.queries, "there is nothing to write for either shape")
		})
	}
}

// TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently is the
// retry contract, which is the one thing about this endpoint's error handling that
// has operational consequences: a delivery that cannot be applied must come back,
// and one that would never find a row must not.
func TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently(t *testing.T) {
	body := eventBody("invoice.payment_failed", invoiceObject("cus_unit_missing"))

	t.Run("no tenant claims the customer", func(t *testing.T) {
		db := &recordingDB{row: stubRow{err: pgx.ErrNoRows}}

		rec := deliver(newHandler(testConfig(testWebhookSecret), db),
			webhookRequest(body, testWebhookSecret))

		// Acknowledged: no number of redeliveries creates the row, so retrying
		// would only fill Stripe's delivery log with failures.
		requireReceived(t, rec)
		require.Len(t, db.queries, 1, "the statement was attempted and matched nothing")
	})

	t.Run("the database refuses the write", func(t *testing.T) {
		db := &recordingDB{row: stubRow{err: errors.New("connection refused")}}

		rec := deliver(newHandler(testConfig(testWebhookSecret), db),
			webhookRequest(body, testWebhookSecret))

		// 500, so Stripe redelivers: the event is the only copy of the fact.
		require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
		require.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	})

	t.Run("a plane with no database at all", func(t *testing.T) {
		rec := deliver(newHandler(testConfig(testWebhookSecret), nil),
			webhookRequest(body, testWebhookSecret))

		require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
		require.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	})
}
