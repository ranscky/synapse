// The config surface's redaction helpers: the one safe way to log configuration
// state, and the permissions check the startup path reports.
//
// Split out of config.go for the 300-line ceiling this project holds every file
// to, and along a line that is real: config.go owns what a control plane's
// configuration *is* -- loading it, defaulting it, validating it -- while this
// file owns what may be said about it afterwards. The two halves change for
// different reasons, and keeping them apart is what makes a new secret hard to
// leak: a secret added to the struct without a RedactedFields line is a value
// that some future log call can print in full, so the edit that touches one file
// is a prompt to look at the other.
package plane

import (
	"fmt"
	"os"
	"runtime"
)

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
		"stripe_webhook_secret", secretState(c.StripeWebhookSecret),
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
