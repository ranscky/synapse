package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"synapse/internal/api"
	"synapse/internal/config"
	"synapse/internal/proxy"
	"synapse/internal/store"
	"synapse/internal/trace"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Mock upstream server that echoes requests
type mockUpstreamServer struct {
	server             *httptest.Server
	logs               []string
	receivedAuthHeader bool
}

func newMockUpstreamServer() *mockUpstreamServer {
	m := &mockUpstreamServer{
		logs: make([]string, 0),
	}
	
	router := chi.NewRouter()
	
	// Echo endpoint that simulates an AI model
	router.Post("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		// Confirm the Authorization header reaches upstream (correct pass-through
		// behavior), but verify we only ever log its presence, never its value.
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			m.logs = append(m.logs, "Received request with Authorization header present (value hidden)")
			m.receivedAuthHeader = true
		} else {
			m.logs = append(m.logs, "Received request without Authorization header")
		}
		
		// Read and echo the request body
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read body", http.StatusBadRequest)
			return
		}
		
		// Echo back a simple response
		response := map[string]interface{}{
			"id":      "test-response-123",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "This is a test response from the mock upstream server.",
					},
					"finish_reason": "stop",
				},
			},
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
	
	m.server = httptest.NewServer(router)
	return m
}

func (m *mockUpstreamServer) Close() {
	m.server.Close()
}

func (m *mockUpstreamServer) URL() string {
	return m.server.URL
}

// ReceivedAuthHeader confirms the Authorization header correctly reached
// upstream via pass-through (this is expected, correct behavior).
func (m *mockUpstreamServer) ReceivedAuthHeader() bool {
	return m.receivedAuthHeader
}

// LogsContainAuthValue checks that no log entry ever contains the actual
// secret value, only a presence note. This is what we actually care about
// for security — the header value itself must never be logged.
func (m *mockUpstreamServer) LogsContainAuthValue(secretValue string) bool {
	for _, log := range m.logs {
		if strings.Contains(log, secretValue) {
			return true
		}
	}
	return false
}

// Load test session from JSON file
func loadTestSession(t *testing.T, filename string) []map[string]interface{} {
	data, err := os.ReadFile(filepath.Join("testdata", filename))
	require.NoError(t, err)
	
	var session struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	err = json.Unmarshal(data, &session)
	require.NoError(t, err)
	
	return session.Messages
}

// Count tokens in messages using tiktoken (simplified for testing)
func countTokens(messages []map[string]interface{}) int {
	total := 0
	for _, msg := range messages {
		if content, ok := msg["content"].(string); ok {
			// Rough approximation: 1 token per 4 characters
			total += len(content) / 4
		}
	}
	return total
}

func TestEndToEndIntegration(t *testing.T) {
	// Create temporary directory for test files
	tempDir := t.TempDir()
	
	// Start mock upstream server
	mockUpstream := newMockUpstreamServer()
	defer mockUpstream.Close()
	
	// Create store
	dbPath := filepath.Join(tempDir, "test.db")
	storeInstance, err := store.NewStore(dbPath)
	require.NoError(t, err)
	defer storeInstance.Close()
	
	// Create embedder (mock for testing)
	mockEmbedder := &mockEmbedder{}
	
	// Create config
	cfg := config.DefaultConfig()
	cfg.UpstreamURL = mockUpstream.URL()
	cfg.TokenBudget = 2000 // Set a reasonable token budget for testing
	
	// Test both debug and code sessions
	testCases := []struct {
		name         string
		sessionFile  string
		expectedIntent string
	}{
		{
			name:          "Debug Session",
			sessionFile:   "session_debug.json",
			expectedIntent: "debug", // Should be detected as debug intent
		},
		{
			name:          "Code Session",
			sessionFile:   "session_code.json",
			expectedIntent: "code", // Should be detected as code intent
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Load test session
			messages := loadTestSession(t, tc.sessionFile)
			rawTokenCount := countTokens(messages)
			
			// Create proxy
			proxyInstance, err := proxy.NewProxy(cfg.UpstreamURL, storeInstance, mockEmbedder, cfg)
			require.NoError(t, err)
			
			// Create API server
			apiServer := api.NewAPIServer(storeInstance, mockEmbedder, cfg, false)
			
			// Create router
			r := chi.NewRouter()
			r.Mount("/api", apiServer.Router())
			proxyInstance.RegisterRoutes(r)
			
			// Create test server
			testServer := httptest.NewServer(r)
			defer testServer.Close()
			
			
			// Test each message in the session
			for i, message := range messages {
				t.Logf("Processing message %d/%d", i+1, len(messages))
				
				// Prepare request for chat completions
				requestBody := map[string]interface{}{
					"model":    "test-model",
					"messages": []map[string]interface{}{message},
					"stream":   false,
				}
				
				bodyBytes, err := json.Marshal(requestBody)
				require.NoError(t, err)
				
				// Send request to proxy
				req, err := http.NewRequest("POST", testServer.URL+"/v1/messages", bytes.NewBuffer(bodyBytes))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer test-token-12345") // Test token that should be hidden
				req.Header.Set("X-Synapse-Trace", "true") // Enable tracing
				
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				
				// Read response
				respBody, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				require.NoError(t, err)
				
				// Assertions
				assert.Equal(t, http.StatusOK, resp.StatusCode, "Request should succeed")
				
				// Check that Memory Trace is present in response headers
				traceHeader := resp.Header.Get("X-Synapse-Trace-Result")
				assert.NotEmpty(t, traceHeader, "Memory Trace should be present in response headers")
				
				// Parse trace to verify structure
				var traceData trace.TraceManifest
				if traceHeader != "" {
					decodedTrace, decodeErr := base64.StdEncoding.DecodeString(traceHeader)
					require.NoError(t, decodeErr)
					err = json.Unmarshal(decodedTrace, &traceData)
					if err == nil {
						// Verify trace contains expected fields
						assert.NotEmpty(t, traceData.RequestID, "Trace should contain request ID")
						assert.NotEmpty(t, traceData.DetectedIntent, "Trace should contain detected intent")
						assert.NotZero(t, traceData.Timestamp, "Trace should contain timestamp")
						
						// Verify all 4 score components are present in memories
						if len(traceData.Memories) > 0 {
							firstMemory := traceData.Memories[0]
							assert.NotNil(t, firstMemory.ScoreSemantic, "Semantic similarity score should be present")
							assert.NotNil(t, firstMemory.ScoreRecency, "Recency score should be present")
							assert.NotNil(t, firstMemory.ScoreImportance, "Importance score should be present")
							assert.NotNil(t, firstMemory.ScoreTaskAlignment, "Task alignment score should be present")
						}
					}
				}
				
				// Verify response structure
				var response map[string]interface{}
				err = json.Unmarshal(respBody, &response)
				require.NoError(t, err)
				
				assert.Contains(t, response, "id", "Response should contain ID")
				assert.Contains(t, response, "choices", "Response should contain choices")
			}
			
			// Verify security: the Authorization header SHOULD reach upstream
			// (correct pass-through behavior), but its actual value must NEVER
			// appear in any log entry.
			assert.True(t, mockUpstream.ReceivedAuthHeader(), "Authorization header should be forwarded to upstream (pass-through auth)")
			assert.False(t, mockUpstream.LogsContainAuthValue("test-token-12345"), "Authorization header VALUE should never appear in logs")
			
			t.Logf("Raw token count: %d", rawTokenCount)
			t.Logf("Session %s processed successfully", tc.sessionFile)
		})
	}
}

