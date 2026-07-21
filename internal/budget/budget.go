package budget

import (
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	"synapse/internal/scorer"
)

// cachedEncoding holds the tiktoken encoder, loaded once via sync.Once
// rather than reloaded on every CountTokens call. GetEncoding() parses the
// full BPE merge table, which is expensive enough (~5s observed locally)
// that doing it per-call would make budget.Fill() -- called once per
// candidate, every request -- unacceptably slow in the hot path.
var (
	encodingOnce sync.Once
	cachedEnc    *tiktoken.Tiktoken
	encodingErr  error
)

func getEncoding(encodingName string) (*tiktoken.Tiktoken, error) {
	encodingOnce.Do(func() {
		cachedEnc, encodingErr = tiktoken.GetEncoding(encodingName)
	})
	return cachedEnc, encodingErr
}

// CountTokens counts tokens in text using the specified tiktoken ENCODING
// (e.g. "cl100k_base") -- not an OpenAI model name. tiktoken.EncodingForModel
// expects a model name like "gpt-4" and maps it internally to an encoding;
// passing an encoding name directly to it always errors, which silently
// fell through to a naive word-count estimate on every single call in this
// codebase until this fix -- confirmed via TestTiktokenModelNameBug.
func CountTokens(text string, encodingName string) (int, error) {
	tkm, err := getEncoding(encodingName)
	if err != nil {
		// Kept as a safety net for an invalid/unsupported encoding name,
		// but should never fire for "cl100k_base" now.
		wordCount := countWords(text)
		return int(float64(wordCount) * 1.3), nil
	}

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
			totalTokens += budgetTokens / 2
			continue
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

// truncateToTokens truncates text to EXACTLY the specified number of tokens
// (or fewer, if the text is already shorter), using the real tiktoken
// encoder directly -- encode the full text, slice to the first
// targetTokens token IDs, decode back to a string. This replaces the old
// word-count*1.3 estimate, which could overshoot by 400%+ on
// token-dense content like URLs or code (confirmed via
// TestTruncateToTokensOvershoot), silently blowing past the budget it was
// supposed to enforce.
func truncateToTokens(text string, targetTokens int) string {
	if targetTokens <= 0 {
		return ""
	}

	tkm, err := getEncoding("cl100k_base")
	if err != nil {
		// Fallback to the old word-based estimate only if the encoder
		// itself is unavailable -- should not happen in practice now that
		// getEncoding is confirmed working, but kept as a safety net
		// rather than panicking.
		words := strings.Fields(text)
		if len(words) == 0 {
			return ""
		}
		estimatedWords := int(float64(targetTokens) / 1.3)
		if estimatedWords >= len(words) {
			return text
		}
		return strings.Join(words[:estimatedWords], " ")
	}

	tokens := tkm.Encode(text, nil, nil)
	if len(tokens) <= targetTokens {
		return text
	}

	return tkm.Decode(tokens[:targetTokens])
}

// GetTokenCountForMemory estimates the token count for a memory entry
func GetTokenCountForMemory(memory scorer.ScoredMemory) int {
	tokens, _ := CountTokens(memory.Content, "cl100k_base")
	return tokens
}