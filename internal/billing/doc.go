// Package billing owns the money-driven half of a tenant's lifecycle: the three
// Stripe webhook events that move synapse_global.tenants.status between active,
// grace_period, and suspended.
//
// It writes exactly one column family on exactly one table, and it reads nothing
// back. What the status means is deliberately not this package's business: it
// records the fact Stripe delivered, and a later phase is what refuses to serve a
// suspended tenant. Until something reads it, a suspended tenant keeps working --
// that is a known gap of this phase rather than an oversight of this package,
// which has no request-serving path to hang enforcement on.
//
// Four decisions are structural rather than conventional:
//
//   - Validation comes first and it is the only authentication. The route carries
//     no token and no JWT middleware, because Stripe cannot hold one; the
//     Stripe-Signature header, checked against PlaneConfig.StripeWebhookSecret,
//     is the whole credential. A request that fails it is answered 400 and
//     reaches no SQL at all, so "the signature protects the database" is a
//     property of the control flow rather than of a check someone could move.
//   - Neither the signature header, nor the body, nor the signing secret is ever
//     logged -- on any path, including the failure paths that exist to be
//     recorded. stripe-go's error text is not logged either: it is not
//     contractually free of the header it was handed, so the rejection line
//     names the event and nothing about the request.
//   - The database is one narrow interface (rowQuerier) rather than a concrete
//     pool. That is what lets the rejecting paths be tested without a database,
//     where the assertion is "no tenant row changed" -- which a nil pool cannot
//     express -- while *pgxpool.Pool stays the only implementation production
//     uses.
//   - An event this package does not act on is acknowledged with 200, not
//     refused. Stripe retries non-2xx deliveries for up to three days and then
//     drops the event, so answering 4xx to a valid event this phase simply does
//     not handle would turn "not implemented yet" into a redelivery storm.
//
// This file is the package comment alone, which is where it lives when the
// implementation file would otherwise cross the 300-line ceiling every file in
// this project is held to -- the same arrangement internal/ledger/doc.go makes.
package billing
