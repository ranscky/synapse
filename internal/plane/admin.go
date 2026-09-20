package plane

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// requireAdmin returns middleware that answers 401 unless the request's
// Authorization header carries exactly the configured admin token.
//
// The admin token is a separate credential from a tenant JWT (see
// internal/tenant.JWTMiddleware) and on purpose: a tenant token must never be
// able to provision tenants.
//
// Fail-closed is the whole design. An unconfigured AdminToken, an absent header,
// and a wrong value are indistinguishable from outside -- one 401 body for all of
// them -- so the endpoint cannot be used as an oracle, and a plane started
// without an admin token simply has an unusable route instead of an open one.
//
// Comparison is constant-time over SHA-256 digests of both sides, which also
// keeps the timing independent of the token's length. Nothing about the presented
// value, the expected value, or the client is ever logged.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.adminTokenMatches(r.Header.Get("Authorization")) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// adminTokenMatches reports whether presented is the configured admin token.
//
// A blank configured token never matches, so a plane with no admin token
// configured has no route an attacker can guess their way into.
func (s *Server) adminTokenMatches(presented string) bool {
	if s.cfg == nil || s.cfg.AdminToken == "" || presented == "" {
		return false
	}

	presentedSum := sha256.Sum256([]byte(presented))
	expectedSum := sha256.Sum256([]byte(s.cfg.AdminToken))

	return subtle.ConstantTimeCompare(presentedSum[:], expectedSum[:]) == 1
}
