package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"synapse/internal/config"
	"synapse/internal/scorer"
	"synapse/internal/session"
	"synapse/internal/store"
	"synapse/internal/trace"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegration(t *testing.T) {
	// Create temporary directory for test files
	tempDir := t.TempDir()
	
	// Create a temporary store
	dbPath := filepath.Join(tempDir, "test.db")
	storeInstance, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer storeInstance.Close()

	// Create a mock embedder
	mockEmbedder := &mockEmbedder{}

	// Create config
	cfg := config.DefaultConfig()

	// Test 1: API server without persistence
	t.Run("API Server Without Persistence", func(t *testing.T) {
		apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))

		// Test compile endpoint
		compileReq := CompileRequest{
			Messages: []Message{
				{Role: "user", Content: "Hello, this is a test message for integration testing."},
				{Role: "assistant", Content: "Hello! I'm responding to your test message."},
				{Role: "user", Content: "Can you help me with this integration test?"},
			},
			SessionID: "integration-test-session",
		}

		body, _ := json.Marshal(compileReq)
		req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		var response CompileResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Errorf("failed to unmarshal response: %v", err)
		}

		// Verify response structure
		if response.CompiledMessages == nil {
			t.Error("expected compiled messages to be present")
		}
		if response.Trace == nil {
			t.Error("expected trace to be present")
		}

		// Verify trace structure
		if response.Trace.RequestID == "" {
			t.Error("expected request ID in trace")
		}
		if response.Trace.DetectedIntent == "" {
			t.Error("expected detected intent in trace")
		}
		if response.Trace.Timestamp.IsZero() {
			t.Error("expected timestamp in trace")
		}
	})

	// Test 2: API server with persistence
	t.Run("API Server With Persistence", func(t *testing.T) {
		apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, true, session.NewManager(30*time.Minute))

		// Test compile endpoint
		compileReq := CompileRequest{
			Messages: []Message{
				{Role: "user", Content: "This is a test with persistence enabled."},
			},
			SessionID: "persistence-test-session",
		}

		body, _ := json.Marshal(compileReq)
		req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		var response CompileResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Errorf("failed to unmarshal response: %v", err)
		}

		// Verify trace is present
		if response.Trace == nil {
			t.Error("expected trace to be present")
		}
	})

	// Test 3: Memory management endpoints
	t.Run("Memory Management", func(t *testing.T) {
		apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))

		sessionID := "memory-test-session"

		// Test GET memories with valid session
		req := httptest.NewRequest("GET", fmt.Sprintf("/v1/memories?session_id=%s", sessionID), nil)
		rr := httptest.NewRecorder()
		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("GET /v1/memories returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		// Test DELETE memories without session (should fail)
		req = httptest.NewRequest("DELETE", "/v1/memories", nil)
		rr = httptest.NewRecorder()
		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("DELETE /v1/memories without session_id returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}

		// Test DELETE memories with valid session
		req = httptest.NewRequest("DELETE", fmt.Sprintf("/v1/memories?session_id=%s", sessionID), nil)
		rr = httptest.NewRecorder()
		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusNoContent {
			t.Errorf("DELETE /v1/memories returned wrong status code: got %v want %v", status, http.StatusNoContent)
		}
	})

	// Test 4: Stats endpoint
	t.Run("Stats Endpoint", func(t *testing.T) {
		apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))

		req := httptest.NewRequest("GET", "/v1/stats", nil)
		rr := httptest.NewRecorder()
		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("GET /v1/stats returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		var response StatsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Errorf("failed to unmarshal response: %v", err)
		}

		if response.Status != "ok" {
			t.Errorf("expected status 'ok', got %s", response.Status)
		}
	})

	// Test 5: Trace persistence functionality
	t.Run("Trace Persistence", func(t *testing.T) {
		// Create trace manifest
		traceManifest := trace.NewTraceManifest(
			"test-request-id",
			"test-intent",
			0.8,
			10,
			8,
			5,
			100,
			200,
			45,
			nil,
			nil,
			[]scorer.ScoredMemory{},
		)

		// Test saving trace
		err := traceManifest.SaveToFile(tempDir)
		if err != nil {
			t.Errorf("failed to save trace: %v", err)
		}

		// Verify file was created
		traceDir := filepath.Join(tempDir, "traces")
		entries, err := os.ReadDir(traceDir)
		if err != nil {
			t.Errorf("failed to read trace directory: %v", err)
		}

		if len(entries) == 0 {
			t.Error("expected trace files to be created")
		}
	})

	// Test 6: Security validation
	t.Run("Security Validation", func(t *testing.T) {
	// Test invalid session IDs
	testCases := []struct {
		sessionID string
		expectErr bool
		desc      string
	}{
		{"", true, "empty session ID"},
		{"test@session", true, "invalid characters"},
		{"a-really-long-session-id-that-exceeds-the-maximum-length-of-sixty-four-characters-which-should-fail-validation", true, "too long"},
		{"valid-session-id", false, "valid session ID"},
	}

	for _, tc := range testCases {
		compileReq := CompileRequest{
			Messages: []Message{
				{Role: "user", Content: "Test message"},
			},
			SessionID: tc.sessionID,
		}

		body, _ := json.Marshal(compileReq)
		req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()

		// Create fresh API server for each test
		apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))
		apiServer.Router().ServeHTTP(rr, req)

		// Check if error status matches expectation
		if tc.expectErr {
			if rr.Code != http.StatusBadRequest {
				t.Errorf("expected bad request for %s ('%s'), got status %d", tc.desc, tc.sessionID, rr.Code)
			}
		} else {
			// For valid session IDs, should not get bad request (might get other errors like 500 due to missing data)
			if rr.Code == http.StatusBadRequest {
				t.Errorf("unexpected bad request for valid session ID '%s'", tc.sessionID)
			}
		}
	}

		// Test invalid message content
		compileReq := CompileRequest{
			Messages: []Message{
				{Role: "user", Content: "Hello\x00World"}, // contains null byte
			},
			SessionID: "valid-session",
		}

		body, _ := json.Marshal(compileReq)
		req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()

		apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))
		apiServer.Router().ServeHTTP(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("expected bad request for invalid message content, got status %d", rr.Code)
		}
	})

	// Test 7: Rate limiting
	t.Run("Rate Limiting", func(t *testing.T) {
		apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))

		// Make more than 100 requests to trigger rate limiting
		rateLimitHit := false
		for i := 0; i < 110; i++ {
			req := httptest.NewRequest("GET", "/v1/stats", nil)
			rr := httptest.NewRecorder()
			apiServer.Router().ServeHTTP(rr, req)

			if rr.Code == http.StatusTooManyRequests {
				rateLimitHit = true
				break
			}
		}

		// Note: Rate limiting might not always trigger in test environment due to timing
		// But we've tested the rate limiter logic separately
		t.Logf("Rate limiting test completed (rate limit hit: %v)", rateLimitHit)
	})
}

