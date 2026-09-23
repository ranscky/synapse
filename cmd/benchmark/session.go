// Session fixtures, token counting, and the small pure helpers the three
// benchmark scenarios share.
//
// Split out of main.go for the reason every file in this project is split: the
// 300-line ceiling. Nothing here touches Synapse's internals -- these are the
// command's own reading and counting primitives.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkoukk/tiktoken-go"
)

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Session represents a chat session
type Session struct {
	Messages []Message `json:"messages"`
}

// loadSession reads one session fixture.
//
// A bare name that is not a path is looked for under testdata/, so the DoD
// command (`go run ./cmd/benchmark testdata/session_merged.json`) and the
// shorter `session_merged.json` both work.
func loadSession(filename string) (*Session, error) {
	// Try to find the file in testdata directory
	fullPath := filename
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		// Try testdata directory
		testdataPath := filepath.Join("testdata", filename)
		if _, err := os.Stat(testdataPath); err == nil {
			fullPath = testdataPath
		}
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	return &session, nil
}

// countTokens counts one conversation with tiktoken, falling back to a
// character approximation only if the encoder itself cannot be built.
func countTokens(messages []Message) int {
	// Initialize tiktoken for GPT-3.5/GPT-4 token counting
	tke, err := tiktoken.EncodingForModel("gpt-3.5-turbo")
	if err != nil {
		// Fallback to cl100k_base encoding
		tke, err = tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			log.Printf("Warning: Failed to initialize tokenizer, using character approximation")
			total := 0
			for _, msg := range messages {
				total += len(msg.Content) / 4 // Rough approximation
			}
			return total
		}
	}

	totalTokens := 0
	for _, msg := range messages {
		tokens := tke.Encode(msg.Content, nil, nil)
		totalTokens += len(tokens)
	}

	return totalTokens
}

// classifyMemType assigns a coarse memory type using keyword/phrase matching.
// Still a heuristic, not real classification - but covers the vocabulary that
// actually shows up in real debugging sessions (panics, stack traces, races),
// not just generic words like "error" or "bug".
func classifyMemType(content string) string {
	lowerContent := strings.ToLower(content)

	errorTerms := []string{
		"error", "bug", "exception", "panic", "sigsegv", "stack trace",
		"nil pointer", "null pointer", "segfault", "crash", "race detected",
		"data race", "failed", "failure", "traceback",
	}
	decisionTerms := []string{
		"implement", "function", "code", "refactor", "design", "structure",
		"validation chain", "convention", "pattern",
	}

	for _, term := range errorTerms {
		if strings.Contains(lowerContent, term) {
			return "error"
		}
	}
	for _, term := range decisionTerms {
		if strings.Contains(lowerContent, term) {
			return "decision"
		}
	}
	return "context"
}

// splitScorable separates pinned system messages from the conversational ones.
//
// System messages are pinned context: they are always included and never compete
// for budget against the rest of the conversation, so they are tracked
// separately and excluded from the scored candidate pool entirely.
func splitScorable(messages []Message) (systemTokens int, scorable []Message) {
	for _, msg := range messages {
		if msg.Role == "system" {
			systemTokens += countTokens([]Message{msg})
			continue
		}
		scorable = append(scorable, msg)
	}
	return systemTokens, scorable
}

// splitSequential cuts a conversation into at most sessions contiguous chunks,
// in order, covering every message exactly once.
//
// This is what makes the Global Brain scenario five sequential sessions rather
// than one session replayed five times: agent_a works through the fixture chunk
// by chunk, the way an agent accumulates work over a day, and the brain ends up
// holding what each of those sessions produced.
func splitSequential(messages []Message, sessions int) [][]Message {
	if len(messages) == 0 || sessions < 1 {
		return nil
	}
	if sessions > len(messages) {
		sessions = len(messages)
	}

	base := len(messages) / sessions
	extra := len(messages) % sessions

	chunks := make([][]Message, 0, sessions)
	start := 0
	for i := 0; i < sessions; i++ {
		size := base
		if i < extra {
			size++
		}
		chunks = append(chunks, messages[start:start+size])
		start += size
	}

	return chunks
}

// lastUserMessage returns the content of the final user message in a chunk.
//
// It is the memory the distilled Global Brain mode pushes, because it is the
// memory the product itself writes back: synapse_compile writes the last user
// message of a conversation into the session it compiled. A chunk with no user
// message falls back to its last message, and an empty chunk yields "".
func lastUserMessage(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].Content
}

// reductionPct is the benchmark's one arithmetic: how much of the raw
// conversation the compiled context removes, as a percentage.
//
// A zero-token fixture has no reduction to report; returning 0 rather than
// NaN or +Inf keeps every printed line a number.
func reductionPct(rawTokens, compiledTokens int) float64 {
	if rawTokens <= 0 {
		return 0
	}
	return float64(rawTokens-compiledTokens) / float64(rawTokens) * 100
}
