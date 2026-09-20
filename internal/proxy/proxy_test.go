package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func (m *mockMemoryStore) Search(ctx context.Context, queryEmbedding []float32, agentID, teamID, sessionID string, topK int) ([]store.MemoryEntry, error) {
	return []store.MemoryEntry{}, nil
}

func (m *mockMemoryStore) Write(ctx context.Context, entry store.MemoryEntry) error {
	return nil
}

func (m *mockMemoryStore) MarkSuperseded(ctx context.Context, oldID, newID string) error {
	return nil
}

// mockEmbedder implements Embedder for testing
type mockEmbedder struct{}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// keywordEmbedder returns a distinguishable embedding based on whether the
// text mentions "database". Unlike mockEmbedder (a constant vector for
// every input), this lets a test construct memories that are genuinely
// semantically distinguishable -- necessary to prove *which* candidates
// retrieval actually surfaced, not just that the pipeline didn't crash.
type keywordEmbedder struct{}

func (k *keywordEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if strings.Contains(text, "database") {
		return []float32{1, 0, 0}, nil
	}
	return []float32{0, 1, 0}, nil
}

// fixedVectorEmbedder always returns the same vector regardless of input
// text. Used where a test needs full control over a memory's similarity to
// the query -- rather than deriving it from text content -- to isolate one
// scoring factor from another.
type fixedVectorEmbedder struct {
	vector []float32
}

