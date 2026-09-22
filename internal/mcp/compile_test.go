package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"synapse/internal/api"
	"synapse/internal/compiler"
	"synapse/internal/config"
	"synapse/internal/session"
	"synapse/internal/store"
	"synapse/internal/trace"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// validIntents is the classifier's whole vocabulary. The tool reports what the
// shared pipeline detected, so a value outside this set means the response and
// the classifier have drifted apart.
var validIntents = []string{"debug", "plan", "code", "write", "generic"}

// stubCompiler stands in for the shared pipeline.
//
// It records what it was handed, which is what lets the tests below assert that
// the MCP layer passed a session id, messages, and a budget through unchanged
// and did not call the pipeline at all for input it should have rejected.
type stubCompiler struct {
	result  *compiler.CompileResult
	err     error
	handled []compileCall
}

// compileCall is one recorded invocation of the stub.
type compileCall struct {
	sessionID   string
	messages    []api.Message
	tokenBudget int
}

// newStubCompiler returns a stub that answers with an empty-but-valid compile
// result, so a test that never inspects the payload still gets a JSON body.
func newStubCompiler() *stubCompiler {
	return &stubCompiler{result: &compiler.CompileResult{
		Messages: []map[string]interface{}{},
		Trace:    &trace.TraceManifest{DetectedIntent: "generic"},
	}}
}

// CompileContext implements Compiler.
func (s *stubCompiler) CompileContext(_ context.Context, sessionID string, messages []api.Message, tokenBudget int) (*compiler.CompileResult, error) {
	s.handled = append(s.handled, compileCall{sessionID: sessionID, messages: messages, tokenBudget: tokenBudget})
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// compileCallRequest builds a CallToolRequest the way an MCP client would.
func compileCallRequest(args any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Name = compileToolName
	req.Params.Arguments = args
	return req
}

// basisEmbedder returns a unit vector along one axis, chosen by the first byte
// of the text.
//
// A real embedder's vectors are not needed to exercise the pipeline's algebra:
// what the test needs is finite, non-degenerate vectors, so that cosine
// similarity is a number rather than a NaN (which json.Marshal would reject)
// and so that two different memories are not near-duplicates of each other.
func basisEmbedder(dim int) apiEmbedder { return apiEmbedder{dim: dim} }

// apiEmbedder is an embedder.Embedder that returns a basis vector.
type apiEmbedder struct{ dim int }

// Embed implements embedder.Embedder.
func (e apiEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, e.dim)
	idx := 0
	if text != "" {
		idx = int(text[0]) % e.dim
	}
	vec[idx] = 1
	return vec, nil
}

