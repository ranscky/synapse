package trace


import (
	"encoding/json"
	"testing"

	"synapse/internal/scorer"
	"synapse/internal/store"
)

func scoredMemory(id, memType, supersededBy string) scorer.ScoredMemory {
	return scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:           id,
			MemoryType:   memType,
			Content:      "content for " + id,
			SupersededBy: supersededBy,
		},
		Total: 0.5,
	}
}

// scoredMemoryBy is scoredMemory with an agent of its own, for the provenance
// tests: a candidate pulled from a plane carries the agent_id of the edge that
// pushed it, which is the value the trace is supposed to report verbatim.
func scoredMemoryBy(agentID, id, memType string) scorer.ScoredMemory {
	memory := scoredMemory(id, memType, "")
	memory.AgentID = agentID
	return memory
}

// TestNewTraceManifest_ExclusionReasons covers all three exclusion
// branches -- duplicate, budget_exceeded, and superseded -- to prove
// they're distinguishable from each other and don't collide. Before this
// change, a superseded memory (which survives dedup but is filtered out
// ahead of budget.Fill by the caller) would have been mislabeled
// "budget_exceeded", since the old logic only checked dedup membership.
func TestNewTraceManifest_ExclusionReasons(t *testing.T) {
	duplicate := scoredMemory("dup-1", "context", "")
	budgetExceeded := scoredMemory("budget-1", "fact", "")
	superseded := scoredMemory("old-decision", "decision", "new-decision")
	selected := scoredMemory("selected-1", "decision", "")

	allScored := []scorer.ScoredMemory{duplicate, budgetExceeded, superseded, selected}
	// duplicate never made it past dedup; the other three did.
	deduped := []scorer.ScoredMemory{budgetExceeded, superseded, selected}
	// only selected actually made it into the compiled context. superseded
	// is absent here because the caller filters it out before budget.Fill
	// runs, exactly like proxy.go/api.go now do.
	finalSelected := []scorer.ScoredMemory{selected}

	manifest := NewTraceManifest(
		"req-1", "decision", 0.9,
		4, 3,
		len(finalSelected),
		0, 3000, 10,
		allScored, deduped, finalSelected,
		"",
	)

	got := make(map[string]TraceMemory, len(manifest.Memories))
	for _, m := range manifest.Memories {
		got[m.ID] = m
	}

	if tm := got["dup-1"]; tm.Included || tm.ExclusionReason != "duplicate" {
		t.Errorf("expected dup-1 excluded as duplicate, got included=%v reason=%q", tm.Included, tm.ExclusionReason)
	}
	if tm := got["budget-1"]; tm.Included || tm.ExclusionReason != "budget_exceeded" {
		t.Errorf("expected budget-1 excluded as budget_exceeded, got included=%v reason=%q", tm.Included, tm.ExclusionReason)
	}
	if tm := got["old-decision"]; tm.Included || tm.ExclusionReason != "superseded" || tm.SupersededBy != "new-decision" {
		t.Errorf("expected old-decision excluded as superseded with SupersededBy=new-decision, got included=%v reason=%q supersededBy=%q",
			tm.Included, tm.ExclusionReason, tm.SupersededBy)
	}
	if tm := got["selected-1"]; !tm.Included || tm.ExclusionReason != "" {
		t.Errorf("expected selected-1 included with no exclusion reason, got included=%v reason=%q", tm.Included, tm.ExclusionReason)
	}
}

// TestNewTraceManifest_SupersededByOmittedWhenNotSuperseded confirms the
// field stays empty (and therefore omitted from JSON via omitempty) for
// every memory that was never marked superseded, whether included or
// excluded for an unrelated reason.
func TestNewTraceManifest_SupersededByOmittedWhenNotSuperseded(t *testing.T) {
	duplicate := scoredMemory("dup-1", "context", "")
	selected := scoredMemory("selected-1", "decision", "")

	manifest := NewTraceManifest(
		"req-2", "decision", 0.9,
		2, 1,
		1,
		0, 3000, 5,
		[]scorer.ScoredMemory{duplicate, selected},
		[]scorer.ScoredMemory{selected},
		[]scorer.ScoredMemory{selected},
		"",
	)

	for _, m := range manifest.Memories {
		if m.SupersededBy != "" {
			t.Errorf("expected empty SupersededBy for %s, got %q", m.ID, m.SupersededBy)
		}
	}
}

