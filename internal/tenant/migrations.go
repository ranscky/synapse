package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaName is the global schema owned by the control plane. It holds the
// tenant registry and cross-tenant bookkeeping only: tenant payload lives in
// its own schema, because schema-per-tenant is the isolation guarantee and a
// row-level filter is explicitly not one.
const SchemaName = "synapse_global"

// LedgerWriterRole is the Postgres role the audit ledger accepts INSERTs from.
// It owns nothing, cannot log in, and holds exactly one privilege on exactly one
// table: INSERT on synapse_global.ledger. UPDATE and DELETE are deliberately
// never granted, which is what makes the ledger append-only in the database
// rather than by convention.
//
// The migration also grants the role to the application's own role, so the
// writer can assume it per transaction (SET LOCAL ROLE). That membership is the
// only thing that makes the guarantee reachable: the compose application user is
// a Postgres superuser and owns the table, and no grant can bind either.
const LedgerWriterRole = "ledger_writer"

// migration is one idempotent DDL statement. The name travels into error
// wrapping so a failed migration can be reported without echoing the SQL.
type migration struct {
	name string
	sql  string
}

// migrations is applied in order, top to bottom, on every plane boot. Every
// statement is IF NOT EXISTS, which is what makes the set safely re-runnable
// without a version table.
//
// gen_random_uuid() is core since PostgreSQL 13, so no pgcrypto extension -- and
// therefore no superuser -- is required.
var migrations = []migration{
	{
		name: "schema",
		sql:  `CREATE SCHEMA IF NOT EXISTS ` + SchemaName,
	},
	{
		name: "tenants",
		sql: `CREATE TABLE IF NOT EXISTS ` + SchemaName + `.tenants (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	slug text UNIQUE NOT NULL,
	plan text NOT NULL DEFAULT 'oss',
	compliance_tier text NOT NULL DEFAULT 'team',
	status text NOT NULL DEFAULT 'active',
	stripe_customer_id text,
	created_at timestamptz NOT NULL DEFAULT now()
)`,
	},
	{
		name: "tenant_keys",
		sql: `CREATE TABLE IF NOT EXISTS ` + SchemaName + `.tenant_keys (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id uuid NOT NULL REFERENCES ` + SchemaName + `.tenants(id),
	key_hash text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
)`,
	},
	{
		name: "tenant_secrets",
		sql: `CREATE TABLE IF NOT EXISTS ` + SchemaName + `.tenant_secrets (
	tenant_id uuid PRIMARY KEY REFERENCES ` + SchemaName + `.tenants(id),
	secret_encrypted text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
)`,
	},
	{
		name: "compliance_access_log",
		sql: `CREATE TABLE IF NOT EXISTS ` + SchemaName + `.compliance_access_log (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id uuid,
	endpoint text,
	query_params_redacted text,
	ip_hash text,
	response_code int,
	created_at timestamptz NOT NULL DEFAULT now()
)`,
	},
	{
		name: "usage_events",
		sql: `CREATE TABLE IF NOT EXISTS ` + SchemaName + `.usage_events (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id uuid NOT NULL,
	agent_id text,
	session_id text,
	raw_tokens int,
	compiled_tokens int,
	reduction_pct float8,
	model text,
	created_at timestamptz NOT NULL DEFAULT now()
)`,
	},
	{
		// The index the metering write path's readers need: usage_events is
		// queried by (tenant_id, created_at) window and by nothing else (see
		// internal/ledger/report.go's totals query). It is added in the phase
		// that started writing rows rather than in a phase that read an empty
		// table -- PROGRESS.md's Phase 20 finding 6 named this phase for it.
		// Named explicitly for the reason ledger_tenant_created_idx is: CREATE
		// INDEX has no unnamed form, so the name is what makes IF NOT EXISTS
		// possible.
		name: "usage_events_tenant_created_idx",
		sql:  `CREATE INDEX IF NOT EXISTS usage_events_tenant_created_idx ON ` + SchemaName + `.usage_events (tenant_id, created_at)`,
	},
	{
		// The audit ledger: one row per compiled request, append-only. tenant_id
		// and request_id carry no foreign key on purpose -- the ledger has to be
		// able to record a request for a tenant row that has since been frozen or
		// removed, and a constraint that could refuse a write would make the
		// record incomplete exactly when it matters most.
		name: "ledger",
		sql: `CREATE TABLE IF NOT EXISTS ` + SchemaName + `.ledger (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	tenant_id uuid NOT NULL,
	request_id uuid NOT NULL,
	trace_json text NOT NULL,
	prev_hash text NOT NULL,
	hash_value text NOT NULL,
	deleted_at timestamptz,
	created_at timestamptz NOT NULL DEFAULT now()
)`,
	},
	{
		// The index the audit read path (per-tenant history, newest first) will
		// use. Named explicitly because CREATE INDEX has no unnamed form: unlike
		// CREATE TABLE, an index without a name is a syntax error, so the name is
		// what makes IF NOT EXISTS possible.
		name: "ledger_tenant_created_idx",
		sql:  `CREATE INDEX IF NOT EXISTS ledger_tenant_created_idx ON ` + SchemaName + `.ledger (tenant_id, created_at)`,
	},
	{
		// NOLOGIN, owns nothing, no password: this role exists only to be granted
		// INSERT below. CREATE ROLE has no IF NOT EXISTS, hence the DO block --
		// and a role created inside this migration's transaction is rolled back
		// with the rest of it if any later statement fails.
		name: "ledger_writer_role",
		sql: `DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '` + LedgerWriterRole + `') THEN
    CREATE ROLE ` + LedgerWriterRole + `;
  END IF;
END $$`,
	},
	{
		// Defensive rather than load-bearing: a new table's default ACL already
		// grants PUBLIC nothing. Stated explicitly so the intent survives any
		// future default-privilege change.
		name: "ledger_revoke_public",
		sql:  `REVOKE ALL ON ` + SchemaName + `.ledger FROM PUBLIC`,
	},
	{
		// Without this the writer cannot reach the table at all: access to any
		// object in a schema requires USAGE on that schema, and synapse_global has
		// an empty ACL, so PUBLIC holds none and only the owner can get in.
		name: "ledger_writer_schema_usage",
		sql:  `GRANT USAGE ON SCHEMA ` + SchemaName + ` TO ` + LedgerWriterRole,
	},
	{
		// INSERT and nothing else: no UPDATE, no DELETE, and no SELECT either --
		// the strongest available form of "never update and never delete", which
		// is this project's ledger hard rule expressed as a privilege.
		name: "ledger_writer_insert",
		sql:  `GRANT INSERT ON ` + SchemaName + `.ledger TO ` + LedgerWriterRole,
	},
	{
		// Granted to whoever runs the migration -- the application's own role --
		// not to a hardcoded name, because the database user is deployment
		// configuration (SYNAPSE_DB_DSN) and a literal 'synapse' would abort the
		// boot of any deployment that names its user differently. Membership is
		// what lets the writer SET LOCAL ROLE ledger_writer per transaction, and
		// it is the reason the ledger's INSERT-only guarantee is reachable at all.
		name: "ledger_writer_membership",
		sql:  `GRANT ` + LedgerWriterRole + ` TO CURRENT_USER`,
	},
}

// RunMigrations creates the synapse_global schema and its tables, idempotently.
//
// Every statement runs inside one transaction: PostgreSQL DDL is transactional,
// so a failure part-way through leaves the database exactly as it was instead of
// leaving a partial schema that the next boot would silently build on top of.
//
// The statements are not guarded by an advisory lock. Migrations run once, from
// the control plane's own startup path, and CREATE ... IF NOT EXISTS is safe to
// repeat -- a second plane booting concurrently is not a supported topology yet
// (see PROGRESS.md).
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("tenant: migrations need a database pool")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: begin migrations: %w", err)
	}
	// A committed transaction makes this a no-op; Rollback only ever fires on
	// the failure paths above and below.
	defer func() { _ = tx.Rollback(ctx) }()

	for _, m := range migrations {
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("tenant: migrate %s: %w", m.name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenant: commit migrations: %w", err)
	}

	return nil
}
