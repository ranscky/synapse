// Tenant DDL for the Postgres backend, split out of pgstore.go to keep both
// files inside the 300-line ceiling. Nothing here is called from outside the
// package: PGStore.migrate is the single entry point.
package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// schemaName is the schema a store owns, e.g. slug "acme-prod" ->
// "tenant_acme_prod". The hyphen fold cannot collide with a real slug because
// the control plane's own slug rule never produces an underscore.
func (s *PGStore) schemaName() string {
	return "tenant_" + strings.ReplaceAll(s.tenantSlug, "-", "_")
}

// table returns the quoted, fully-qualified memories table identifier.
func (s *PGStore) table() string {
	return pgx.Identifier{s.schemaName(), "memories"}.Sanitize()
}

// migrate applies the tenant's DDL idempotently, inside one transaction:
// PostgreSQL DDL is transactional, so a half-created tenant schema can never be
// left behind for the next boot to build on top of.
func (s *PGStore) migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tenant migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, stmt := range s.schemaStatements() {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: migrate tenant schema %s: %w", s.schemaName(), err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit tenant migration: %w", err)
	}

	return nil
}

// schemaStatements is the tenant's complete DDL, in order. Every statement is
// IF NOT EXISTS, which is what makes the set safe to re-run on every boot.
//
// The extension comes first because vector(384) and the HNSW access method do
// not exist without it. The pgvector image ships the extension but does not
// enable it per database, so this is required, not defensive. Every identifier
// is quoted through pgx.Identifier on top of the slug validation NewPGStore
// performs, so no slug text can reach a statement unquoted.
func (s *PGStore) schemaStatements() []string {
	table := s.table()
	schema := pgx.Identifier{s.schemaName()}.Sanitize()

	return []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE SCHEMA IF NOT EXISTS ` + schema,
		`CREATE TABLE IF NOT EXISTS ` + table + ` (
	id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
	session_id text NOT NULL,
	agent_id text NOT NULL DEFAULT 'default',
	content text NOT NULL,
	memory_type text NOT NULL,
	importance float8 NOT NULL DEFAULT 0.5,
	sync_status text NOT NULL DEFAULT 'synced',
	visibility text NOT NULL DEFAULT 'org',
	team_id text,
	superseded_by uuid,
	conflict_status text NOT NULL DEFAULT 'none',
	conflict_with_id uuid,
	embedding vector(384),
	created_at timestamptz NOT NULL DEFAULT now()
)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_embedding_hnsw ON ` + table +
			` USING hnsw (embedding vector_l2_ops) WITH (m=16, ef_construction=64)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_session_created ON ` + table +
			` (session_id, created_at DESC)`,
	}
}
