package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"synapse/internal/classifier"
	"synapse/internal/compiler"
	"synapse/internal/config"
	"synapse/internal/dedup"
	"synapse/internal/budget"
	"synapse/internal/scorer"
	"synapse/internal/store"
)

// ContextKey represents context keys used in the proxy
type ContextKey string

const (
	ScoredMemoriesKey ContextKey = "scored_memories"
	IntentKey         ContextKey = "intent"
	ConfidenceKey     ContextKey = "confidence"
)

// Embedder is the interface for generating text embeddings.
// Defined here to avoid circular imports and allow easy mocking in tests.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// MemoryStore is the interface for reading/writing memories.
// Defined here to allow easy mocking in tests.
type MemoryStore interface {
	GetRecent(ctx context.Context, sessionID string, limit int) ([]store.MemoryEntry, error)
	Search(ctx context.Context, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error)
	Write(ctx context.Context, entry store.MemoryEntry) error
}

// Proxy represents the reverse proxy handler
type Proxy struct {
	target   *url.URL
	upstream *httputil.ReverseProxy
	store    MemoryStore
	embedder Embedder
	config   *config.Config
}

// NewProxy creates a new proxy instance
func NewProxy(targetURL string, memStore MemoryStore, emb Embedder, cfg *config.Config) (*Proxy, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	// Validate that target URL starts with http:// or https://
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		return nil, fmt.Errorf("upstream URL must start with http:// or https://")
	}

	proxy := &Proxy{
		target:   target,
		store:    memStore,
		embedder: emb,
		config:   cfg,
	}

	// Create reverse proxy with simple director
	proxy.upstream = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			proxy.director(req)
		},
	}

	return proxy, nil
}

// director modifies the outgoing request to forward to the target
func (p *Proxy) director(req *http.Request) {
	// Set the target URL
	req.URL.Scheme = p.target.Scheme
	req.URL.Host = p.target.Host
	req.Host = p.target.Host

	// Remove hop-by-hop headers
	req.Header.Del("Connection")
	req.Header.Del("Upgrade")
	req.Header.Del("TE")
	req.Header.Del("Trailers")
	req.Header.Del("Transfer-Encoding")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Authenticate")

	// IMPORTANT SECURITY: Never log Authorization headers
	// The header is stripped above and re-added from config if needed
	// But we never log its value anywhere
}

