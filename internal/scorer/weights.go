package scorer

import (
	"synapse/internal/classifier"
)

// MemoryType represents the type of memory entry
type MemoryType string

const (
	Decision    MemoryType = "decision"
	Error       MemoryType = "error"
	Fact        MemoryType = "fact"
	Preference  MemoryType = "preference"
	Context     MemoryType = "context"
)

// Default weights for each factor
const (
	DefaultWeightSemanticSimilarity = 0.4
	DefaultWeightRecency            = 0.1
	DefaultWeightImportance         = 0.3
	DefaultWeightTaskAlignment      = 0.2
)

// DefaultRecencyHalfLifeHours is the fixed time scale recency decay is
// anchored to when Weights.RecencyHalfLifeHours isn't explicitly set (the
// zero value, which every existing GetWeights(...) call site produces
// today). 24 hours: a memory from exactly one day ago scores R=0.5, two
// days ago R=0.25, a week ago R≈0.02 -- chosen to match the timescale of a
// typical multi-session coding conversation (hours to a few days), not
// weeks or months.
const DefaultRecencyHalfLifeHours = 24.0

// TaskWeights defines the weight mapping for intent × memory type combinations
var TaskWeights = map[classifier.Intent]map[MemoryType]float64{
	classifier.Debug: {
		Decision:   1.0,
		Error:      1.0,
		Fact:       0.6,
		Preference: 0.1,
		Context:    0.4,
	},
	classifier.Plan: {
		Decision:   1.0,
		Fact:       0.8,
		Error:      0.3,
		Preference: 0.3,
		Context:    0.7,
	},
	classifier.Code: {
		Decision:   0.8,
		Fact:       0.7,
		Error:      0.6,
		Preference: 0.2,
		Context:    0.6,
	},
	classifier.Write: {
		Decision:   0.4,
		Fact:       0.6,
		Error:      0.1,
		Preference: 0.7,
		Context:    0.8,
	},
	classifier.Generic: {
		Decision:   0.5,
		Error:      0.5,
		Fact:       0.5,
		Preference: 0.5,
		Context:    0.5,
	},
}

// ImportanceWeights defines the importance lookup table
var ImportanceWeights = map[MemoryType]float64{
	Decision:   1.0,
	Error:      0.9,
	Fact:       0.7,
	Context:    0.5,
	Preference: 0.3,
}

// GetWeights returns the configured weights, using defaults if not specified
func GetWeights(semantic, recency, importance, taskAlignment float64) Weights {
	return Weights{
		SemanticSimilarity: semantic,
		Recency:            recency,
		Importance:         importance,
		TaskAlignment:      taskAlignment,
	}
}

// Weights holds the scoring weights for each factor
type Weights struct {
	SemanticSimilarity   float64
	Recency              float64
	Importance           float64
	TaskAlignment        float64
	RecencyHalfLifeHours float64 // 0 (zero value) means "use DefaultRecencyHalfLifeHours"
	// ConflictScorePenalty multiplies the Total of a memory another agent has
	// contradicted and that is therefore a candidate to be superseded (see
	// store.ConflictStatusSupersededCandidate). 0 (the zero value, which every
	// existing GetWeights(...) call site produces) means
	// "use DefaultConflictScorePenalty", so those call sites keep scoring exactly
	// as they did before conflicts existed.
	ConflictScorePenalty float64
}

// DefaultConflictScorePenalty is the multiplier a superseded candidate's Total is
// scaled by when Weights.ConflictScorePenalty is not set: 0.5 halves it, which is
// enough to put a contradicted memory behind the newer memory that named it while
// leaving it in the pool and in the ranking. It is a starting guess like the
// supersession band, not a tuned value, and it must stay in step with config's
// conflict-score-penalty default -- the same literal-in-two-places arrangement
// conflict.DefaultJaccardThreshold has with config's conflict-jaccard-threshold,
// because config cannot be imported here without pulling a YAML file and the
// whole v1 configuration surface into the scorer.
const DefaultConflictScorePenalty = 0.5

// conflictPenalty returns the penalty this scorer's weights ask for, resolving the
// unset case to DefaultConflictScorePenalty.
func (w Weights) conflictPenalty() float64 {
	if w.ConflictScorePenalty <= 0 {
		return DefaultConflictScorePenalty
	}
	return w.ConflictScorePenalty
}

// GetTaskAlignmentWeight returns the task alignment weight for a given intent and memory type
func GetTaskAlignmentWeight(intent classifier.Intent, memoryType MemoryType) float64 {
	if weights, ok := TaskWeights[intent]; ok {
		if weight, ok := weights[memoryType]; ok {
			return weight
		}
	}
	// Fallback to generic weights
	if weights, ok := TaskWeights[classifier.Generic]; ok {
		if weight, ok := weights[memoryType]; ok {
			return weight
		}
	}
	return 0.5 // Ultimate fallback
}
