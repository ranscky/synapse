package plane

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redactedMap turns RedactedFields' flat key/value slice into a map so a test
// can assert on key/value pairing instead of on concatenated output.
func redactedMap(t *testing.T, cfg *PlaneConfig) map[string]any {
	t.Helper()

	fields := cfg.RedactedFields()
	require.Zero(t, len(fields)%2, "RedactedFields must return key/value pairs")

	out := make(map[string]any, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		key, ok := fields[i].(string)
		require.Truef(t, ok, "field key at index %d must be a string", i)
		out[key] = fields[i+1]
	}

	return out
}

func TestRedactedFieldsHidesSecretValues(t *testing.T) {
	cfg := validConfig()
	cfg.DatabaseDSN = "postgres://user:dbpass@127.0.0.1:5432/synapse"
	cfg.JWTSecret = "jwt-value-that-must-not-be-logged"
	cfg.AdminToken = "admin-value-that-must-not-be-logged"
	cfg.MasterKey = "master-value-that-must-not-be-logged"
	cfg.StripeWebhookSecret = "whsec-value-that-must-not-be-logged"

	fields := redactedMap(t, cfg)

	// The configured state is visible...
	assert.Equal(t, "set", fields["database_dsn"])
	assert.Equal(t, "set", fields["jwt_secret"])
	assert.Equal(t, "set", fields["admin_token"])
	assert.Equal(t, "set", fields["master_key"])
	assert.Equal(t, "set", fields["stripe_webhook_secret"])

	// ...while non-secret keys keep their real values.
	assert.Equal(t, cfg.ListenAddr, fields["listen_addr"])
	assert.Equal(t, cfg.LogLevel, fields["log_level"])
	assert.Equal(t, cfg.LedgerRetentionDays, fields["ledger_retention_days"])

	// And nothing anywhere in the rendered fields leaks a secret value.
	rendered := fmt.Sprint(cfg.RedactedFields()...)
	assert.NotContains(t, rendered, cfg.DatabaseDSN)
	assert.NotContains(t, rendered, cfg.JWTSecret)
	assert.NotContains(t, rendered, cfg.AdminToken)
	assert.NotContains(t, rendered, cfg.MasterKey)
	assert.NotContains(t, rendered, cfg.StripeWebhookSecret)
	assert.NotContains(t, rendered, "whsec")
	assert.NotContains(t, rendered, "dbpass")
}

func TestRedactedFieldsReportsUnsetSecrets(t *testing.T) {
	cfg := &PlaneConfig{ListenAddr: DefaultListenAddr, LogLevel: DefaultLogLevel, LedgerRetentionDays: DefaultLedgerRetentionDays}

	fields := redactedMap(t, cfg)

	assert.Equal(t, "unset", fields["database_dsn"])
	assert.Equal(t, "unset", fields["jwt_secret"])
	assert.Equal(t, "unset", fields["admin_token"])
	assert.Equal(t, "unset", fields["master_key"])
	assert.Equal(t, "unset", fields["stripe_webhook_secret"])
}

func TestUnsafePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.FileMode permissions are synthesized on Windows, so the check is not applicable")
	}

	path := writeConfig(t, "log-level: info\n")

	require.NoError(t, os.Chmod(path, 0o600))
	assert.Empty(t, UnsafePermissions(path))

	require.NoError(t, os.Chmod(path, 0o640))
	assert.Equal(t, "0640", UnsafePermissions(path))

	require.NoError(t, os.Chmod(path, 0o644))
	assert.Equal(t, "0644", UnsafePermissions(path))

	assert.Empty(t, UnsafePermissions(filepath.Join(t.TempDir(), "absent.yaml")))
}
