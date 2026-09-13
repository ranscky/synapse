package dedup

import (
	"synapse/internal/scorer"
	"synapse/internal/store"
)

// Deduplicate removes near-duplicate memories based on cosine similarity threshold
func Deduplicate(scored []scorer.ScoredMemory, threshold float64) []scorer.ScoredMemory {
	if len(scored) == 0 {
		return []scorer.ScoredMemory{}
	}

	// Result slice to hold non-duplicated memories
	result := make([]scorer.ScoredMemory, 0, len(scored))

	// For each memory, check similarity against already accepted memories
	for _, current := range scored {
		// Check if current memory is similar to any already accepted memory
		isDuplicate := false
		for _, accepted := range result {
			similarity := store.CosineSimilarity(current.Embedding, accepted.Embedding)
			if similarity > threshold {
				isDuplicate = true
				break
			}
		}

		// If not a duplicate, add to result
		if !isDuplicate {
			result = append(result, current)
		}
	}

	return result
}