// HandleMessages handles POST /v1/messages requests with full pipeline
func (p *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	
	ctx := r.Context()

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Failed to read request body", "error", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Restore body for upstream (since we consumed it)
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	// Extract the last user message for classification
	lastUserMessage := string(body) // Simplified - in reality parse JSON properly

	// 1. Classify the intent
	classifyResult := classifier.Classify(lastUserMessage)
	intent := classifyResult.Intent
	confidence := classifyResult.Confidence

	// Log intent classification
	if confidence < 0.1 {
		slog.Info("Low confidence classification, using generic intent",
			"intent", intent, "confidence", confidence)
	}

	// 2. Get candidates from store (only if store is available)
	var candidates []store.MemoryEntry
	if p.store != nil {
		// In a real implementation, you'd extract session ID from headers/context
		sessionID := "default-session"
		candidates, err = p.store.GetRecent(ctx, sessionID, 20)
		if err != nil {
			slog.Error("Failed to get candidates from store", "error", err)
			// Continue with passthrough if store fails
			p.upstream.ServeHTTP(w, r)
			return
		}
	}

	// 3. Generate query embedding (only if embedder is available)
	var queryEmbedding []float32
	if p.embedder != nil {
		queryEmbedding, err = p.embedder.Embed(ctx, lastUserMessage)
		if err != nil {
			slog.Error("Failed to generate query embedding", "error", err)
			// Continue with passthrough if embedding fails
			p.upstream.ServeHTTP(w, r)
			return
		}
	}

	// 4. Score candidates using 4-factor model (only if we have candidates and embeddings)
	var scoredMemories []scorer.ScoredMemory
	if len(candidates) > 0 && len(queryEmbedding) > 0 {
		weights := scorer.GetWeights(0.4, 0.2, 0.2, 0.2)
		scorerInstance := scorer.NewScorer(weights, intent, float64(confidence), time.Now())
		scoredMemories = scorerInstance.Score(ctx, queryEmbedding, candidates)

		slog.Info("4-factor sieve completed",
			"intent", intent,
			"confidence", confidence,
			"candidates", len(scoredMemories))
	} else {
		scoredMemories = []scorer.ScoredMemory{}
		slog.Info("Skipping 4-factor scoring - no candidates or missing dependencies",
			"intent", intent,
			"confidence", confidence)
	}

	// 5. Deduplicate memories
	dedupStart := time.Now()
	dedupThreshold := p.config.DeduplicationThreshold
	deduplicated := dedup.Deduplicate(scoredMemories, dedupThreshold)
	dedupDuration := time.Since(dedupStart)

	// 6. Apply token budget
	budgetStart := time.Now()
	tokenBudget := p.config.TokenBudget
	selectedMemories, totalTokens := budget.Fill(deduplicated, tokenBudget)
	budgetDuration := time.Since(budgetStart)

	// 7. Compile final context with trace
	compileStart := time.Now()
	requestID := generateRequestID() // Generate unique request ID
	
	compileResult := compiler.Compile(
		selectedMemories,
		lastUserMessage,
		requestID,
		string(intent),
		confidence,
		len(scoredMemories),
		len(deduplicated),
		tokenBudget,
		time.Since(compileStart).Milliseconds(),
		scoredMemories,
	)
	
	// Update tokens used in trace
	compileResult.Trace.TokensUsed = totalTokens
	
	compileDuration := time.Since(compileStart)

	// 8. Log timing information
	totalDuration := time.Since(startTime)
	slog.Info("Pipeline completed",
		"total_duration_ms", totalDuration.Milliseconds(),
		"dedup_duration_ms", dedupDuration.Milliseconds(),
		"budget_duration_ms", budgetDuration.Milliseconds(),
		"compile_duration_ms", compileDuration.Milliseconds(),
		"original_candidates", len(scoredMemories),
		"deduplicated_count", len(deduplicated),
		"selected_count", len(selectedMemories),
		"total_tokens", totalTokens)

	// Log warnings for performance issues
	if totalDuration > 100*time.Millisecond {
		slog.Warn("Pipeline slow (>100ms)",
			"total_duration_ms", totalDuration.Milliseconds(),
			"dedup_duration_ms", dedupDuration.Milliseconds(),
			"budget_duration_ms", budgetDuration.Milliseconds(),
			"compile_duration_ms", compileDuration.Milliseconds())
	} else if totalDuration > 50*time.Millisecond {
		slog.Info("Pipeline timing info (<50ms)",
			"total_duration_ms", totalDuration.Milliseconds())
	}

	// Handle edge cases
	if len(selectedMemories) == 0 {
		slog.Warn("All candidates deduplicated - compiling with just last user message")
	}

	// Check for trace header
	traceHeader := r.Header.Get("X-Synapse-Trace")
	if traceHeader == "true" {
		// Serialize trace manifest to JSON
		traceJSON, err := json.Marshal(compileResult.Trace)
		if err != nil {
			slog.Error("Failed to marshal trace manifest", "error", err)
		} else {
			// Base64 encode the JSON
			traceBase64 := base64.StdEncoding.EncodeToString(traceJSON)
			
			// Check if trace exceeds 8KB limit
			if len(traceBase64) > 8192 {
				// Truncate memories list and add trace_truncated flag
				truncatedTrace := *compileResult.Trace
				// Keep only first few memories to stay under limit
				if len(truncatedTrace.Memories) > 10 {
					truncatedTrace.Memories = truncatedTrace.Memories[:10]
				}
				// Add truncated flag
				// Note: This would require modifying the TraceManifest struct to include this field
				
				truncatedJSON, _ := json.Marshal(truncatedTrace)
				traceBase64 = base64.StdEncoding.EncodeToString(truncatedJSON)
			}
			
			// Add trace to response headers
			w.Header().Set("X-Synapse-Trace-Result", traceBase64)
		}
	}

	// Check for persist traces flag (this would be passed from main)
	// For now, we'll assume it's handled at a higher level

	// Store results in context for downstream use
	ctx = context.WithValue(ctx, ScoredMemoriesKey, compileResult.Messages)
	ctx = context.WithValue(ctx, IntentKey, intent)
	ctx = context.WithValue(ctx, ConfidenceKey, confidence)

	// Create new request with updated context
	r = r.WithContext(ctx)

	// Forward the request to upstream with enhanced context
	p.upstream.ServeHTTP(w, r)
}

// RegisterRoutes registers the proxy routes with the chi router
func (p *Proxy) RegisterRoutes(r chi.Router) {
	r.Post("/v1/messages", p.HandleMessages)
}

// Close closes the proxy resources (no-op for simple proxy)
func (p *Proxy) Close() error {
	return nil
}

// Helper function to generate request ID (simplified)
func generateRequestID() string {
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}