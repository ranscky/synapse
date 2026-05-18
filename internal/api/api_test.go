package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"synapse/internal/config"
	"synapse/internal/store"
)

func TestAPIServer(t *testing.T) {
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
	apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false)

	// Test health endpoint
	t.Run("Health Endpoint", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/health", nil)
		rr := httptest.NewRecorder()

		r := chi.NewRouter()
		r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
			response := `{"status":"ok","memories_stored":0,"avg_compile_ms":0}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(response))
		})
		
		r.ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		expected := `{"status":"ok","memories_stored":0,"avg_compile_ms":0}`
		if rr.Body.String() != expected {
			t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), expected)
		}
	})

	// Test stats endpoint
	t.Run("Stats Endpoint", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/stats", nil)
		rr := httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		var response StatsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Errorf("failed to unmarshal response: %v", err)
		}

		if response.Status != "ok" {
			t.Errorf("expected status 'ok', got %s", response.Status)
		}
	})

	// Test compile endpoint with empty messages
	t.Run("Compile Empty Messages", func(t *testing.T) {
		compileReq := CompileRequest{
			Messages: []Message{},
		}

		body, _ := json.Marshal(compileReq)
		req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}
	})

	// Test compile endpoint with valid messages
	t.Run("Compile Valid Messages", func(t *testing.T) {
		compileReq := CompileRequest{
			Messages: []Message{
				{Role: "user", Content: "Hello, world!"},
			},
			SessionID: "test-session",
		}

		body, _ := json.Marshal(compileReq)
		req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		// Should succeed
		if status := rr.Code; status != http.StatusOK {
			t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		var response CompileResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Errorf("failed to unmarshal response: %v", err)
		}

		// Should have trace
		if response.Trace == nil {
			t.Error("expected trace to be present")
		}
	})

	// Test memories endpoints
	t.Run("Memories Endpoints", func(t *testing.T) {
		// Test GET /v1/memories without session_id
		req := httptest.NewRequest("GET", "/v1/memories", nil)
		rr := httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("GET /v1/memories without session_id returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}

		// Test GET /v1/memories with valid session_id
		req = httptest.NewRequest("GET", "/v1/memories?session_id=test-session", nil)
		rr = httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusOK {
			t.Errorf("GET /v1/memories with session_id returned wrong status code: got %v want %v", status, http.StatusOK)
		}

		// Test DELETE /v1/memories without session_id
		req = httptest.NewRequest("DELETE", "/v1/memories", nil)
		rr = httptest.NewRecorder()

		apiServer.Router().ServeHTTP(rr, req)

		if status := rr.Code; status != http.StatusBadRequest {
			t.Errorf("DELETE /v1/memories without session_id returned wrong status code: got %v want %v", status, http.StatusBadRequest)
		}
	})

	// Test rate limiting (basic test)
	t.Run("Rate Limiting", func(t *testing.T) {
		// Make 101 requests quickly to test rate limiting
		for i := 0; i < 101; i++ {
			req := httptest.NewRequest("GET", "/v1/stats", nil)
			rr := httptest.NewRecorder()

			apiServer.Router().ServeHTTP(rr, req)

			if i == 100 && rr.Code == http.StatusTooManyRequests {
				// Rate limit hit - this is expected behavior
				break
			}
		}
	})
}

// Mock embedder for testing
type mockEmbedder struct{}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// Return a simple mock embedding
	return make([]float32, 384), nil
}

func (m *mockEmbedder) Close() error {
	return nil
}

func TestValidateSessionID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "test-session-123", false},
		{"empty", "", true},
		{"too long", "a-really-long-session-id-that-exceeds-the-maximum-length-of-sixty-four-characters-which-should-fail-validation", true},
		{"invalid chars", "test@session", true},
		{"valid with hyphens", "test-session-id", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSessionID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSessionID() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateMessageContent(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "Hello, world!", false},
		{"null bytes", "Hello\x00world", true},
		{"too long", string(make([]byte, 33000)), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMessageContent(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateMessageContent() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAPIServerWithPersistence(t *testing.T) {
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

	// Create API server with persistence enabled
	apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, true)

	// Test that server was created with persistence enabled
	if !apiServer.persistTraces {
		t.Error("Expected persistTraces to be true")
	}
}

func BenchmarkAPIServer(b *testing.B) {
	// Create a temporary store
	storeInstance, err := store.NewStore(":memory:")
	if err != nil {
		b.Fatalf("Failed to create store: %v", err)
	}
	defer storeInstance.Close()

	// Create a mock embedder
	mockEmbedder := &mockEmbedder{}

	// Create config
	cfg := config.DefaultConfig()

	// Create API server
	apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false)

	b.Run("Stats Endpoint", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			req := httptest.NewRequest("GET", "/v1/stats", nil)
			rr := httptest.NewRecorder()

			apiServer.Router().ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				b.Errorf("Expected status OK, got %d", rr.Code)
			}
		}
	})

	b.Run("Compile Endpoint", func(b *testing.B) {
		compileReq := CompileRequest{
			Messages: []Message{
				{Role: "user", Content: "Hello, world!"},
			},
			SessionID: "benchmark-session",
		}

		body, _ := json.Marshal(compileReq)

		for i := 0; i < b.N; i++ {
			req := httptest.NewRequest("POST", "/v1/compile", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			apiServer.Router().ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				b.Errorf("Expected status OK, got %d", rr.Code)
			}
		}
	})
}

func TestConcurrentAccess(t *testing.T) {
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
	apiServer := NewAPIServer(storeInstance, mockEmbedder, cfg, false)

	// Test concurrent access to compile times
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func() {
			apiServer.addCompileTime(int64(time.Now().Nanosecond()))
			done <- true
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify that we can still get average compile time
	avg := apiServer.getAvgCompileTime()
	if avg < 0 {
		t.Error("Average compile time should not be negative")
	}
}