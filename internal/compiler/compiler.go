package compiler

import (
	"fmt"
	"sort"
	"strings"
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
//
// localAgentID is the compiling node's own agent-id (config.AgentID), and it is
// used for attribution only -- it decides nothing about what is scored or
// selected, and it never widens what a caller may read. It is what every traced
// memory's cross_agent is measured against: a memory whose own agent_id is
// different from this one came from another agent. Pass "" for a node with no
// identity of its own (a standalone node): nothing can be attributed to anyone
// there, so no memory is ever reported as cross-agent.
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
	localAgentID string,
) *CompileResult {
	compileStart := time.Now()

	// Result slice for compiled messages
	result := make([]map[string]interface{}, 0)

	// Sort selected memories by timestamp (oldest first)
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].Timestamp.Before(selected[j].Timestamp)
	})

	// Build a single consolidated context block from all selected memories,
	// rather than injecting one message per memory with role alternating by
	// type. The old approach produced invalid multi-turn structure -- e.g.
	// several "assistant" turns in a row with no "user" turn between them --
	// which strict providers reject outright (Anthropic requires alternating
	// user/assistant turns) and others silently mishandle. Folding all
	// memories into one "user" message alongside the actual question
	// guarantees the compiled output is always exactly one valid turn,
	// regardless of how many memories are selected or what type they are.
	var contextBlock strings.Builder
	for _, memory := range selected {
		fmt.Fprintf(&contextBlock, "[Memory: %s] %s\n", memory.MemoryType, memory.Content)
	}

	switch {
	case contextBlock.Len() > 0 && lastUserMessage != "":
		result = append(result, map[string]interface{}{
			"role":    "user",
			"content": contextBlock.String() + "\n" + lastUserMessage,
		})
	case contextBlock.Len() > 0:
		result = append(result, map[string]interface{}{
			"role":    "user",
			"content": contextBlock.String(),
		})
	case lastUserMessage != "":
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
		localAgentID,
	)

	// Audit ledger (Phase 18): if this process is an enterprise tenant's node
	// and a sink is installed, the trace is snapshotted here and appended in a
	// goroutine -- never awaited, so the compilation's own duration is what it
	// was before this call existed. See ledger.go for why the snapshot is
	// synchronous and the append is not.
	recordTrace(traceManifest)

	return &CompileResult{
		Messages: result,
		Trace:    traceManifest,
	}
}

// CompileWithContext includes system message if present.
//
// localAgentID is forwarded to Compile unchanged; see its doc comment for what
// it means.
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
	localAgentID string,
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
		localAgentID,
	)
	result = append(result, compileResult.Messages...)

	compileResult.Messages = result
	return compileResult
}