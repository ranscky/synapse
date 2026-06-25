package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkoukk/tiktoken-go"
	"synapse/internal/budget"
	"synapse/internal/classifier"
	"synapse/internal/config"
	"synapse/internal/dedup"
	"synapse/internal/embedder"
	"synapse/internal/scorer"
	"synapse/internal/store"
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

func main() {
	// budgetOverride: -1 means "not set, use config default"
	budgetFlag := flag.Int("budget", -1, "Override token budget (defaults to config value)")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		log.Fatal("Usage: benchmark [--budget N] <session_file>")
	}

	sessionFile := args[0]

	// Load session
	session, err := loadSession(sessionFile)
	if err != nil {
		log.Fatalf("Failed to load session: %v", err)
	}

	// Count raw tokens
	rawTokens := countTokens(session.Messages)
	fmt.Printf("Raw messages: %d tokens\n", rawTokens)

	// Run Synapse pipeline
	compiledTokens, err := runSynapsePipeline(session.Messages, *budgetFlag)
	if err != nil {
		log.Fatalf("Failed to run Synapse pipeline: %v", err)
	}

	fmt.Printf("Compiled messages: %d tokens\n", compiledTokens)

	// Calculate reduction
	if rawTokens > 0 {
		reduction := float64(rawTokens-compiledTokens) / float64(rawTokens) * 100
		fmt.Printf("Reduction: %.1f%%\n", reduction)

		// Check if reduction meets target
		if reduction < 40.0 {
			fmt.Printf("WARNING: Token reduction (%.1f%%) is below target (40%%). Consider tuning weights.\n", reduction)
		} else {
			fmt.Printf("SUCCESS: Token reduction (%.1f%%) meets target (≥40%%).\n", reduction)
		}
	}
}

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

func runSynapsePipeline(messages []Message, budgetOverride int) (int, error) {
	// Create temporary store
	storeInstance, err := store.NewStore(":memory:")
	if err != nil {
		return 0, fmt.Errorf("failed to create store: %w", err)
	}
	defer storeInstance.Close()

	// Create config with default weights
	cfg := config.DefaultConfig()

	// Apply budget override if the flag was explicitly set (-1 = not set)
	if budgetOverride >= 0 {
		cfg.TokenBudget = budgetOverride
	}

	// Real embedder: hash-based, content-sensitive. Not semantically meaningful
	// (no notion of paraphrase/synonymy) but unlike a constant mock vector, it
	// actually varies with input text, so dedup and semantic scoring have real
	// signal to act on instead of comparing identical vectors for every message.
	// TODO: swap for ONNXEmbedder once internal/embedder's ONNX path has a real
	// model.Embed implementation (currently a stub returning a fixed ramp vector).
	embedderInstance := embedder.NewHashEmbedder(384)

	ctx := context.Background()

	// System messages are pinned context, not scored conversational history.
	// They're always included and never compete for budget against the rest
	// of the conversation, so they're tracked separately and excluded from
	// the scored candidate pool entirely.
	var systemTokens int
	var scorableMessages []Message
	for _, msg := range messages {
		if msg.Role == "system" {
			systemTokens += countTokens([]Message{msg})
			continue
		}
		scorableMessages = append(scorableMessages, msg)
	}

	// Create mock memories from messages, with real per-message embeddings
	var memories []store.MemoryEntry
	for i, msg := range scorableMessages {
		embedding, err := embedderInstance.Embed(ctx, msg.Content)
		if err != nil {
			return 0, fmt.Errorf("failed to embed message %d: %w", i, err)
		}

		memType := classifyMemType(msg.Content)

		memories = append(memories, store.MemoryEntry{
			ID:         fmt.Sprintf("mem-%d", i),
			SessionID:  "benchmark-session",
			Content:    msg.Content,
			MemoryType: memType,
			Embedding:  embedding,
			Timestamp:  time.Now().Add(-time.Duration(i) * time.Hour), // Older memories get lower recency scores
		})
	}

	// Classify intent from last few scorable messages (system prompt excluded,
	// since it's not part of the conversational signal we're classifying)
	intentText := ""
	if len(scorableMessages) > 0 {
		intentText = scorableMessages[len(scorableMessages)-1].Content
		if len(scorableMessages) > 1 {
			intentText = scorableMessages[len(scorableMessages)-2].Content + " " + intentText
		}
	}

	classification := classifier.Classify(intentText)

	// Create scorer with weights from config
	weights := scorer.Weights{
		SemanticSimilarity: cfg.WeightSemanticSimilarity,
		Recency:            cfg.WeightRecency,
		Importance:         cfg.WeightImportance,
		TaskAlignment:      cfg.WeightTaskAlignment,
	}

	scorerInstance := scorer.NewScorer(weights, classification.Intent, classification.Confidence, time.Now())

	// Real query embedding too — using the same intentText that drove
	// classification, so semantic similarity is scored against something
	// representative of what the user is actually asking right now, not an
	// arbitrary constant.
	queryEmbedding, err := embedderInstance.Embed(ctx, intentText)
	if err != nil {
		return 0, fmt.Errorf("failed to embed query: %w", err)
	}

	scoredMemories := scorerInstance.Score(ctx, queryEmbedding, memories)

	// Deduplicate
	dedupInstance := dedup.Deduplicate(scoredMemories, cfg.DeduplicationThreshold)

	// Reserve budget for the pinned system prompt before filling the rest of
	// the budget with scored conversational history.
	remainingBudget := cfg.TokenBudget - systemTokens
	if remainingBudget < 0 {
		remainingBudget = 0
	}

	// Apply budget
	selectedMemories, tokensUsed := budget.Fill(dedupInstance, remainingBudget)

	for _, sm := range selectedMemories {
		fmt.Printf("  selected: %s — %.40s...\n", sm.ID, sm.Content)
	}
	selectedIDs := make(map[string]bool)
	for _, sm := range selectedMemories {
    	selectedIDs[sm.ID] = true
	}
	for _, sm := range dedupInstance {
    	if !selectedIDs[sm.ID] {
        	fmt.Printf("  excluded: %s (T=%.3f S=%.3f total=%.3f) — %.40s...\n",
            	sm.ID, sm.ScoreT, sm.ScoreS, sm.Total, sm.Content)
    	}
	}

	totalTokensUsed := tokensUsed + systemTokens

	fmt.Printf("System prompt tokens (pinned): %d\n", systemTokens)
	fmt.Printf("Candidates retrieved: %d\n", len(memories))
	fmt.Printf("After deduplication: %d\n", len(dedupInstance))
	fmt.Printf("Final selected: %d\n", len(selectedMemories))
	fmt.Printf("Tokens used: %d\n", totalTokensUsed)
	fmt.Printf("Token budget: %d\n", cfg.TokenBudget)
	fmt.Printf("Detected intent: %s (confidence: %.2f)\n", classification.Intent, classification.Confidence)

	return totalTokensUsed, nil
}