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
	ID             string    `json:"id"`                         // UUID
	SessionID      string    `json:"session_id"`                 // Session identifier
	Content        string    `json:"content"`                    // Memory content (max 2048 bytes)
	MemoryType     string    `json:"memory_type"`                // "decision"|"fact"|"error"|"preference"|"context"
	Timestamp      time.Time `json:"timestamp"`                  // Creation timestamp
	Importance     float64   `json:"importance,omitempty"`       // Importance score
	Embedding      []float32 `json:"embedding,omitempty"`        // 384-dim embedding vector
	SupersededBy   string    `json:"superseded_by,omitempty"`    // ID of the memory that superseded this one, if any. Empty means still active/current. Populated by a later write, never set at the same time a memory is first created.
	SyncStatus     string    `json:"sync_status,omitempty"`      // Where this memory currently lives, relative to a control plane: "local_only" | "sync_pending" | "synced". A memory written by the standalone v1 binary has never left the machine, so its zero value is normalized to "local_only" on write; a memory written through a tenant's Postgres schema is already on the plane, so its zero value is normalized to "synced". See the SyncStatus* constants.
	AgentID        string    `json:"agent_id,omitempty"`         // The agent that wrote this memory: the node that pushed it to a control plane, or this node on a locally written row. Populated on every read that can carry it -- a candidate pulled from a plane carries the agent_id of the edge that pushed it -- and left empty by the local SQLite backend, whose table has no agent column (a standalone node IS the agent). Never an isolation key: the tenant is always the verified token's schema, never a value from a row.
	Visibility     string    `json:"visibility,omitempty"`       // Who may read this memory: "private" | "team" | "org" (see the Visibility* constants). Enforced by the Postgres backend's Search; the local SQLite backend has no visibility concept at all, because one file is one process is one agent, so a local row is private by construction and this field is inert there. Blank means the column's own default, "org".
	TeamID         string    `json:"team_id,omitempty"`          // The team a "team"-scoped memory belongs to, matched against the reader's own team id. Left empty when the memory is org- or private-scoped. Never an isolation key: the tenant is still the verified token's schema, and a team id can only ever narrow a search inside it.
	ConflictStatus string    `json:"conflict_status,omitempty"`  // Whether a contradiction has been recorded for this memory: one of the ConflictStatus* values. Written by the Postgres backend's write path from the verdict of the ContradictionDetector installed on it -- the detector's verdict is the single source of truth, so a caller-supplied value is not honored -- and left empty by the local SQLite backend, whose table has no such column. ConflictStatusSupersededCandidate is the older memory a newer one contradicts: flagged, kept in the pool, and demoted by the scorer, never dropped.
	ConflictWithID string    `json:"conflict_with_id,omitempty"` // The other half of ConflictStatus: the id of the memory this one disagrees with, so either row read on its own names its counterpart. The older row names the newer one and vice versa. Empty whenever no contradiction has been recorded.
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
		embedding BLOB,
		superseded_by TEXT DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_memories_session_id ON memories(session_id);
	CREATE INDEX IF NOT EXISTS idx_memories_timestamp ON memories(timestamp);
	CREATE INDEX IF NOT EXISTS idx_memories_memory_type ON memories(memory_type);
	`

	_, err := s.db.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	// CREATE TABLE IF NOT EXISTS above is a no-op against a database that
	// already has a `memories` table from before this column existed --
	// which describes every real synapse.db created before this change,
	// including any existing dev database. SQLite has no
	// "ADD COLUMN IF NOT EXISTS", so this attempts the migration
	// unconditionally on every startup and swallows the one specific error
	// that means "already applied" (a previous startup already migrated
	// this file), surfacing anything else as a real failure. The DEFAULT
	// '' backfills existing rows so SupersededBy scans as an empty string
	// rather than needing sql.NullString everywhere that reads it back.
	if _, err := s.db.Exec(`ALTER TABLE memories ADD COLUMN superseded_by TEXT DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("failed to migrate superseded_by column: %w", err)
		}
	}

	// Same migration shape as superseded_by above: run unconditionally, swallow
	// only the "already applied" error. DEFAULT 'local_only' backfills rows
	// written before this column existed, which is accurate -- every one of them
	// predates any control plane this binary could sync with -- and it matches
	// the value Write() normalizes a blank SyncStatus to, so a row reads back
	// identically whether it was migrated or inserted.
	if _, err := s.db.Exec(`ALTER TABLE memories ADD COLUMN sync_status TEXT NOT NULL DEFAULT 'local_only'`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("failed to migrate sync_status column: %w", err)
		}
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

	// A blank SyncStatus is normalized to this backend's own default: a memory
	// written to the local SQLite file has never been sent anywhere, which is
	// exactly what the column's DEFAULT 'local_only' says. Normalizing here
	// (rather than passing "" through) keeps the value a reader gets identical
	// to the value a migrated pre-existing row gets.
	if entry.SyncStatus == "" {
		entry.SyncStatus = SyncStatusLocalOnly
	}

		// Insert memory entry
	insertQuery := `
	INSERT OR REPLACE INTO memories (id, session_id, content, memory_type, timestamp, importance, embedding, superseded_by, sync_status)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	var embeddingBytes []byte
	if len(entry.Embedding) > 0 {
		embeddingBytes = embeddingToBytes(entry.Embedding)
	}

	_, err := s.db.ExecContext(ctx, insertQuery, entry.ID, entry.SessionID, entry.Content, entry.MemoryType, entry.Timestamp, entry.Importance, embeddingBytes, entry.SupersededBy, entry.SyncStatus)
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
//
// agentID, teamID, and currentSessionID exist so this method satisfies the same
// contract as the Postgres backend's Search, and are deliberately ignored here:
// a local SQLite file belongs to exactly one process, one agent, and one
// operator, so there is no tenant, no team, and no other agent whose memories
// could be reached in the first place -- every row in it is already private to
// the only reader it has. Enforcing visibility locally would mean inventing a
// scope the schema does not store (the memories table has no visibility or
// team_id column), so the honest implementation is to accept the parameters and
// not pretend they do something.
func (s *Store) Search(ctx context.Context, queryEmbedding []float32, agentID, teamID, sessionID string, topK int) ([]MemoryEntry, error) {
	if topK <= 0 {
		topK = 20 // Default to 20 if not specified
	}

	// Pull all candidates for the session - we need the full set to rank by
	// similarity, not just the most recent topK (recency and relevance are
	// different things; limiting here before ranking would silently throw
	// away the most semantically relevant older entries).
		searchQuery := `
	SELECT id, session_id, content, memory_type, timestamp, importance, embedding, superseded_by, sync_status
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
		err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType, &entry.Timestamp, &entry.Importance, &embeddingBytes, &entry.SupersededBy, &entry.SyncStatus)
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
		sim := CosineSimilarity(queryEmbedding, e.Embedding)
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

