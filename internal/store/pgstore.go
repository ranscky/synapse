// Postgres + pgvector backend for the v2 control-plane path. One PGStore is
// bound to exactly one tenant and talks only to that tenant's own schema
// (schema-per-tenant is the isolation guarantee; a row-level filter is not one).
//
// Nothing in the v1 scoring/retrieval/compilation pipeline changes to use this:
// PGStore implements the same four-method shape the callers already depend on,
// so the scorer, compiler, retrieval, and dedup packages are untouched.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// SyncStatus* are the only values MemoryEntry.SyncStatus ever holds. local_only
// is the standalone v1 state (nothing has left the machine), sync_pending is a
// row queued for the next push, synced is a row the plane has acknowledged.
const (
	SyncStatusLocalOnly   = "local_only"
	SyncStatusSyncPending = "sync_pending"
	SyncStatusSynced      = "synced"
)

// EmbeddingDimensions is the width of every embedding this project produces
// (all-MiniLM-L6-v2 -> 384). The Postgres column is vector(384), so a query
// vector of any other width cannot be compared with <-> at all.
const EmbeddingDimensions = 384

const (
	// defaultSearchTopK mirrors the SQLite backend's Search default.
	defaultSearchTopK = 20
	// defaultRecentLimit mirrors the SQLite backend's GetRecent default.
	defaultRecentLimit = 100
	// defaultAgentID mirrors the tenant schema's own DEFAULT for agent_id. A
	// blank AgentID is written as this value rather than as an empty string,
	// because the column is NOT NULL and an empty string would be a second,
	// invisible spelling of "unattributed".
	defaultAgentID = "default"
	// migrateTimeout bounds the tenant DDL run at construction time.
	migrateTimeout = 30 * time.Second
)

// tenantSlugPattern is the accepted tenant slug. It is deliberately wider than
// the control plane's own ^[a-z0-9-]{3,32}$ rule so a slug the plane issued can
// always be used here; hyphens are folded to underscores for the schema name
// (see schemaName), which cannot collide because the plane never issues a slug
// containing an underscore.
var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// PGStore is the Postgres/pgvector memory backend for one tenant.
type PGStore struct {
	pool       *pgxpool.Pool
	tenantSlug string
	// detector is the cross-agent contradiction check Write runs before it
	// inserts. A nil detector -- the zero state, and the state of every store
	// nothing called SetConflictDetector on -- means no detection at all, which is
	// exactly what this backend did before conflicts existed. See pgconflict.go.
	detector ConflictDetector
}

// NewPGStore returns a store bound to tenantSlug, creating that tenant's schema
// and table if they do not exist yet.
//
// The slug is validated before it reaches any SQL, and the resulting identifiers
// are quoted through pgx.Identifier as well, so a slug can never become part of
// a statement's text.
func NewPGStore(pool *pgxpool.Pool, tenantSlug string) (*PGStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("store: pgstore needs a database pool")
	}
	if !tenantSlugPattern.MatchString(tenantSlug) {
		return nil, fmt.Errorf("store: tenant slug %q must match %s", tenantSlug, tenantSlugPattern.String())
	}

	s := &PGStore{pool: pool, tenantSlug: tenantSlug}

	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	if err := s.migrate(ctx); err != nil {
		return nil, err
	}

	return s, nil
}

