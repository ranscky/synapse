package scorer

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"synapse/internal/classifier"
	"synapse/internal/store"
)

func TestCosineSimilarity(t *testing.T) {
	// Test identical vectors
	vec1 := []float32{1.0, 0.0, 0.0}
	vec2 := []float32{1.0, 0.0, 0.0}
	similarity := cosineSimilarity(vec1, vec2)
	assert.Equal(t, 1.0, similarity, "Identical vectors should have similarity of 1.0")

	// Test orthogonal vectors
	vec3 := []float32{1.0, 0.0, 0.0}
	vec4 := []float32{0.0, 1.0, 0.0}
	similarity = cosineSimilarity(vec3, vec4)
	assert.Equal(t, 0.0, similarity, "Orthogonal vectors should have similarity of 0.0")

	// Test opposite vectors
	vec5 := []float32{1.0, 0.0, 0.0}
	vec6 := []float32{-1.0, 0.0, 0.0}
	similarity = cosineSimilarity(vec5, vec6)
	assert.Equal(t, -1.0, similarity, "Opposite vectors should have similarity of -1.0")

	// Test different lengths (should return 0)
	vec7 := []float32{1.0, 0.0}
	vec8 := []float32{1.0, 0.0, 0.0}
	similarity = cosineSimilarity(vec7, vec8)
	assert.Equal(t, 0.0, similarity, "Different length vectors should return 0.0")

	// Test empty vectors
	vec9 := []float32{}
	vec10 := []float32{}
	similarity = cosineSimilarity(vec9, vec10)
	assert.Equal(t, 0.0, similarity, "Empty vectors should return 0.0")
}

func TestGetImportanceScore(t *testing.T) {
	// Test known importance values
	assert.Equal(t, 1.0, getImportanceScore(Decision), "Decision should have importance 1.0")
	assert.Equal(t, 0.9, getImportanceScore(Error), "Error should have importance 0.9")
	assert.Equal(t, 0.7, getImportanceScore(Fact), "Fact should have importance 0.7")
	assert.Equal(t, 0.5, getImportanceScore(Context), "Context should have importance 0.5")
	assert.Equal(t, 0.3, getImportanceScore(Preference), "Preference should have importance 0.3")

	// Test unknown type (should return default)
	assert.Equal(t, 0.5, getImportanceScore("unknown"), "Unknown type should return default 0.5")
}

func TestScorerScore(t *testing.T) {
	// Create test memories with known properties
	now := time.Now()
	sessionID := "test-session"

	memories := []store.MemoryEntry{
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "Error occurred during processing",
			MemoryType: "error",
			Timestamp:  now.Add(-1 * time.Hour), // Recent error
			Embedding:  []float32{1.0, 0.0, 0.0}, // High similarity to query
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "User decided to implement feature",
			MemoryType: "decision",
			Timestamp:  now.Add(-2 * time.Hour), // Older decision
			Embedding:  []float32{0.8, 0.2, 0.1}, // Medium similarity
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "General conversation context",
			MemoryType: "context",
			Timestamp:  now.Add(-24 * time.Hour), // Very old context
			Embedding:  []float32{0.1, 0.1, 0.1}, // Low similarity
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "User preference for dark mode",
			MemoryType: "preference",
			Timestamp:  now.Add(-3 * time.Hour), // Medium age
			Embedding:  []float32{0.3, 0.3, 0.3}, // Medium-low similarity
		},
		{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    "Fact about database performance",
			MemoryType: "fact",
			Timestamp:  now.Add(-12 * time.Hour), // Medium age
			Embedding:  []float32{0.6, 0.4, 0.2}, // Medium similarity
		},
	}

	// Query vector similar to first memory
	query := []float32{0.9, 0.1, 0.0}

	// Create scorer with debug intent (high weight for errors)
	weights := GetWeights(0.4, 0.2, 0.2, 0.2)
	scorer := NewScorer(weights, classifier.Debug, now)

	// Score the memories
	ctx := context.Background()
	scored := scorer.Score(ctx, query, memories)

	// Verify we get the same number of results
	assert.Len(t, scored, len(memories), "Should return same number of memories")

	// Verify sorting - recent error should rank highly due to debug intent
	// The first memory should rank higher due to high semantic similarity and error type
	assert.True(t, scored[0].Total >= scored[1].Total, "Memories should be sorted by total score")

	// Verify all scores are in valid ranges (with tolerance for floating point)
	for _, s := range scored {
		assert.True(t, s.ScoreS >= -1.0-1e-6 && s.ScoreS <= 1.0+1e-6, "Semantic similarity should be in [-1,1]")
		assert.True(t, s.ScoreR >= 0.0-1e-6 && s.ScoreR <= 1.0+1e-6, "Recency score should be in [0,1]")
		assert.True(t, s.ScoreI >= 0.0-1e-6 && s.ScoreI <= 1.0+1e-6, "Importance score should be in [0,1]")
		assert.True(t, s.ScoreT >= 0.0-1e-6 && s.ScoreT <= 1.0+1e-6, "Task alignment score should be in [0,1]")
		assert.True(t, s.Total >= 0.0-1e-6, "Total score should be non-negative")
	}
}

