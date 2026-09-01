package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"synapse/internal/budget"
	"synapse/internal/classifier"
	"synapse/internal/compiler"
	"synapse/internal/config"
	"synapse/internal/dedup"
	"synapse/internal/scorer"
	"synapse/internal/session"
	"synapse/internal/store"

	"github.com/go-chi/chi/v5"
)

// ContextKey represents context keys used in the proxy
type ContextKey string

const (
	ScoredMemoriesKey ContextKey = "scored_memories"
	IntentKey         ContextKey = "intent"
	ConfidenceKey     ContextKey = "confidence"
	SessionIDKey      ContextKey = "session_id"
	UserMessageKey    ContextKey = "user_message"
	TraceSessionIDKey ContextKey = "trace_session_id"
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
	sessionMgr *session.Manager
	recordCompileTime func(int64) // Optional callback for recording compile time in tests
}

// NewProxy creates a new proxy instance
func NewProxy(targetURL string, memStore MemoryStore, emb Embedder, cfg *config.Config, sessionMgr *session.Manager, recordCompileTime func(int64)) (*Proxy, error) {
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
		sessionMgr: sessionMgr,
		recordCompileTime: recordCompileTime, // Assign the callback if provided
	}

	// Create reverse proxy with director and response capture
	proxy.upstream = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			proxy.director(req)
		},
		ModifyResponse: func(resp *http.Response) error {
			// Surface the inspector's per-conversation session ID on the
			// response so it's visible in devtools/logs even without
			// opening the UI. Read via resp.Request.Context(), not a
			// pre-RoundTrip copy -- context.WithValue doesn't survive
			// the actual HTTP round trip otherwise.
			if sid, ok := resp.Request.Context().Value(TraceSessionIDKey).(string); ok && sid != "" {
				resp.Header.Set("X-Synapse-Session-Id", sid)
			}
			proxy.captureResponse(resp)
			return nil
		},
	}

	return proxy, nil
}

// deriveSessionID returns a stable session ID derived from whichever auth
// header is present. Checks Authorization first (OpenAI/Bearer-style
// clients, and Ollama's Bearer-token convention), then x-api-key (the
// actual Anthropic Messages API convention, which Synapse is otherwise
// built around) -- without this second check, real Anthropic-shaped
// clients that only send x-api-key would silently get a fresh session ID
// on every request, with no error, just zero memory recall. Two different
// keys of equal length no longer collide because we hash the content, not
// the length. Falls back to "default-session" only when neither header is
// present at all.
func deriveSessionID(r *http.Request, fallback string) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		authHeader = r.Header.Get("x-api-key")
	}
	if authHeader != "" {
		sum := sha256.Sum256([]byte(authHeader))
		return fmt.Sprintf("sess-%s", hex.EncodeToString(sum[:8]))
	}
	if fallback != "" {
		// No API key to bucket by -- fall back to the per-conversation
		// fingerprint instead of one flat "default-session" bucket, so
		// unrelated keyless conversations don't share memory recall.
		return fallback
	}
	return "default-session"
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

	// Splice the compiled, memory-augmented messages into the outgoing
	// body, replacing the client's original full-history "messages" array
	// while preserving every other top-level field (model, system, stream,
	// max_tokens, etc.) untouched. Without this, the compiler's output is
	// only ever visible in the trace header/UI — the upstream never
	// actually receives the reduced context.
	if compiledMessages, ok := req.Context().Value(ScoredMemoriesKey).([]map[string]interface{}); ok {
		if rewritten, err := rewriteRequestBody(req, compiledMessages); err != nil {
			slog.Error("Failed to rewrite request body with compiled context, forwarding original body", "error", err)
		} else {
			req.Body = io.NopCloser(bytes.NewReader(rewritten))
			req.ContentLength = int64(len(rewritten))
			req.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
		}
	}

	// IMPORTANT SECURITY: Never log Authorization headers
	// The header is passed through to upstream unchanged (client's own API
	// key reaches the provider; Synapse never holds provider credentials).
}

// rewriteRequestBody reads the original request body, replaces its
// top-level "messages" field with the compiled messages, and returns the
// re-marshaled body. All other top-level fields (model, system, stream,
// max_tokens, etc.) are preserved unchanged, since this function doesn't
// know or care which provider's schema those fields belong to.
func rewriteRequestBody(req *http.Request, compiledMessages []map[string]interface{}) ([]byte, error) {
	original, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read original body: %w", err)
	}
	req.Body.Close()

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(original, &bodyMap); err != nil {
		return nil, fmt.Errorf("failed to parse original body as JSON: %w", err)
	}

	bodyMap["messages"] = compiledMessages

	rewritten, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal rewritten body: %w", err)
	}

	return rewritten, nil
}

