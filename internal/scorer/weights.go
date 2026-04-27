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
	DefaultWeightRecency            = 0.2
	DefaultWeightImportance         = 0.2
	DefaultWeightTaskAlignment      = 0.2
)

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
	SemanticSimilarity float64
	Recency            float64
	Importance         float64
	TaskAlignment      float64
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
