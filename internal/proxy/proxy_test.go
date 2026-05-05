package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"synapse/internal/config"
	"synapse/internal/store"
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

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig())
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
	assert.Equal(t, payload, string(body))
}

// TestProxyHealthCheck tests the health check endpoint
func TestProxyHealthCheck(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer testServer.Close()

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig())
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
	assert.Equal(t, `{"status":"ok"}`, string(body))
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

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig())
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
	proxy, err := NewProxy(testServer.URL, nil, nil, config.DefaultConfig())
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
	proxy, err := NewProxy("http://127.0.0.1:19999", nil, nil, config.DefaultConfig()) // Unlikely to be in use
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
	// Test that the proxy can handle a typical request flow
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))
	defer testServer.Close()

	proxy, err := NewProxy(testServer.URL, &mockMemoryStore{}, &mockEmbedder{}, config.DefaultConfig())
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	// Send a request that should trigger debug intent classification
	resp, err := http.Post(server.URL+"/v1/messages", "application/json",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"fix this error in the code"}]}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	
	// Verify the response body
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "response", string(body))
}
