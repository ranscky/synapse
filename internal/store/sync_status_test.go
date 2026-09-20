package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyncStatusMigrationAndRoundTrip covers the v2 column on the SQLite side: a
// database created before the column existed must gain it on open, and the value
// must survive a write/read cycle so both backends mean the same thing by
// SyncStatus.
func TestSyncStatusMigrationAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sync-status.db")

	// A pre-v2 database: the original schema, with no sync_status column.
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_journal_mode=WAL&_fk=true")
	require.NoError(t, err)
	_, err = db.Exec(`
	CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		content TEXT NOT NULL,
		memory_type TEXT NOT NULL,
		timestamp DATETIME NOT NULL,
		importance REAL DEFAULT 0.0,
		embedding BLOB,
		superseded_by TEXT DEFAULT ''
	)`)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO memories (id, session_id, content, memory_type, timestamp) VALUES ('legacy', 's1', 'legacy row', 'fact', ?)`,
		time.Now(),
	)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Opening it through NewStore must migrate it in place.
	st, err := NewStore(dbPath)
	require.NoError(t, err)
	defer st.Close()

	require.NoError(t, st.Write(ctx, MemoryEntry{
		ID: "new", SessionID: "s1", Content: "new row", MemoryType: "fact",
		Timestamp: time.Now(), SyncStatus: SyncStatusSyncPending,
	}))
	require.NoError(t, st.Write(ctx, MemoryEntry{
		ID: "blank", SessionID: "s1", Content: "blank status row", MemoryType: "fact",
		Timestamp: time.Now(),
	}))

	got, err := st.GetRecent(ctx, "s1", 10)
	require.NoError(t, err)
	require.Len(t, got, 3)

	byID := make(map[string]MemoryEntry, len(got))
	for _, e := range got {
		byID[e.ID] = e
	}

	assert.Equal(t, SyncStatusLocalOnly, byID["legacy"].SyncStatus, "a row migrated in place was never synced")
	assert.Equal(t, SyncStatusSyncPending, byID["new"].SyncStatus)
	assert.Equal(t, SyncStatusLocalOnly, byID["blank"].SyncStatus, "a blank status normalizes to the local default")
}
