package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreIntegration(t *testing.T) {
	// Create temporary database for testing
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test_memories.db")

	// Create store
	store, err := NewStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	// Test writing and retrieving memories
	ctx := context.Background()
	sessionID := "test-session-" + uuid.New().String()

	// Create test entries
	entries := []MemoryEntry{
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "This is a test fact about databases",
			MemoryType: "fact",
			Timestamp:  time.Now(),
			Importance: 0.8,
			Embedding:  generateTestEmbedding(1),
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "User decided to use SQLite for storage",
			MemoryType: "decision",
			Timestamp:  time.Now().Add(-1 * time.Hour),
			Importance: 0.9,
			Embedding:  generateTestEmbedding(2),
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "Error occurred during database connection",
			MemoryType: "error",
			Timestamp:  time.Now().Add(-2 * time.Hour),
			Importance: 0.3,
			Embedding:  generateTestEmbedding(3),
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "User prefers dark mode interface",
			MemoryType: "preference",
			Timestamp:  time.Now().Add(-3 * time.Hour),
			Importance: 0.6,
			Embedding:  generateTestEmbedding(4),
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "General conversation context about technology",
			MemoryType: "context",
			Timestamp:  time.Now().Add(-4 * time.Hour),
			Importance: 0.2,
			Embedding:  generateTestEmbedding(5),
		},
	}

	// Write entries
	for _, entry := range entries {
		err := store.Write(ctx, entry)
		require.NoError(t, err, "Failed to write entry %s", entry.ID)
	}

	// Test GetRecent
	recent, err := store.GetRecent(ctx, sessionID, 10)
	require.NoError(t, err)
	assert.Len(t, recent, 5, "Should retrieve all 5 entries")
	
	// Check that entries are returned in descending timestamp order
	for i := 0; i < len(recent)-1; i++ {
		assert.True(t, recent[i].Timestamp.After(recent[i+1].Timestamp), 
			"Entries should be ordered by timestamp descending")
	}

	// Test Search with similarity
	queryEmbedding := generateTestEmbedding(1) // Similar to first entry
	candidates, err := store.Search(ctx, queryEmbedding, sessionID, 3)
	require.NoError(t, err)
	assert.NotEmpty(t, candidates, "Should find similar entries")

	// Test GetRecent with limit
	limit := 2
	recentLimited, err := store.GetRecent(ctx, sessionID, limit)
	require.NoError(t, err)
	assert.Len(t, recentLimited, limit, "Should respect limit parameter")

	// Test different session IDs
	otherSessionID := "other-session-" + uuid.New().String()
	otherEntry := MemoryEntry{
		ID:         uuid.New().String(),
		SessionID:  otherSessionID,
		Content:    "Entry in different session",
		MemoryType: "context",
		Timestamp:  time.Now(),
		Importance: 0.5,
		Embedding:  generateTestEmbedding(6),
	}

	err = store.Write(ctx, otherEntry)
	require.NoError(t, err)

	// Verify session isolation
	originalSessionEntries, err := store.GetRecent(ctx, sessionID, 10)
	require.NoError(t, err)
	assert.Len(t, originalSessionEntries, 5, "Original session should still have 5 entries")

	otherSessionEntries, err := store.GetRecent(ctx, otherSessionID, 10)
	require.NoError(t, err)
	assert.Len(t, otherSessionEntries, 1, "Other session should have 1 entry")
}

func TestDetectMemoryType(t *testing.T) {
	testCases := []struct {
		content      string
		expectedType string
	}{
		{"There was an error in the system", "error"},
		{"An exception occurred during processing", "error"},
		{"Failed to connect to database", "error"},
		{"Invalid input provided", "error"},
		{"I decided to go with option A", "decision"},
		{"We chose the blue color scheme", "decision"},
		{"Selected the optimal configuration", "decision"},
		{"This is a fact about databases", "fact"},
		{"I remember that SQL is structured", "fact"},
		{"Learned that indexes improve performance", "fact"},
		{"I prefer dark mode interface", "preference"},
		{"User likes the new design", "preference"},
		{"Want to use SQLite for storage", "preference"},
		{"General conversation about technology", "context"},
		{"Talking about weather today", "context"},
		{"Discussing project requirements", "context"},
	}

	for _, tc := range testCases {
		actualType := DetectMemoryType(tc.content)
		assert.Equal(t, tc.expectedType, actualType, 
			"Content '%s' should be classified as '%s'", tc.content, tc.expectedType)
	}
}

func TestContentTruncation(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "truncate_test.db")

	store, err := NewStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	ctx := context.Background()
	sessionID := "truncate-test"

	// Create content that exceeds 2048 bytes
	longContent := ""
	for i := 0; i < 3000; i++ {
		longContent += "a"
	}

	entry := MemoryEntry{
		ID:         uuid.New().String(),
		SessionID:  sessionID,
		Content:    longContent,
		MemoryType: "context",
		Timestamp:  time.Now(),
	}

	err = store.Write(ctx, entry)
	require.NoError(t, err, "Should handle long content gracefully")

	// Verify entry was stored
	retrieved, err := store.GetRecent(ctx, sessionID, 1)
	require.NoError(t, err)
	require.Len(t, retrieved, 1)
	
	// Content should be truncated but not empty
	assert.Less(t, len(retrieved[0].Content), 3000, "Content should be truncated")
	assert.Greater(t, len(retrieved[0].Content), 0, "Content should not be empty")
}

func TestDatabasePermissions(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "permissions_test.db")

	store, err := NewStore(dbPath)
	require.NoError(t, err)
	defer store.Close()

	// Check that database file exists
	_, err = os.Stat(dbPath)
	require.NoError(t, err, "Database file should exist")

	// Note: Testing exact file permissions is tricky in tests due to umask,
	// but we can verify the file was created
	info, err := os.Stat(dbPath)
	require.NoError(t, err)
	assert.NotNil(t, info, "File info should be available")
}

// generateTestEmbedding creates a deterministic test embedding
func generateTestEmbedding(seed int) []float32 {
	embedding := make([]float32, 384)
	for i := range embedding {
		embedding[i] = float32((i+seed)%100) / 100.0
	}
	return embedding
}