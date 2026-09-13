package scorer

import (
	"context"
	"math"
	"testing"
	"time"

	"synapse/internal/classifier"
	"synapse/internal/store"
)

// TestImportanceCanOutweighModestRecencyGap guards the actual fix: with
// default weights (I=0.3, R=0.1), a higher-importance memory should be
// able to outrank a more recent, lower-importance one over a realistic,
// modest age gap -- not just at extreme, multi-week gaps where recency
// SHOULD dominate (that's correct decay, not a bug). Before the recency
// normalization fix, recency structurally won regardless of gap size,
// because batch-relative min-max normalization always claimed its full
// weight budget; importance's fixed 0-1 lookup table almost never could.
func TestImportanceCanOutweighModestRecencyGap(t *testing.T) {
	now := time.Now()
	embedding := []float32{0.8, 0.6, 0.0}

	fact := store.MemoryEntry{
		ID:         "fact-12h",
		MemoryType: "fact", // importance 0.7
		Timestamp:  now.Add(-12 * time.Hour),
		Embedding:  embedding,
	}
	freshContext := store.MemoryEntry{
		ID:         "context-1h",
		MemoryType: "context", // importance 0.5
		Timestamp:  now.Add(-1 * time.Hour),
		Embedding:  embedding,
	}

	weights := GetWeights(DefaultWeightSemanticSimilarity, DefaultWeightRecency, DefaultWeightImportance, DefaultWeightTaskAlignment)
	scorer := NewScorer(weights, classifier.Generic, 0.0, now)

	ctx := context.Background()
	scored := scorer.Score(ctx, embedding, []store.MemoryEntry{fact, freshContext})

	var factScore, contextScore float64
	for _, s := range scored {
		if s.ID == "fact-12h" {
			factScore = s.Total
		}
		if s.ID == "context-1h" {
			contextScore = s.Total
		}
	}

	t.Logf("fact (importance 0.7, 12h old):     total=%.4f", factScore)
	t.Logf("context (importance 0.5, 1h old):   total=%.4f", contextScore)

	if factScore <= contextScore {
		t.Errorf("importance should outweigh a modest 11-hour recency gap: fact=%.4f, context=%.4f", factScore, contextScore)
	}
}

// TestVeryOldMemoryStillDecays confirms the fixed half-life decay still
// correctly favors a very recent memory over a genuinely stale one (30
// days), even with importance now weighted higher -- this is intentional,
// not a regression. Recency dominating at extreme gaps is correct; only
// batch-relative normalization dominating at ANY gap was the bug.
func TestVeryOldMemoryStillDecays(t *testing.T) {
	now := time.Now()
	embedding := []float32{0.8, 0.6, 0.0}

	oldFact := store.MemoryEntry{
		ID:         "fact-30d",
		MemoryType: "fact",
		Timestamp:  now.Add(-720 * time.Hour),
		Embedding:  embedding,
	}
	freshContext := store.MemoryEntry{
		ID:         "context-1h",
		MemoryType: "context",
		Timestamp:  now.Add(-1 * time.Hour),
		Embedding:  embedding,
	}

	weights := GetWeights(DefaultWeightSemanticSimilarity, DefaultWeightRecency, DefaultWeightImportance, DefaultWeightTaskAlignment)
	scorer := NewScorer(weights, classifier.Generic, 0.0, now)

	ctx := context.Background()
	scored := scorer.Score(ctx, embedding, []store.MemoryEntry{oldFact, freshContext})

	var factScore, contextScore float64
	for _, s := range scored {
		if s.ID == "fact-30d" {
			factScore = s.Total
		}
		if s.ID == "context-1h" {
			contextScore = s.Total
		}
	}

	if factScore >= contextScore {
		t.Errorf("a 30-day-old memory should still lose to a 1-hour-old one -- decay should still function at extreme gaps: fact=%.4f, context=%.4f", factScore, contextScore)
	}
}


// TestScorerUsesInjectedNowNotWallClock is a regression test for the dead
// `now` parameter bug: NewScorer accepted a `now time.Time` constructor
// argument, but scoreMemory and computeRecencyRange both called
// time.Since(memory.Timestamp) directly instead of s.now.Sub(...), so the
// injected clock was decorative -- recency was always computed against
// real wall-clock time regardless of what `now` a caller passed in.
//
// This is proven by picking a fixed reference point years in the past
// (2020-01-01) and a memory timestamped exactly 1 hour before it. If the
// scorer is correctly using s.now, the recency score matches the expected
// half-life decay for a 1-hour-old memory exactly. If it's silently using
// real wall-clock time instead, the memory would appear to be several
// years old and score close to zero.
func TestScorerUsesInjectedNowNotWallClock(t *testing.T) {
	fixedNow := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	embedding := []float32{0.5, 0.5, 0.5}

	memory := store.MemoryEntry{
		ID:         "fixed-point-1h-old",
		MemoryType: "context",
		Timestamp:  fixedNow.Add(-1 * time.Hour),
		Embedding:  embedding,
	}

	weights := GetWeights(DefaultWeightSemanticSimilarity, DefaultWeightRecency, DefaultWeightImportance, DefaultWeightTaskAlignment)
	scorer := NewScorer(weights, classifier.Generic, 0.0, fixedNow)

	ctx := context.Background()
	scored := scorer.Score(ctx, embedding, []store.MemoryEntry{memory})
	if len(scored) != 1 {
		t.Fatalf("expected exactly one scored memory, got %d", len(scored))
	}

	expectedScoreR := math.Exp2(-1.0 / DefaultRecencyHalfLifeHours)

	if math.Abs(scored[0].ScoreR-expectedScoreR) > 1e-9 {
		t.Errorf("recency score should be computed against the injected `now` (%.6f expected for a 1-hour-old memory relative to fixedNow), got %.6f -- "+
			"this large a gap indicates time.Since()/wall-clock time is still being used instead of s.now",
			expectedScoreR, scored[0].ScoreR)
	}
}