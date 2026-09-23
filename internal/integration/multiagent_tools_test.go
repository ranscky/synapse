//go:build integration

// What this phase's test calls on an edge node: the three verbs a client uses --
// store a memory, compile a session, proxy a turn -- plus the two helpers that
// read the node's own state and its trace.
//
// Split from multiagent_edge_test.go, which owns what a node *is*: the constructors
// and the wiring. This file owns what a node *answers*, and everything here goes
// through the node's real surfaces -- the MCP transport for the tools, HTTP for the
// compile and the proxied turn -- so a break in either is a test failure rather
// than a call that bypassed the thing under test.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"synapse/internal/api"
	"synapse/internal/trace"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// multiagentWriteResult is the payload synapse_write_memory answers with, in the
// shape internal/mcp documents for it.
type multiagentWriteResult struct {
	ID               string `json:"id"`
	ConflictDetected bool   `json:"conflict_detected"`
	ConflictWithID   string `json:"conflict_with_id"`
	Sanitized        bool   `json:"sanitized"`
}

// writeMemory stores one memory through this node's synapse_write_memory tool, the
// way an editor would: over the MCP transport, with the type and scope a caller
// chooses.
func (e *multiagentEdge) writeMemory(t *testing.T, content, memoryType, visibility, sessionID string) multiagentWriteResult {
	t.Helper()

	var request mcpgo.CallToolRequest
	request.Params.Name = multiagentWriteTool
	request.Params.Arguments = map[string]any{
		"content":     content,
		"memory_type": memoryType,
		"visibility":  visibility,
		"session_id":  sessionID,
	}

	result, err := e.client.CallTool(context.Background(), request)
	require.NoError(t, err)
	require.False(t, result.IsError, "synapse_write_memory refused the write: %s", multiagentToolText(t, result))

	var decoded multiagentWriteResult
	require.NoError(t, json.Unmarshal([]byte(multiagentToolText(t, result)), &decoded))

	return decoded
}

// multiagentToolText returns the text payload of an MCP tool result.
func multiagentToolText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()

	require.Len(t, result.Content, 1, "a tool result carries exactly one content block")
	text, ok := mcpgo.AsTextContent(result.Content[0])
	require.True(t, ok, "the tool result must carry text content")

	return text.Text
}

// compile sends one conversation to this node's POST /v1/compile and returns the
// decoded response. The trace it carries -- the whole manifest, memories included
// -- is the Memory Trace every trace assertion in this phase reads; the MCP compile
// tool answers with the same scores but not the provenance fields.
func (e *multiagentEdge) compile(t *testing.T, sessionID, lastUserMessage string) api.CompileResponse {
	t.Helper()

	requestBody, err := json.Marshal(api.CompileRequest{
		Messages: []api.Message{
			{Role: "system", Content: "You are a careful pair programmer."},
			{Role: "user", Content: lastUserMessage},
		},
		SessionID: sessionID,
	})
	require.NoError(t, err)

	resp, err := http.Post(e.baseURL+"/v1/compile", "application/json", bytes.NewReader(requestBody))
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "POST /v1/compile answered %d: %s", resp.StatusCode, string(body))

	var decoded api.CompileResponse
	require.NoError(t, json.Unmarshal(body, &decoded), "body: %s", string(body))
	require.NotNil(t, decoded.Trace, "a compile always answers with its trace")

	return decoded
}

// chat sends one OpenAI-shaped turn through this node's proxy to the model
// upstream. It is the path live traffic takes, which is where an edge's memories
// come from when nobody calls a memory tool: the last user turn and the assistant
// reply are both captured as memories.
//
// No Authorization header is sent, deliberately -- see the header assertion in the
// test. The proxy forwards the client's own credential when there is one, and this
// scenario is about what an edge does with the credential it holds.
func (e *multiagentEdge) chat(t *testing.T, userMessage string) map[string]any {
	t.Helper()

	requestBody, err := json.Marshal(map[string]any{
		"model": "multiagent-test-model",
		"messages": []map[string]string{
			{"role": "user", "content": userMessage},
		},
		"stream": false,
	})
	require.NoError(t, err)

	resp, err := http.Post(e.baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(requestBody))
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "POST /v1/chat/completions answered %d: %s", resp.StatusCode, string(body))

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded), "body: %s", string(body))

	return decoded
}

// pendingSync reports how many of this node's memories are queued for the plane:
// the flusher's own view, read straight out of the local database.
func (e *multiagentEdge) pendingSync(t *testing.T) int {
	t.Helper()

	count, err := e.local.CountPendingSync(context.Background())
	require.NoError(t, err)

	return count
}

// multiagentTraceMemory returns the first trace entry whose content preview
// contains needle, and reports whether there was one.
//
// The preview rather than the whole content because the trace carries previews:
// trace.TraceMemory caps them, deliberately, and this test is asserting which
// memory was surfaced rather than what its full text is.
func multiagentTraceMemory(manifest *trace.TraceManifest, needle string) (trace.TraceMemory, bool) {
	for _, memory := range manifest.Memories {
		if strings.Contains(memory.ContentPreview, needle) {
			return memory, true
		}
	}

	return trace.TraceMemory{}, false
}