// Write inserts entry, or leaves the existing row untouched when its id is
// already present.
//
// The id must be a uuid: the column is a uuid and the v1 callers generate
// "req-<nano>" strings. Reporting that plainly is better than inventing an id
// the caller cannot later use for supersession.
//
// When a conflict detector is installed (SetConflictDetector) and the memory
// contradicts one of the tenant's recent org-scoped memories, two rows end up
// marked instead of one: this one as conflicting, and the older one it disagrees
// with as a superseded candidate. Both stay in every read -- a conflict is a label
// and a score penalty, never a deletion. Without a detector, this method behaves
// exactly as it did before conflicts existed.
func (s *PGStore) Write(ctx context.Context, entry MemoryEntry) error {
	if _, err := uuid.Parse(entry.ID); err != nil {
		return fmt.Errorf("store: memory id %q is not a uuid", entry.ID)
	}

	content := sanitized(entry.Content)
	if content == "[SANITIZED]" && entry.Content != "[SANITIZED]" {
		slog.Warn("Memory content sanitized due to prompt injection", "memory_id", entry.ID)
	}

	// A blank SyncStatus is normalized to this backend's own default: this store
	// is the control-plane side of a tenant, so a row written here is already on
	// the plane -- matching the column's DEFAULT 'synced'.
	syncStatus := entry.SyncStatus
	if syncStatus == "" {
		syncStatus = SyncStatusSynced
	}

	// A zero Timestamp would otherwise be stored as year 1 and sort below every
	// real memory, so it is replaced with now. Everything else about the entry is
	// stored exactly as given.
	createdAt := entry.Timestamp
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	// Left as NULL when the caller has no embedding, matching the SQLite
	// backend's empty BLOB: such a row is still a memory, it just cannot be
	// reached by semantic search.
	var embedding any
	if len(entry.Embedding) > 0 {
		embedding = pgvector.NewVector(entry.Embedding)
	}

	var supersededBy any
	if entry.SupersededBy != "" {
		supersededBy = entry.SupersededBy
	}

	// agent_id is NOT NULL in the tenant schema and defaults to 'default', so
	// a blank one is stored as that same value rather than as an empty string:
	// every row can then name an agent, and 'default' means exactly what the
	// column's own default means.
	agentID := entry.AgentID
	if agentID == "" {
		agentID = defaultAgentID
	}

	// The scope the row is stored under, and the team id that goes with it.
	// Both are normalized here rather than at each call site, so a pushed
	// memory and a locally written one cannot end up with different scopes for
	// the same input -- see visibilityForWrite.
	visibility, teamID, err := visibilityForWrite(entry.ID, entry)
	if err != nil {
		return err
	}

	// Conflict detection runs before the insert, and on the text the row will
	// actually hold: a memory sanitization rewrote must not be compared against
	// wording that never reached the database. Its verdict is a label on this row
	// plus, after the insert, one on an existing row -- see pgconflict.go for why it
	// is best effort and never fails the write.
	candidate := entry
	candidate.Content = content
	conflictStatus, conflictWithID, conflictingID := s.detectConflict(ctx, candidate)

	query := `INSERT INTO ` + s.table() + ` (
	id, session_id, content, memory_type, importance, sync_status, superseded_by, embedding, created_at, agent_id, visibility, team_id, conflict_status, conflict_with_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (id) DO NOTHING`

	_, err = s.pool.Exec(ctx, query,
		entry.ID, entry.SessionID, content, entry.MemoryType, entry.Importance,
		syncStatus, supersededBy, embedding, createdAt, agentID, visibility, teamID,
		conflictStatus, conflictWithID,
	)
	if err != nil {
		return fmt.Errorf("store: insert memory: %w", err)
	}

	// The other half of a contradiction: the memory this one disagrees with becomes
	// a candidate to be superseded. It is marked after the insert so that the id it
	// names already exists, and it is left in the pool -- the scorer, not this
	// store, is what demotes it. A failure here is reported rather than swallowed,
	// and that is safe for the caller: the insert above is idempotent on the id, so
	// a retry inserts nothing and marks again.
	if conflictingID != "" {
		if err := s.markSupersededCandidate(ctx, conflictingID, entry.ID); err != nil {
			return err
		}
		// Ids only: a memory's content never reaches a log line.
		slog.Warn("conflict_detected", "new_id", entry.ID, "conflicts_with", conflictingID)
	}

	return nil
}

