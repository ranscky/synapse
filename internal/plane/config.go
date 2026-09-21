// Package plane holds the Synapse v2 control plane's internals.
//
// Phase 3 scope: configuration loading, validation, secret-safe helpers, the
// tenant provisioning HTTP contract, the admin-token middleware, and the
// /health and /v2/tenants handlers. The package depends on interfaces it
// declares itself (Database, TenantProvisioner) rather than on a database
// handle, so every route is testable without PostgreSQL. The tenant
// implementation lives in internal/tenant, which depends on this package for
// PlaneConfig -- the reverse import would be a cycle.
package plane

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultListenAddr is the control plane's default bind address. It is
	// loopback-only on purpose: the plane is never exposed on 0.0.0.0 by
	// default, and Validate refuses to boot on a non-loopback address.
	DefaultListenAddr = "127.0.0.1:9090"

	// DefaultLogLevel is used when log-level is absent or blank.
	DefaultLogLevel = "info"

	// DefaultLedgerRetentionDays is used when ledger-retention-days is
	// absent, blank, or zero. The ledger table itself is append-only -- this
	// value only ever gates future archival/reporting work, it never causes
	// a row to be deleted.
	DefaultLedgerRetentionDays = 365

	// DefaultReportTemplatePath is the HTML template GET
	// /v2/compliance/report renders its PDF from, relative to the plane's
	// working directory (which is a deployment's own choice: the repo root for
	// a developer running ./plane, the image's working directory in a
	// container). It is a default rather than a requirement because the JSON
	// report needs no template at all: a plane that never serves PDFs should
	// not fail to boot over a file.
	DefaultReportTemplatePath = "ui/plane/compliance-report.html"

	// MinJWTSecretLen is the shortest accepted HMAC signing key. 32 bytes is
	// SHA-256's output size, i.e. the point below which the key rather than
	// the hash becomes the weak link.
	MinJWTSecretLen = 32
)

// Environment variables that override the four secret YAML keys. A non-empty
// variable always wins over the file value, which is what lets production
// inject secrets from the environment while a local dev YAML still works
// when nothing is exported.
const (
	EnvDatabaseDSN = "SYNAPSE_DB_DSN"
	EnvJWTSecret   = "SYNAPSE_JWT_SECRET"
	EnvAdminToken  = "SYNAPSE_ADMIN_TOKEN"
	EnvMasterKey   = "SYNAPSE_MASTER_KEY"
)

