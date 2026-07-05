package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"synapse/internal/scorer"
)

// TraceManifest represents the memory trace output for a compilation request
type TraceManifest struct {
	RequestID             string        `json:"request_id"`
	Timestamp             time.Time     `json:"timestamp"`
	DetectedIntent        string        `json:"detected_intent"`
	IntentConfidence      float64       `json:"intent_confidence"`
	CandidatesRetrieved   int           `json:"candidates_retrieved"`
	CandidatesAfterDedup  int           `json:"candidates_after_dedup"`
	MemoriesCompiled      int           `json:"memories_compiled"`
	TokensUsed            int           `json:"tokens_used"`
	TokenBudget           int           `json:"token_budget"`
	CompileDurationMs     int64         `json:"compile_duration_ms"`
	Memories              []TraceMemory `json:"memories"`
	TraceTruncated        bool          `json:"trace_truncated,omitempty"`
}

// TraceMemory represents a memory entry in the trace manifest
type TraceMemory struct {
	ID                 string  `json:"id"`
	MemoryType         string  `json:"memory_type"`
	ContentPreview     string  `json:"content_preview"`
	ScoreSemantic      float64 `json:"score_semantic"`
	ScoreRecency       float64 `json:"score_recency"`
	ScoreImportance    float64 `json:"score_importance"`
	ScoreTaskAlignment float64 `json:"score_task_alignment"`
	ScoreTotal         float64 `json:"score_total"`
	Included           bool    `json:"included"`
	ExclusionReason    string  `json:"exclusion_reason,omitempty"`
}

// NewTraceManifest creates a new trace manifest from pipeline data.
//
// dedupedMemories is the slice returned by dedup.Deduplicate — memories that
// survived the similarity pass. Anything in scoredMemories but NOT in
// dedupedMemories was dropped as a duplicate. Anything in dedupedMemories but
// NOT in selectedMemories was dropped because it didn't fit the token budget.
func NewTraceManifest(
	requestID string,
	intent string,
	confidence float64,
	candidatesRetrieved int,
	candidatesAfterDedup int,
	memoriesCompiled int,
	tokensUsed int,
	tokenBudget int,
	compileDurationMs int64,
	scoredMemories []scorer.ScoredMemory,
	dedupedMemories []scorer.ScoredMemory,
	selectedMemories []scorer.ScoredMemory,
) *TraceManifest {
	// Build lookup maps for O(1) membership checks.
	dedupedMap := make(map[string]bool, len(dedupedMemories))
	for _, m := range dedupedMemories {
		dedupedMap[m.ID] = true
	}

	selectedMap := make(map[string]bool, len(selectedMemories))
	for _, m := range selectedMemories {
		selectedMap[m.ID] = true
	}

	traceMemories := make([]TraceMemory, len(scoredMemories))
	for i, memory := range scoredMemories {
		included := selectedMap[memory.ID]

		var exclusionReason string
		if !included {
			if !dedupedMap[memory.ID] {
				exclusionReason = "duplicate"
			} else {
				exclusionReason = "budget_exceeded"
			}
		}

		contentPreview := memory.Content
		if len(contentPreview) > 100 {
			contentPreview = contentPreview[:100]
		}

		traceMemories[i] = TraceMemory{
			ID:                 memory.ID,
			MemoryType:         memory.MemoryType,
			ContentPreview:     contentPreview,
			ScoreSemantic:      memory.ScoreS,
			ScoreRecency:       memory.ScoreR,
			ScoreImportance:    memory.ScoreI,
			ScoreTaskAlignment: memory.ScoreT,
			ScoreTotal:         memory.Total,
			Included:           included,
			ExclusionReason:    exclusionReason,
		}
	}

	return &TraceManifest{
		RequestID:            requestID,
		Timestamp:            time.Now(),
		DetectedIntent:       intent,
		IntentConfidence:     confidence,
		CandidatesRetrieved:  candidatesRetrieved,
		CandidatesAfterDedup: candidatesAfterDedup,
		MemoriesCompiled:     memoriesCompiled,
		TokensUsed:           tokensUsed,
		TokenBudget:          tokenBudget,
		CompileDurationMs:    compileDurationMs,
		Memories:             traceMemories,
	}
}

// SaveToFile saves the trace manifest to a file in the specified directory
func (tm *TraceManifest) SaveToFile(baseDir string) error {
	dateDir := filepath.Join(baseDir, "traces", time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		return fmt.Errorf("failed to create trace directory: %w", err)
	}

	filename := filepath.Join(dateDir, fmt.Sprintf("%s.json", tm.RequestID))

	data, err := json.MarshalIndent(tm, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal trace manifest: %w", err)
	}

	if err := os.WriteFile(filename, data, 0600); err != nil {
		return fmt.Errorf("failed to write trace file: %w", err)
	}

	return nil
}