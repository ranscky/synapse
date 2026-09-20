package tenant

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// apiKeyEntropyBytes is the number of random bytes behind an issued tenant API
// key: 256 bits, the same strength as the HMAC signing key the plane uses.
const apiKeyEntropyBytes = 32

// GenerateAPIKey returns a fresh tenant API key: 32 bytes from crypto/rand,
// hex-encoded into a 64-character string.
//
// The plaintext is handed to the caller once and never persisted or logged --
// only HashAPIKey's output reaches synapse_global.tenant_keys.
func GenerateAPIKey() (string, error) {
	buf := make([]byte, apiKeyEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("tenant: read random bytes for api key: %w", err)
	}

	return hex.EncodeToString(buf), nil
}

// HashAPIKey returns the bcrypt hash of key, in the form stored in
// synapse_global.tenant_keys.key_hash.
//
// bcrypt rather than a plain digest because an API key is a credential a human
// pastes around: salted and deliberately slow is what keeps a leaked hash from
// being worth attacking offline.
func HashAPIKey(key string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("tenant: hash api key: %w", err)
	}

	return string(hash), nil
}

// VerifyAPIKey reports whether key is the credential behind hash. It is the only
// supported way to check a presented key: bcrypt.CompareHashAndPassword is
// constant-time with respect to the hash, which is why the comparison lives here
// and not at a call site.
func VerifyAPIKey(hash, key string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(key)) == nil
}
