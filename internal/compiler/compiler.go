package compiler

import (
	"fmt"
	"sort"
	"time"

	"synapse/internal/scorer"
	"synapse/internal/trace"
)

// CompileResult holds both the compiled messages and trace manifest
type CompileResult struct {
	Messages []map[string]interface{}
	Trace    *trace.TraceManifest
}

// Compile assembles the final message context from original messages, selected memories, and last user message
func Compile(
	selected []scorer.ScoredMemory,
	lastUserMessage string,
	requestID string,
	intent string,
	confidence float64,
	candidatesRetrieved int,
	candidatesAfterDedup int,
	tokenBudget int,
	compileDurationMs int64,
	allScoredMemories []scorer.ScoredMemory,
	dedupedMemories []scorer.ScoredMemory,
) *CompileResult {
	compileStart := time.Now()

	// Result slice for compiled messages
	result := make([]map[string]interface{}, 0)

	// Sort selected memories by timestamp (oldest first)
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].Timestamp.Before(selected[j].Timestamp)
	})

	// Add selected memories as assistant/user turns
	for _, memory := range selected {
		header := fmt.Sprintf("[Memory | Type: %s | Score: %.2f]", memory.MemoryType, memory.Total)
		content := header + " " + memory.Content

		role := "assistant"
		if memory.MemoryType == "decision" || memory.MemoryType == "error" {
			role = "user"
		}

		result = append(result, map[string]interface{}{
			"role":    role,
			"content": content,
		})
	}

	// Add the last user message
	if lastUserMessage != "" {
		result = append(result, map[string]interface{}{
			"role":    "user",
			"content": lastUserMessage,
		})
	}

	// Calculate final compile duration
	finalCompileDuration := compileDurationMs + time.Since(compileStart).Milliseconds()

	// Create trace manifest
	traceManifest := trace.NewTraceManifest(
		requestID,
		intent,
		confidence,
		candidatesRetrieved,
		candidatesAfterDedup,
		len(selected),
		0, // tokensUsed - will be calculated by caller
		tokenBudget,
		finalCompileDuration,
		allScoredMemories,
		dedupedMemories,
		selected,
	)

	return &CompileResult{
		Messages: result,
		Trace:    traceManifest,
	}
}

// CompileWithContext includes system message if present
func CompileWithContext(
	systemMessage string,
	selected []scorer.ScoredMemory,
	lastUserMessage string,
	requestID string,
	intent string,
	confidence float64,
	candidatesRetrieved int,
	candidatesAfterDedup int,
	tokenBudget int,
	compileDurationMs int64,
	allScoredMemories []scorer.ScoredMemory,
	dedupedMemories []scorer.ScoredMemory,
) *CompileResult {
	result := make([]map[string]interface{}, 0)

	if systemMessage != "" {
		result = append(result, map[string]interface{}{
			"role":    "system",
			"content": systemMessage,
		})
	}

	compileResult := Compile(
		selected,
		lastUserMessage,
		requestID,
		intent,
		confidence,
		candidatesRetrieved,
		candidatesAfterDedup,
		tokenBudget,
		compileDurationMs,
		allScoredMemories,
		dedupedMemories,
	)
	result = append(result, compileResult.Messages...)

	compileResult.Messages = result
	return compileResult
}