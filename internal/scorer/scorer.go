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
// Scorer needs confidence as input
type Scorer struct {
	weights    Weights
	now        time.Time
	intent     classifier.Intent
	confidence float64 // NEW
}

// NewScorer creates a new scorer with the specified weights and intent
func NewScorer(weights Weights, intent classifier.Intent, confidence float64, now time.Time) *Scorer {
	return &Scorer{
		weights:    weights,
		now:        now,
		intent:     intent,
		confidence: confidence,
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

	// R (Recency): fixed half-life exponential decay, anchored to an
	// absolute time scale rather than normalized against the current
	// candidate batch's own min/max age. The old batch-relative
	// normalization always stretched recency to span the full 0.0-1.0
	// range regardless of the *actual* size of the age gap -- a 30-day-old
	// memory and a 1-hour-old memory in the same batch would be stretched
	// to exactly 0.0 and 1.0, artificially guaranteeing recency's full
	// weight budget every time, while importance (a fixed 0-1 lookup
	// table) never got that same boost. This anchors "recent" to the same
	// meaning in every batch, independent of what else is in it.
	hoursSince := time.Since(memory.Timestamp).Hours()
	if hoursSince < 0 {
		hoursSince = 0
	}
	halfLife := s.weights.RecencyHalfLifeHours
	if halfLife <= 0 {
		halfLife = DefaultRecencyHalfLifeHours
	}
	scoreR := math.Exp2(-hoursSince / halfLife)
	
	// T (Task Alignment): blend the intent-specific weight with the generic
	// fallback weight, proportional to classifier confidence. Low confidence
	// means we trust the detected intent less, so we lean toward the
	// intent-agnostic baseline instead of fully committing to a possibly-wrong
	// task alignment signal.
	intentWeight := GetTaskAlignmentWeight(s.intent, MemoryType(memory.MemoryType))
	genericWeight := GetTaskAlignmentWeight(classifier.Generic, MemoryType(memory.MemoryType))
	scoreT := s.confidence*intentWeight + (1-s.confidence)*genericWeight

	// I (Importance): lookup table
	scoreI := getImportanceScore(MemoryType(memory.MemoryType))

	// T (Task Alignment): TaskWeights[intent][memory.MemoryType]
	// scoreT := GetTaskAlignmentWeight(s.intent, MemoryType(memory.MemoryType))

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