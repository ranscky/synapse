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
	ID                string  `json:"id"`
	MemoryType        string  `json:"memory_type"`
	ContentPreview    string  `json:"content_preview"`
	ScoreSemantic     float64 `json:"score_semantic"`
	ScoreRecency      float64 `json:"score_recency"`
	ScoreImportance   float64 `json:"score_importance"`
	ScoreTaskAlignment float64 `json:"score_task_alignment"`
	ScoreTotal        float64 `json:"score_total"`
	Included          bool    `json:"included"`
	ExclusionReason   string  `json:"exclusion_reason,omitempty"`
}

// NewTraceManifest creates a new trace manifest from pipeline data
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
	selectedMemories []scorer.ScoredMemory,
) *TraceManifest {
	// Create a map of selected memory IDs for quick lookup
	selectedMap := make(map[string]bool)
	for _, memory := range selectedMemories {
		selectedMap[memory.ID] = true
	}

	// Convert scored memories to trace memories
	traceMemories := make([]TraceMemory, len(scoredMemories))
	for i, memory := range scoredMemories {
		included := selectedMap[memory.ID]
		exclusionReason := ""
		
		if !included {
			// Determine exclusion reason (simplified logic - in practice this would be more complex)
			exclusionReason = "budget_exceeded" // Default assumption
		}

		// Create content preview (first 100 characters)
		contentPreview := memory.Content
		if len(contentPreview) > 100 {
			contentPreview = contentPreview[:100]
		}

		traceMemories[i] = TraceMemory{
			ID:                memory.ID,
			MemoryType:        memory.MemoryType,
			ContentPreview:    contentPreview,
			ScoreSemantic:     memory.ScoreS,
			ScoreRecency:      memory.ScoreR,
			ScoreImportance:   memory.ScoreI,
			ScoreTaskAlignment: memory.ScoreT,
			ScoreTotal:        memory.Total,
			Included:          included,
			ExclusionReason:   exclusionReason,
		}
	}

	return &TraceManifest{
		RequestID:             requestID,
		Timestamp:             time.Now(),
		DetectedIntent:        intent,
		IntentConfidence:      confidence,
		CandidatesRetrieved:   candidatesRetrieved,
		CandidatesAfterDedup:  candidatesAfterDedup,
		MemoriesCompiled:      memoriesCompiled,
		TokensUsed:            tokensUsed,
		TokenBudget:           tokenBudget,
		CompileDurationMs:     compileDurationMs,
		Memories:              traceMemories,
	}
}

// SaveToFile saves the trace manifest to a file in the specified directory
func (tm *TraceManifest) SaveToFile(baseDir string) error {
	// Create directory structure: ~/.synapse/traces/<date>/
	dateDir := filepath.Join(baseDir, "traces", time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		return fmt.Errorf("failed to create trace directory: %w", err)
	}

	// Create filename: <request_id>.json
	filename := filepath.Join(dateDir, fmt.Sprintf("%s.json", tm.RequestID))
	
	// Marshal to JSON
	data, err := json.MarshalIndent(tm, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal trace manifest: %w", err)
	}

	// Write to file with 0600 permissions
	if err := os.WriteFile(filename, data, 0600); err != nil {
		return fmt.Errorf("failed to write trace file: %w", err)
	}

	return nil
}
