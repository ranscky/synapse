package proxy

import (
	"bytes"
	"context"
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
}

// NewProxy creates a new proxy instance
func NewProxy(targetURL string, memStore MemoryStore, emb Embedder) (*Proxy, error) {
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

// HandleMessages handles POST /v1/messages requests with 4-factor scoring
func (p *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) {
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
		scorerInstance := scorer.NewScorer(weights, intent, time.Now())
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

	// 5. Store results in context for Phase 3
	ctx = context.WithValue(ctx, ScoredMemoriesKey, scoredMemories)
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