func (f *fixedVectorEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return f.vector, nil
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


// TestProxyRetrievesOldRelevantMemoryBeyondRecencyWindow is a regression
// test for the retrieval fix in internal/retrieval: before that package
// existed, candidates came from GetRecent(ctx, sessionID, 20), which meant
// a memory older than the 20 most recent entries could never appear as a
// candidate no matter how relevant it was to the current query. This test
// writes 25 memories directly into a real store -- the oldest one
// semantically relevant to the upcoming question, the other 24 recent
// filler -- and confirms the old, relevant one is retrieved and scored.
func TestProxyRetrievesOldRelevantMemoryBeyondRecencyWindow(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))
	defer testServer.Close()

	realStore, err := store.NewStore(":memory:")
	require.NoError(t, err)
	defer realStore.Close()

	proxy, err := NewProxy(testServer.URL, realStore, &keywordEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)

	server := httptest.NewServer(r)
	defer server.Close()

	authHeader := "Bearer test-retrieval-regression-key"

	// Compute the same session ID the proxy will derive for this
	// Authorization header, so we can pre-populate the store under the
	// exact bucket the incoming request will read from.
	sessionReq := httptest.NewRequest("POST", "/v1/messages", nil)
	sessionReq.Header.Set("Authorization", authHeader)
	sessionID := deriveSessionID(sessionReq, "")

	ctx := context.Background()
	baseTime := time.Now().Add(-24 * time.Hour)

	// The one memory that matters: oldest of the 25, semantically relevant
	// to the upcoming query, embedding [1,0,0].
	err = realStore.Write(ctx, store.MemoryEntry{
		ID:         "old-relevant-decision",
		SessionID:  sessionID,
		Content:    "we decided to use PostgreSQL for the database",
		MemoryType: "decision",
		Timestamp:  baseTime,
		Embedding:  []float32{1, 0, 0},
	})
	require.NoError(t, err)

	// 24 more recent, irrelevant filler memories -- embedding [0,1,0],
	// orthogonal to the query -- so under the old GetRecent(20) behavior
	// these would fill the entire candidate window and push the relevant
	// one out entirely.
	for i := 0; i < 24; i++ {
		err = realStore.Write(ctx, store.MemoryEntry{
			ID:         fmt.Sprintf("filler-%d", i),
			SessionID:  sessionID,
			Content:    fmt.Sprintf("unrelated chat filler message number %d", i),
			MemoryType: "context",
			Timestamp:  baseTime.Add(time.Duration(i+1) * time.Minute),
			Embedding:  []float32{0, 1, 0},
		})
		require.NoError(t, err)
	}

	// Ask a question only the old memory can answer, using the same auth
	// header so it lands in the same session bucket.
	httpReq, err := http.NewRequest("POST", server.URL+"/v1/messages",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"what database are we using again?"}]}`))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", authHeader)
	httpReq.Header.Set("X-Synapse-Trace", "true")

	resp, err := http.DefaultClient.Do(httpReq)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	traceHeader := resp.Header.Get("X-Synapse-Trace-Result")
	require.NotEmpty(t, traceHeader)

	decodedTrace, err := base64.StdEncoding.DecodeString(traceHeader)
	require.NoError(t, err)

	var traceData trace.TraceManifest
	require.NoError(t, json.Unmarshal(decodedTrace, &traceData))

	// The core assertion: the 25th-oldest, relevant memory was retrieved
	// as a candidate at all. Under the old GetRecent(ctx, sessionID, 20)
	// behavior this memory would never have entered the candidate pool,
	// since it's older than the most recent 20 entries -- regardless of
	// how relevant it is to the query.
	found := false
	for _, m := range traceData.Memories {
		if m.ID == "old-relevant-decision" {
			found = true
			assert.Greater(t, m.ScoreSemantic, 0.9,
				"the relevant old memory should score high on semantic similarity")
		}
	}
	assert.True(t, found,
		"old relevant memory should be retrieved via semantic search even though it's older than the last 20 entries")
}


// TestProxyUsesConfiguredScoringWeights is a regression test for the
// hardcoded-weights bug: HandleMessages (and the /v1/compile equivalent in
// api.go) previously called scorer.GetWeights(0.4, 0.2, 0.2, 0.2) directly,
// ignoring config.Config's WeightSemanticSimilarity / WeightRecency /
// WeightImportance / WeightTaskAlignment fields entirely -- so nothing a
// user set in synapse.yaml (or even config.DefaultConfig()'s own
// 0.4/0.1/0.3/0.2) actually reached the scorer.
//
// This is proven by running an identical pair of candidates through two
// configs that each isolate a single factor (semantic-only vs
// importance-only, with recency and task alignment weighted to zero in
// both). The two candidates are constructed so semantic-only ranks one
// first and importance-only ranks the other first. That ranking flip is
// only possible if the configured weights are genuinely reaching the
// scorer -- a fixed hardcoded weight set would produce the same ranking
// regardless of which config was passed in.
func TestProxyUsesConfiguredScoringWeights(t *testing.T) {
	runWithWeights := func(t *testing.T, semantic, recency, importance, taskAlignment float64) map[string]float64 {
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("response"))
		}))
		defer testServer.Close()

		realStore, err := store.NewStore(":memory:")
		require.NoError(t, err)
		defer realStore.Close()

		cfg := config.DefaultConfig()
		cfg.WeightSemanticSimilarity = semantic
		cfg.WeightRecency = recency
		cfg.WeightImportance = importance
		cfg.WeightTaskAlignment = taskAlignment

		proxy, err := NewProxy(testServer.URL, realStore, &fixedVectorEmbedder{vector: []float32{1, 0, 0}}, cfg, session.NewManager(30*time.Minute), nil)
		require.NoError(t, err)
		defer proxy.Close()

		r := chi.NewRouter()
		proxy.RegisterRoutes(r)
		server := httptest.NewServer(r)
		defer server.Close()

		authHeader := "Bearer weights-regression-test-key"
		sessionReq := httptest.NewRequest("POST", "/v1/messages", nil)
		sessionReq.Header.Set("Authorization", authHeader)
		sessionID := deriveSessionID(sessionReq, "")

		ctx := context.Background()
		now := time.Now()

		// High semantic similarity to the query (embedding matches
		// exactly), low importance (context: 0.5).
		require.NoError(t, realStore.Write(ctx, store.MemoryEntry{
			ID:         "high-semantic-low-importance",
			SessionID:  sessionID,
			Content:    "filler content that happens to embed identically to the query",
			MemoryType: "context",
			Timestamp:  now,
			Embedding:  []float32{1, 0, 0},
		}))

		// Zero semantic similarity to the query (orthogonal embedding),
		// high importance (decision: 1.0).
		require.NoError(t, realStore.Write(ctx, store.MemoryEntry{
			ID:         "low-semantic-high-importance",
			SessionID:  sessionID,
			Content:    "an important decision that happens to embed orthogonally",
			MemoryType: "decision",
			Timestamp:  now,
			Embedding:  []float32{0, 1, 0},
		}))

		httpReq, err := http.NewRequest("POST", server.URL+"/v1/messages",
			bytes.NewBufferString(`{"messages":[{"role":"user","content":"generic query text"}]}`))
		require.NoError(t, err)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", authHeader)
		httpReq.Header.Set("X-Synapse-Trace", "true")

		resp, err := http.DefaultClient.Do(httpReq)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		traceHeader := resp.Header.Get("X-Synapse-Trace-Result")
		require.NotEmpty(t, traceHeader)
		decodedTrace, err := base64.StdEncoding.DecodeString(traceHeader)
		require.NoError(t, err)

		var traceData trace.TraceManifest
		require.NoError(t, json.Unmarshal(decodedTrace, &traceData))

		scores := make(map[string]float64)
		for _, m := range traceData.Memories {
			scores[m.ID] = m.ScoreTotal
		}
		return scores
	}

	semanticOnly := runWithWeights(t, 1.0, 0.0, 0.0, 0.0)
	require.Contains(t, semanticOnly, "high-semantic-low-importance")
	require.Contains(t, semanticOnly, "low-semantic-high-importance")
	assert.Greater(t, semanticOnly["high-semantic-low-importance"], semanticOnly["low-semantic-high-importance"],
		"with semantic-only weighting, the semantically-matching memory should score higher")

	importanceOnly := runWithWeights(t, 0.0, 0.0, 1.0, 0.0)
	require.Contains(t, importanceOnly, "high-semantic-low-importance")
	require.Contains(t, importanceOnly, "low-semantic-high-importance")
	assert.Greater(t, importanceOnly["low-semantic-high-importance"], importanceOnly["high-semantic-low-importance"],
		"with importance-only weighting, the higher-importance memory should score higher -- "+
			"a ranking flip from the semantic-only case above, which is only possible if config weights actually reach the scorer")
}


// postgresMongoEmbedder returns controlled embeddings for the supersession
// regression tests below: vectors at a known 45-degree angle (cosine
// similarity ~0.707), landing inside the default supersession similarity
// band (0.5-0.90) without being a near-duplicate dedup would also flag.
type postgresMongoEmbedder struct{}

func (e *postgresMongoEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "mongodb") {
		return []float32{0.70710678, 0.70710678}, nil
	}
	return []float32{1, 0}, nil
}

// TestProxyMarksMemoryAsSuperseded is an end-to-end regression test for
// the supersession detection wired into HandleMessages: a later message
// containing an explicit contradiction signal ("switched to", "instead
// of") about the same topic as an earlier decision-type memory should
// mark that earlier memory as superseded in the store, rather than
// leaving both sitting there as if equally current.
func TestProxyMarksMemoryAsSuperseded(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))
	defer testServer.Close()

	realStore, err := store.NewStore(":memory:")
	require.NoError(t, err)
	defer realStore.Close()

	cfg := config.DefaultConfig()
	// Defaults (0.5-0.90) already cover the ~0.707 similarity this test's
	// embedder produces, but set explicitly so this test doesn't silently
	// start failing if the defaults change later for unrelated reasons.
	cfg.SupersessionSimilarityMin = 0.5
	cfg.SupersessionSimilarityMax = 0.90

	proxy, err := NewProxy(testServer.URL, realStore, &postgresMongoEmbedder{}, cfg, session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)
	server := httptest.NewServer(r)
	defer server.Close()

	authHeader := "Bearer supersession-regression-test-key"
	sessionReq := httptest.NewRequest("POST", "/v1/messages", nil)
	sessionReq.Header.Set("Authorization", authHeader)
	sessionID := deriveSessionID(sessionReq, "")

	// First message: states the original decision.
	req1, err := http.NewRequest("POST", server.URL+"/v1/messages",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"we decided to use PostgreSQL for the database"}]}`))
	require.NoError(t, err)
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", authHeader)
	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	// Second message: contradicts it, same topic, same session.
	req2, err := http.NewRequest("POST", server.URL+"/v1/messages",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"we decided we switched to MongoDB instead of PostgreSQL"}]}`))
	require.NoError(t, err)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", authHeader)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	// Confirm directly against the store: the first memory should now be
	// marked superseded by the second.
	entries, err := realStore.GetRecent(context.Background(), sessionID, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	var oldEntry, newEntry store.MemoryEntry
	for _, e := range entries {
		if strings.Contains(e.Content, "we decided to use PostgreSQL") {
			oldEntry = e
		} else {
			newEntry = e
		}
	}
	require.NotEmpty(t, oldEntry.ID, "expected to find the original PostgreSQL decision in the store")
	require.NotEmpty(t, newEntry.ID, "expected to find the MongoDB decision in the store")

	assert.Equal(t, newEntry.ID, oldEntry.SupersededBy,
		"the original decision should be marked superseded by the contradicting one")
	assert.Equal(t, "", newEntry.SupersededBy,
		"the new (superseding) memory itself should not be marked as superseded")
}

// TestProxyDoesNotSupersedeWithoutContradictionSignal is a false-positive
// guard: two topically-similar decision memories in the same session, at
// a similarity within the supersession band, should NOT be marked
// superseded if neither message contains an explicit contradiction
// signal -- similarity alone is not enough (see FindSupersededCandidate's
// doc comment for why).
func TestProxyDoesNotSupersedeWithoutContradictionSignal(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))
	defer testServer.Close()

	realStore, err := store.NewStore(":memory:")
	require.NoError(t, err)
	defer realStore.Close()

	proxy, err := NewProxy(testServer.URL, realStore, &postgresMongoEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)
	server := httptest.NewServer(r)
	defer server.Close()

	authHeader := "Bearer supersession-negative-test-key"
	sessionReq := httptest.NewRequest("POST", "/v1/messages", nil)
	sessionReq.Header.Set("Authorization", authHeader)
	sessionID := deriveSessionID(sessionReq, "")

	req1, err := http.NewRequest("POST", server.URL+"/v1/messages",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"we decided to use PostgreSQL for the database"}]}`))
	require.NoError(t, err)
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", authHeader)
	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	// Contains "mongodb" (same ~0.707 similarity as the positive test
	// above) and "decided" (same memory type), but no contradiction phrase.
	req2, err := http.NewRequest("POST", server.URL+"/v1/messages",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"we also decided MongoDB works well for analytics"}]}`))
	require.NoError(t, err)
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", authHeader)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	entries, err := realStore.GetRecent(context.Background(), sessionID, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	for _, e := range entries {
		assert.Equal(t, "", e.SupersededBy,
			"neither memory should be marked superseded without an explicit contradiction signal")
	}
}


// TestProxyExcludesSupersededMemoryOnNextTurn is a regression test for the
// trace lineage and compiled-context enforcement work: once a memory has
// been marked superseded, the *next* turn's trace should show it excluded
// with reason "superseded" and the correct SupersededBy target, and its
// content must not appear in that turn's compiled context.
//
// This deliberately checks the *third* request, not the second. The
// memory gets marked superseded during the second request's own
// processing (see TestProxyMarksMemoryAsSuperseded), but that request's
// own candidates were fetched before the mark happened, so its own
// compiled output isn't a reliable place to check enforcement -- see the
// "Known limitation" comment on the 5b filter step in HandleMessages. The
// third request's candidates come from a fresh retrieval that reflects
// the now-persisted mark, which is where exclusion is actually guaranteed.
func TestProxyExcludesSupersededMemoryOnNextTurn(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response"))
	}))
	defer testServer.Close()

	realStore, err := store.NewStore(":memory:")
	require.NoError(t, err)
	defer realStore.Close()

	proxy, err := NewProxy(testServer.URL, realStore, &postgresMongoEmbedder{}, config.DefaultConfig(), session.NewManager(30*time.Minute), nil)
	require.NoError(t, err)
	defer proxy.Close()

	r := chi.NewRouter()
	proxy.RegisterRoutes(r)
	server := httptest.NewServer(r)
	defer server.Close()

	authHeader := "Bearer supersession-lineage-test-key"
	sessionReq := httptest.NewRequest("POST", "/v1/messages", nil)
	sessionReq.Header.Set("Authorization", authHeader)
	sessionID := deriveSessionID(sessionReq, "")

	send := func(content string, withTrace bool) *http.Response {
		req, err := http.NewRequest("POST", server.URL+"/v1/messages",
			bytes.NewBufferString(`{"messages":[{"role":"user","content":"`+content+`"}]}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", authHeader)
		if withTrace {
			req.Header.Set("X-Synapse-Trace", "true")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	resp1 := send("we decided to use PostgreSQL for the database", false)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)

	resp2 := send("we decided we switched to MongoDB instead of PostgreSQL", false)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	// Find the two memory IDs directly, same as TestProxyMarksMemoryAsSuperseded.
	entries, err := realStore.GetRecent(context.Background(), sessionID, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	var oldID, newID string
	for _, e := range entries {
		if strings.Contains(e.Content, "we decided to use PostgreSQL") {
			oldID = e.ID
		} else {
			newID = e.ID
		}
	}
	require.NotEmpty(t, oldID)
	require.NotEmpty(t, newID)

	// Third request: fresh retrieval should now see the persisted mark.
	resp3 := send("can you summarize what we have decided so far", true)
	defer resp3.Body.Close()
	require.Equal(t, http.StatusOK, resp3.StatusCode)

	traceHeader := resp3.Header.Get("X-Synapse-Trace-Result")
	require.NotEmpty(t, traceHeader)
	decodedTrace, err := base64.StdEncoding.DecodeString(traceHeader)
	require.NoError(t, err)

	var traceData trace.TraceManifest
	require.NoError(t, json.Unmarshal(decodedTrace, &traceData))

	found := false
	for _, m := range traceData.Memories {
		if m.ID == oldID {
			found = true
			assert.False(t, m.Included, "the superseded memory should not be included in the third turn's compiled context")
			assert.Equal(t, "superseded", m.ExclusionReason)
			assert.Equal(t, newID, m.SupersededBy)
		}
	}
	assert.True(t, found, "expected the superseded memory to still appear in the trace (excluded, not silently dropped from the candidate list)")
}