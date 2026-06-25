package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"synapse/internal/classifier"
	"synapse/internal/compiler"
	"synapse/internal/config"
	"synapse/internal/dedup"
	"synapse/internal/budget"
	"synapse/internal/embedder"
	"synapse/internal/scorer"
	"synapse/internal/store"
	"synapse/internal/trace"
)

// APIServer represents the REST API server
type APIServer struct {
	router          *chi.Mux
	store           *store.Store
	embedder        embedder.Embedder
	config          *config.Config
	rateLimiter     *RateLimiter
	persistTraces   bool
	compileTimes    []int64
	compileTimesMu  sync.RWMutex
}

// CompileRequest represents the request body for /v1/compile
type CompileRequest struct {
	Messages  []Message `json:"messages"`
	SessionID string    `json:"session_id"`
}

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompileResponse represents the response for /v1/compile
type CompileResponse struct {
	CompiledMessages []map[string]interface{} `json:"compiled_messages"`
	Trace            *trace.TraceManifest     `json:"trace"`
}

// StatsResponse represents the response for /v1/stats
type StatsResponse struct {
	Status          string `json:"status"`
	MemoriesStored  int    `json:"memories_stored"`
	AvgCompileMs    int64  `json:"avg_compile_ms"`
}

// NewAPIServer creates a new API server instance
func NewAPIServer(store *store.Store, emb embedder.Embedder, cfg *config.Config, persistTraces bool) *APIServer {
	api := &APIServer{
		router:        chi.NewRouter(),
		store:         store,
		embedder:      emb,
		config:        cfg,
		rateLimiter:   NewRateLimiter(100, time.Second), // 100 requests per second
		persistTraces: persistTraces,
		compileTimes:  make([]int64, 0, 1000), // Keep last 1000 compile times
	}
	
	api.setupRoutes()
	return api
}

// setupRoutes configures the API routes
func (a *APIServer) setupRoutes() {
	a.router.Use(a.rateLimitMiddleware)
	a.router.Use(a.loggingMiddleware)
	a.router.Use(a.securityHeadersMiddleware)
	
	a.router.Post("/v1/compile", a.handleCompile)
	a.router.Get("/v1/memories", a.handleGetMemories)
	a.router.Delete("/v1/memories", a.handleDeleteMemories)
	a.router.Get("/v1/stats", a.handleStats)
	a.router.Get("/openapi.yaml", a.serveOpenAPI)
}

// Router returns the chi router
func (a *APIServer) Router() *chi.Mux {
	return a.router
}

// validateSessionID validates the session ID format
func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	
	if len(sessionID) > 64 {
		return fmt.Errorf("session_id must be <= 64 characters")
	}
	
	// Check for valid characters (alphanumeric + hyphens)
	for _, r := range sessionID {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
			return fmt.Errorf("session_id contains invalid characters")
		}
	}
	
	return nil
}

// validateMessageContent validates message content
func validateMessageContent(content string) error {
	if strings.Contains(content, "\x00") {
		return fmt.Errorf("message content contains null bytes")
	}
	
	if len(content) > 32768 { // 32KB limit
		return fmt.Errorf("message content exceeds 32KB limit")
	}
	
	return nil
}

// extractSessionID extracts or generates session ID
func (a *APIServer) extractSessionID(r *http.Request, req CompileRequest) string {
	if req.SessionID != "" {
		return req.SessionID
	}
	
	// Use hash of Authorization header as session key
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		// In practice, you'd hash this properly
		return fmt.Sprintf("sess-%d", len(authHeader))
	}
	
	// Fallback to default
	return "default-session"
}