// captureResponse reads the upstream's response, extracts the assistant's
// reply, and writes it to the store as a memory entry — completing the
// write path so memory captures both sides of the conversation, not just
// the user's outgoing message. Best-effort: any failure here is logged but
// never blocks the response from reaching the original client, since
// resp.Body must still be readable by whoever called Synapse.
func (p *Proxy) captureResponse(resp *http.Response) {
	if p.store == nil {
		return
	}

	req := resp.Request
	if req == nil {
		return
	}

	ctx := req.Context()

	// Prefer the session ID stored in context by HandleMessages; fall back
	// to re-deriving it from the Authorization header so captureResponse
	// always writes to the correct session bucket.
	sessionID, _ := ctx.Value(SessionIDKey).(string)
	if sessionID == "" {
		traceID, _ := ctx.Value(TraceSessionIDKey).(string)
		sessionID = deriveSessionID(req, traceID)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("Failed to read upstream response body for memory capture", "error", err)
		return
	}
	resp.Body.Close()
	// Restore the body so the original client still receives it.
	resp.Body = io.NopCloser(bytes.NewBuffer(body))

	assistantReply := extractAssistantReply(body)
	if assistantReply == "" {
		// Couldn't find a reply in the expected shape — nothing to write,
		// but the response to the client is unaffected either way.
		return
	}

	var embedding []float32
	if p.embedder != nil {
		embedding, err = p.embedder.Embed(ctx, assistantReply)
		if err != nil {
			slog.Error("Failed to generate embedding for assistant reply", "error", err)
		}
	}

	memEntry := store.MemoryEntry{
		ID:         generateRequestID(),
		SessionID:  sessionID,
		Content:    assistantReply,
		MemoryType: store.DetectMemoryType(assistantReply),
		Timestamp:  time.Now(),
		Embedding:  embedding,
	}

	if writeErr := p.store.Write(ctx, memEntry); writeErr != nil {
		slog.Error("Failed to write assistant reply to memory", "error", writeErr)
	}
}

// extractTextContent normalizes a message's content field, which per both
// the Anthropic and OpenAI APIs can be either a plain string or an array
// of typed content blocks (e.g. [{"type":"text","text":"..."}] alongside
// image blocks, tool-use blocks, etc.). Modern clients -- Cline included
// -- commonly send array-shaped content even for plain text messages.
// Mirrors the same string-or-array handling session.Message already uses
// (json.RawMessage), applied here to request parsing rather than session
// fingerprinting. Returns the concatenated text from any "text"-typed
// blocks, or the plain string directly if that's what was sent.
// Best-effort: returns empty string on an unrecognized shape rather than
// erroring the whole request.
func extractTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				sb.WriteString(block.Text)
			}
		}
		return sb.String()
	}

	return ""
}

