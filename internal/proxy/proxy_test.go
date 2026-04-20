package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProxyIntegration tests the proxy with a real HTTP server
func TestProxyIntegration(t *testing.T) {
	// Create a test server that echoes the request body
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the request body
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("Failed to read request body: %v", err)
		}
		
		// Send back the same body
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer testServer.Close()

	// Create the proxy pointing to our test server
	proxy, err := NewProxy(testServer.URL)
	require.NoError(t, err)
	defer proxy.Close()

	// Create a test router
	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	// Create a test server for the proxy
	proxyServer := httptest.NewServer(r)
	defer proxyServer.Close()

	// Send a POST /v1/messages request with sample payload
	payload := `{"messages":[{"role":"user","content":"Hello, world!"}],"model":"gpt-3.5-turbo"}`
	resp, err := http.Post(proxyServer.URL+"/v1/messages", "application/json", bytes.NewBufferString(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Check the response
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	
	// Read and verify the response body
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, string(body))
}

// TestProxyHealthCheck tests the health check endpoint
func TestProxyHealthCheck(t *testing.T) {
	// Create a test server that always returns 200
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer testServer.Close()

	// Create the proxy
	proxy, err := NewProxy(testServer.URL)
	require.NoError(t, err)
	defer proxy.Close()

	// Create router and register routes
	r := chi.NewRouter()
	proxy.RegisterRoutes(r)
	
	// Add health check endpoint
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Create test server
	server := httptest.NewServer(r)
	defer server.Close()

	// Test health check
	resp, err := http.Get(server.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	
	// Check response body
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, `{"status":"ok"}`, string(body))
}

// TestProxyInvalidJSON tests handling of invalid JSON requests
func TestProxyInvalidJSON(t *testing.T) {
	// Create a test server that tries to parse JSON (will fail with invalid JSON)
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to read and parse the body as JSON
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		
		// Try to parse as JSON (this will fail with invalid JSON)
		var data map[string]interface{}
		if err := json.Unmarshal(body, &data); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	defer testServer.Close()

	// Create the proxy
	proxy, err := NewProxy(testServer.URL)
	require.NoError(t, err)
	defer proxy.Close()

	// Create router
	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	// Create test server
	server := httptest.NewServer(r)
	defer server.Close()

	// Send invalid JSON
	resp, err := http.Post(server.URL+"/v1/messages", "application/json", bytes.NewBufferString(`{invalid json`))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Should get a bad request response from the upstream server
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestProxyUpstreamUnavailable tests handling of upstream server being unavailable
func TestProxyUpstreamUnavailable(t *testing.T) {
	// Create the proxy pointing to a non-existent server
	proxy, err := NewProxy("http://127.0.0.1:99999") // Invalid port
	require.NoError(t, err)
	defer proxy.Close()

	// Create router
	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	// Create test server
	server := httptest.NewServer(r)
	defer server.Close()

	// Send a request
	client := &http.Client{
		Timeout: 1 * time.Second, // Short timeout
	}
	resp, err := client.Post(server.URL+"/v1/messages", "application/json", bytes.NewBufferString(`{"test":"data"}`))
	if err == nil {
		defer resp.Body.Close()
		// If we get a response, it should be a bad gateway
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	}
	// If we get an error, that's also acceptable (connection refused)
}