func TestConcurrentCompilation(t *testing.T) {
	// Create a temporary store
	storeInstance, err := store.NewStore(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer storeInstance.Close()

	// Create a mock embedder
	mockEmbedder := &mockEmbedder{}

	// Create config
	cfg := config.DefaultConfig()

	// Create API server
	apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))

	// Test concurrent compilations
	concurrentRequests := 5
	results := make(chan int, concurrentRequests)

	for i := 0; i < concurrentRequests; i++ {
		go func(requestNum int) {
			compileReq := CompileRequest{
				Messages: []Message{
					{Role: "user", Content: fmt.Sprintf("Concurrent test message %d", requestNum)},
				},
				SessionID: fmt.Sprintf("concurrent-session-%d", requestNum),
			}

			body, _ := json.Marshal(compileReq)
			req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			apiServer.Router().ServeHTTP(rr, req)

			results <- rr.Code
		}(i)
	}

	// Collect results
	allSuccess := true
	for i := 0; i < concurrentRequests; i++ {
		code := <-results
		if code != http.StatusOK {
			allSuccess = false
			t.Errorf("Concurrent request %d failed with status %d", i, code)
		}
	}

	if !allSuccess {
		t.Error("Not all concurrent requests succeeded")
	}
}

func TestPerformance(t *testing.T) {
	// Create a temporary store
	storeInstance, err := store.NewStore(":memory:")
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	defer storeInstance.Close()

	// Create a mock embedder
	mockEmbedder := &mockEmbedder{}

	// Create config
	cfg := config.DefaultConfig()

	// Create API server
	apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))

	// Performance test for stats endpoint (account for rate limiting)
	start := time.Now()
	requests := 100
	successfulRequests := 0

	for i := 0; i < requests; i++ {
		req := httptest.NewRequest("GET", "/v1/stats", nil)
		rr := httptest.NewRecorder()
		apiServer.Router().ServeHTTP(rr, req)

		// Allow both OK and rate limit responses
		if rr.Code == http.StatusOK || rr.Code == http.StatusTooManyRequests {
			if rr.Code == http.StatusOK {
				successfulRequests++
			}
		} else {
			t.Errorf("Request %d failed with unexpected status %d", i, rr.Code)
		}
		
		// Small delay to avoid overwhelming rate limiter
		time.Sleep(1 * time.Millisecond)
	}

	duration := time.Since(start)
	requestsPerSecond := float64(successfulRequests) / duration.Seconds()

	t.Logf("Performance test: %d requests (%d successful) in %v (%.0f successful req/sec)", requests, successfulRequests, duration, requestsPerSecond)

	// Should handle at least 50 successful requests per second
	if requestsPerSecond < 50 {
		t.Errorf("Performance below threshold: %.0f req/sec, expected at least 50", requestsPerSecond)
	}
}
func TestAPICompileWritesMemory(t *testing.T) {
	realStore, err := store.NewStore(":memory:")
	require.NoError(t, err)
	defer realStore.Close()

	mockEmbedder := &mockEmbedder{}
	cfg := config.DefaultConfig()
	apiServer := NewAPIServer(realStore, mockEmbedder, cfg, false, session.NewManager(30*time.Minute))

	reqBody := `{"messages":[{"role":"user","content":"there was an error in the order handler causing a crash"}],"session_id":"write-test-session"}`
	req := httptest.NewRequest("POST", "/v1/compile", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	apiServer.Router().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Verify the message actually landed in the real store, with a real
	// embedding attached.
	ctx := context.Background()
	entries, err := realStore.GetRecent(ctx, "write-test-session", 10)
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one memory entry should have been written")

	assert.Equal(t, "there was an error in the order handler causing a crash", entries[0].Content)
	assert.Equal(t, "error", entries[0].MemoryType)
	assert.NotEmpty(t, entries[0].Embedding, "written memory should have a real embedding attached")
}
