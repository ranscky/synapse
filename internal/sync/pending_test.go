package sync

import (
	"context"
	"testing"

	"synapse/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingBackend is a store.Backend double: it records what it was handed and
// answers reads with a canned set, so a test can assert both that the decorator
// marked what it stored and that it changed nothing else on the way through.
type recordingBackend struct {
	writes []store.MemoryEntry

	searchAgentID   string
	searchTeamID    string
	searchSessionID string
	searchTopK      int
	searchResult    []store.MemoryEntry

	recentSessionID string
	recentLimit     int

	supersededOld string
	supersededNew string
}

// Write implements store.Backend.
func (b *recordingBackend) Write(_ context.Context, entry store.MemoryEntry) error {
	b.writes = append(b.writes, entry)
	return nil
}

// Search implements store.Backend.
func (b *recordingBackend) Search(_ context.Context, _ []float32, agentID, teamID, sessionID string, topK int) ([]store.MemoryEntry, error) {
	b.searchAgentID, b.searchTeamID, b.searchSessionID, b.searchTopK = agentID, teamID, sessionID, topK
	return b.searchResult, nil
}

// GetRecent implements store.Backend.
func (b *recordingBackend) GetRecent(_ context.Context, sessionID string, limit int) ([]store.MemoryEntry, error) {
	b.recentSessionID, b.recentLimit = sessionID, limit
	return b.searchResult, nil
}

// MarkSuperseded implements store.Backend.
func (b *recordingBackend) MarkSuperseded(_ context.Context, oldID, newID string) error {
	b.supersededOld, b.supersededNew = oldID, newID
	return nil
}

// TestPendingWriterQueuesWhatItStores is the phase's reason for this type: a
// memory written through it is stored as sync_pending, which is the one value
// PendingSync matches on -- so the row a flusher reads is the row a writer
// queued, with no window in between.
func TestPendingWriterQueuesWhatItStores(t *testing.T) {
	ctx := context.Background()

	t.Run("a blank sync status becomes pending", func(t *testing.T) {
		inner := &recordingBackend{}
		w := NewPendingWriter(inner)

		require.NoError(t, w.Write(ctx, store.MemoryEntry{ID: "mem-1", Content: "the build runs on CI"}))

		require.Len(t, inner.writes, 1, "the write has to reach the backend")
		assert.Equal(t, store.SyncStatusSyncPending, inner.writes[0].SyncStatus,
			"an unstated sync status is what the queue is for")
		assert.Equal(t, "mem-1", inner.writes[0].ID, "and nothing else about the memory changes")
	})

	t.Run("a stated sync status is left alone", func(t *testing.T) {
		for _, status := range []string{store.SyncStatusLocalOnly, store.SyncStatusSynced} {
			inner := &recordingBackend{}
			w := NewPendingWriter(inner)

			require.NoError(t, w.Write(ctx, store.MemoryEntry{ID: "mem-2", SyncStatus: status}))

			require.Len(t, inner.writes, 1)
			assert.Equal(t, status, inner.writes[0].SyncStatus,
				"a caller that named a state keeps it, exactly as store.Write's own normalization does")
		}
	})

	t.Run("a nil backend is an error, not a panic", func(t *testing.T) {
		var w *PendingWriter
		assert.Error(t, w.Write(ctx, store.MemoryEntry{ID: "mem-3"}))
		assert.Error(t, NewPendingWriter(nil).Write(ctx, store.MemoryEntry{ID: "mem-3"}))
	})
}

// TestPendingWriterDelegatesEverythingElse pins the other half of the contract:
// marking is a write-path concern, so reads, recency, and supersession travel
// through unchanged -- arguments included, since the plane's visibility rules run
// on exactly those three strings.
func TestPendingWriterDelegatesEverythingElse(t *testing.T) {
	ctx := context.Background()

	canned := []store.MemoryEntry{{ID: "mem-4", Content: "the retry budget is three"}}
	inner := &recordingBackend{searchResult: canned}
	w := NewPendingWriter(inner)

	searched, err := w.Search(ctx, []float32{1, 0}, "agent_a", "team_blue", "sess-1", 7)
	require.NoError(t, err)
	assert.Equal(t, canned, searched, "the backend's answer is passed straight through")
	assert.Equal(t, "agent_a", inner.searchAgentID)
	assert.Equal(t, "team_blue", inner.searchTeamID)
	assert.Equal(t, "sess-1", inner.searchSessionID)
	assert.Equal(t, 7, inner.searchTopK)

	recent, err := w.GetRecent(ctx, "sess-2", 9)
	require.NoError(t, err)
	assert.Equal(t, canned, recent)
	assert.Equal(t, "sess-2", inner.recentSessionID)
	assert.Equal(t, 9, inner.recentLimit)

	require.NoError(t, w.MarkSuperseded(ctx, "mem-old", "mem-new"))
	assert.Equal(t, "mem-old", inner.supersededOld)
	assert.Equal(t, "mem-new", inner.supersededNew)
}