func TestScorerEmptyCandidates(t *testing.T) {
	weights := GetWeights(0.4, 0.2, 0.2, 0.2)
	scorer := NewScorer(weights, classifier.Generic, time.Now())
	
	ctx := context.Background()
	scored := scorer.Score(ctx, []float32{1.0}, []store.MemoryEntry{})
	
	assert.Empty(t, scored, "Should return empty slice for empty candidates")
}

func TestGetTaskAlignmentWeight(t *testing.T) {
	// Test known intent and memory type combinations
	weight := GetTaskAlignmentWeight(classifier.Debug, Error)
	assert.Equal(t, 1.0, weight, "Debug intent with Error type should have weight 1.0")

	weight = GetTaskAlignmentWeight(classifier.Plan, Fact)
	assert.Equal(t, 0.8, weight, "Plan intent with Fact type should have weight 0.8")

	weight = GetTaskAlignmentWeight(classifier.Code, Decision)
	assert.Equal(t, 0.8, weight, "Code intent with Decision type should have weight 0.8")

	weight = GetTaskAlignmentWeight(classifier.Write, Preference)
	assert.Equal(t, 0.7, weight, "Write intent with Preference type should have weight 0.7")

	// Test fallback to generic
	weight = GetTaskAlignmentWeight("unknown_intent", Context)
	assert.Equal(t, 0.5, weight, "Unknown intent should fall back to generic weight")

	// Test unknown memory type with known intent
	weight = GetTaskAlignmentWeight(classifier.Debug, "unknown_type")
	assert.Equal(t, 0.5, weight, "Unknown memory type should fall back to generic weight")
}

func TestComputeRecencyRange(t *testing.T) {
	now := time.Now()
	
	memories := []store.MemoryEntry{
		{Timestamp: now.Add(-1 * time.Hour)},
		{Timestamp: now.Add(-5 * time.Hour)},
		{Timestamp: now.Add(-2 * time.Hour)},
	}

	scorer := &Scorer{now: now}
	min, max := scorer.computeRecencyRange(memories)
	
	assert.InDelta(t, 1.0, min, 1e-6, "Minimum hours should be approximately 1.0")
	assert.InDelta(t, 5.0, max, 1e-6, "Maximum hours should be approximately 5.0")

	// Test single memory
	singleMemory := []store.MemoryEntry{{Timestamp: now.Add(-3 * time.Hour)}}
	min, max = scorer.computeRecencyRange(singleMemory)
	assert.InDelta(t, 3.0, min, 1e-6, "Single memory min should be approximately 3.0")
	assert.InDelta(t, 3.0, max, 1e-6, "Single memory max should be approximately 3.0")

	// Test empty slice
	min, max = scorer.computeRecencyRange([]store.MemoryEntry{})
	assert.Equal(t, 0.0, min, "Empty slice min should be 0.0")
	assert.Equal(t, 0.0, max, "Empty slice max should be 0.0")
}