// TestCompileToolIsRegisteredAndDescribesTheSieve drives the real protocol
// through mcp-go's in-process client: everything except the transport is the
// production path, so a tool that is registered but not callable (or vice
// versa) fails here rather than in an editor.
func TestCompileToolIsRegisteredAndDescribesTheSieve(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	c, err := client.NewInProcessClient(srv.mcp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(ctx))

	_, err = c.Initialize(ctx, initializeRequest())
	require.NoError(t, err)

	tools, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	require.NoError(t, err)

	var found *mcpgo.Tool
	for i := range tools.Tools {
		if tools.Tools[i].Name == compileToolName {
			found = &tools.Tools[i]
			break
		}
	}
	require.NotNil(t, found, "tools/list must advertise synapse_compile")

	require.Equal(t, compileToolDescription, found.Description)
	require.Contains(t, found.InputSchema.Required, "messages")
	require.Contains(t, found.InputSchema.Required, "session_id")
	require.Contains(t, found.InputSchema.Properties, "token_budget")
	require.Contains(t, found.InputSchema.Properties, "messages")

	// Listed and callable are two different maps inside mcp-go; only a call
	// proves both are populated.
	res, err := c.CallTool(ctx, compileCallRequest(map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
		"session_id": "sess-registered",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "synapse_compile must not report an error")
}

// TestCompileToolCompilesSessionThroughSharedPipeline is the DoD test: a real
// store, a real APIServer (the same object POST /v1/compile is served by), a
// five-message conversation, and a token budget that actually bites. Nothing
// about the compilation is stubbed except the embedder, which cannot be real
// without an ONNX session in a unit test.
func TestCompileToolCompilesSessionThroughSharedPipeline(t *testing.T) {
	const (
		sessionID   = "sess-phase23"
		budget      = 12
		lastMessage = "there is a stack trace in the order handler, it crashes on an empty payload"
	)
	ctx := context.Background()

	storeInstance, err := store.NewStore(filepath.Join(t.TempDir(), "mcp-compile.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = storeInstance.Close() })

	cfg := config.DefaultConfig()
	pipeline := api.NewAPIServer(storeInstance, basisEmbedder(384), cfg, false, session.NewManager(30*time.Minute))

	srv := NewServer(storeInstance, *cfg, pipeline)

	// Memories of this session, written through the store -- so they went
	// through the same sanitization pass the REST write path applies. The first
	// one is given the query's own embedding, so the semantic factor is
	// exercised rather than sitting at zero for every candidate.
	queryEmbedding, err := basisEmbedder(384).Embed(ctx, lastMessage)
	require.NoError(t, err)
	seedMemories(t, storeInstance, sessionID, queryEmbedding)

	c, err := client.NewInProcessClient(srv.mcp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, initializeRequest())
	require.NoError(t, err)

	messages := fiveMessageSession(lastMessage)

	res, err := c.CallTool(ctx, compileCallRequest(map[string]any{
		"messages":     messages,
		"session_id":   sessionID,
		"token_budget": budget,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "a valid compile must not be reported as an error")

	text := toolText(t, res)
	var got compileToolResult
	require.NoError(t, json.Unmarshal([]byte(text), &got))
	t.Logf("synapse_compile response: %s", text)

	require.NotEmpty(t, got.CompiledMessages, "the tool must return compiled_messages")
	require.Greater(t, got.TokensUsed, 0, "the seeded memories must have been scored and fitted into the budget")
	require.LessOrEqual(t, got.TokensUsed, budget, "tokens_used must respect the caller's token_budget")
	require.Greater(t, got.ReductionPct, 0.0, "a budget this small must have trimmed the candidate pool")
	require.LessOrEqual(t, got.ReductionPct, 100.0)
	require.Contains(t, validIntents, got.DetectedIntent)
	require.NotEmpty(t, got.TraceID, "every compiled answer must be tied to its trace")
	require.NotEmpty(t, got.Memories, "the seeded session memories must be reported with their scores")

	// The memory that was seeded with the query's own embedding has to come
	// back with a full semantic score -- evidence that the 4-factor scores in
	// this response came from the pipeline's scoring pass and are not
	// decoration.
	var semanticLead float64
	for _, memory := range got.Memories {
		if memory.ID == "mem-0" {
			semanticLead = memory.ScoreSemantic
		}
	}
	require.InDelta(t, 1.0, semanticLead, 1e-6, "mem-0 shares the query embedding and must score semantic 1.0")

	// The four factor scores and the trace id have to be on the wire, not just
	// in the Go struct: this is the .clinerules requirement that a caller can
	// see WHY a memory was surfaced.
	var raw struct {
		Memories []map[string]any `json:"memories"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &raw))
	for _, memory := range raw.Memories {
		for _, key := range []string{"score_semantic", "score_recency", "score_importance", "score_task_alignment", "score_total"} {
			require.Contains(t, memory, key)
		}
		// Memories that lost the budget check are reported by score only; their
		// content is not echoed back to a caller that did not receive it.
		require.NotContains(t, memory, "content_preview")
	}

	// The question the caller asked is still what the context ends with.
	require.Contains(t, compiledText(got.CompiledMessages), lastMessage)

	// persist=true parity with /v1/compile: the last user message is now a
	// memory of this session, written by the shared pipeline rather than by
	// anything in this package.
	entries, err := storeInstance.GetRecent(ctx, sessionID, 20)
	require.NoError(t, err)
	require.True(t, containsContent(entries, lastMessage), "the compile must have written the last user message, as POST /v1/compile does")

	// Nothing a header or a key would carry may appear in tool output.
	for _, banned := range []string{"Authorization", "Bearer", "api-key", "jwt"} {
		require.NotContains(t, text, banned)
	}

	// With no token_budget, the configured default is the ceiling.
	resDefault, err := c.CallTool(ctx, compileCallRequest(map[string]any{
		"messages":   messages,
		"session_id": sessionID,
	}))
	require.NoError(t, err)
	require.False(t, resDefault.IsError)

	var gotDefault compileToolResult
	require.NoError(t, json.Unmarshal([]byte(toolText(t, resDefault)), &gotDefault))
	require.LessOrEqual(t, gotDefault.TokensUsed, cfg.TokenBudget, "tokens_used must respect the configured budget")
	require.Contains(t, validIntents, gotDefault.DetectedIntent)
}

// TestCompilePassesArgumentsThroughUnchanged pins the seam: what the tool
// received is what the pipeline was handed, message roles and contents
// included, with no reshaping in between.
func TestCompilePassesArgumentsThroughUnchanged(t *testing.T) {
	stub := newStubCompiler()
	srv := NewServer(nil, *config.DefaultConfig(), stub)

	res, err := srv.handleCompile(context.Background(), compileCallRequest(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "assistant", "content": "second"},
			map[string]any{"role": "user", "content": "third"},
		},
		"session_id":   "sess-passthrough",
		"token_budget": 123,
	}))
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, stub.handled, 1)

	handled := stub.handled[0]
	require.Equal(t, "sess-passthrough", handled.sessionID)
	require.Equal(t, 123, handled.tokenBudget)
	require.Equal(t, []api.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "second"},
		{Role: "user", Content: "third"},
	}, handled.messages)
}

// TestCompileRejectsInvalidParams covers the input the tool must refuse before
// the pipeline ever sees it. The stub records calls, so "rejected" is asserted
// rather than assumed: a rejected call is one the pipeline never received.
func TestCompileRejectsInvalidParams(t *testing.T) {
	valid := []any{map[string]any{"role": "user", "content": "hello"}}

	tests := []struct {
		name string
		args any
	}{
		{"missing session_id", map[string]any{"messages": valid}},
		{"empty session_id", map[string]any{"messages": valid, "session_id": ""}},
		{"illegal session_id", map[string]any{"messages": valid, "session_id": "not a session id"}},
		{"missing messages", map[string]any{"session_id": "sess-invalid"}},
		{"empty messages", map[string]any{"messages": []any{}, "session_id": "sess-invalid"}},
		{"message is not an object", map[string]any{"messages": []any{"hello"}, "session_id": "sess-invalid"}},
		{"null byte in content", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "a\x00b"}}, "session_id": "sess-invalid"}},
		{"negative token_budget", map[string]any{"messages": valid, "session_id": "sess-invalid", "token_budget": -1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newStubCompiler()
			srv := NewServer(nil, *config.DefaultConfig(), stub)

			res, err := srv.handleCompile(context.Background(), compileCallRequest(tt.args))
			require.NoError(t, err, "a caller mistake is a typed tool error, not a transport failure")
			require.True(t, res.IsError, "invalid input must be reported as an error result")

			text := toolText(t, res)
			require.Contains(t, text, errorTypeInvalidParams)

			var payload errorEnvelope
			require.NoError(t, json.Unmarshal([]byte(text), &payload))
			require.Equal(t, errorTypeInvalidParams, payload.Error.Type)
			require.NotEmpty(t, payload.Error.Message)

			require.Empty(t, stub.handled, "the pipeline must not be reached with invalid input")
		})
	}
}

// TestCompileReportsPipelineFailureAsToolError covers the other failure kind: a
// pipeline that ran and failed. The cause is logged, not returned -- an error
// from the pipeline can name a database path or an upstream host, and neither
// belongs in a model's context.
func TestCompileReportsPipelineFailureAsToolError(t *testing.T) {
	stub := newStubCompiler()
	stub.err = errors.New("failed to search memories: /var/lib/synapse/secret.db is locked")
	srv := NewServer(nil, *config.DefaultConfig(), stub)

	res, err := srv.handleCompile(context.Background(), compileCallRequest(map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
		"session_id": "sess-failure",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)

	text := toolText(t, res)
	require.Contains(t, text, errorTypeCompileFailed)
	require.NotContains(t, text, "secret.db", "the pipeline's internal cause must not reach the caller")
}

// TestCompileWithoutPipelineFailsSafelyNotPanics covers a server built with no
// compile path at all: it has to answer, because the alternative is a process
// that starts and then dies inside a tool call.
func TestCompileWithoutPipelineFailsSafelyNotPanics(t *testing.T) {
	srv := NewServer(nil, *config.DefaultConfig(), nil)

	res, err := srv.handleCompile(context.Background(), compileCallRequest(map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
		"session_id": "sess-no-pipeline",
	}))
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, toolText(t, res), errorTypeCompileFailed)
}

// toolText returns the text content of a tool result, failing if there is none.
func toolText(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()

	require.Len(t, res.Content, 1, "tool results carry exactly one content block")
	text, ok := mcpgo.AsTextContent(res.Content[0])
	require.True(t, ok, "the result must carry text content")
	return text.Text
}

// fiveMessageSession is a five-message OpenAI-compatible conversation whose
// last user turn asks about the failure the seeded memories describe.
func fiveMessageSession(lastMessage string) []any {
	return []any{
		map[string]any{"role": "system", "content": "You are a careful pair programmer."},
		map[string]any{"role": "user", "content": "we are looking at the order service today"},
		map[string]any{"role": "assistant", "content": "understood, what should I look at first?"},
		map[string]any{"role": "user", "content": "start with the handler and the retry logic"},
		map[string]any{"role": "user", "content": lastMessage},
	}
}

// seedMemories writes a handful of memories into a session. The first one is
// stored with the caller's query embedding; the rest get distinct basis vectors
// so they are nobody's near-duplicate.
func seedMemories(t *testing.T, st *store.Store, sessionID string, queryEmbedding []float32) {
	t.Helper()

	ctx := context.Background()
	now := time.Now()
	contents := []string{
		"the order handler panics when the payload is empty",
		"we decided the retry budget is three attempts",
		"the staging database is rebuilt every night",
	}
	embeddings := [][]float32{queryEmbedding, basisVector(384, 1), basisVector(384, 2)}

	for i, content := range contents {
		entry := store.MemoryEntry{
			ID:         fmt.Sprintf("mem-%d", i),
			SessionID:  sessionID,
			Content:    content,
			MemoryType: "context",
			Timestamp:  now.Add(-time.Duration(i) * time.Hour),
			Importance: 0.5,
			Embedding:  embeddings[i],
		}
		require.NoError(t, st.Write(ctx, entry))
	}
}

// basisVector returns a unit vector along axis idx, so that vectors written
// here and vectors returned by the embedder are never near-duplicates of each
// other and never degenerate.
func basisVector(dim, idx int) []float32 {
	vec := make([]float32, dim)
	vec[idx%dim] = 1
	return vec
}

// compiledText concatenates the content of every compiled message.
func compiledText(messages []map[string]interface{}) string {
	var b strings.Builder
	for _, message := range messages {
		if content, ok := message["content"].(string); ok {
			b.WriteString(content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// containsContent reports whether any entry holds exactly content.
func containsContent(entries []store.MemoryEntry, content string) bool {
	for _, entry := range entries {
		if entry.Content == content {
			return true
		}
	}
	return false
}
