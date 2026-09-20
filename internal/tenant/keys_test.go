package tenant

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hexKeyPattern is the shape of an issued API key: 32 random bytes, hex-encoded.
var hexKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestGenerateAPIKeyIs32RandomBytesHexEncoded(t *testing.T) {
	first, err := GenerateAPIKey()
	require.NoError(t, err)
	assert.Regexp(t, hexKeyPattern, first)

	second, err := GenerateAPIKey()
	require.NoError(t, err)
	assert.Regexp(t, hexKeyPattern, second)

	assert.NotEqual(t, first, second, "two issued keys must never collide")
}

func TestHashAPIKeyNeverStoresTheKey(t *testing.T) {
	key, err := GenerateAPIKey()
	require.NoError(t, err)

	hash, err := HashAPIKey(key)
	require.NoError(t, err)

	assert.NotEqual(t, key, hash)
	assert.NotContains(t, hash, key)

	assert.True(t, VerifyAPIKey(hash, key))
	assert.False(t, VerifyAPIKey(hash, key+"0"), "a near-miss key must not verify")
	assert.False(t, VerifyAPIKey(hash, ""))
	assert.False(t, VerifyAPIKey("", key), "an empty stored hash must never verify")
}

func TestHashAPIKeySaltsEveryHash(t *testing.T) {
	key, err := GenerateAPIKey()
	require.NoError(t, err)

	first, err := HashAPIKey(key)
	require.NoError(t, err)

	second, err := HashAPIKey(key)
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "bcrypt must salt every hash")
	assert.True(t, VerifyAPIKey(first, key))
	assert.True(t, VerifyAPIKey(second, key))
}
