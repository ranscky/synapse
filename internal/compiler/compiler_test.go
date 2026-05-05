package compiler

import (
	"strings"
	"testing"
	"time"

	"synapse/internal/scorer"
	"synapse/internal/store"
)

func TestCompile(t *testing.T) {
	// Create test memories
	mem1 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:         "1",
			SessionID:  "test",
			Content:    "First memory content",
			MemoryType: "fact",
			Timestamp:  time.Now().Add(-2 * time.Hour),
		},
		Total: 0.8,
	}
	
	mem2 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:         "2",
			SessionID:  "test",
			Content:    "Second memory content",
			MemoryType: "decision",
			Timestamp:  time.Now().Add(-1 * time.Hour),
		},
		Total: 0.9,
	}
	
	tests := []struct {
		name            string
		selected        []scorer.ScoredMemory
		lastUserMessage string
		expectedLength  int
	}{
		{
			name:            "Normal compilation",
			selected:        []scorer.ScoredMemory{mem1, mem2},
			lastUserMessage: "Last user message",
			expectedLength:  3, // 2 memories + 1 user message
		},
		{
			name:            "No memories",
			selected:        []scorer.ScoredMemory{},
			lastUserMessage: "Just user message",
			expectedLength:  1, // Just user message
		},
		{
			name:            "Empty user message",
			selected:        []scorer.ScoredMemory{mem1},
			lastUserMessage: "",
			expectedLength:  1, // Just the memory
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Compile(tt.selected, tt.lastUserMessage)
			
			if len(result) != tt.expectedLength {
				t.Errorf("Compile() returned %d messages, want %d", len(result), tt.expectedLength)
			}
			
			// Check that memories are sorted by timestamp (oldest first)
			if len(result) > 1 && len(tt.selected) > 1 {
				// Check that the first memory message has content from mem1 (older)
				firstMemMsg := result[0]
				if content, ok := firstMemMsg["content"].(string); ok {
					if !contains(content, "First memory content") {
						t.Errorf("Expected first message to contain 'First memory content', got %s", content)
					}
				}
			}
		})
	}
}

func TestCompileWithContext(t *testing.T) {
	mem1 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:         "1",
			SessionID:  "test",
			Content:    "Memory content",
			MemoryType: "fact",
			Timestamp:  time.Now(),
		},
		Total: 0.8,
	}
	
	tests := []struct {
		name           string
		systemMessage  string
		selected       []scorer.ScoredMemory
		lastUserMessage string
		expectedLength int
	}{
		{
			name:           "With system message",
			systemMessage:  "System instruction",
			selected:       []scorer.ScoredMemory{mem1},
			lastUserMessage: "User message",
			expectedLength: 3, // system + memory + user
		},
		{
			name:           "Without system message",
			systemMessage:  "",
			selected:       []scorer.ScoredMemory{mem1},
			lastUserMessage: "User message",
			expectedLength: 2, // memory + user
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CompileWithContext(tt.systemMessage, tt.selected, tt.lastUserMessage)
			
			if len(result) != tt.expectedLength {
				t.Errorf("CompileWithContext() returned %d messages, want %d", len(result), tt.expectedLength)
			}
			
			// Check system message presence
			if tt.systemMessage != "" {
				systemMsg := result[0]
				if role, ok := systemMsg["role"].(string); !ok || role != "system" {
					t.Errorf("Expected first message to be system role, got %v", role)
				}
			}
		})
	}
}

func TestMemoryHeaders(t *testing.T) {
	mem1 := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:         "1",
			SessionID:  "test",
			Content:    "Test content",
			MemoryType: "decision",
			Timestamp:  time.Now(),
		},
		Total: 0.87,
	}
	
	result := Compile([]scorer.ScoredMemory{mem1}, "User message")
	
	if len(result) < 1 {
		t.Fatalf("Expected at least one message")
	}
	
	// Check that the memory message includes the header
	memoryMsg := result[0]
	if content, ok := memoryMsg["content"].(string); ok {
		if !contains(content, "[Memory | Type: decision | Score: 0.87]") {
			t.Errorf("Expected memory message to contain header, got %s", content)
		}
	}
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