// handleCompile handles POST /v1/compile
func (a *APIServer) handleCompile(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	
	// Parse request body
	var req CompileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}
	
	// Validate request
	if len(req.Messages) == 0 {
		http.Error(w, "messages_required", http.StatusBadRequest)
		return
	}
	
	if err := validateSessionID(req.SessionID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	// Validate message contents
	for _, msg := range req.Messages {
		if err := validateMessageContent(msg.Content); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	
	// Extract session ID
	sessionID := a.extractSessionID(r, req)
	
	// Get last user message for classification
	lastUserMessage := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserMessage = req.Messages[i].Content
			break
		}
	}
	
	// 1. Classify the intent
	classifyResult := classifier.Classify(lastUserMessage)
	intent := classifyResult.Intent
	confidence := classifyResult.Confidence
	
	// 2. Get candidates from store
	ctx := r.Context()
	candidates, err := a.store.GetRecent(ctx, sessionID, 20)
	if err != nil {
		slog.Error("Failed to get candidates from store", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	
	// 3. Generate query embedding
	var queryEmbedding []float32
	if a.embedder != nil && lastUserMessage != "" {
		queryEmbedding, err = a.embedder.Embed(ctx, lastUserMessage)
		if err != nil {
			slog.Error("Failed to generate query embedding", "error", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}
	
	// 4. Score candidates using 4-factor model
	var scoredMemories []scorer.ScoredMemory
	if len(candidates) > 0 && len(queryEmbedding) > 0 {
		weights := scorer.GetWeights(0.4, 0.2, 0.2, 0.2)
		scorerInstance := scorer.NewScorer(weights, intent, float64(confidence), time.Now())
		scoredMemories = scorerInstance.Score(ctx, queryEmbedding, candidates)
	} else {
		scoredMemories = []scorer.ScoredMemory{}
	}
	
	// 5. Deduplicate memories
	dedupThreshold := a.config.DeduplicationThreshold
	deduplicated := dedup.Deduplicate(scoredMemories, dedupThreshold)
	
	// 6. Apply token budget
	tokenBudget := a.config.TokenBudget
	selectedMemories, totalTokens := budget.Fill(deduplicated, tokenBudget)
	
	// 7. Compile final context with trace
	compileStart := time.Now()
	requestID := generateRequestID()
	
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
	
	// Log timing information
	totalDuration := time.Since(startTime)
	compileDurationMs := compileDuration.Milliseconds()
	slog.Info("API compile completed",
		"total_duration_ms", totalDuration.Milliseconds(),
		"compile_duration_ms", compileDurationMs,
		"original_candidates", len(scoredMemories),
		"deduplicated_count", len(deduplicated),
		"selected_count", len(selectedMemories),
		"total_tokens", totalTokens)
	
	// Track compile time for stats
	a.addCompileTime(compileDurationMs)
	
	// Save trace if persistence is enabled
	if a.persistTraces && compileResult.Trace != nil {
		homeDir, err := os.UserHomeDir()
		if err == nil {
			baseDir := filepath.Join(homeDir, ".synapse")
			if err := compileResult.Trace.SaveToFile(baseDir); err != nil {
				slog.Error("Failed to save trace file", "error", err)
				// Don't fail the request, just log the error
			}
		}
	}
	
	// Create response
	response := CompileResponse{
		CompiledMessages: compileResult.Messages,
		Trace:            compileResult.Trace,
	}
	
	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	
	// Write response
	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Error("Failed to encode response", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// handleGetMemories handles GET /v1/memories
func (a *APIServer) handleGetMemories(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	limit := r.URL.Query().Get("limit")
	
	if sessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	
	if err := validateSessionID(sessionID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	// Parse limit (default to 20)
	limitInt := 20
	if limit != "" {
		// In practice, parse and validate the limit
	}
	
	// Get memories from store
	ctx := r.Context()
	memories, err := a.store.GetRecent(ctx, sessionID, limitInt)
	if err != nil {
		slog.Error("Failed to get memories", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	
	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	
	// Write response
	if err := json.NewEncoder(w).Encode(memories); err != nil {
		slog.Error("Failed to encode response", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// handleDeleteMemories handles DELETE /v1/memories
func (a *APIServer) handleDeleteMemories(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	
	if sessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	
	if err := validateSessionID(sessionID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	// Delete memories from store
	ctx := r.Context()
	if err := a.store.Delete(ctx, sessionID); err != nil {
		slog.Error("Failed to delete memories", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	
	slog.Info("Memories deleted", "session_id", sessionID)
	w.WriteHeader(http.StatusNoContent)
}

// addCompileTime adds a compile time to the tracking slice
func (a *APIServer) addCompileTime(duration int64) {
	a.compileTimesMu.Lock()
	defer a.compileTimesMu.Unlock()
	
	// Add new time
	a.compileTimes = append(a.compileTimes, duration)
	
	// Keep only last 1000 times
	if len(a.compileTimes) > 1000 {
		a.compileTimes = a.compileTimes[len(a.compileTimes)-1000:]
	}
}

// getAvgCompileTime calculates the average compile time
func (a *APIServer) getAvgCompileTime() int64 {
	a.compileTimesMu.RLock()
	defer a.compileTimesMu.RUnlock()
	
	if len(a.compileTimes) == 0 {
		return 0
	}
	
	var sum int64
	for _, t := range a.compileTimes {
		sum += t
	}
	
	return sum / int64(len(a.compileTimes))
}

// handleStats handles GET /v1/stats
func (a *APIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	// Get memory count from store
	ctx := r.Context()
	memoriesStored, err := a.store.CountMemories(ctx)
	if err != nil {
		slog.Error("Failed to count memories", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	
	// Get average compile time
	avgCompileMs := a.getAvgCompileTime()
	
	stats := StatsResponse{
		Status:         "ok",
		MemoriesStored: memoriesStored,
		AvgCompileMs:   avgCompileMs,
	}
	
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		slog.Error("Failed to encode stats response", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// serveOpenAPI serves the OpenAPI specification
func (a *APIServer) serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	// Read the openapi.yaml file from the root directory
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		http.Error(w, "OpenAPI specification not found", http.StatusNotFound)
		return
	}
	
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(data)
}

// rateLimitMiddleware applies rate limiting to requests
func (a *APIServer) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := getClientIP(r)
		if !a.rateLimiter.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs API requests with sanitized headers
func (a *APIServer) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(ww, r)
		slog.Info("API request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.statusCode,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// securityHeadersMiddleware adds security headers
func (a *APIServer) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent sensitive headers from being logged
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		next.ServeHTTP(w, r)
	})
}

// Helper function to generate request ID
func generateRequestID() string {
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// Helper function to get client IP
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP in the list
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	
	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	
	// Fall back to RemoteAddr
	return r.RemoteAddr
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}