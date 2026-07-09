package classifier

import (
	"regexp"
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

// codeBlockPattern strips fenced code blocks (```...```) before classification.
// Code content dilutes keyword density without carrying intent signal of its own.
var codeBlockPattern = regexp.MustCompile("(?s)```.*?```")

// inlineCodePattern strips inline code spans (`like this`).
var inlineCodePattern = regexp.MustCompile("`[^`]*`")

// keywordSets maps each intent to its characteristic keywords
var keywordSets = map[Intent][]string{
	Debug: {
    // phrases (checked first)
    "nil pointer", "race condition", "memory leak", "goroutine leak",
    "stack trace", "segmentation fault", "out of memory", "deadlock",
    "null pointer", "index out of range", "panic:",
    // single words
    "error", "exception", "traceback", "crash", "bug", "fix", "broken",
    "failing", "stderr", "panic", "segfault", "leak", "race", "nil",
    "deadlocked", "freeze", "hang", "corrupt", "debug", "debugging",
	},
	Plan: {
		"how should", "best practice", "best approach", "design pattern",
		"trade off", "tradeoff",
		"architecture", "design", "plan", "roadmap", "strategy", "structure",
		"approach", "pattern",
	},
	Code: {
		"add feature", "rest api", "rest handler",
		"implement", "write", "function", "method", "class", "refactor",
		"struct", "interface", "endpoint", "handler", "middleware",
	},
	Write: {
		"blog post",
		"draft", "write", "document", "explain", "describe", "summarize",
		"blog", "readme",
	},
}

// ClassifyResult contains the classification result and confidence
type ClassifyResult struct {
	Intent     Intent
	Confidence float64
}

// stripCode removes fenced and inline code from text so that classification
// runs only against natural-language content. Code tokens (variable names,
// syntax) inflate word count without containing intent signal, which silently
// crushes confidence scores on code-heavy technical conversations.
func stripCode(text string) string {
	text = codeBlockPattern.ReplaceAllString(text, " ")
	text = inlineCodePattern.ReplaceAllString(text, " ")
	return text
}

// cleanWord strips leading/trailing punctuation for matching, but preserves
// internal punctuation like "panic:" or "race-condition" so phrase matching
// can still work where useful.
func cleanWord(w string) string {
	return strings.TrimFunc(w, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != ':' && r != '-'
	})
}

// countPhraseHits counts non-overlapping occurrences of multi-word phrases
// in the lowercased, code-stripped text.
func countPhraseHits(lowerText string, phrases []string) (hits int, consumed string) {
	consumed = lowerText
	for _, p := range phrases {
		if !strings.Contains(p, " ") {
			continue // single words handled separately, by token
		}
		for strings.Contains(consumed, p) {
			hits++
			consumed = strings.Replace(consumed, p, " ", 1)
		}
	}
	return hits, consumed
}

// Classify determines the intent of a text using keyword/phrase scoring.
//
// Confidence is computed as this intent's share of *all* keyword hits across
// every intent (hitCount / totalHitsAcrossIntents), not hitCount / wordCount.
// That keeps confidence meaningful regardless of message length: a short,
// keyword-dense message and a long, code-heavy message with the same
// proportion of debug-relevant signal will score similarly. The previous
// hitCount/len(words) formula silently collapsed toward zero on any long or
// code-block-heavy message, independent of topical relevance.
func Classify(text string) ClassifyResult {
	cleanText := stripCode(text)
	lowerText := strings.ToLower(cleanText)

	intentScores := make(map[Intent]int)
	totalHits := 0

	// Phrase matching first (longest signal, least ambiguous)
	for intent, keywords := range keywordSets {
		hits, remaining := countPhraseHits(lowerText, keywords)
		intentScores[intent] += hits
		totalHits += hits
		if intent == Debug {
			// only need one pass to consume matched phrases from the
			// shared text; reuse the same consumed string going forward
			lowerText = remaining
		}
	}

	// Re-run phrase consumption properly per-intent using a working copy,
	// since phrases for different intents shouldn't compete over the same
	// substring removal. Simpler and correct: just match phrases against
	// the original cleaned text without mutating shared state.
	intentScores = make(map[Intent]int)
	totalHits = 0
	workingText := strings.ToLower(cleanText)

	for intent, keywords := range keywordSets {
		hits, _ := countPhraseHits(workingText, keywords)
		intentScores[intent] += hits
		totalHits += hits
	}

	// Single-word matching, token by token
	words := strings.Fields(workingText)
	for _, word := range words {
		cw := cleanWord(word)
		if cw == "" {
			continue
		}
		for intent, keywords := range keywordSets {
			matched := false
			for _, keyword := range keywords {
				if strings.Contains(keyword, " ") {
					continue // phrases already counted above
				}
				if cw == keyword {
					matched = true
					break
				}
			}
			if matched {
				intentScores[intent]++
				totalHits++
				break // a word counts toward at most one intent
			}
		}
	}

	if totalHits == 0 {
		return ClassifyResult{Intent: Generic, Confidence: 0.0}
	}

	// Iterate in a fixed, explicit order (not map range, which Go
	// randomizes per-process) so tied scores resolve deterministically
	// rather than depending on map iteration luck.
	priorityOrder := []Intent{Debug, Code, Plan, Write}

	bestIntent := Generic
	bestHits := 0
	for _, intent := range priorityOrder {
		hits := intentScores[intent]
		if hits > bestHits {
			bestHits = hits
			bestIntent = intent
		}
	}

	if bestHits == 0 {
		return ClassifyResult{Intent: Generic, Confidence: 0.0}
	}

	confidence := float64(bestHits) / float64(totalHits)

	return ClassifyResult{
		Intent:     bestIntent,
		Confidence: confidence,
	}
}