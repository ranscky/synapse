package compiler

import (
	"fmt"
	"sort"

	"synapse/internal/scorer"
)

// Compile assembles the final message context from original messages, selected memories, and last user message
func Compile(selected []scorer.ScoredMemory, lastUserMessage string) []map[string]interface{} {
	// Result slice for compiled messages
	result := make([]map[string]interface{}, 0)
	
	// Sort selected memories by timestamp (oldest first)
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].Timestamp.Before(selected[j].Timestamp)
	})
	
	// Add selected memories as assistant/user turns
	for _, memory := range selected {
		// Create header with memory metadata
		header := fmt.Sprintf("[Memory | Type: %s | Score: %.2f]", memory.MemoryType, memory.Total)
		content := header + " " + memory.Content
		
		// Determine role based on memory type
		role := "assistant"
		if memory.MemoryType == "decision" || memory.MemoryType == "error" {
			role = "user"
		}
		
		message := map[string]interface{}{
			"role":    role,
			"content": content,
		}
		result = append(result, message)
	}
	
	// Add the last user message
	if lastUserMessage != "" {
		result = append(result, map[string]interface{}{
			"role":    "user",
			"content": lastUserMessage,
		})
	}
	
	return result
}

// CompileWithContext includes system message if present
func CompileWithContext(systemMessage string, selected []scorer.ScoredMemory, lastUserMessage string) []map[string]interface{} {
	result := make([]map[string]interface{}, 0)
	
	// Add system message if present
	if systemMessage != "" {
		result = append(result, map[string]interface{}{
			"role":    "system",
			"content": systemMessage,
		})
	}
	
	// Add compiled memories and last user message
	messages := Compile(selected, lastUserMessage)
	result = append(result, messages...)
	
	return result
}