// PlaneConfig is the control plane's complete runtime configuration.
//
// The secret fields -- DatabaseDSN, JWTSecret, AdminToken, MasterKey -- must
// never be logged. Use RedactedFields for any log line that reports config
// state: it emits "set"/"unset" instead of the value.
type PlaneConfig struct {
	// ListenAddr is the plane's HTTP bind address; loopback only.
	ListenAddr string `yaml:"listen-addr"`
	// DatabaseDSN is the Postgres connection string. Secret: never logged.
	DatabaseDSN string `yaml:"database-dsn"`
	// JWTSecret signs issued JWTs. Secret: never logged.
	JWTSecret string `yaml:"jwt-secret"`
	// AdminToken guards admin endpoints. Secret: never logged.
	AdminToken string `yaml:"admin-token"`
	// MasterKey wraps per-tenant API keys at rest. Secret: never logged.
	MasterKey string `yaml:"master-key"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `yaml:"log-level"`
	// LedgerRetentionDays bounds ledger reporting history, not row lifetime.
	LedgerRetentionDays int `yaml:"ledger-retention-days"`
	// ReportTemplatePath is the HTML template the compliance report renders
	// its PDF from. Blank means DefaultReportTemplatePath. Not a secret, so it
	// is reported verbatim.
	ReportTemplatePath string `yaml:"report-template"`
}

// DefaultConfig returns the configuration the plane boots with when no YAML
// file is present, plus any secret supplied through the environment. Exported
// so callers and tests share one known-good baseline.
func DefaultConfig() *PlaneConfig {
	return &PlaneConfig{
		ListenAddr:          DefaultListenAddr,
		DatabaseDSN:         os.Getenv(EnvDatabaseDSN),
		JWTSecret:           os.Getenv(EnvJWTSecret),
		AdminToken:          os.Getenv(EnvAdminToken),
		MasterKey:           os.Getenv(EnvMasterKey),
		LogLevel:            DefaultLogLevel,
		LedgerRetentionDays: DefaultLedgerRetentionDays,
		ReportTemplatePath:  DefaultReportTemplatePath,
	}
}

// LoadConfig reads the plane YAML file at path, fills absent keys from the
// documented defaults, then lets the SYNAPSE_* environment variables override
// the four secret fields. Environment wins over file because production is
// expected to inject secrets from the environment, while a local dev YAML
// stays usable when nothing is exported.
//
// A missing file is not an error: defaults plus environment values are
// returned instead, matching the v1 loader's behavior. Validation is a
// separate step (see Validate) so callers control when a bad config becomes
// fatal -- LoadConfig itself never calls os.Exit.
func LoadConfig(path string) (*PlaneConfig, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No file at all is a legitimate deployment shape (env-only,
			// containers). Validate still runs in the caller, so an absent
			// DSN or JWT secret remains fatal.
			applyEnvOverrides(cfg)
			return cfg, nil
		}
		return nil, fmt.Errorf("plane: read config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("plane: parse config %s: %w", path, err)
	}

	// A blank scalar means "use the default": yaml.Unmarshal cannot tell an
	// absent key from a deliberately empty one, and an empty listen-addr,
	// log-level, or retention window is never a meaningful choice.
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = DefaultLogLevel
	}
	if cfg.LedgerRetentionDays == 0 {
		cfg.LedgerRetentionDays = DefaultLedgerRetentionDays
	}
	if cfg.ReportTemplatePath == "" {
		cfg.ReportTemplatePath = DefaultReportTemplatePath
	}

	applyEnvOverrides(cfg)

	return cfg, nil
}

// applyEnvOverrides lets the environment win over file values for the four
// secret fields. A variable that is unset OR empty leaves the file value in
// place, so an exported-but-blank variable can never silently disable a
// secret that is already configured on disk.
func applyEnvOverrides(cfg *PlaneConfig) {
	if v := os.Getenv(EnvDatabaseDSN); v != "" {
		cfg.DatabaseDSN = v
	}
	if v := os.Getenv(EnvJWTSecret); v != "" {
		cfg.JWTSecret = v
	}
	if v := os.Getenv(EnvAdminToken); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv(EnvMasterKey); v != "" {
		cfg.MasterKey = v
	}
}

// Validate reports the first configuration problem that must stop the plane
// from booting, or nil when the config is usable. Error messages name the
// offending key and never include its value: a validation failure is still a
// line in the log, so a rejected secret must not be echoed there.
//
// DatabaseDSN, JWTSecret, and MasterKey are fatal-by-design. AdminToken is
// deliberately not required yet: a plane without one refuses POST /v2/tenants
// with a 401 rather than provisioning anything, so the missing credential fails
// closed at the route that needs it.
//
// MasterKey became required in Phase 18, which is the phase Phase 1's note
// named: provisioning now mints each tenant's ledger signing secret, and that
// secret is AES-256-GCM-wrapped under this key. A plane booting without it would
// accept a provisioning request, commit the tenant row, and then fail to store
// the secret -- a half-provisioned tenant whose audit chain can never be written
// or verified. Refusing to boot is the cheaper failure, and it is the same
// reasoning that made jwt-secret fatal.
//
// The key's *shape* is deliberately not checked here. Hex-decoding it and
// demanding exactly 32 bytes is internal/tenant's rule (masterKeyFromEnv), next
// to the code that uses it; duplicating it in this package would be two rules to
// keep in step, which is how the two drift apart.
func (c *PlaneConfig) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("plane: listen-addr is required")
	}

	if !isLoopbackAddr(c.ListenAddr) {
		return fmt.Errorf("plane: listen-addr must be loopback (127.0.0.1, localhost, or ::1): binding the control plane to a public interface is not supported")
	}

	if c.DatabaseDSN == "" {
		return fmt.Errorf("plane: database-dsn is required (set it in the config file or %s)", EnvDatabaseDSN)
	}

	if c.JWTSecret == "" {
		return fmt.Errorf("plane: jwt-secret is required (set it in the config file or %s)", EnvJWTSecret)
	}

	if len(c.JWTSecret) < MinJWTSecretLen {
		return fmt.Errorf("plane: jwt-secret is too short, minimum %d characters required", MinJWTSecretLen)
	}

	if c.MasterKey == "" {
		return fmt.Errorf("plane: master-key is required to wrap tenant signing secrets (set it in the config file or %s)", EnvMasterKey)
	}

	if c.LedgerRetentionDays < 0 {
		return fmt.Errorf("plane: ledger-retention-days must not be negative")
	}

	return nil
}

// isLoopbackAddr reports whether listen-addr targets a loopback host. It uses
// net.SplitHostPort so "127.0.0.1:9090", "localhost:9090", and "[::1]:9090"
// are all recognized, while anything unparseable -- including the bare
// ":9090" form, which binds every interface -- is rejected rather than
// guessed at.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	switch host {
	case "localhost", "::1":
		return true
	}

	return strings.HasPrefix(host, "127.")
}

// RedactedFields returns charmbracelet/log key-value pairs describing the
// loaded configuration. Secret fields are reported as "set" or "unset" and
// their values are never included, which makes this the only safe way to log
// config state.
func (c *PlaneConfig) RedactedFields() []any {
	return []any{
		"listen_addr", c.ListenAddr,
		"database_dsn", secretState(c.DatabaseDSN),
		"jwt_secret", secretState(c.JWTSecret),
		"admin_token", secretState(c.AdminToken),
		"master_key", secretState(c.MasterKey),
		"log_level", c.LogLevel,
		"ledger_retention_days", c.LedgerRetentionDays,
		"report_template", c.ReportTemplatePath,
	}
}

// secretState reports whether a secret is configured without revealing it.
func secretState(secret string) string {
	if secret == "" {
		return "unset"
	}
	return "set"
}

// UnsafePermissions returns the octal permissions of the config file at path
// when it is readable by group or other, or "" when the permissions are
// acceptable or the file cannot be stat'd. Callers log the returned value --
// this package owns no logger, so the decision stays with the caller.
//
// Windows is exempt: os.FileMode permissions there are synthesized (0666 or
// 0444 from the read-only attribute), so the check would report every Windows
// config file as unsafe and `chmod 600` is not the fix on that platform.
func UnsafePermissions(path string) string {
	if runtime.GOOS == "windows" {
		return ""
	}

	info, err := os.Stat(path)
	if err != nil {
		return ""
	}

	perm := info.Mode().Perm()
	if perm&0o044 == 0 {
		return ""
	}

	return fmt.Sprintf("%04o", perm)
}