// extractAssistantReply tries the Anthropic Messages API shape first
// (content: [{"type":"text","text":"..."}]), then Ollama's native
// /api/chat shape ({"message":{"content":"..."}}), then the OpenAI Chat
// Completions shape ({"choices":[{"message":{"content":"..."}}]}) if
// neither of those match. This proxy's own /v1/messages route and
// Ollama's native Anthropic-compatible endpoint return the first shape;
// the native /api/chat route returns the second; /v1/chat/completions
// (used by Cline's "OpenAI Compatible" provider and most other coding
// tools pointed at a self-hosted endpoint) returns the third. Returns an
// empty string if none of the shapes match, rather than erroring -- this
// is a best-effort extraction, not a strict contract with the upstream.
func extractAssistantReply(body []byte) string {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(body, &parsed); err == nil {
		var sb strings.Builder
		for _, block := range parsed.Content {
			if block.Type == "text" {
				sb.WriteString(block.Text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}

	var ollamaShape struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &ollamaShape); err == nil && ollamaShape.Message.Content != "" {
		return ollamaShape.Message.Content
	}

	var openaiShape struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &openaiShape); err == nil && len(openaiShape.Choices) > 0 && openaiShape.Choices[0].Message.Content != "" {
		return openaiShape.Choices[0].Message.Content
	}

	return ""
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

	// Parse the request body to extract the actual last user message and
	// system prompt, rather than using the raw JSON body (which previously
	// fed the entire request structure into the classifier/embedder,
	// diluting any real signal with JSON syntax, role labels, and unrelated
	// message history).
	var parsedRequest struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}

	// Fingerprint this conversation for the trace inspector. Parsed
	// separately from `parsedRequest` above because session.Message keeps
	// Content as raw JSON (string or content-block array), whereas
	// parsedRequest simplifies Content to a string for classification.
	var sessionParse struct {
		Messages []session.Message `json:"messages"`
	}
	var traceSessionID string
	if p.sessionMgr != nil {
		if err := json.Unmarshal(body, &sessionParse); err != nil {
			slog.Warn("Failed to parse messages for session fingerprinting", "error", err)
		} else {
			traceSessionID, _ = p.sessionMgr.Identify(sessionParse.Messages)
		}
	}

	if err := json.Unmarshal(body, &parsedRequest); err != nil {
		slog.Warn("Rejecting request: body is not valid JSON", "error", err)
		http.Error(w, "Request body must be valid JSON", http.StatusBadRequest)
		return
	}

	systemPrompt := parsedRequest.System
	lastUserMessage := ""
	for i := len(parsedRequest.Messages) - 1; i >= 0; i-- {
		if parsedRequest.Messages[i].Role == "user" {
			lastUserMessage = extractTextContent(parsedRequest.Messages[i].Content)
			break
		}
	}

	// 1. Classify the intent
	classifyResult := classifier.Classify(lastUserMessage)
	intent := classifyResult.Intent
	confidence := classifyResult.Confidence

	// Fall back to classifying the system prompt when the user message alone
	// gives weak signal (e.g. "what did I just tell you?" carries no debug/
	// code/plan/write keywords on its own). Only applied when confidence is
	// already low, and only for short system prompts (persona-line length,
	// not full boilerplate specs) -- a long system prompt would dilute its
	// own keyword density the same way long code-heavy messages did before
	// stripCode() was added, so it's excluded rather than risking a worse
	// signal than doing nothing.
	const lowConfidenceThreshold = 0.3
	const maxSystemPromptLenForClassification = 300
	if confidence < lowConfidenceThreshold && systemPrompt != "" && len(systemPrompt) <= maxSystemPromptLenForClassification {
		systemClassifyResult := classifier.Classify(systemPrompt)
		if systemClassifyResult.Confidence > confidence {
			slog.Info("Low confidence from user message, using system prompt classification instead",
				"user_intent", intent, "user_confidence", confidence,
				"system_intent", systemClassifyResult.Intent, "system_confidence", systemClassifyResult.Confidence)
			intent = systemClassifyResult.Intent
			confidence = systemClassifyResult.Confidence
		}
	}

	// Log intent classification
	if confidence < 0.1 {
		slog.Info("Low confidence classification, using generic intent",
			"intent", intent, "confidence", confidence)
	}

	// 2. Derive session ID from the Authorization header so each API key
	// gets its own memory bucket. Falls back to "default-session" only when
	// no Authorization header is present at all.
	sessionID := deriveSessionID(r, traceSessionID)

	var candidates []store.MemoryEntry
	var storeDuration time.Duration
	if p.store != nil {
		storeStart := time.Now()
		candidates, err = p.store.GetRecent(ctx, sessionID, 20)
		storeDuration = time.Since(storeStart)
		if err != nil {
			slog.Error("Failed to get candidates from store", "error", err)
			p.upstream.ServeHTTP(w, r)
			return
		}
	}

	// 3. Generate query embedding (only if embedder is available)
	var queryEmbedding []float32
	var embedDuration time.Duration
	if p.embedder != nil {
		embedStart := time.Now()
		queryEmbedding, err = p.embedder.Embed(ctx, lastUserMessage)
		embedDuration = time.Since(embedStart)
		if err != nil {
			slog.Error("Failed to generate query embedding", "error", err)
			p.upstream.ServeHTTP(w, r)
			return
		}
	}

	// 3b. Write this message to the store so memory actually accumulates
	// from real traffic. Best-effort: a write failure shouldn't block the
	// request from being forwarded upstream, but it is logged so silent
	// data loss doesn't go unnoticed.
	if p.store != nil && lastUserMessage != "" {
		memEntry := store.MemoryEntry{
			ID:         generateRequestID(),
			SessionID:  sessionID,
			Content:    lastUserMessage,
			MemoryType: store.DetectMemoryType(lastUserMessage),
			Timestamp:  time.Now(),
			Embedding:  queryEmbedding,
		}
		if writeErr := p.store.Write(ctx, memEntry); writeErr != nil {
			slog.Error("Failed to write memory entry", "error", writeErr)
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
		0, // placeholder -- real duration set below, after Compile() actually runs
		scoredMemories,
		deduplicated,
	)
	compileDuration := time.Since(compileStart)

	// Report the real compile duration to the stats tracker, if wired.
	if p.recordCompileTime != nil {
		p.recordCompileTime(compileDuration.Milliseconds())
	}

	// Update tokens used and the REAL compile duration in the trace. Both
	// must be set after the fact: Go evaluates function arguments before
	// making the call, so passing time.Since(compileStart) directly as an
	// argument to Compile() always measured ~0ms -- before any compiling
	// work had actually happened.
	compileResult.Trace.TokensUsed = totalTokens
	compileResult.Trace.CompileDurationMs = compileDuration.Milliseconds()
	
	if compileResult.Trace.CandidatePoolTokens > 0 {
		compileResult.Trace.ReductionPct = float64(compileResult.Trace.CandidatePoolTokens-totalTokens) / float64(compileResult.Trace.CandidatePoolTokens) * 100
	}

	if p.sessionMgr != nil && traceSessionID != "" {
		p.sessionMgr.SetTrace(traceSessionID, compileResult.Trace)
		p.sessionMgr.SetIntent(traceSessionID, string(intent))
	}

	// Sum tokens across the full pre-dedup, pre-budget candidate pool using
	// the same CountTokens/encoding budget.Fill already uses, so this is an
	// apples-to-apples comparison against totalTokens (the compiled output)
	// rather than a separately-estimated baseline. Scoped to the retrieved
	// candidate pool, not literal zero-context -- that's the number that
	// actually reflects what the 4-factor sieve saved.
	candidatePoolTokens := 0
	for _, m := range scoredMemories {
		t, _ := budget.CountTokens(m.Content, "cl100k_base")
		candidatePoolTokens += t
	}
	reductionPct := 0.0
	if candidatePoolTokens > 0 {
		reductionPct = float64(candidatePoolTokens-totalTokens) / float64(candidatePoolTokens) * 100
	}

	// 8. Log timing information
	totalDuration := time.Since(startTime)
	slog.Info("Pipeline completed",
		"total_duration_ms", totalDuration.Milliseconds(),
		"store_duration_ms", storeDuration.Milliseconds(),
		"embed_duration_ms", embedDuration.Milliseconds(),
		"dedup_duration_ms", dedupDuration.Milliseconds(),
		"budget_duration_ms", budgetDuration.Milliseconds(),
		"compile_duration_ms", compileDuration.Milliseconds(),
		"original_candidates", len(scoredMemories),
		"deduplicated_count", len(deduplicated),
		"selected_count", len(selectedMemories),
		"candidate_pool_tokens", candidatePoolTokens,
		"total_tokens", totalTokens,
		"reduction_pct", fmt.Sprintf("%.1f", reductionPct))

	if totalDuration > 100*time.Millisecond {
		slog.Warn("Pipeline slow (>100ms)",
			"total_duration_ms", totalDuration.Milliseconds(),
			"store_duration_ms", storeDuration.Milliseconds(),
			"embed_duration_ms", embedDuration.Milliseconds(),
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
		traceJSON, err := json.Marshal(compileResult.Trace)
		if err != nil {
			slog.Error("Failed to marshal trace manifest", "error", err)
		} else {
			traceBase64 := base64.StdEncoding.EncodeToString(traceJSON)

			// Check if trace exceeds 8KB limit
			if len(traceBase64) > 8192 {
				truncatedTrace := *compileResult.Trace
				if len(truncatedTrace.Memories) > 10 {
					truncatedTrace.Memories = truncatedTrace.Memories[:10]
				}
				truncatedJSON, _ := json.Marshal(truncatedTrace)
				traceBase64 = base64.StdEncoding.EncodeToString(truncatedJSON)
			}

			w.Header().Set("X-Synapse-Trace-Result", traceBase64)
		}
	}

	// Store results in context for downstream use, including data
	// ModifyResponse will need to write the assistant's reply to the same
	// session once the upstream responds.
	ctx = context.WithValue(ctx, ScoredMemoriesKey, compileResult.Messages)
	ctx = context.WithValue(ctx, IntentKey, intent)
	ctx = context.WithValue(ctx, ConfidenceKey, confidence)
	ctx = context.WithValue(ctx, SessionIDKey, sessionID)
	ctx = context.WithValue(ctx, UserMessageKey, lastUserMessage)
	ctx = context.WithValue(ctx, TraceSessionIDKey, traceSessionID)

	// Create new request with updated context
	r = r.WithContext(ctx)

	// Forward the request to upstream with enhanced context
	p.upstream.ServeHTTP(w, r)
}

// HandleOllamaChat accepts Ollama's native POST /api/chat request shape
// (which already matches HandleMessages' generic {model, messages:
// [{role, content}], ...} parsing) and forces stream:false before
// delegating to the same pipeline. Forcing non-streaming is a deliberate
// scope decision, not an oversight: captureResponse fully buffers the
// upstream response (io.ReadAll) before it's returned to the client, so
// this pipeline was never going to deliver true token-by-token streaming
// regardless of route -- and a streaming response's NDJSON chunks
// wouldn't match extractAssistantReply's shape anyway, silently losing
// memory capture. Forcing non-streaming trades live typing in the
// terminal for a reliably complete, correctly-captured reply. The client
// (e.g. `ollama run`) will see a pause and then the full response appear
// at once, rather than word-by-word. True incremental streaming support
// would need a rework of captureResponse itself and applies to every
// route, not just this one -- worth a deliberate separate pass later,
// not bundled into this fix.
func (p *Proxy) HandleOllamaChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Failed to read request body", "error", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(body, &bodyMap); err != nil {
		slog.Warn("Rejecting request: body is not valid JSON", "error", err)
		http.Error(w, "Request body must be valid JSON", http.StatusBadRequest)
		return
	}
	bodyMap["stream"] = false

	rewritten, err := json.Marshal(bodyMap)
	if err != nil {
		slog.Error("Failed to re-marshal request body", "error", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))

	p.HandleMessages(w, r)
}

