// The two response bodies this package writes, and the two writers that emit
// them.
//
// Split out of stripe.go for the 300-line ceiling this project holds every file
// to, along the line the plane's own handlers.go draws: the endpoint's logic is
// in stripe.go, and the wire shapes every endpoint in a package shares are here.
// The shapes are deliberately the control plane's: {"received":true} for an
// accepted delivery and {"error":"reason"} for a rejected one, with no detail
// beyond a machine-readable reason -- a Stripe delivery log is not a place to
// publish why a signature failed or which tenant a customer id belonged to.
package billing

import (
	"encoding/json"
	"net/http"
)

// receivedResponse is the 200 body: the one thing Stripe needs to know, which is
// that the delivery was accepted. Everything else about the event is either in
// the log or in the tenant row.
type receivedResponse struct {
	Received bool `json:"received"`
}

// errorResponse is the only error body this package writes: a machine-readable
// reason and nothing else. Neither the signature, nor the body, nor the signing
// secret is ever reflected to a client.
type errorResponse struct {
	Error string `json:"error"`
}

// writeError writes the single error body shape this package uses.
func writeError(w http.ResponseWriter, code int, reason string) {
	writeJSON(w, code, errorResponse{Error: reason})
}

// writeJSON marshals body and writes it with the given status code. A marshal
// failure cannot happen for these fixed shapes, but it is handled rather than
// ignored, so a client always gets valid JSON.
func writeJSON(w http.ResponseWriter, code int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	// A failed write means the client went away; there is nothing useful to do
	// about it on either the success or the rejection path.
	_, _ = w.Write(payload)
}
