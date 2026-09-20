// Backend selection. This file is the only place in the codebase that decides
// whether a process talks to a local SQLite file or to a tenant's Postgres
// schema; every other package depends on the Backend interface (or on its own
// narrower one, as internal/proxy and internal/retrieval already do) and never
// on a concrete store.
package store

import (
	"context"
	"fmt"
	"time"

	"synapse/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// openPoolTimeout bounds pool creation plus its confirming ping.
const openPoolTimeout = 15 * time.Second

// Backend is the storage contract every memory backend satisfies: the SQLite
// store and the Postgres store both implement it as-is, and the scorer,
// compiler, retrieval, and dedup packages are unaware of which one is behind it.
//
// It is deliberately the four methods the pipeline actually calls. Closing a
// backend is not part of the contract because the local SQLite store owns its
// file while a PGStore owns a pool the factory handed it; callers that opened
// resources close them through the concrete type.
type Backend interface {
	// Write stores a memory entry, sanitized, replacing nothing.
	Write(ctx context.Context, entry MemoryEntry) error
	// Search returns the topK most semantically similar entries the caller is
	// allowed to see: agentID, teamID, and currentSessionID are the reader's
	// own verified scope, and the Postgres backend enforces visibility with
	// them. The SQLite backend accepts and ignores them (see its own docs).
	Search(ctx context.Context, queryEmbedding []float32, agentID, teamID, sessionID string, topK int) ([]MemoryEntry, error)
	// GetRecent returns a session's most recent entries, newest first.
	GetRecent(ctx context.Context, sessionID string, limit int) ([]MemoryEntry, error)
	// MarkSuperseded records that one memory has been replaced by another.
	MarkSuperseded(ctx context.Context, id, supersededByID string) error
}

// OpenPGPool opens a pgx pool for dsn with the pgvector types registered on
// every connection.
//
// Registration is not optional: pgx has no built-in codec for the vector OID,
// so without AfterConnect every insert or comparison involving an embedding
// fails at encode time. Doing it here (rather than at each call site) is what
// keeps the factory and the tests from drifting into two different pool setups.
//
// The extension is created first, on its own connection, because of an ordering
// constraint this cost a real failure to find: the vector OID does not exist
// until the extension does, and RegisterTypes fails the whole connection with
// "vector type not found in the database" when it is missing -- so a pool whose
// AfterConnect registers the type can never be the thing that enables it.
//
// The DSN is never echoed in an error: pgx quotes the whole connection string
// -- password included -- in its parse error, so that cause is deliberately
// dropped rather than wrapped. Nothing here is logged.
func OpenPGPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf("store: a postgres dsn is required")
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: postgres dsn is not a valid connection string")
	}

	if err := ensureVectorExtension(ctx, poolCfg.ConnConfig); err != nil {
		return nil, err
	}

	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: open postgres pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: postgres pool is unreachable: %w", err)
	}

	return pool, nil
}

// ensureVectorExtension enables pgvector on one connection that does not try to
// register the vector type, so the type exists by the time any pooled
// connection asks for it. IF NOT EXISTS makes this one round trip on the first
// open of a database and a no-op afterwards.
func ensureVectorExtension(ctx context.Context, connCfg *pgx.ConnConfig) error {
	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		// The connection error quotes host, user and database -- and the DSN's
		// password only because pgx redacts it. The cause is dropped anyway so
		// that stays true no matter how pgx's formatting changes.
		return fmt.Errorf("store: could not connect to postgres to enable pgvector")
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("store: enable pgvector extension: %w", err)
	}

	return nil
}

// NewStoreFromConfig returns the backend cfg describes: the local SQLite store
// when no control plane is configured, or a tenant-scoped Postgres store when
// one is.
//
// control-plane-url is the switch, and its default -- empty -- means the v1
// single-binary behavior is unchanged, so an existing synapse.yaml keeps working
// without edits. tenantSlug is only consulted on the Postgres path; the SQLite
// path is one file per process and has no tenant concept.
//
// On the Postgres path the pool is opened here and its ownership passes to the
// returned *PGStore, which closes it via Close().
func NewStoreFromConfig(cfg config.Config, tenantSlug string) (Backend, error) {
	if cfg.ControlPlaneURL == "" {
		return NewStore(cfg.DBPath)
	}

	if cfg.DatabaseDSN == "" {
		// Name the key, never the value: a DSN carries the database password.
		return nil, fmt.Errorf("store: database-dsn (or %s) is required when control-plane-url is set", config.EnvDatabaseDSN)
	}

	ctx, cancel := context.WithTimeout(context.Background(), openPoolTimeout)
	defer cancel()

	pool, err := OpenPGPool(ctx, cfg.DatabaseDSN)
	if err != nil {
		return nil, err
	}

	pg, err := NewPGStore(pool, tenantSlug)
	if err != nil {
		pool.Close()
		return nil, err
	}

	return pg, nil
}
