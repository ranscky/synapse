package tenant

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"synapse/internal/plane"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretsTestMasterKey is 64 hex characters, i.e. the 32 bytes
// masterKeyFromEnv accepts. Test material, and never a key any deployment uses.
const secretsTestMasterKey = "8f3a1c5e7b9d2043658f3a1c5e7b9d2043658f3a1c5e7b9d2043658f3a1c5e7b"

// clearMasterKeyEnv unsets SYNAPSE_MASTER_KEY for the duration of a test and
// restores whatever was there afterwards, so a developer's exported value cannot
// change the outcome. t.Setenv cannot unset a variable, hence the manual
// save/restore -- the shape internal/plane's config tests already use.
func clearMasterKeyEnv(t *testing.T) {
	t.Helper()

	old, had := os.LookupEnv(plane.EnvMasterKey)
	require.NoError(t, os.Unsetenv(plane.EnvMasterKey))

	t.Cleanup(func() {
		if had {
			_ = os.Setenv(plane.EnvMasterKey, old)
			return
		}
		_ = os.Unsetenv(plane.EnvMasterKey)
	})
}

// secretsTestKey decodes the test master key, so every test below starts from the
// same 32 bytes the environment would have produced.
func secretsTestKey(t *testing.T) []byte {
	t.Helper()

	key, err := hex.DecodeString(secretsTestMasterKey)
	require.NoError(t, err)
	require.Len(t, key, masterKeyBytes)

	return key
}

// TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue walks the shapes a deployment
// can get wrong. The assertion that matters beyond the error itself is the last
// one: none of these messages may contain the value that failed, because this
// string can reach a log line.
func TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "accepted", value: secretsTestMasterKey},
		{name: "empty", value: "", wantErr: plane.EnvMasterKey + " is required"},
		{name: "not hex", value: "zz" + strings.Repeat("0", 62), wantErr: "must be hex-encoded"},
		{name: "too short", value: strings.Repeat("ab", 16), wantErr: "must decode to 32 bytes"},
		{name: "too long", value: strings.Repeat("ab", 33), wantErr: "must decode to 32 bytes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearMasterKeyEnv(t)
			t.Setenv(plane.EnvMasterKey, tt.value)

			key, err := masterKeyFromEnv()

			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Len(t, key, masterKeyBytes)
				assert.Equal(t, tt.value, hex.EncodeToString(key), "the key must be the hex-decoded value")
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)

			// The unset case has no value to leak, so the control only applies to
			// the values this test actually supplied.
			if tt.value != "" {
				assert.NotContains(t, err.Error(), tt.value, "a rejected key must never be echoed")
			}
		})
	}
}

func TestSealSecretRoundTripsAndHidesThePlaintext(t *testing.T) {
	key := secretsTestKey(t)
	tenantID := uuid.NewString()
	raw := []byte("0123456789abcdef0123456789abcdef")

	stored, err := sealSecret(key, tenantID, raw)
	require.NoError(t, err)
	require.NotEmpty(t, stored)

	// The row holds an envelope, not the key: neither encoding of the plaintext
	// appears in what would be written to the database.
	assert.NotContains(t, stored, hex.EncodeToString(raw))
	assert.NotContains(t, stored, base64.StdEncoding.EncodeToString(raw))

	opened, err := openSecret(key, tenantID, stored)
	require.NoError(t, err)
	assert.Equal(t, raw, opened)
}

func TestSealSecretUsesAFreshNonce(t *testing.T) {
	key := secretsTestKey(t)
	tenantID := uuid.NewString()
	raw := []byte("0123456789abcdef0123456789abcdef")

	first, err := sealSecret(key, tenantID, raw)
	require.NoError(t, err)
	second, err := sealSecret(key, tenantID, raw)
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "a repeated nonce under one key is the failure GCM cannot survive")
}

func TestOpenSecretRejectsAnythingButTheOriginalCiphertext(t *testing.T) {
	key := secretsTestKey(t)
	tenantID := uuid.NewString()
	raw := []byte("0123456789abcdef0123456789abcdef")

	stored, err := sealSecret(key, tenantID, raw)
	require.NoError(t, err)

	t.Run("another tenant", func(t *testing.T) {
		_, err := openSecret(key, uuid.NewString(), stored)
		require.Error(t, err, "the tenant id is authenticated as additional data")
	})

	t.Run("another master key", func(t *testing.T) {
		other, err := hex.DecodeString(strings.Repeat("cd", masterKeyBytes))
		require.NoError(t, err)

		_, err = openSecret(other, tenantID, stored)
		require.Error(t, err)
	})

	t.Run("altered ciphertext", func(t *testing.T) {
		envelope, err := base64.StdEncoding.DecodeString(stored)
		require.NoError(t, err)

		envelope[len(envelope)-1] ^= 0x01

		_, err = openSecret(key, tenantID, base64.StdEncoding.EncodeToString(envelope))
		require.Error(t, err)
	})

	t.Run("not base64", func(t *testing.T) {
		_, err := openSecret(key, tenantID, "not base64 at all")
		require.Error(t, err)
	})

	t.Run("too short for a nonce", func(t *testing.T) {
		_, err := openSecret(key, tenantID, base64.StdEncoding.EncodeToString([]byte{1, 2, 3}))
		require.Error(t, err)
	})
}

// TestCanonicalTenantIDMatchesWhatPostgresStores is what makes the tenant id
// usable as additional authenticated data: the same tenant must canonicalize to
// one string however the caller cased it, or a ciphertext written by one caller
// would not open for the next.
func TestCanonicalTenantIDMatchesWhatPostgresStores(t *testing.T) {
	id := uuid.NewString()

	canonical, err := canonicalTenantID(strings.ToUpper(id))
	require.NoError(t, err)
	assert.Equal(t, id, canonical)

	for _, bad := range []string{"", "not-a-uuid", "1234"} {
		_, err := canonicalTenantID(bad)
		require.Error(t, err, "%q must not parse as a tenant id", bad)
	}
}
