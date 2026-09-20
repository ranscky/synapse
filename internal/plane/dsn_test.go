package plane

import "testing"

func TestDBHost(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "url form",
			dsn:  "postgres://synapse:secret-password@127.0.0.1:5432/synapse?sslmode=disable",
			want: "127.0.0.1:5432",
		},
		{
			// pgx fills in the default port, so the reported target is a
			// complete host:port even when the DSN omits one.
			name: "url form without a port",
			dsn:  "postgres://synapse:secret-password@db.internal/synapse",
			want: "db.internal:5432",
		},
		{
			name: "keyword/value form",
			dsn:  "host=127.0.0.1 port=5433 user=synapse password=secret-password dbname=synapse",
			want: "127.0.0.1:5433",
		},
		{
			name: "keyword/value form without a port",
			dsn:  "host=db.internal user=synapse password=secret-password",
			want: "db.internal:5432",
		},
		{name: "empty dsn", dsn: "", want: fallbackDBHost},
		{name: "unparseable dsn", dsn: "postgres://user:pw@%%%/db", want: fallbackDBHost},
		{name: "garbage", dsn: "not a dsn at all", want: fallbackDBHost},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DBHost(tt.dsn)

			if got != tt.want {
				t.Fatalf("DBHost(%q) = %q, want %q", tt.dsn, got, tt.want)
			}

			// The whole point of this helper: whatever it returns is safe to log.
			if got != fallbackDBHost && got == tt.dsn {
				t.Fatalf("DBHost leaked the dsn: %q", got)
			}
		})
	}
}

// TestDBHostNeverReturnsThePassword guards the fatal startup log line: a
// credential-bearing DSN must only ever yield a host.
func TestDBHostNeverReturnsThePassword(t *testing.T) {
	const password = "hunter2-must-not-appear-in-logs"
	dsns := []string{
		"postgres://synapse:" + password + "@127.0.0.1:5432/synapse",
		"host=127.0.0.1 user=synapse password=" + password,
		"postgres://synapse:" + password + "@%%%/synapse",
	}

	for _, dsn := range dsns {
		got := DBHost(dsn)
		if got == password || got == dsn {
			t.Fatalf("DBHost(%q) = %q, which exposes the credential", dsn, got)
		}
	}
}