// Search returns the topK nearest live memories to queryEmbedding that the
// caller is allowed to see, ordered by pgvector L2 distance (<->), which is what
// the HNSW index is built for.
//
// Visibility is enforced in SQL by visibilityWhere, and it is the only
// authorization rule on this path. agentID and teamID are the caller's own
// verified identity -- on the plane they come from the signed token, never from
// a request body -- and currentSessionID is the session the caller is working
// in. Together they decide which rows exist as far as this call is concerned:
// org-scoped memories from any agent, team-scoped memories from the caller's own
// team, and only the caller's own private memories from the caller's own
// session. A memory outside that set is not filtered out of the result; it is
// never selected, so it cannot leak through a fallback, a limit, or a caller
// that forgets to filter.
//
// Two deliberate differences from the SQLite backend's Search, documented
// rather than silent:
//
//   - Superseded memories are excluded in SQL. The v1 callers drop them anyway
//     (proxy, api, and supersession all skip SupersededBy != ""), so this is the
//     same result set with less data crossing the wire.
//   - An org-scoped memory is reachable regardless of session, which is the
//     point of a shared plane: this is the one read that can widen past one
//     session, and the scope predicate is what keeps that widening safe. The
//     SQLite backend scopes every search to one session and has no scopes at
//     all, because one file is one agent.
//
// An empty query vector falls back to recency ordering -- which applies the very
// same predicate, so the fallback is not a way around visibility -- because <->
// cannot compare against nothing: the retrieval pipeline relies on that fallback
// whenever there is no embedder or no query text. A wrong-width vector is a
// caller bug and is reported instead of being guessed at.
func (s *PGStore) Search(ctx context.Context, queryEmbedding []float32, agentID, teamID, currentSessionID string, topK int) ([]MemoryEntry, error) {
	if topK <= 0 {
		topK = defaultSearchTopK
	}

	if len(queryEmbedding) == 0 {
		return s.recent(ctx, agentID, teamID, currentSessionID, topK)
	}
	if len(queryEmbedding) != EmbeddingDimensions {
		return nil, fmt.Errorf("store: query embedding has %d dimensions, want %d", len(queryEmbedding), EmbeddingDimensions)
	}

	args := []any{pgvector.NewVector(queryEmbedding)}
	where := `superseded_by IS NULL AND ` + visibilityWhere(&args, agentID, teamID, currentSessionID)

	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY embedding <-> $1 LIMIT $%d`,
		pgColumns, s.table(), where, len(args)+1)
	args = append(args, topK)

	return s.queryEntries(ctx, query, args...)
}

// GetRecent returns the most recent live memories for one session, newest
// first. limit <= 0 falls back to the same default the SQLite backend uses.
//
// The session scope is unconditional here (matching the SQLite backend and the
// documented contract): a caller asking for one session's recent memories must
// not receive another session's.
//
// It deliberately applies no visibility predicate, and that is a real gap
// rather than a decision: Search is the only read that enforces scope. It is not
// reachable as a leak today because nothing calls GetRecent on a PGStore -- the
// plane's own HTTP surface (plane.MemorySearcher) exposes Search alone, and the
// v1 endpoints that do call GetRecent (api's GET /api/memories, the sync queue)
// are wired to the local SQLite store. Narrowing it needs two more parameters
// and changes two v1 interfaces, which is why it is recorded here and in
// PROGRESS.md as the follow-up this phase did not do rather than quietly left
// out.
func (s *PGStore) GetRecent(ctx context.Context, sessionID string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		limit = defaultRecentLimit
	}

	query := `SELECT ` + pgColumns + ` FROM ` + s.table() + `
WHERE superseded_by IS NULL AND session_id = $1
ORDER BY created_at DESC
LIMIT $2`

	return s.queryEntries(ctx, query, sessionID, limit)
}

// Close releases the pool this store was handed. The caller that opened the pool
// transfers ownership to PGStore: pgxpool.Close is idempotent, so a second close
// is harmless, but there should only ever be one.
func (s *PGStore) Close() error {
	if s == nil || s.pool == nil {
		return nil
	}
	s.pool.Close()
	return nil
}
