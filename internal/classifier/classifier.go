package classifier

import (
	"strings"
	"unicode"
)

// Intent represents the type of user intent detected in a message
type Intent string

const (
	Debug   Intent = "debug"
	Plan    Intent = "plan"
	Code    Intent = "code"
	Write   Intent = "write"
	Generic Intent = "generic"
)

// keywordSets maps each intent to its characteristic keywords
var keywordSets = map[Intent][]string{
	Debug: {
		"error", "exception", "traceback", "crash", "bug", "fix", "broken", "failing", "stderr",
	},
	Plan: {
		"architecture", "design", "plan", "roadmap", "strategy", "structure", "how should",
	},
	Code: {
		"implement", "write", "function", "method", "class", "refactor", "add feature",
	},
	Write: {
		"draft", "write", "document", "explain", "describe", "summarize", "blog", "readme",
	},
}

// ClassifyResult contains the classification result and confidence
type ClassifyResult struct {
	Intent     Intent
	Confidence float64
}

// Classify determines the intent of a text using TF-IDF style keyword scoring
func Classify(text string) ClassifyResult {
	// Convert to lowercase and split into words
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return ClassifyResult{Intent: Generic, Confidence: 0.0}
	}

	// Count keyword hits for each intent
	intentScores := make(map[Intent]int)
	
	for intent, keywords := range keywordSets {
		for _, word := range words {
			// Remove punctuation for cleaner matching
			cleanWord := strings.TrimFunc(word, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsNumber(r)
			})
			
			for _, keyword := range keywords {
				if strings.Contains(cleanWord, keyword) || cleanWord == keyword {
					intentScores[intent]++
					break // Don't count multiple matches for the same word
				}
			}
		}
	}

	// Calculate scores as hit count / total words
	bestIntent := Generic
	bestScore := 0.0
	
	for intent, hitCount := range intentScores {
		score := float64(hitCount) / float64(len(words))
		if score > bestScore && score > 0.05 {
			bestScore = score
			bestIntent = intent
		}
	}

	return ClassifyResult{
		Intent:     bestIntent,
		Confidence: bestScore,
	}
}