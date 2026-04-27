package scorer

import (
	"context"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"synapse/internal/classifier"
	"synapse/internal/store"
)

// ScoredMemory represents a memory entry with its computed scores
type ScoredMemory struct {
	store.MemoryEntry
	ScoreS      float64 // Semantic Similarity
	ScoreR      float64 // Recency
	ScoreI      float64 // Importance
	ScoreT      float64 // Task Alignment
	Total       float64 // Combined total score
}

// Scorer handles the 4-factor scoring of memory candidates
type Scorer struct {
	weights Weights
	now     time.Time
	intent  classifier.Intent
}

// NewScorer creates a new scorer with the specified weights and intent
func NewScorer(weights Weights, intent classifier.Intent, now time.Time) *Scorer {
	return &Scorer{
		weights: weights,
		now:     now,
		intent:  intent,
	}
}

// Score computes scores for memory candidates using the 4-factor model
func (s *Scorer) Score(ctx context.Context, query []float32, candidates []store.MemoryEntry) []ScoredMemory {
	if len(candidates) == 0 {
		return []ScoredMemory{}
	}

	// Precompute recency normalization values
	minHours, maxHours := s.computeRecencyRange(candidates)

	// Create scored memories slice
	scoredMemories := make([]ScoredMemory, len(candidates))

	// Use goroutine pool for parallel scoring
	numWorkers := runtime.GOMAXPROCS(0)
	if numWorkers > len(candidates) {
		numWorkers = len(candidates)
	}

	// Create channel for work distribution
	jobs := make(chan int, len(candidates))
	results := make(chan ScoredMemory, len(candidates))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go s.worker(ctx, jobs, results, &wg, query, candidates, minHours, maxHours)
	}

	// Send job indices
	for i := range candidates {
		jobs <- i
	}
	close(jobs)

	// Close results channel when all workers are done
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	i := 0
	for scored := range results {
		scoredMemories[i] = scored
		i++
	}

	// Sort by total score descending, with timestamp tiebreaker
	sort.Slice(scoredMemories, func(i, j int) bool {
		if scoredMemories[i].Total != scoredMemories[j].Total {
			return scoredMemories[i].Total > scoredMemories[j].Total
		}
		// Tiebreaker: newer timestamps first
		return scoredMemories[i].Timestamp.After(scoredMemories[j].Timestamp)
	})

	return scoredMemories
}

// worker processes scoring jobs in parallel
func (s *Scorer) worker(
	ctx context.Context,
	jobs <-chan int,
	results chan<- ScoredMemory,
	wg *sync.WaitGroup,
	query []float32,
	candidates []store.MemoryEntry,
	minHours, maxHours float64,
) {
	defer wg.Done()

	for i := range jobs {
		select {
		case <-ctx.Done():
			return
		default:
			scored := s.scoreMemory(query, candidates[i], minHours, maxHours)
			results <- scored
		}
	}
}

// scoreMemory computes all four scores for a single memory entry
func (s *Scorer) scoreMemory(query []float32, memory store.MemoryEntry, minHours, maxHours float64) ScoredMemory {
	// S (Semantic Similarity): cosine similarity
	scoreS := cosineSimilarity(query, memory.Embedding)

	// R (Recency): 1.0 / (1.0 + hours_since_creation), normalized
	hoursSince := time.Since(memory.Timestamp).Hours()
	if hoursSince < 0 {
		hoursSince = 0
	}
	scoreR := 1.0 / (1.0 + hoursSince)
	
	// Normalize recency score to 0.0-1.0 range
	if maxHours > minHours {
		scoreR = (scoreR - (1.0/(1.0+maxHours))) / ((1.0/(1.0+minHours)) - (1.0/(1.0+maxHours)))
	} else {
		scoreR = 1.0 // All have same recency
	}

	// I (Importance): lookup table
	scoreI := getImportanceScore(MemoryType(memory.MemoryType))

	// T (Task Alignment): TaskWeights[intent][memory.MemoryType]
	scoreT := GetTaskAlignmentWeight(s.intent, MemoryType(memory.MemoryType))

	// Total = S*wS + R*wR + I*wI + T*wT
	total := scoreS*s.weights.SemanticSimilarity +
		scoreR*s.weights.Recency +
		scoreI*s.weights.Importance +
		scoreT*s.weights.TaskAlignment

	return ScoredMemory{
		MemoryEntry: memory,
		ScoreS:      scoreS,
		ScoreR:      scoreR,
		ScoreI:      scoreI,
		ScoreT:      scoreT,
		Total:       total,
	}
}

// computeRecencyRange calculates the min/max hours for normalization
func (s *Scorer) computeRecencyRange(candidates []store.MemoryEntry) (float64, float64) {
	if len(candidates) == 0 {
		return 0, 0
	}

	minHours := math.MaxFloat64
	maxHours := 0.0

	for _, memory := range candidates {
		hoursSince := time.Since(memory.Timestamp).Hours()
		if hoursSince < 0 {
			hoursSince = 0
		}
		if hoursSince < minHours {
			minHours = hoursSince
		}
		if hoursSince > maxHours {
			maxHours = hoursSince
		}
	}

	if minHours == math.MaxFloat64 {
		minHours = 0
	}

	return minHours, maxHours
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

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// getImportanceScore returns the importance weight for a memory type
func getImportanceScore(memoryType MemoryType) float64 {
	if score, ok := ImportanceWeights[memoryType]; ok {
		return score
	}
	return 0.5 // Default fallback
}