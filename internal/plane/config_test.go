package plane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleConfig exercises every key with a distinct value so a mis-wired yaml
// tag shows up as a wrong assertion rather than a silent zero value.
const sampleConfig = `listen-addr: "127.0.0.1:9191"
database-dsn: "postgres://from-file/db"
jwt-secret: "0123456789abcdef0123456789abcdef"
admin-token: "admin-from-file"
master-key: "master-from-file"
log-level: "debug"
ledger-retention-days: 30
`

// clearSecretEnv unsets the four secret environment variables for the duration
// of a test and restores whatever was there afterwards, so a developer's
// exported values cannot change the outcome. t.Setenv cannot unset a variable,
// hence the manual save/restore.
func clearSecretEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{EnvDatabaseDSN, EnvJWTSecret, EnvAdminToken, EnvMasterKey} {
		old, had := os.LookupEnv(name)
		require.NoError(t, os.Unsetenv(name))
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(name, old)
				return
			}
			_ = os.Unsetenv(name)
		})
	}
}

// writeConfig writes body to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "synapse-plane.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

// validConfig is a baseline that passes Validate, for tests that only want to
// vary one field at a time.
func validConfig() *PlaneConfig {
	return &PlaneConfig{
		ListenAddr:          "127.0.0.1:9090",
		DatabaseDSN:         "postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable",
		JWTSecret:           strings.Repeat("k", MinJWTSecretLen),
		LogLevel:            "info",
		LedgerRetentionDays: 365,
	}
}

func TestDefaultConfigDefaults(t *testing.T) {
	clearSecretEnv(t)

	cfg := DefaultConfig()

	assert.Equal(t, "127.0.0.1:9090", cfg.ListenAddr)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, 365, cfg.LedgerRetentionDays)
	assert.Empty(t, cfg.DatabaseDSN)
	assert.Empty(t, cfg.JWTSecret)
	assert.Empty(t, cfg.AdminToken)
	assert.Empty(t, cfg.MasterKey)
}

func TestDefaultConfigPicksUpEnvironment(t *testing.T) {
	clearSecretEnv(t)
	t.Setenv(EnvDatabaseDSN, "postgres://from-env/db")
	t.Setenv(EnvJWTSecret, "env-secret-0123456789abcdef0123456789")

	cfg := DefaultConfig()

	assert.Equal(t, "postgres://from-env/db", cfg.DatabaseDSN)
	assert.Equal(t, "env-secret-0123456789abcdef0123456789", cfg.JWTSecret)
}

func TestLoadConfigFromFile(t *testing.T) {
	clearSecretEnv(t)

	cfg, err := LoadConfig(writeConfig(t, sampleConfig))
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1:9191", cfg.ListenAddr)
	assert.Equal(t, "postgres://from-file/db", cfg.DatabaseDSN)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", cfg.JWTSecret)
	assert.Equal(t, "admin-from-file", cfg.AdminToken)
	assert.Equal(t, "master-from-file", cfg.MasterKey)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, 30, cfg.LedgerRetentionDays)
	assert.NoError(t, cfg.Validate())
}

func TestLoadConfigMissingFileUsesDefaults(t *testing.T) {
	clearSecretEnv(t)

	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err)

	assert.Equal(t, DefaultListenAddr, cfg.ListenAddr)
	assert.Equal(t, DefaultLogLevel, cfg.LogLevel)
	assert.Equal(t, DefaultLedgerRetentionDays, cfg.LedgerRetentionDays)
	assert.Empty(t, cfg.JWTSecret)
}

func TestLoadConfigBlankValuesFallBackToDefaults(t *testing.T) {
	clearSecretEnv(t)

	cfg, err := LoadConfig(writeConfig(t, "listen-addr: \"\"\nlog-level: \"\"\nledger-retention-days: 0\n"))
	require.NoError(t, err)

	assert.Equal(t, DefaultListenAddr, cfg.ListenAddr)
	assert.Equal(t, DefaultLogLevel, cfg.LogLevel)
	assert.Equal(t, DefaultLedgerRetentionDays, cfg.LedgerRetentionDays)
}

