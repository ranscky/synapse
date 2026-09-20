package plane

import (
	"net"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fallbackDBHost is reported when a DSN cannot be parsed far enough to name its
// host. It is deliberately a word, never a fragment of the DSN.
const fallbackDBHost = "unknown"

// DBHost extracts "host:port" from a Postgres DSN for log lines that must name
// the database target without echoing the DSN itself.
//
// A DSN carries the password, and pgx wraps the raw DSN in its own parse errors,
// so neither the DSN nor a pgx error is ever safe to log. This helper exists so
// the fatal startup paths can still say *which* database the plane failed to
// reach: it parses with pgxpool's own parser (covering both URL and
// keyword/value DSNs) and returns only the host and port.
//
// Anything unparseable yields fallbackDBHost. The parse error is dropped rather
// than returned, because its text can contain the DSN.
func DBHost(dsn string) string {
	if dsn == "" {
		return fallbackDBHost
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Host == "" {
		return fallbackDBHost
	}

	if cfg.ConnConfig.Port == 0 {
		return cfg.ConnConfig.Host
	}

	return net.JoinHostPort(cfg.ConnConfig.Host, strconv.Itoa(int(cfg.ConnConfig.Port)))
}