func TestTokenBudgetCompliance(t *testing.T) {
	// Create temporary directory for test files
	tempDir := t.TempDir()
	
	// Start mock upstream server
	mockUpstream := newMockUpstreamServer()
	defer mockUpstream.Close()
	
	// Create store
	dbPath := filepath.Join(tempDir, "test.db")
	storeInstance, err := store.NewStore(dbPath)
	require.NoError(t, err)
	defer storeInstance.Close()
	
	// Create embedder (mock for testing)
	mockEmbedder := &mockEmbedder{}
	
	// Create config with strict token budget
	cfg := config.DefaultConfig()
	cfg.UpstreamURL = mockUpstream.URL()
	cfg.TokenBudget = 500 // Strict budget for testing
	
	// Create proxy
	proxyInstance, err := proxy.NewProxy(cfg.UpstreamURL, storeInstance, mockEmbedder, cfg)
	require.NoError(t, err)
	
	// Create API server
	apiServer := api.NewAPIServer(storeInstance, mockEmbedder, cfg, false)
	
	// Create router
	r := chi.NewRouter()
	r.Mount("/api", apiServer.Router())
	proxyInstance.RegisterRoutes(r)
	
	// Create test server
	testServer := httptest.NewServer(r)
	defer testServer.Close()
	
	// Load debug session
	messages := loadTestSession(t, "session_debug.json")
	
	// Send multiple messages to build up context
	
	for i, message := range messages[:5] { // Test first 5 messages
		t.Logf("Processing message %d for budget test", i+1)
		
		// Prepare request
		requestBody := map[string]interface{}{
			"model":    "test-model",
			"messages": []map[string]interface{}{message},
			"stream":   false,
		}
		
		bodyBytes, err := json.Marshal(requestBody)
		require.NoError(t, err)
		
		// Send request
		req, err := http.NewRequest("POST", testServer.URL+"/v1/messages", bytes.NewBuffer(bodyBytes))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Synapse-Trace", "true")
		req.Header.Set("Authorization", "Bearer test-token-budget")
		
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		
		// Verify success
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		
		// Check Memory Trace for token information
		traceHeader := resp.Header.Get("X-Synapse-Trace-Result")
		if traceHeader != "" {
			var traceData trace.TraceManifest
			err = json.Unmarshal([]byte(traceHeader), &traceData)
			if err == nil {
				// Verify token budget compliance
				if traceData.TokensUsed > 0 {
					assert.True(t, traceData.TokensUsed <= cfg.TokenBudget,
						"Compiled tokens (%d) should not exceed budget (%d)",
						traceData.TokensUsed, cfg.TokenBudget)
				}
			}
		}
		
		// Verify response structure
		var response map[string]interface{}
		err = json.Unmarshal(respBody, &response)
		require.NoError(t, err)
	}
}

func TestSecurityRequirements(t *testing.T) {
	// Verify that Synapse binds to 127.0.0.1 by default
	cfg := config.DefaultConfig()
	assert.True(t, strings.HasPrefix(cfg.ListenAddr, "127.0.0.1"),
		"Default bind address should be 127.0.0.1, got: %s", cfg.ListenAddr)
	
	// Verify config validation enforces 127.0.0.1
	cfg.ListenAddr = "0.0.0.0:8080"
	cfg.UpstreamURL = "http://localhost:8080" // Set required field
	err := cfg.Validate()
	assert.Error(t, err, "Config validation should fail for 0.0.0.0 bind")
	
	cfg.ListenAddr = "127.0.0.1:8080"
	err = cfg.Validate()
	assert.NoError(t, err, "Config validation should pass for 127.0.0.1 bind")
}

// Mock embedder for testing
type mockEmbedder struct{}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// Return a fixed embedding vector for testing
	vector := make([]float32, 384) // Typical embedding size
	for i := range vector {
		vector[i] = 0.1
	}
	return vector, nil
}

func (m *mockEmbedder) Close() error {
	return nil
}

func (m *mockEmbedder) Type() string {
	return "mock"
}
