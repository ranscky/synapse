// The billing webhook route: POST /v2/billing/webhook, the one route Stripe calls
// into this control plane directly.
//
// Split out of handlers.go for the reason the sync, search, and compliance
// endpoints live in their own files: handlers.go holds the server, its routes, and
// the shared HTTP helpers, and every file in this project is held to the 300-line
// ceiling. This is the smallest of them -- one dependency, no request body this
// package parses, and a response that is an acknowledgement and nothing else.
//
// The route is deliberately open. Every other write surface here is behind a
// tenant token or the admin token and this one cannot be, because Stripe is not a
// tenant and holds no Synapse credential. Its authentication is the
// Stripe-Signature header, validated inside internal/billing against the signing
// secret the webhook endpoint was created with.
//
// The handler arrives as an injected http.HandlerFunc rather than as an import,
// and that is a property of the import graph: internal/billing reads the schema
// name from internal/tenant, internal/tenant imports this package for
// PlaneConfig, so plane -> billing would be a cycle. cmd/plane is the only place
// that can see both, and it wires them, exactly as it does for the ledger
// verifier and the compliance auditor.
package plane

import "net/http"

// billingWebhookRoute is where Stripe POSTs subscription and invoice events.
const billingWebhookRoute = "/v2/billing/webhook"

// handleBillingWebhook delegates every delivery to the injected billing handler.
//
// A plane constructed without one answers 503 instead of dropping the route: an
// endpoint that exists and refuses is a better failure than a 404, because a 404
// tells Stripe the endpoint is gone, and a deployment that never configured
// billing is not the same thing as one that removed the webhook. The error body
// is this package's generic shape and reveals nothing beyond the dependency's
// name.
func (s *Server) handleBillingWebhook(w http.ResponseWriter, r *http.Request) {
	if s.billingWebhook == nil {
		writeError(w, http.StatusServiceUnavailable, "billing_unavailable")
		return
	}

	s.billingWebhook(w, r)
}
