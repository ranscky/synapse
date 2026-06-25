package budget

import (
	"strings"

	"github.com/pkoukk/tiktoken-go"
	"synapse/internal/scorer"
)

// CountTokens counts the number of tokens in text using the specified model
func CountTokens(text string, model string) (int, error) {
	// Try to get tokenizer for the specified model
	tkm, err := tiktoken.EncodingForModel(model)
	if err != nil {
		// Fall back to naive word count × 1.3 coefficient
		wordCount := countWords(text)
		return int(float64(wordCount) * 1.3), nil
	}

	// Encode text and count tokens
	tokenCount := len(tkm.Encode(text, nil, nil))
	return tokenCount, nil
}

// Fill selects memories that fit within the token budget using greedy algorithm
func Fill(candidates []scorer.ScoredMemory, budgetTokens int) ([]scorer.ScoredMemory, int) {
	if len(candidates) == 0 || budgetTokens <= 0 {
		return []scorer.ScoredMemory{}, 0
	}

	selected := make([]scorer.ScoredMemory, 0)
	totalTokens := 0

	// Greedy: iterate candidates and add while under budget
	for _, candidate := range candidates {
		// Estimate tokens for this memory's content
		tokens, _ := CountTokens(candidate.Content, "cl100k_base") // Use default model
		
		// Check if adding this memory would exceed budget
		if totalTokens+tokens <= budgetTokens {
			selected = append(selected, candidate)
			totalTokens += tokens
		} else if len(selected) == 0 {
			// Edge case: first memory is too large - include anyway with truncation
			// Truncate to budget/2 tokens
			truncatedContent := truncateToTokens(candidate.Content, budgetTokens/2)
			truncatedCandidate := candidate
			truncatedCandidate.Content = truncatedContent
			selected = append(selected, truncatedCandidate)
			totalTokens = budgetTokens / 2
			break
		} else {
			// else: this candidate doesn't fit, but smaller lower-ranked candidates
			// might still fit in the remaining space — keep checking instead of
			// stopping the whole selection here.
		}
	}

	return selected, totalTokens
}

// countWords counts words in text using whitespace separation
func countWords(text string) int {
	// Split on whitespace and count non-empty parts
	words := strings.Fields(text)
	return len(words)
}

// truncateToTokens truncates text to approximately the specified number of tokens
func truncateToTokens(text string, targetTokens int) string {
	// Simple approach: estimate average tokens per word and truncate accordingly
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	
	// Rough estimate: assume 1.3 tokens per word on average
	estimatedWords := int(float64(targetTokens) / 1.3)
	if estimatedWords >= len(words) {
		return text // No truncation needed
	}
	
	// Join first estimatedWords words
	return strings.Join(words[:estimatedWords], " ")
}

// GetTokenCountForMemory estimates the token count for a memory entry
func GetTokenCountForMemory(memory scorer.ScoredMemory) int {
	tokens, _ := CountTokens(memory.Content, "cl100k_base")
	return tokens
}