// CosineSimilarity computes the cosine similarity between two vectors.
//
// This used to be duplicated three ways -- separately in internal/scorer,
// internal/dedup, and here -- on the theory that importing this package
// from those would create a circular dependency. It wouldn't have: store
// has no internal dependencies of its own, so it's scorer and dedup that
// should import store's copy, not the other way around. The triplication
// wasn't just redundant, it was actively dangerous: dedup's copy at one
// point used normA*normB instead of math.Sqrt(normA)*math.Sqrt(normB), a
// real bug caused by three independent implementations drifting out of
// sync with no single source of truth to check against. This is now that
// single source of truth -- scorer and dedup both call this directly.
func CosineSimilarity(a, b []float32) float64 {
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
	SELECT id, session_id, content, memory_type, timestamp, importance, embedding, superseded_by, sync_status
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
		err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Content, &entry.MemoryType, &entry.Timestamp, &entry.Importance, &embeddingBytes, &entry.SupersededBy, &entry.SyncStatus)
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

// MarkSuperseded records that oldID has been superseded by newID -- e.g.
// an earlier "we use PostgreSQL" decision replaced by a newer "we migrated
// to MongoDB" one. This is a separate, small UPDATE rather than folded
// into Write(), since Write() always inserts or fully replaces a row for
// a *new* memory, whereas this mutates an *existing* row's superseded_by
// field in place without touching anything else about it.
//
// Returns an error (rather than silently no-op'ing) if oldID doesn't
// exist, so a caller with a bad ID finds out immediately rather than the
// mismatch going unnoticed.
func (s *Store) MarkSuperseded(ctx context.Context, oldID, newID string) error {
	if oldID == "" || newID == "" {
		return fmt.Errorf("MarkSuperseded requires non-empty oldID and newID")
	}

	result, err := s.db.ExecContext(ctx, `UPDATE memories SET superseded_by = ? WHERE id = ?`, newID, oldID)
	if err != nil {
		return fmt.Errorf("failed to mark memory %s as superseded: %w", oldID, err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to confirm supersession update for memory %s: %w", oldID, err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("no memory found with id %s to mark as superseded", oldID)
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
