
package supersession

import (
	"testing"

	"synapse/internal/store"
)

func TestHasContradictionSignal(t *testing.T) {
	positives := []string{
		"we switched to MongoDB instead of PostgreSQL",
		"Instead of Postgres we now use Mongo",
		"we no longer use the old auth system",
		"Correction: the deploy target is us-east-1",
		"we migrated from EC2 to Fargate",
	}
	for _, content := range positives {
		if !HasContradictionSignal(content) {
			t.Errorf("expected contradiction signal in %q, found none", content)
		}
	}

	negatives := []string{
		"we decided to use PostgreSQL for the database",
		"the deploy target is us-east-1",
		"Postgres is used for billing, MongoDB for app data",
	}
	for _, content := range negatives {
		if HasContradictionSignal(content) {
			t.Errorf("expected no contradiction signal in %q, but found one", content)
		}
	}
}

// vecA and vecB are unit vectors 45 degrees apart, giving a cosine
// similarity of ~0.707 -- comfortably inside a typical supersession band
// like the default (0.5-0.90) without being a near-duplicate.
var (
	vecA = []float32{1, 0}
	vecB = []float32{0.70710678, 0.70710678}
)

func baseMemory(id, sessionID, memType, content string, embedding []float32) store.MemoryEntry {
	return store.MemoryEntry{
		ID:         id,
		SessionID:  sessionID,
		MemoryType: memType,
		Content:    content,
		Embedding:  embedding,
	}
}

func TestFindSupersededCandidate_TruePositive(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "decision", "we use PostgreSQL", vecA)
	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", vecB)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "old" {
		t.Errorf("expected old memory to be superseded, got %q", got)
	}
}

func TestFindSupersededCandidate_NoContradictionSignal(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "decision", "we use PostgreSQL", vecA)
	// Same type, same similarity band, but no explicit contradiction phrase.
	newMem := baseMemory("new", "sess-1", "decision", "MongoDB also works well for analytics", vecB)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession without a contradiction signal, got %q", got)
	}
}

func TestFindSupersededCandidate_DifferentMemoryType(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "fact", "we use PostgreSQL", vecA)
	// Contradiction signal present, similarity in band, but types differ.
	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", vecB)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession across different memory types, got %q", got)
	}
}

func TestFindSupersededCandidate_DifferentSession(t *testing.T) {
	oldMem := baseMemory("old", "sess-OTHER", "decision", "we use PostgreSQL", vecA)
	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", vecB)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession across different sessions, got %q", got)
	}
}

func TestFindSupersededCandidate_AlreadySuperseded(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "decision", "we use PostgreSQL", vecA)
	oldMem.SupersededBy = "someone-else"
	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", vecB)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession of an already-superseded memory, got %q", got)
	}
}

func TestFindSupersededCandidate_SimilarityTooLow(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "decision", "we use PostgreSQL", vecA)
	// Orthogonal vector: similarity 0.0, below any reasonable band minimum.
	unrelated := []float32{0, 1}
	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", unrelated)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession when similarity is below the band minimum, got %q", got)
	}
}

func TestFindSupersededCandidate_SimilarityTooHigh(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "decision", "we use PostgreSQL", vecA)
	// Identical embedding: similarity 1.0, above the band maximum --
	// dedup's job, not supersession's.
	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", vecA)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession when similarity is above the band maximum, got %q", got)
	}
}

func TestFindSupersededCandidate_IneligibleMemoryType(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "context", "we use PostgreSQL", vecA)
	newMem := baseMemory("new", "sess-1", "context", "we switched to MongoDB instead of PostgreSQL", vecB)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession for a memory type outside eligibleMemoryTypes, got %q", got)
	}
}

func TestFindSupersededCandidate_PicksMostSimilarAmongMultiple(t *testing.T) {
	// Two same-type, same-session, in-band candidates. closer should win
	// over farther, even though both individually qualify.
	closer := baseMemory("closer", "sess-1", "decision", "we use PostgreSQL for the primary DB", vecB)
	fartherVec := []float32{0.6, 0.8}
	farther := baseMemory("farther", "sess-1", "decision", "we use PostgreSQL somewhere else", fartherVec)

	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", vecB)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{farther, closer}, 0.0, 1.0)
	if got != "closer" {
		t.Errorf("expected the most similar candidate (%q) to be picked, got %q", "closer", got)
	}
}

func TestFindSupersededCandidate_NoEmbeddingOnNewMemory(t *testing.T) {
	oldMem := baseMemory("old", "sess-1", "decision", "we use PostgreSQL", vecA)
	newMem := baseMemory("new", "sess-1", "decision", "we switched to MongoDB instead of PostgreSQL", nil)

	got := FindSupersededCandidate(newMem, []store.MemoryEntry{oldMem}, 0.5, 0.90)
	if got != "" {
		t.Errorf("expected no supersession when the new memory has no embedding to compare, got %q", got)
	}
}