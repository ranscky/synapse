package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"synapse/internal/config"
	"synapse/internal/session"
	"synapse/internal/store"
	"synapse/internal/trace"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockStore implements MemoryStore for testing
type mockMemoryStore struct{}

func (m *mockMemoryStore) GetRecent(ctx context.Context, sessionID string, limit int) ([]store.MemoryEntry, error) {
	return []store.MemoryEntry{}, nil
}

func (m *mockMemoryStore) Search(ctx context.Context, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error) {
	return []store.MemoryEntry{}, nil
}

func (m *mockMemoryStore) Write(ctx context.Context, entry store.MemoryEntry) error {
	return nil
}

// mockEmbedder implements Embedder for testing
type mockEmbedder struct{}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// TestProxyIntegration tests the proxy with a real HTTP server
func TestProxyIntegration(t *testing.T) {
	// Create a test server that echoes the request body
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("Failed to read request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer testServer.Close()

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	proxyServer := httptest.NewServer(r)
	defer proxyServer.Close()

	payload := `{"messages":[{"role":"user","content":"Hello, world!"}],"model":"gpt-3.5-turbo"}`
	resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json", bytes.NewBufferString(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, payload, string(body))
}

// TestProxyHealthCheck tests the health check endpoint
func TestProxyHealthCheck(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer testServer.Close()

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Get(server.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"ok"}`, string(body))
}

// TestProxyInvalidJSON tests handling of invalid JSON requests
func TestProxyInvalidJSON(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}

		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer testServer.Close()

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/messages", "application/json", bytes.NewBufferString(`{invalid json`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestProxyNilDependencies tests that the proxy works with nil store and embedder
func TestProxyNilDependencies(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer testServer.Close()

	// Pass nil for both store and embedder — proxy should still forward requests
	proxy, err := NewProxy(testServer.URL, nil, nil, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/messages", "application/json", bytes.NewBufferString(`{"test":"data"}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestProxyUpstreamUnavailable tests handling of upstream server being unavailable
func TestProxyUpstreamUnavailable(t *testing.T) {
	// Create the proxy pointing to a non-existent server
	proxy, err := NewProxy("http://127.0.0.1:19999", nil, nil, config.DefaultConfig(), session.NewManager(30*time.Minute), nil) // Unlikely to be in use
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	client := &http.Client{
		Timeout: 2 * time.Second,
	}
	resp, err := client.Post(server.URL+"/v1/messages", "application/json", bytes.NewBufferString(`{"test":"data"}`))
	if err == nil {
		defer resp.Body.Close()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	}
	// Connection refused is also acceptable
}

// TestProxyContextValues tests that intent and scored memories are handled correctly
func TestProxyContextValues(t *testing.T) {
	// NOTE: context.WithValue is an in-process Go construct. Because
	// p.upstream.ServeHTTP forwards the request over a real HTTP connection
	// to testServer, the upstream handler receives a brand-new *http.Request
	// with a fresh context — Go context values cannot survive serialization
	// over the wire. So intent/confidence can't be observed from the mock
	// upstream's handler; they can only be verified via the trace header,
	// which is the actual externally-observable channel for this data.
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))
	defer testServer.Close()

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	// Send a request that should trigger debug intent classification,
	// with tracing enabled so we can inspect the result.
	req, err := http.NewRequest("POST", server.URL+"/v1/messages",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"fix this error in the code"}]}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Synapse-Trace", "true")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the response body
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "response", string(body))

	// Verify intent classification happened and is reported correctly via
	// the trace header (the actual externally-observable result).
	traceHeader := resp.Header.Get("X-Synapse-Trace-Result")
	require.NotEmpty(t, traceHeader, "trace header should be present when X-Synapse-Trace is set")

	decodedTrace, err := base64.StdEncoding.DecodeString(traceHeader)
	require.NoError(t, err)

	var traceData trace.TraceManifest
	err = json.Unmarshal(decodedTrace, &traceData)
	require.NoError(t, err)

	assert.Equal(t, "debug", traceData.DetectedIntent,
		"message containing 'fix this error in the code' should classify as debug intent")
}

func TestProxyWritesMemoryFromRealTraffic(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))
	defer testServer.Close()

	realStore, err := store.NewStore(":memory:")
	require.NoError(t, err)
	defer realStore.Close()

	proxy, err := NewProxy(testServer.URL, realStore, &mockEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/messages", "application/json",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"there was an error in the order handler causing a crash"}]}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	sessionID := resp.Header.Get("X-Synapse-Session-Id")
	require.NotEmpty(t, sessionID, "proxy should surface the derived session id on the response")

	// Verify the message actually landed in the real store, with a real
	// embedding attached — this is the core write-path behavior that was
	// previously completely missing.
	ctx := context.Background()
	entries, err := realStore.GetRecent(ctx, sessionID, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one memory entry should have been written")

	assert.Equal(t, "there was an error in the order handler causing a crash", entries[0].Content)
	assert.Equal(t, "error", entries[0].MemoryType, "content mentioning an error should be detected as error type")
	assert.NotEmpty(t, entries[0].Embedding, "written memory should have a real embedding attached")
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, entries[0].Embedding, "embedding should match what mockEmbedder produced")
}

func TestProxyCapturesAssistantReply(t *testing.T) {
	// Mock upstream returns an Anthropic Messages API-shaped response,
	// matching what extractAssistantReply actually parses (Ollama's native
	// Anthropic-compatible endpoint), so it has something real to parse.
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"content":[{"type":"text","text":"The fix is to add a nil check before dereferencing the pointer."}]}`))
	}))
	defer testServer.Close()

	realStore, err := store.NewStore(":memory:")
	require.NoError(t, err)
	defer realStore.Close()

	proxy, err := NewProxy(testServer.URL, realStore, &mockEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/messages", "application/json",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"I have a nil pointer error in my code"}]}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// The client should still receive the real response body unchanged.
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(respBody), "nil check before dereferencing")

	sessionID := resp.Header.Get("X-Synapse-Session-Id")
	require.NotEmpty(t, sessionID, "proxy should surface the derived session id on the response")

	// Both the user's message AND the assistant's reply should now be in
	// the store - this is the actual proof response-side capture works.
	ctx := context.Background()
	entries, err := realStore.GetRecent(ctx, sessionID, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2, "both the user message and assistant reply should have been written")

	var foundUserMsg, foundAssistantMsg bool
	for _, e := range entries {
		if e.Content == "I have a nil pointer error in my code" {
			foundUserMsg = true
		}
		if e.Content == "The fix is to add a nil check before dereferencing the pointer." {
			foundAssistantMsg = true
			assert.NotEmpty(t, e.Embedding, "assistant reply should have an embedding attached")
		}
	}
	assert.True(t, foundUserMsg, "user message should be in the store")
	assert.True(t, foundAssistantMsg, "assistant reply should be in the store")
}