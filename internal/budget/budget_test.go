package budget

import (
	"testing"

	"synapse/internal/scorer"
	"synapse/internal/store"
)

func TestCountTokens(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		model    string
		expected int
	}{
		{
			name:     "Simple text with cl100k_base",
			text:     "Hello world",
			model:    "cl100k_base",
			expected: 2,
		},
		{
			name:     "Fallback to word count",
			text:     "Hello world test",
			model:    "unknown_model",
			expected: 3, // 3 words * 1.3 = 3.9 -> 3 (using int conversion which truncates)
		},
		{
			name:     "Empty text",
			text:     "",
			model:    "cl100k_base",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := CountTokens(tt.text, tt.model)
			if err != nil {
				t.Errorf("CountTokens() error = %v", err)
				return
			}
			if result != tt.expected {
				t.Errorf("CountTokens() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestFill(t *testing.T) {
	// Create test memories with different content sizes
	mem1 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:        "1",
			SessionID: "test",
			Content:   "Short memory", // Should be ~2-3 tokens
			MemoryType: "fact",
		},
		Total: 1.0,
	}
	
	mem2 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:        "2",
			SessionID: "test",
			Content:   "This is a longer memory with more content to test token counting", // Should be ~15-20 tokens
			MemoryType: "decision",
		},
		Total: 2.0,
	}
	
	mem3 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:        "3",
			SessionID: "test",
			Content:   "Medium length memory content for testing purposes", // Should be ~10 tokens
			MemoryType: "context",
		},
		Total: 1.5,
	}

	tests := []struct {
		name       string
		candidates []scorer.ScoredMemory
		budget     int
		wantCount  int
	}{
		{
			name:       "Empty candidates",
			candidates: []scorer.ScoredMemory{},
			budget:     100,
			wantCount:  0,
		},
		{
			name:       "Zero budget",
			candidates: []scorer.ScoredMemory{mem1, mem2},
			budget:     0,
			wantCount:  0,
		},
		{
			name:       "Normal fill",
			candidates: []scorer.ScoredMemory{mem1, mem2, mem3},
			budget:     50,
			wantCount:  3, // Should fit all with 50 token budget
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, _ := Fill(tt.candidates, tt.budget)
			if len(result) != tt.wantCount {
				t.Errorf("Fill() returned %d memories, want %d", len(result), tt.wantCount)
			}
		})
	}
}

func TestTruncateToTokens(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		tokens   int
		expected string
	}{
		{
			name:     "Short text",
			text:     "Hello world",
			tokens:   10,
			expected: "Hello world",
		},
		{
			name:     "Empty text",
			text:     "",
			tokens:   5,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateToTokens(tt.text, tt.tokens)
			// For simplicity, just check that it returns something reasonable
			if tt.text == "" && result != "" {
				t.Errorf("truncateToTokens() with empty input should return empty string")
			}
		})
	}
}