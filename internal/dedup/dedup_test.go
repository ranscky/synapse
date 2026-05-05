package dedup

import (
	"reflect"
	"testing"
	"time"

	"synapse/internal/scorer"
	"synapse/internal/store"
)

func TestDeduplicate(t *testing.T) {
	// Create test memories with embeddings
	mem1 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:        "1",
			SessionID: "test",
			Content:   "Test memory 1",
			MemoryType: "fact",
			Timestamp: time.Now(),
		},
		ScoreS: 0.8,
		ScoreR: 0.7,
		ScoreI: 0.6,
		ScoreT: 0.5,
		Total:  2.6,
	}
	
	// Create nearly identical embedding to mem1 (high similarity)
	mem2 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:        "2",
			SessionID: "test",
			Content:   "Test memory 2",
			MemoryType: "fact",
			Timestamp: time.Now(),
		},
		ScoreS: 0.81,
		ScoreR: 0.69,
		ScoreI: 0.61,
		ScoreT: 0.49,
		Total:  2.61,
	}
	
	// Create distinct memory with very different embedding
	mem3 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:        "3",
			SessionID: "test",
			Content:   "Distinct memory",
			MemoryType: "decision",
			Timestamp: time.Now(),
		},
		ScoreS: 0.2,
		ScoreR: 0.3,
		ScoreI: 0.4,
		ScoreT: 0.1,
		Total:  1.0,
	}
	
	// Set embeddings - mem1 and mem2 should be similar, mem3 should be different
	mem1.Embedding = []float32{1.0, 0.0, 0.0, 0.0}
	mem2.Embedding = []float32{0.95, 0.05, 0.0, 0.0} // Very similar to mem1
	mem3.Embedding = []float32{0.0, 0.0, 1.0, 0.0}   // Different from both
	
	tests := []struct {
		name      string
		input     []scorer.ScoredMemory
		threshold float64
		expected  []scorer.ScoredMemory
	}{
		{
			name:      "Empty slice",
			input:     []scorer.ScoredMemory{},
			threshold: 0.92,
			expected:  []scorer.ScoredMemory{},
		},
		{
			name:      "No duplicates",
			input:     []scorer.ScoredMemory{mem1, mem3},
			threshold: 0.92,
			expected:  []scorer.ScoredMemory{mem1, mem3},
		},
		{
			name:      "With duplicates removed",
			input:     []scorer.ScoredMemory{mem1, mem2, mem3},
			threshold: 0.92,
			expected:  []scorer.ScoredMemory{mem1, mem3}, // mem2 should be removed as duplicate of mem1
		},
		{
			name:      "Low threshold but still some duplicates",
			input:     []scorer.ScoredMemory{mem1, mem2, mem3},
			threshold: 0.9, // Higher threshold to actually keep mem2
			expected:  []scorer.ScoredMemory{mem1, mem3}, // mem2 should still be removed due to similarity with mem1
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Deduplicate(tt.input, tt.threshold)
			
			// Compare lengths first
			if len(result) != len(tt.expected) {
				t.Errorf("Expected %d memories, got %d", len(tt.expected), len(result))
				return
			}
			
			// Compare IDs to verify correct memories were kept
			expectedIDs := make(map[string]bool)
			for _, mem := range tt.expected {
				expectedIDs[mem.ID] = true
			}
			
			actualIDs := make(map[string]bool)
			for _, mem := range result {
				actualIDs[mem.ID] = true
			}
			
			if !reflect.DeepEqual(expectedIDs, actualIDs) {
				t.Errorf("Expected IDs %v, got %v", expectedIDs, actualIDs)
			}
		})
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a    []float32
		b    []float32
		want float64
	}{
		{
			name: "Identical vectors",
			a:    []float32{1, 0, 0},
			b:    []float32{1, 0, 0},
			want: 1.0,
		},
		{
			name: "Orthogonal vectors",
			a:    []float32{1, 0, 0},
			b:    []float32{0, 1, 0},
			want: 0.0,
		},
		{
			name: "Opposite vectors",
			a:    []float32{1, 0, 0},
			b:    []float32{-1, 0, 0},
			want: -1.0,
		},
		{
			name: "Different lengths",
			a:    []float32{1, 0},
			b:    []float32{1, 0, 0},
			want: 0.0,
		},
		{
			name: "Empty vectors",
			a:    []float32{},
			b:    []float32{},
			want: 0.0,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("cosineSimilarity() = %v, want %v", got, tt.want)
			}
		})
	}
}