// v2 sync-queue accessors for the SQLite backend.
//
// Phase 8 added this file and nothing else to internal/store. It is a new file
// rather than an edit to store.go on purpose: every method here is additive, so
// no v1 line changed and the frozen v1 read/write paths behave exactly as they
// did before (the sync package only ever calls these four methods, which is why
// it can depend on the store without the store depending on it).
package store

import (
	"context"
	"fmt"
	"strings"
)

// Sanitize applies the store's pre-storage content pipeline to content.
//
// It exists so the v2 push path (a memory arriving from an edge node and being
// written by the plane) reuses the same pattern list the local write path uses
// instead of restating it. A zero-value Store is enough to call the method: it
// reads no fields, so this wrapper is the exported, package-level form of
// exactly that call. There is one pattern list in this project and it is the
// one in Store.Sanitize.
func Sanitize(content string) string {
	var s Store
	return s.Sanitize(content)
}

// syncMemoryColumns is the projection every sync-queue read uses, in the same
// order GetRecent and Search scan it.
const syncMemoryColumns = `id, session_id, content, memory_type, timestamp, importance, embedding, superseded_by, sync_status`

// PendingSync returns up to limit memories whose SyncStatus is sync_pending,
// oldest first, so the flusher drains the queue in the order memories were
// written and a new push cannot starve an old one.
//
// A non-positive limit is an error rather than a default: this is the one read
// in the package whose unbounded form would return the whole table, and a
// caller that forgot to pass its batch size should find out rather than quietly
// push every memory it has.
//
// Ordering is timestamp then id: timestamps come from time.Now() in the write
// path and can collide, and the id tie-break keeps a stable order across calls
// (two flushes in the same second must not disagree about which row is oldest).
//
// No index backs sync_status. Adding one means editing the v1 schema
// initialiser, and a scan of a local SQLite memory table is cheap at the sizes
// this store is designed for; a v2 phase that owns migrations can revisit it.
func (s *Store) PendingSync(ctx context.Context, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: PendingSync needs a positive limit")
	}

	query := `SELECT ` + syncMemoryColumns + `
	FROM memories
	WHERE sync_status = ?
	ORDER BY timestamp ASC, id ASC
	LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, SyncStatusSyncPending, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query pending memories: %w", err)
	}
	defer rows.Close()

	var entries []MemoryEntry
	for rows.Next() {
		var entry MemoryEntry
		var embeddingBytes []byte
		if err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType,
			&entry.Timestamp, &entry.Importance, &embeddingBytes, &entry.SupersededBy, &entry.SyncStatus); err != nil {
			return nil, fmt.Errorf("store: scan pending memory: %w", err)
		}
		entry.Embedding = bytesToEmbedding(embeddingBytes)
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pending memories: %w", err)
	}

	return entries, nil
}

// MarkSynced flips the named memories from sync_pending to synced.
//
// It runs as one parameterised statement, so a batch is marked in a single
// round trip and an id that is not present is simply not matched. An empty id
// list is a no-op, not an error: "nothing to mark" is a legitimate outcome and
// the flusher should not have to special-case it.
func (s *Store) MarkSynced(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	query := fmt.Sprintf(`UPDATE memories SET sync_status = ? WHERE id IN (%s)`, placeholders)

	args := make([]any, 0, len(ids)+1)
	args = append(args, SyncStatusSynced)
	for _, id := range ids {
		args = append(args, id)
	}

	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: mark memories synced: %w", err)
	}

	return nil
}

// CountPendingSync reports how many memories are waiting to be pushed. The
// flusher calls it once per interval to decide whether the backlog is worth
// warning about.
func (s *Store) CountPendingSync(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE sync_status = ?`, SyncStatusSyncPending).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count pending memories: %w", err)
	}

	return count, nil
}

// DropOldestPendingSync removes the oldest pending memories beyond keep from
// the sync queue and reports how many left it.
//
// "Removes" means marked local_only, never deleted. These memories are the
// overflow of a backlog the plane has been unreachable long enough to build, so
// something has to give; what gives is the promise to sync them, not the
// operator's data. They stay readable locally, they stop being retried, and the
// caller logs an ERROR naming the count so the condition cannot pass unnoticed.
// Deleting rows here would mean a network outage quietly destroying memories.
//
// keep < 0 is clamped to 0 (every pending row is abandoned), and a no-op when
// there is no overflow: the subquery selects nothing and RowsAffected is zero.
func (s *Store) DropOldestPendingSync(ctx context.Context, keep int) (int, error) {
	if keep < 0 {
		keep = 0
	}

	// The newest keep memories are what survive; the subquery enumerates the
	// queue from the newest end and skips exactly those, so everything it
	// returns is the overflow. Ordering descending here (and not ascending) is
	// the whole correctness of this statement: OFFSET counts from the end of
	// the ORDER BY, and an ascending order would skip the oldest rows -- the
	// ones that must be abandoned -- and abandon the newest instead.
	query := `UPDATE memories SET sync_status = ? WHERE id IN (
	SELECT id FROM memories WHERE sync_status = ? ORDER BY timestamp DESC, id DESC LIMIT -1 OFFSET ?
)`

	result, err := s.db.ExecContext(ctx, query, SyncStatusLocalOnly, SyncStatusSyncPending, keep)
	if err != nil {
		return 0, fmt.Errorf("store: abandon oldest pending memories: %w", err)
	}

	abandoned, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: confirm abandoned pending memories: %w", err)
	}

	return int(abandoned), nil
}
