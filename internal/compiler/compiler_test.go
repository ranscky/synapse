package compiler

import (
	"strings"
	"testing"
	"time"

	"synapse/internal/scorer"
	"synapse/internal/store"
	"synapse/internal/trace"
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
			expectedLength:  1, // memories folded into a single consolidated user message
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
			result := Compile(
				tt.selected,
				tt.lastUserMessage,
				"test-request-id",
				"generic",
				0.5,
				len(tt.selected),
				len(tt.selected),
				3000,
				10,
				tt.selected,
				[]scorer.ScoredMemory{mem1},
				"",
			)
			
			if len(result.Messages) != tt.expectedLength {
				t.Errorf("Compile() returned %d messages, want %d", len(result.Messages), tt.expectedLength)
			}
			
			// Check that memories are sorted by timestamp (oldest first)
			if len(result.Messages) > 1 && len(tt.selected) > 1 {
				// Check that the first memory message has content from mem1 (older)
				firstMemMsg := result.Messages[0]
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
			expectedLength: 2, // system + consolidated memory/user message
		},
		{
			name:           "Without system message",
			systemMessage:  "",
			selected:       []scorer.ScoredMemory{mem1},
			lastUserMessage: "User message",
			expectedLength: 1, // consolidated memory/user message
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CompileWithContext(
				tt.systemMessage,
				tt.selected,
				tt.lastUserMessage,
				"test-request-id",
				"generic",
				0.5,
				len(tt.selected),
				len(tt.selected),
				3000,
				10,
				tt.selected,
				[]scorer.ScoredMemory{mem1},
				"",
			)
			
			if len(result.Messages) != tt.expectedLength {
				t.Errorf("CompileWithContext() returned %d messages, want %d", len(result.Messages), tt.expectedLength)
			}
			
			// Check system message presence
			if tt.systemMessage != "" {
				systemMsg := result.Messages[0]
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
	
	result := Compile(
		[]scorer.ScoredMemory{mem1},
		"User message",
		"test-request-id",
		"generic",
		0.5,
		1,
		1,
		3000,
		10,
		[]scorer.ScoredMemory{mem1},
		[]scorer.ScoredMemory{mem1},
		"",
	)
	
	if len(result.Messages) < 1 {
		t.Fatalf("Expected at least one message")
	}
	
	// Check that the memory message includes the header
	memoryMsg := result.Messages[0]
	if content, ok := memoryMsg["content"].(string); ok {
		if !contains(content, "[Memory: decision]") {
			t.Errorf("Expected memory message to contain header, got %s", content)
		}
	}
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// TestCompile_TraceAgentProvenance proves the Phase 11 provenance fields make it
// all the way through the real compile path rather than only through
// trace.NewTraceManifest in isolation: a candidate that carries agent_a's
// agent_id (what the plane returns for a memory another edge pushed) has to come
// out of Compile's trace still naming agent_a and still flagged cross-agent,
// next to a memory of this node's own that is neither. Dedup and the token
// budget sit between the candidate list and the trace, so this is also the
// assertion that both preserve the embedded MemoryEntry's agent.
func TestCompile_TraceAgentProvenance(t *testing.T) {
	fromAgentA := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:         "plane-1",
			SessionID:  "sess-agent-a",
			Content:    "the org decided the retrieval pipeline embeds once per turn",
			MemoryType: "decision",
			Timestamp:  time.Now().Add(-30 * time.Minute),
			AgentID:    "agent_a",
		},
		Total: 0.9,
	}
	local := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:         "local-1",
			SessionID:  "sess-agent-b",
			Content:    "a local note about the retrieval pipeline",
			MemoryType: "fact",
			Timestamp:  time.Now().Add(-5 * time.Minute),
		},
		Total: 0.4,
	}

	result := Compile(
		[]scorer.ScoredMemory{fromAgentA, local},
		"what did we decide about the retrieval pipeline?",
		"req-agent",
		"generic",
		0.5,
		2,
		2,
		3000,
		10,
		[]scorer.ScoredMemory{fromAgentA, local},
		[]scorer.ScoredMemory{fromAgentA, local},
		"agent_b", // this node is agent_b
	)

	byID := make(map[string]trace.TraceMemory, len(result.Trace.Memories))
	for _, memory := range result.Trace.Memories {
		byID[memory.ID] = memory
	}

	if got, ok := byID["plane-1"]; !ok {
		t.Fatal("plane-1 is missing from the trace")
	} else if got.AgentID != "agent_a" || !got.CrossAgent {
		t.Errorf("plane-1: agent_id=%q cross_agent=%v, want agent_a/true", got.AgentID, got.CrossAgent)
	}

	if got, ok := byID["local-1"]; !ok {
		t.Fatal("local-1 is missing from the trace")
	} else if got.AgentID != "" || got.CrossAgent {
		t.Errorf("local-1: agent_id=%q cross_agent=%v, want \"\"/false", got.AgentID, got.CrossAgent)
	}
}