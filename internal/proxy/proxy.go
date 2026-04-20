package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/go-chi/chi/v5"
	"synapse/internal/embedder"
	"synapse/internal/store"
)

// Proxy represents the reverse proxy handler
type Proxy struct {
	target     *url.URL
	transport  http.RoundTripper
	store      *store.Store
	embedder   embedder.Embedder
	upstream   *httputil.ReverseProxy
}

// Message represents a chat message
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// MessagesRequest represents the incoming messages request
type MessagesRequest struct {
	Messages []Message `json:"messages"`
	Model    string    `json:"model"`
	Stream   bool      `json:"stream"`
}

// NewProxy creates a new proxy instance
func NewProxy(targetURL string, store *store.Store, embedder embedder.Embedder) (*Proxy, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	proxy := &Proxy{
		target:   target,
		transport: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		store:    store,
		embedder: embedder,
	}

	// Create reverse proxy
	proxy.upstream = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			proxy.director(req)
		},
		Transport: proxy.transport,
	}

	return proxy, nil
}

// director modifies the outgoing request to forward to the target
func (p *Proxy) director(req *http.Request) {
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
}

// HandleMessages handles POST /v1/messages requests
func (p *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	
	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Parse the messages request
	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Error("Failed to parse messages request", "error", err)
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Extract session ID from headers or generate one
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	// Process messages if we have any
	if len(req.Messages) > 0 {
		if err := p.processMessages(ctx, sessionID, req.Messages); err != nil {
			slog.Error("Failed to process messages", "error", err)
		}
	}

	// Forward the request to upstream
	r.Body = io.NopCloser(bytes.NewReader(body))
	p.upstream.ServeHTTP(w, r)
}

// processMessages processes and stores messages from the conversation history
func (p *Proxy) processMessages(ctx context.Context, sessionID string, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}

	// Process all messages except the last one (which is the current query)
	historyMessages := messages
	if len(messages) > 1 {
		historyMessages = messages[:len(messages)-1]
	}

	memoriesWritten := 0
	for _, msg := range historyMessages {
		// Skip empty messages
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}

		// Detect memory type based on content
		memoryType := store.DetectMemoryType(msg.Content)

		// Create memory entry
		entry := store.MemoryEntry{
			ID:         uuid.New().String(),
			SessionID:  sessionID,
			Content:    msg.Content,
			MemoryType: memoryType,
			Timestamp:  time.Now(),
			Importance: 0.0, // TODO: Implement importance scoring
		}

		// Store the memory entry
		if err := p.store.Write(ctx, entry); err != nil {
			slog.Error("Failed to write memory entry", "error", err, "session_id", sessionID)
			continue
		}

		memoriesWritten++
	}

	// Process the last message as a query (if it exists and is not empty)
	if len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		if strings.TrimSpace(lastMsg.Content) != "" {
			// Generate embedding for the query
			embedding, err := p.embedder.Embed(ctx, lastMsg.Content)
			if err != nil {
				slog.Error("Failed to generate embedding for query", "error", err)
				return nil
			}

			// Search for similar memories
			candidates, err := p.store.Search(ctx, embedding, sessionID, 20)
			if err != nil {
				slog.Error("Failed to search memories", "error", err)
			} else {
				slog.Info("Retrieved memory candidates", "count", len(candidates), "session_id", sessionID)
			}
		}
	}

	slog.Info("Processed messages", "memories_written", memoriesWritten, "session_id", sessionID)
	return nil
}

// RegisterRoutes registers the proxy routes with the chi router
func (p *Proxy) RegisterRoutes(r chi.Router) {
	r.Post("/v1/messages", p.HandleMessages)
	// Add other routes as needed
}

// Close closes the proxy resources
func (p *Proxy) Close() error {
	if closer, ok := p.transport.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}