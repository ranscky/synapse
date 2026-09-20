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
