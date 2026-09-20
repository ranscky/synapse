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
// SQLite backend scans an empty string for the same state.
const pgColumns = `id::text, session_id, content, memory_type, created_at, importance, sync_status, coalesce(superseded_by::text, ''), embedding`

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
func scanEntry(rows pgx.Rows) (MemoryEntry, error) {
	var (
		entry      MemoryEntry
		embedding  *pgvector.Vector
		superseded string
	)

	err := rows.Scan(
		&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType,
		&entry.Timestamp, &entry.Importance, &entry.SyncStatus, &superseded, &embedding,
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
