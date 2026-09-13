package trace


import (
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
	)

	for _, m := range manifest.Memories {
		if m.SupersededBy != "" {
			t.Errorf("expected empty SupersededBy for %s, got %q", m.ID, m.SupersededBy)
		}
	}
}