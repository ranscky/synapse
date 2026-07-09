package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
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

// embeddingToBytes serializes a []float32 embedding into a byte slice for
// storage in a BLOB column. Each float32 is encoded as 4 bytes, little-endian.
func embeddingToBytes(embedding []float32) []byte {
	buf := make([]byte, len(embedding)*4)
	for i, v := range embedding {
		bits := math.Float32bits(v)
		binary.LittleEndian.PutUint32(buf[i*4:], bits)
	}
	return buf
}

// bytesToEmbedding deserializes a byte slice back into a []float32 embedding.
func bytesToEmbedding(data []byte) []float32 {
	if len(data) == 0 {
		return nil
	}
	embedding := make([]float32, len(data)/4)
	for i := range embedding {
		bits := binary.LittleEndian.Uint32(data[i*4:])
		embedding[i] = math.Float32frombits(bits)
	}
	return embedding
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

	// SQLite's :memory: database is per-connection, not per-process. Without
	// this, concurrent requests can cause Go's database/sql to open additional
	// connections, each getting its own empty, schema-less in-memory database
	// (since initSchema only ran against the first connection). Forcing a
	// single connection keeps :memory: behaving as one shared database.
	if dbPath == ":memory:" {
		db.SetMaxOpenConns(1)
	}

	store := &Store{db: db}
	
	// Initialize database schema
	if err := store.initSchema(); err != nil {
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Set file permissions to 0600. Must happen after initSchema(), since
	// database/sql connections are lazy -- sql.Open doesn't touch disk, and
	// the SQLite file isn't actually created until the first real query
	// (initSchema's CREATE TABLE). Chmod'ing before that point always
	// failed with "no such file or directory" on a fresh install.
	if dbPath != ":memory:" {
    	if err := os.Chmod(dbPath, 0600); err != nil {
        	slog.Warn("Failed to set database file permissions", "error", err)
		}
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
		importance REAL DEFAULT 0.0,
		embedding BLOB
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
	// Sanitize content before writing
	sanitizedContent := s.Sanitize(entry.Content)
	
	// Log warning if content was sanitized
	if sanitizedContent == "[SANITIZED]" && entry.Content != "[SANITIZED]" {
		slog.Warn("Memory content sanitized due to prompt injection", "memory_id", entry.ID)
	}
	
	entry.Content = sanitizedContent

	// Truncate content if it exceeds 2048 bytes (double-check after sanitization)
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
	INSERT OR REPLACE INTO memories (id, session_id, content, memory_type, timestamp, importance, embedding)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	`

	var embeddingBytes []byte
	if len(entry.Embedding) > 0 {
		embeddingBytes = embeddingToBytes(entry.Embedding)
	}

	_, err := s.db.ExecContext(ctx, insertQuery, entry.ID, entry.SessionID, entry.Content, entry.MemoryType, entry.Timestamp, entry.Importance, embeddingBytes)
	if err != nil {
		return fmt.Errorf("failed to insert memory: %w", err)
	}

	return nil
}

// Search performs real semantic search using cosine similarity against
// queryEmbedding. Candidates are pulled from SQLite (filtered by session,
// not yet ranked), similarity is computed in Go against each candidate's
// stored embedding, then results are sorted by similarity descending and
// truncated to topK.
//
// This is an application-level implementation, not a native sqlite-vec
// vector index - fine for per-session memory stores (realistically tens to
// a few hundred entries), but doesn't scale the way a real vector index
// would for very large stores. Noted as a future upgrade, not pretended
// away.
func (s *Store) Search(ctx context.Context, queryEmbedding []float32, sessionID string, topK int) ([]MemoryEntry, error) {
	if topK <= 0 {
		topK = 20 // Default to 20 if not specified
	}

	// Pull all candidates for the session - we need the full set to rank by
	// similarity, not just the most recent topK (recency and relevance are
	// different things; limiting here before ranking would silently throw
	// away the most semantically relevant older entries).
	searchQuery := `
	SELECT id, session_id, content, memory_type, timestamp, importance, embedding
	FROM memories
	WHERE session_id = ?
	`

	rows, err := s.db.QueryContext(ctx, searchQuery, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to search memories: %w", err)
	}
	defer rows.Close()

	var entries []MemoryEntry
	for rows.Next() {
		var entry MemoryEntry
		var embeddingBytes []byte
		err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType, &entry.Timestamp, &entry.Importance, &embeddingBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to scan memory entry: %w", err)
		}
		entry.Embedding = bytesToEmbedding(embeddingBytes)
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating results: %w", err)
	}

	// If there's no query embedding to compare against, fall back to
	// recency ordering (the old behavior) rather than returning an
	// arbitrary or undefined order.
	if len(queryEmbedding) == 0 {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Timestamp.After(entries[j].Timestamp)
		})
		if len(entries) > topK {
			entries = entries[:topK]
		}
		return entries, nil
	}

	// Rank by cosine similarity against the query embedding.
	type scoredEntry struct {
		entry      MemoryEntry
		similarity float64
	}
	scored := make([]scoredEntry, 0, len(entries))
	for _, e := range entries {
		sim := cosineSimilarity(queryEmbedding, e.Embedding)
		scored = append(scored, scoredEntry{entry: e, similarity: sim})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].similarity > scored[j].similarity
	})

	if len(scored) > topK {
		scored = scored[:topK]
	}

	results := make([]MemoryEntry, len(scored))
	for i, s := range scored {
		results[i] = s.entry
	}

	return results, nil
}

// cosineSimilarity computes the cosine similarity between two vectors.
// Duplicated from internal/scorer rather than imported, since internal/
// scorer imports internal/store - importing scorer here would create a
// circular import. Keep this in sync with scorer's version if either
// changes; both must use math.Sqrt(normA)*math.Sqrt(normB), not
// normA*normB (a real bug found and fixed in internal/dedup earlier in
// this project for exactly this reason).
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0.0
	}

	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += float64(a[i] * b[i])
		normA += float64(a[i] * a[i])
		normB += float64(b[i] * b[i])
	}

	if normA == 0 || normB == 0 {
		return 0.0
	}

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// GetRecent retrieves recent memory entries for a session
func (s *Store) GetRecent(ctx context.Context, sessionID string, limit int) ([]MemoryEntry, error) {
	if limit <= 0 {
		limit = 100 // Default limit
	}

	
query := `
	SELECT id, session_id, content, memory_type, timestamp, importance, embedding
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
		var embeddingBytes []byte
		err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType, &entry.Timestamp, &entry.Importance, &embeddingBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to scan memory entry: %w", err)
		}
		entry.Embedding = bytesToEmbedding(embeddingBytes)
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

// Delete removes all memories for a specific session
func (s *Store) Delete(ctx context.Context, sessionID string) error {
	query := `DELETE FROM memories WHERE session_id = ?`
	
	_, err := s.db.ExecContext(ctx, query, sessionID)
	if err != nil {
		return fmt.Errorf("failed to delete memories for session %s: %w", sessionID, err)
	}
	
	return nil
}

// CountMemories counts the total number of memories in the store
func (s *Store) CountMemories(ctx context.Context) (int, error) {
	query := `SELECT COUNT(*) FROM memories`
	
	var count int
	err := s.db.QueryRowContext(ctx, query).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count memories: %w", err)
	}
	
	return count, nil
}

// Sanitize sanitizes memory content before writing
func (s *Store) Sanitize(content string) string {
	// Strip null bytes
	content = strings.ReplaceAll(content, "\x00", "")
	
	// Check for prompt injection patterns
	injectionPatterns := []string{
		"ignore previous",
		"ignore all",
		"disregard",
		"you are now",
		"new instructions:",
		"system:",
		"###instruction",
	}
	
	lowerContent := strings.ToLower(content)
	for _, pattern := range injectionPatterns {
		if strings.Contains(lowerContent, pattern) {
			// Log warning with memory ID (content is not logged for security)
			slog.Warn("Prompt injection detected and neutralized", "pattern", pattern)
			return "[SANITIZED]"
		}
	}
	
	// Cap at 2048 bytes (already enforced in Write, but double-check here)
	if len(content) > 2048 {
		// Find the last rune boundary within the limit
		truncated := content[:2048]
		// Ensure we don't cut off a multi-byte UTF-8 character
		for len(truncated) > 0 && !utf8.ValidString(truncated) {
			truncated = truncated[:len(truncated)-1]
		}
		return truncated
	}
	
	return content
}

// Close closes the database connection
func (s *Store) Close() error {
	return s.db.Close()
}