func TestLoadConfigEnvironmentWinsOverFile(t *testing.T) {
	clearSecretEnv(t)

	// Set after writing the file: LoadConfig reads the environment itself.
	t.Setenv(EnvDatabaseDSN, "postgres://from-env/db")
	t.Setenv(EnvJWTSecret, "env-secret-0123456789abcdef0123456789")
	t.Setenv(EnvAdminToken, "admin-from-env")
	t.Setenv(EnvMasterKey, "master-from-env")

	cfg, err := LoadConfig(writeConfig(t, sampleConfig))
	require.NoError(t, err)

	assert.Equal(t, "postgres://from-env/db", cfg.DatabaseDSN)
	assert.Equal(t, "env-secret-0123456789abcdef0123456789", cfg.JWTSecret)
	assert.Equal(t, "admin-from-env", cfg.AdminToken)
	assert.Equal(t, "master-from-env", cfg.MasterKey)
	// Non-secret keys are untouched by the environment.
	assert.Equal(t, "127.0.0.1:9191", cfg.ListenAddr)
}

func TestLoadConfigEmptyEnvironmentDoesNotBlankFileValue(t *testing.T) {
	clearSecretEnv(t)
	t.Setenv(EnvJWTSecret, "")

	cfg, err := LoadConfig(writeConfig(t, sampleConfig))
	require.NoError(t, err)

	assert.Equal(t, "0123456789abcdef0123456789abcdef", cfg.JWTSecret)
}

func TestLoadConfigInvalidYAMLFails(t *testing.T) {
	clearSecretEnv(t)

	_, err := LoadConfig(writeConfig(t, "listen-addr: [unterminated\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plane: parse config")
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*PlaneConfig)
		wantError string
	}{
		{name: "valid config", mutate: func(*PlaneConfig) {}},
		{name: "localhost accepted", mutate: func(c *PlaneConfig) { c.ListenAddr = "localhost:9090" }},
		{name: "ipv6 loopback accepted", mutate: func(c *PlaneConfig) { c.ListenAddr = "[::1]:9090" }},
		{name: "zero retention allowed", mutate: func(c *PlaneConfig) { c.LedgerRetentionDays = 0 }},

		{name: "empty listen-addr", mutate: func(c *PlaneConfig) { c.ListenAddr = "" }, wantError: "listen-addr is required"},
		{name: "wildcard bind rejected", mutate: func(c *PlaneConfig) { c.ListenAddr = "0.0.0.0:9090" }, wantError: "must be loopback"},
		{name: "bare port bind rejected", mutate: func(c *PlaneConfig) { c.ListenAddr = ":9090" }, wantError: "must be loopback"},
		{name: "lan bind rejected", mutate: func(c *PlaneConfig) { c.ListenAddr = "192.168.1.10:9090" }, wantError: "must be loopback"},
		{name: "missing port rejected", mutate: func(c *PlaneConfig) { c.ListenAddr = "127.0.0.1" }, wantError: "must be loopback"},
		{name: "missing database-dsn", mutate: func(c *PlaneConfig) { c.DatabaseDSN = "" }, wantError: "database-dsn is required"},
		{name: "missing jwt-secret", mutate: func(c *PlaneConfig) { c.JWTSecret = "" }, wantError: "jwt-secret is required"},
		{name: "short jwt-secret", mutate: func(c *PlaneConfig) { c.JWTSecret = strings.Repeat("k", MinJWTSecretLen-1) }, wantError: "jwt-secret is too short"},
		{name: "negative retention", mutate: func(c *PlaneConfig) { c.LedgerRetentionDays = -1 }, wantError: "ledger-retention-days must not be negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)

			err := cfg.Validate()
			if tt.wantError == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}

func TestValidateErrorsNeverEchoSecretValues(t *testing.T) {
	const (
		secret = "super-secret-signing-key-0123456789"
		dsn    = "postgres://user:sup3rs3cr3t@db.internal:5432/synapse"
	)

	// Accepted secret but missing DSN: the error must not leak the secret that
	// was accepted, nor any part of the DSN.
	cfg := validConfig()
	cfg.JWTSecret = secret
	cfg.DatabaseDSN = ""

	err := cfg.Validate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), dsn)
	assert.NotContains(t, err.Error(), "sup3rs3cr3t")

	// Rejected secret: the error must not echo the value that failed.
	short := "too-short-secret"
	cfg = validConfig()
	cfg.JWTSecret = short

	err = cfg.Validate()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), short)
}