// HandleOpenAIChat accepts the OpenAI Chat Completions request shape
// (POST /v1/chat/completions), which the "OpenAI Compatible" provider
// setting in Cline and most other coding-agent tools sends by default
// when pointed at a self-hosted/local endpoint. Without this route, that
// traffic fell through to HandlePassthrough and silently bypassed the
// entire memory pipeline -- no error, no captured history, just quiet
// passthrough to upstream. Same delegation pattern as HandleOllamaChat:
// OpenAI's request shape (messages: [{role, content}]) already matches
// HandleMessages' generic parsing, so this just forces stream:false
// (same streaming-capture reasoning as the Ollama route) and delegates.
func (p *Proxy) HandleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Failed to read request body", "error", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	var bodyMap map[string]interface{}
	if err := json.Unmarshal(body, &bodyMap); err != nil {
		slog.Warn("Rejecting request: body is not valid JSON", "error", err)
		http.Error(w, "Request body must be valid JSON", http.StatusBadRequest)
		return
	}
	bodyMap["stream"] = false

	rewritten, err := json.Marshal(bodyMap)
	if err != nil {
		slog.Error("Failed to re-marshal request body", "error", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))

	p.HandleMessages(w, r)
}

// HandlePassthrough forwards a request straight through to upstream
// unmodified, with no pipeline processing. Used for Ollama's health check
// (GET/HEAD /) and, via NotFound, for every other control-plane endpoint
// (model listing, status, pulls, embeddings, version, etc.) that doesn't
// carry conversation content. Only endpoints that actually carry chat
// content (/v1/messages, /api/chat) get the memory-augmented pipeline --
// whitelisting every other Ollama/provider endpoint one at a time as
// clients discover them by trial and error doesn't scale, and doesn't
// match Synapse's own design goal of requiring no client-side changes.
func (p *Proxy) HandlePassthrough(w http.ResponseWriter, r *http.Request) {
	p.upstream.ServeHTTP(w, r)
}

func (p *Proxy) RegisterRoutes(r chi.Router) {
	r.Get("/", p.HandlePassthrough)
	r.Head("/", p.HandlePassthrough)
	r.Post("/v1/messages", p.HandleMessages)
	r.Post("/api/chat", p.HandleOllamaChat)
	r.Post("/v1/chat/completions", p.HandleOpenAIChat)
	r.NotFound(p.HandlePassthrough)
}

// Close closes the proxy resources (no-op for simple proxy)
func (p *Proxy) Close() error {
	return nil
}

// requestIDCounter guarantees uniqueness even when two IDs are requested
// within the same clock tick. time.Now().UnixNano() alone isn't a safe
// uniqueness source -- OS clock resolution varies (Windows' default system
// timer is far coarser than Linux's), so two fast successive calls can
// return identical nanosecond values, producing colliding request/memory
// IDs. Since memories.id is the primary key with INSERT OR REPLACE, a
// collision silently overwrites one entry with the other.
var requestIDCounter uint64

// Helper function to generate request ID
func generateRequestID() string {
	seq := atomic.AddUint64(&requestIDCounter, 1)
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), seq)
}