// Read-path helpers for the Postgres backend, split out of pgstore.go to keep
// both files inside the 300-line ceiling.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// pgColumns is the projection both read paths use. id and superseded_by are
// cast to text so they scan into the plain string fields on MemoryEntry, and
// superseded_by is coalesced because it is NULL for every live memory while the
// SQLite backend scans an empty string for the same state. agent_id is included
// because a search result has to name the agent that pushed the memory -- that
// is what an edge node scores and displays, and it is why the tenant column
// exists at all. visibility and team_id are included for the same reason the
// caller's own scope is bound into the query: a result has to say which scope it
// came from, or a caller has no way to tell an org-wide memory from its own
// private one, and team_id is coalesced because it is NULL whenever a memory is
// not team-scoped.
const pgColumns = `id::text, session_id, content, memory_type, created_at, importance, sync_status, coalesce(superseded_by::text, ''), embedding, agent_id, visibility, coalesce(team_id, '')`

// queryEntries runs one read statement and scans every row, so both read paths
// share a single error-wrapping and iteration story.
func (s *PGStore) queryEntries(ctx context.Context, query string, args ...any) ([]MemoryEntry, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query memories: %w", err)
	}
	defer rows.Close()

	entries := make([]MemoryEntry, 0, 8)
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate memories: %w", err)
	}

	return entries, nil
}

// scanEntry reads one row in pgColumns order. A row written without an embedding
// leaves Embedding nil rather than an empty non-nil slice, matching what the
// SQLite backend's bytesToEmbedding returns for an empty BLOB.
//
// agent_id is NOT NULL in the schema, so it always scans into a string; the
// only reason it is a plain string rather than a pointer is that the schema
// guarantees the default ('default') rather than NULL for a row that never
// named an agent. visibility is NOT NULL for the same reason (its default is
// 'org'), and team_id is coalesced to the empty string in the projection for
// every row that is not team-scoped, so three more plain strings are enough.
func scanEntry(rows pgx.Rows) (MemoryEntry, error) {
	var (
		entry      MemoryEntry
		embedding  *pgvector.Vector
		superseded string
	)

	err := rows.Scan(
		&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType,
		&entry.Timestamp, &entry.Importance, &entry.SyncStatus, &superseded, &embedding,
		&entry.AgentID, &entry.Visibility, &entry.TeamID,
	)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("store: scan memory entry: %w", err)
	}

	entry.SupersededBy = superseded
	if embedding != nil {
		entry.Embedding = embedding.Slice()
	}

	return entry, nil
}

// recent is the recency-ordered read behind Search's no-embedding fallback.
//
// It applies the same visibility predicate Search does, and that is the whole
// point of it being a separate method rather than a branch inline in Search: the
// fallback fires exactly when a caller has nothing to embed (no embedder
// configured, or an empty query), which is a state an attacker can arrange on
// purpose. A predicate applied only to the vector path would make "send no
// embedding" a way to read every private memory in the tenant.
func (s *PGStore) recent(ctx context.Context, agentID, teamID, currentSessionID string, limit int) ([]MemoryEntry, error) {
	args := []any{}
	where := `superseded_by IS NULL AND ` + visibilityWhere(&args, agentID, teamID, currentSessionID)

	query := fmt.Sprintf(`SELECT %s FROM %s WHERE %s ORDER BY created_at DESC LIMIT $%d`,
		pgColumns, s.table(), where, len(args)+1)
	args = append(args, limit)

	return s.queryEntries(ctx, query, args...)
}
