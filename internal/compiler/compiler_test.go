package compiler

import (
	"encoding/json"
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

// TestCompile_TraceCarriesConflictFields is Phase 14's proof that the store's
// conflict marking survives the whole compile path -- candidate list, dedup,
// scoring, budget, trace -- rather than only trace.NewTraceManifest in
// isolation. Both memories arrive from the plane exactly as PGStore.Search and
// pgread.scanEntry return them after a Phase 13 detection: the older one a
// superseded candidate naming the newer, the newer one conflicting and naming
// the older. Compile decides nothing about either field and has no code of its
// own for them; it only has to carry both through unchanged, which is what the
// JSON assertions below pin down.
func TestCompile_TraceCarriesConflictFields(t *testing.T) {
	older := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:             "pg-1",
			SessionID:      "session-a",
			Content:        "We decided to use Postgres",
			MemoryType:     "decision",
			Timestamp:      time.Now().Add(-30 * time.Minute),
			AgentID:        "agent_a",
			ConflictStatus: store.ConflictStatusSupersededCandidate,
			ConflictWithID: "my-1",
		},
		// Already demoted by the scorer's conflict penalty, which is what a
		// superseded candidate's Total looks like by the time a trace exists.
		Total: 0.45,
	}
	newer := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:             "my-1",
			SessionID:      "session-b",
			Content:        "We decided to use MySQL",
			MemoryType:     "decision",
			Timestamp:      time.Now().Add(-5 * time.Minute),
			AgentID:        "agent_b",
			ConflictStatus: store.ConflictStatusConflict,
			ConflictWithID: "pg-1",
		},
		Total: 0.9,
	}

	result := Compile(
		[]scorer.ScoredMemory{newer, older},
		"what did we decide about the database?",
		"req-conflict",
		"generic",
		0.5,
		2,
		2,
		3000,
		10,
		[]scorer.ScoredMemory{newer, older},
		[]scorer.ScoredMemory{newer, older},
		"agent_b", // this node is agent_b, so agent_a's memory is cross-agent too
	)

	data, err := json.Marshal(result.Trace)
	if err != nil {
		t.Fatalf("failed to marshal trace: %v", err)
	}

	var decoded struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal trace: %v", err)
	}

	byID := make(map[string]map[string]interface{}, len(decoded.Memories))
	for _, memory := range decoded.Memories {
		id, _ := memory["id"].(string)
		byID[id] = memory
	}

	for _, tc := range []struct {
		id      string
		status  string
		withID  string
		agentID string
	}{
		{"pg-1", store.ConflictStatusSupersededCandidate, "my-1", "agent_a"},
		{"my-1", store.ConflictStatusConflict, "pg-1", "agent_b"},
	} {
		entry, ok := byID[tc.id]
		if !ok {
			t.Fatalf("%s is missing from the trace", tc.id)
		}
		if got := entry["conflict_status"]; got != tc.status {
			t.Errorf("%s conflict_status = %v, want %q", tc.id, got, tc.status)
		}
		if got := entry["conflict_with_id"]; got != tc.withID {
			t.Errorf("%s conflict_with_id = %v, want %q", tc.id, got, tc.withID)
		}
		// The conflict pair is additive: the Phase 11 provenance fields the
		// same entry already carried are untouched by it.
		if got := entry["agent_id"]; got != tc.agentID {
			t.Errorf("%s agent_id = %v, want %q", tc.id, got, tc.agentID)
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