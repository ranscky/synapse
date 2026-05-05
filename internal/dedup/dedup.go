package dedup

import (
	"synapse/internal/scorer"
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
			similarity := cosineSimilarity(current.Embedding, accepted.Embedding)
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

// cosineSimilarity computes the cosine similarity between two vectors
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0.0
	}

	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += float64(a[i] * b[i])
		normA += float64(a[i] * a[i])
		normB += float64(b[i] * b[i])
	}

	if normA == 0 || normB == 0 {
		return 0.0
	}

	return dotProduct / (float64(normA) * float64(normB))
}