// TestNewTraceManifest_AgentProvenance is Phase 11's contract: the trace names
// the agent that wrote each memory, and says whether that agent is someone
// other than the node that assembled the context. All five cases matter, not
// just the happy one -- a blank AgentID is the local SQLite backend's normal
// state (one file is one agent), so it has to read as "unattributed" rather
// than as "another agent's memory", which would flag every standalone node's
// own memories as cross-agent.
func TestNewTraceManifest_AgentProvenance(t *testing.T) {
	tests := []struct {
		name           string
		memoryAgent    string
		localAgent     string
		wantAgentID    string
		wantCrossAgent bool
	}{
		{"another agent's memory is cross-agent", "agent_a", "agent_b", "agent_a", true},
		{"this node's own memory is not", "agent_b", "agent_b", "agent_b", false},
		{"an unattributed memory is not cross-agent", "", "agent_b", "", false},
		{"a standalone node reports nothing as cross-agent", "", "", "", false},
		{"a plane memory is cross-agent even for a node with no id", "agent_a", "", "agent_a", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := scoredMemoryBy(tt.memoryAgent, "mem-1", "fact")

			manifest := NewTraceManifest(
				"req-agent", "generic", 0.9,
				1, 1, 1,
				0, 3000, 5,
				[]scorer.ScoredMemory{memory},
				[]scorer.ScoredMemory{memory},
				[]scorer.ScoredMemory{memory},
				tt.localAgent,
			)

			if len(manifest.Memories) != 1 {
				t.Fatalf("got %d traced memories, want 1", len(manifest.Memories))
			}

			got := manifest.Memories[0]
			if got.AgentID != tt.wantAgentID {
				t.Errorf("AgentID = %q, want %q", got.AgentID, tt.wantAgentID)
			}
			if got.CrossAgent != tt.wantCrossAgent {
				t.Errorf("CrossAgent = %v, want %v", got.CrossAgent, tt.wantCrossAgent)
			}
		})
	}
}

// TestTraceMemory_ProvenanceAlwaysMarshaled covers the other half of the
// contract: agent_id and cross_agent are on every memory entry in the trace
// JSON, including the ones that are empty or false. They deliberately carry no
// omitempty -- a trace reader has to be able to tell "this memory came from
// agent_a" from "this memory came from nobody named", and an absent key says
// neither.
func TestTraceMemory_ProvenanceAlwaysMarshaled(t *testing.T) {
	cross := scoredMemoryBy("agent_a", "cross-1", "decision")
	local := scoredMemory("local-1", "fact", "")

	manifest := NewTraceManifest(
		"req-json", "generic", 0.9,
		2, 2, 2,
		0, 3000, 5,
		[]scorer.ScoredMemory{cross, local},
		[]scorer.ScoredMemory{cross, local},
		[]scorer.ScoredMemory{cross, local},
		"agent_b",
	)

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("failed to marshal trace manifest: %v", err)
	}

	var decoded struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal trace manifest: %v", err)
	}
	if len(decoded.Memories) != 2 {
		t.Fatalf("got %d memory entries in JSON, want 2", len(decoded.Memories))
	}

	byID := make(map[string]map[string]interface{}, len(decoded.Memories))
	for _, memory := range decoded.Memories {
		id, _ := memory["id"].(string)
		byID[id] = memory
		for _, key := range []string{"agent_id", "cross_agent"} {
			if _, ok := memory[key]; !ok {
				t.Errorf("memory %s is missing %q in the trace JSON", id, key)
			}
		}
	}

	if got := byID["cross-1"]["agent_id"]; got != "agent_a" {
		t.Errorf("cross-1 agent_id = %v, want \"agent_a\"", got)
	}
	if got := byID["cross-1"]["cross_agent"]; got != true {
		t.Errorf("cross-1 cross_agent = %v, want true", got)
	}
	if got := byID["local-1"]["agent_id"]; got != "" {
		t.Errorf("local-1 agent_id = %v, want \"\"", got)
	}
	if got := byID["local-1"]["cross_agent"]; got != false {
		t.Errorf("local-1 cross_agent = %v, want false", got)
	}
}