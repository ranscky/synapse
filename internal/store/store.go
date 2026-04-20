package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

// MemoryEntry represents a stored memory entry
type MemoryEntry struct {
	ID         string    `json:"id"`                    // UUID
	SessionID  string    `json:"session_id"`            // Session identifier
	Content    string    `json:"content"`               // Memory content (max 2048 bytes)
	MemoryType string    `json:"memory_type"`           // "decision"|"fact"|"error"|"preference"|"context"
	Timestamp  time.Time `json:"timestamp"`             // Creation timestamp
	Importance float64   `json:"importance,omitempty"`  // Importance score
	Embedding  []float32 `json:"embedding,omitempty"`   // 384-dim embedding vector
}

// Store represents the memory store using SQLite-vec
type Store struct {
	db *sql.DB
}

// NewStore creates a new store with SQLite-vec backend
func NewStore(dbPath string) (*Store, error) {
	// Create directory if it doesn't exist
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	// Open database with proper permissions
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL&_fk=true", dbPath))
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set file permissions to 0600
	if err := os.Chmod(dbPath, 0600); err != nil {
		slog.Warn("Failed to set database file permissions", "error", err)
	}

	store := &Store{db: db}
	
	// Initialize database schema
	if err := store.initSchema(); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	slog.Info("Store initialized", "db_path", dbPath)
	return store, nil
}

// initSchema initializes the database schema
func (s *Store) initSchema() error {
	// Create memories table
	query := `
	CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		content TEXT NOT NULL,
		memory_type TEXT NOT NULL,
		timestamp DATETIME NOT NULL,
		importance REAL DEFAULT 0.0
	);

	CREATE INDEX IF NOT EXISTS idx_memories_session_id ON memories(session_id);
	CREATE INDEX IF NOT EXISTS idx_memories_timestamp ON memories(timestamp);
	CREATE INDEX IF NOT EXISTS idx_memories_memory_type ON memories(memory_type);
	`

	_, err := s.db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	return nil
}

// Write stores a memory entry
func (s *Store) Write(ctx context.Context, entry MemoryEntry) error {
	// Truncate content if it exceeds 2048 bytes
	if len(entry.Content) > 2048 {
		// Find the last rune boundary within the limit
		truncated := entry.Content[:2048]
		// Ensure we don't cut off a multi-byte UTF-8 character
		for len(truncated) > 0 && !utf8.ValidString(truncated) {
			truncated = truncated[:len(truncated)-1]
		}
		slog.Warn("Memory content truncated", "original_length", len(entry.Content), "truncated_length", len(truncated))
		entry.Content = truncated
	}

	// Insert memory entry
	insertQuery := `
	INSERT OR REPLACE INTO memories (id, session_id, content, memory_type, timestamp, importance)
	VALUES (?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.ExecContext(ctx, insertQuery, entry.ID, entry.SessionID, entry.Content, entry.MemoryType, entry.Timestamp, entry.Importance)
	if err != nil {
		return fmt.Errorf("failed to insert memory: %w", err)
	}

	return nil
}

// Search performs search using content matching (simplified implementation)
func (s *Store) Search(ctx context.Context, queryEmbedding []float32, sessionID string, topK int) ([]MemoryEntry, error) {
	if topK <= 0 {
		topK = 20 // Default to 20 if not specified
	}

	// Simplified search by content similarity (would use embeddings in full implementation)
	searchQuery := `
	SELECT id, session_id, content, memory_type, timestamp, importance
	FROM memories
	WHERE session_id = ?
	ORDER BY timestamp DESC
	LIMIT ?
	`

	rows, err := s.db.QueryContext(ctx, searchQuery, sessionID, topK)
	if err != nil {
		return nil, fmt.Errorf("failed to search memories: %w", err)
	}
	defer rows.Close()

	var entries []MemoryEntry
	for rows.Next() {
		var entry MemoryEntry
		err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType, &entry.Timestamp, &entry.Importance)
		if err != nil {
			return nil, fmt.Errorf("failed to scan memory entry: %w", err)
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating results: %w", err)
	}

	return entries, nil
}

// GetRecent retrieves recent memory entries for a session
func (s *Store) GetRecent(ctx context.Context, sessionID string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		limit = 100 // Default limit
	}

	query := `
	SELECT id, session_id, content, memory_type, timestamp, importance
	FROM memories
	WHERE session_id = ?
	ORDER BY timestamp DESC
	LIMIT ?
	`

	rows, err := s.db.QueryContext(ctx, query, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get recent memories: %w", err)
	}
	defer rows.Close()

	var entries []MemoryEntry
	for rows.Next() {
		var entry MemoryEntry
		err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType, &entry.Timestamp, &entry.Importance)
		if err != nil {
			return nil, fmt.Errorf("failed to scan memory entry: %w", err)
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating results: %w", err)
	}

	return entries, nil
}

// DetectMemoryType detects the memory type based on content
func DetectMemoryType(content string) string {
	contentLower := strings.ToLower(content)
	
	// Check for error indicators
	if strings.Contains(contentLower, "error") || strings.Contains(contentLower, "exception") ||
		strings.Contains(contentLower, "fail") || strings.Contains(contentLower, "invalid") {
		return "error"
	}
	
	// Check for decision indicators
	if strings.Contains(contentLower, "decided") || strings.Contains(contentLower, "chosen") ||
		strings.Contains(contentLower, "selected") || strings.Contains(contentLower, "chose") {
		return "decision"
	}
	
	// Check for fact indicators
	if strings.Contains(contentLower, "fact") || strings.Contains(contentLower, "remember") ||
		strings.Contains(contentLower, "know") || strings.Contains(contentLower, "learned") {
		return "fact"
	}
	
	// Check for preference indicators
	if strings.Contains(contentLower, "prefer") || strings.Contains(contentLower, "like") ||
		strings.Contains(contentLower, "dislike") || strings.Contains(contentLower, "want") {
		return "preference"
	}
	
	// Default to context
	return "context"
}

// Close closes the database connection
func (s *Store) Close() error {
	return s.db.Close()
}