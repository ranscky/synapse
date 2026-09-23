// Phase 28: the billing-status gate every tenant surface sits behind.
//
// A tenant's subscription state is written by the Stripe webhook (Phase 27) into
// synapse_global.tenants.status, and this file is where it is enforced: a
// suspended tenant does not reach a paid endpoint at all, a tenant in its grace
// period is served but told, in a response header, how many days it has left.
//
// The middleware is exported and constructor-shaped -- RequireActiveStatus(pool)
// -- so the route table can install it (see Server.Routes) and cmd/plane can
// build it from the one pool the plane opens. It is deliberately not a method on
// Server: its whole state is the pool, and keeping it that way is what lets it be
// read as one function.
//
// Two spellings in this file are forced by the import graph and are called out
// rather than hidden:
//
//   - The schema-qualified table name. internal/tenant owns SchemaName, but
//     internal/tenant imports this package for PlaneConfig, so this package
//     cannot import it back. "synapse_global" is a constant there and a literal
//     here, and this is the only place in plane that writes it.
//   - The status values. internal/billing owns the vocabulary it writes, but
//     internal/billing imports internal/tenant (and therefore this package), so
//     the cycle runs the same way. The three values are exported from here and
//     internal/billing's own constants alias them, so the vocabulary still has
//     exactly one spelling between the package that writes it and the package
//     that enforces it.
package plane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// tenantStatusTimeout bounds the gate's one status SELECT. It is short on
	// purpose: the query is one indexed single-row read, and a status lookup that
	// hangs is a request that hangs behind it.
	tenantStatusTimeout = 2 * time.Second

	// gracePeriodDays is how long internal/billing's grace_period status buys a
	// tenant before a subscription deletion suspends it. The webhook only stamps
	// the start time; the window is a property of the enforcement decision, which
	// is why it lives here.
	gracePeriodDays = 7

	// gracePeriodHeader is the response header a tenant in its grace period is
	// warned with. Its value is "{days}days", so a client does not have to know
	// the window to render it.
	gracePeriodHeader = "X-Synapse-Grace-Period"

	// upgradeURL is where the 402 sends a suspended tenant. Fixed rather than
	// configured: it is the published pricing page, not a deployment detail.
	upgradeURL = "https://synapse.ai/pricing"

	// tenantsTable is the row this gate reads, schema-qualified. The schema name
	// is spelled out because of the import cycle described in the file comment
	// above.
	tenantsTable = "synapse_global.tenants"

	// tenantStatusQuery reads both columns the gate needs in one round trip: the
	// status it decides on, and the grace period's start time, which is only
	// meaningful when the status is grace_period. The id arrives as a string --
	// the value internal/tenant verified out of the token -- so it is cast.
	tenantStatusQuery = `SELECT status, grace_period_started_at FROM ` + tenantsTable + ` WHERE id = $1::uuid`
)

// Tenant status values, as stored in synapse_global.tenants.status. This package
// is where they are enforced; internal/billing is where they are written, and its
// StatusActive/StatusGracePeriod/StatusSuspended alias these, so a typo cannot
// make the two halves disagree.
const (
	// TenantStatusActive is a tenant in good standing: nothing to enforce.
	TenantStatusActive = "active"
	// TenantStatusGracePeriod is a tenant whose payment failed but whose access
	// has not been cut off yet.
	TenantStatusGracePeriod = "grace_period"
	// TenantStatusSuspended is a tenant whose subscription was deleted.
	TenantStatusSuspended = "suspended"
)

// paymentRequiredResponse is the only 402 body this package writes. It is a shape
// of its own rather than errorResponse because a client needs somewhere to go:
// the machine-readable reason, and the pricing page that fixes it.
type paymentRequiredResponse struct {
	Error      string `json:"error"`
	UpgradeURL string `json:"upgrade_url"`
}

// RequireActiveStatus returns middleware that refuses a suspended tenant and warns
// a tenant in its grace period.
//
// The tenant it acts on is the one TenantIDFromCtx returns -- the value
// internal/tenant's JWT middleware parked after verifying the signature, the
// expiry, and the tenant claim -- so this gate must be installed inside
// requireJWT, and an empty id (a request that never passed through it) is a
// rejection rather than a lookup of nothing. Nothing here reads a header, a body,
// a query parameter, or an API key: a caller cannot name a status it does not
// have.
//
// The decisions, in the order they are made:
//
//   - No verified tenant id: 401 with this package's generic unauthorized body,
//     the same fail-closed shape the handlers use for the same impossible case.
//   - A tenant the registry does not know (the token is valid, the row is gone):
//     401. A credential naming a tenant that no longer exists must not buy
//     anything.
//   - A status that could not be read: 500 internal. This is the one rule that
//     must not bend -- a database that cannot answer the question must not let a
//     suspended tenant through, and the pgx error (which can quote the connection
//     target) is never reflected to the client.
//   - suspended: 402 payment_required with the upgrade URL, and next is never
//     called.
//   - grace_period: the X-Synapse-Grace-Period header is set, and the request is
//     served normally.
//   - active, or any value this phase does not know: served normally. Unknown
//     values are not refusals here; refusing a tenant for a status written by a
//     later phase would be a coupling this gate has no business having.
//
// A nil pool installs no gate and the request is passed through: a plane with no
// database handle has no status to read, and cmd/plane refuses to boot without a
// DSN, so this is reachable only from a test that injects its own dependencies.
func RequireActiveStatus(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if pool == nil {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID := TenantIDFromCtx(r.Context())
			if tenantID == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}

			ctx, cancel := context.WithTimeout(r.Context(), tenantStatusTimeout)
			defer cancel()

			var (
				status  string
				started *time.Time
			)

			err := pool.QueryRow(ctx, tenantStatusQuery, tenantID).Scan(&status, &started)

			switch {
			case errors.Is(err, pgx.ErrNoRows):
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			case err != nil:
				writeError(w, http.StatusInternalServerError, "internal")
				return
			}

			if status == TenantStatusSuspended {
				writeJSON(w, http.StatusPaymentRequired, paymentRequiredResponse{
					Error:      "payment_required",
					UpgradeURL: upgradeURL,
				})
				return
			}

			if status == TenantStatusGracePeriod {
				w.Header().Set(gracePeriodHeader, fmt.Sprintf("%ddays", remainingGraceDays(started, time.Now())))
			}

			next.ServeHTTP(w, r)
		})
	}
}

// remainingGraceDays returns how much of gracePeriodDays is left, given the
// timestamp internal/billing stamped when the tenant entered the grace period.
//
// A nil stamp counts as zero elapsed days, which reports the full window: the
// only way a grace_period row carries no timestamp is one written before the
// column existed, and answering "0 days left" for it would cut access off on the
// strength of a missing value. A stamp in the future is treated the same way, and
// the result is floored at zero, so a window that has run out reads "0days"
// rather than a negative number. Billing suspends a tenant whose subscription is
// deleted; until it does, the header counts down.
func remainingGraceDays(started *time.Time, now time.Time) int {
	if started == nil {
		return gracePeriodDays
	}

	elapsed := int(now.Sub(*started).Hours() / 24)
	if elapsed < 0 {
		elapsed = 0
	}

	if elapsed >= gracePeriodDays {
		return 0
	}

	return gracePeriodDays - elapsed
}
