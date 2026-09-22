package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"synapse/internal/api"
	"synapse/internal/compiler"
	"synapse/internal/trace"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// compileToolName is the tool an MCP client calls to compile a conversation.
const compileToolName = "synapse_compile"

// compileToolDescription is the text tools/list advertises. The weights are
// spelled out because they are the answer to "what did this tool actually do
// with my conversation": a model reading only the schema has no other way to
// know the sieve is 0.4/0.3/0.2/0.1 over semantic similarity, importance, task
// alignment, and recency.
const compileToolDescription = "Compile conversation history into token-budgeted, task-aware context using the 4-Factor Sieve (Semantic 0.4, Importance 0.3, Task Alignment 0.2, Recency 0.1 with 24h half-life decay)."

// Compiler is the compile pipeline this server calls.
//
// It is an interface so that the tool's own tests can inject a pipeline stub,
// and so that this package depends on an operation rather than on the REST
// server that happens to provide it: *api.APIServer satisfies this as it
// stands, which is what makes synapse_compile and POST /v1/compile the same
// compilation instead of two implementations that agree today.
type Compiler interface {
	// CompileContext runs the full classify -> retrieve -> score -> dedup ->
	// budget -> compile pipeline for one session and returns the compiled
	// messages with the trace that explains them. A tokenBudgetOverride of 0
	// means the configured default.
	CompileContext(ctx context.Context, sessionID string, messages []api.Message, tokenBudgetOverride int) (*compiler.CompileResult, error)
}

// compileArgs is the tool's input, decoded from the MCP arguments object.
//
// Role and content are strings and nothing else: an editor sends whatever the
// conversation contained, and a message that is not shaped like an
// OpenAI-compatible message is a caller mistake the pipeline must not be
// handed (see parseCompileArgs).
type compileArgs struct {
	Messages    []compileMessage `json:"messages"`
	SessionID   string           `json:"session_id"`
	TokenBudget int              `json:"token_budget"`
}

// compileMessage is one entry of the messages array.
type compileMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// registerCompileTool registers synapse_compile and its handler.
func (s *Server) registerCompileTool() {
	s.mcp.AddTool(
		mcpgo.NewTool(compileToolName,
			mcpgo.WithDescription(compileToolDescription),
			mcpgo.WithArray("messages",
				mcpgo.Required(),
				mcpgo.Description("OpenAI-compatible messages array: [{\"role\":\"user\",\"content\":\"...\"}, ...]."),
				mcpgo.Items(map[string]any{
					"type": "object",
					"properties": map[string]any{
						"role":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required": []string{"role", "content"},
				}),
			),
			mcpgo.WithString("session_id",
				mcpgo.Required(),
				mcpgo.Description("Session the conversation belongs to. Memories are retrieved from it and the last user message is written back to it."),
			),
			mcpgo.WithInteger("token_budget",
				mcpgo.Description("Optional token ceiling for the compiled memory context. Omit or send 0 to use this node's configured budget."),
			),
		),
		s.handleCompile,
	)
}

// handleCompile answers a synapse_compile call.
//
// It validates, then delegates: every step of the compilation itself belongs to
// the shared pipeline, and this function owns only what is specific to being
// reached over MCP -- argument shapes, the error envelope a model can read, and
// the response fields that make the result legible to a caller that never sees
// a trace manifest.
func (s *Server) handleCompile(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args, err := parseCompileArgs(req)
	if err != nil {
		return toolError(errorTypeInvalidParams, err.Error()), nil
	}
	if s.compiler == nil {
		// A server built without a pipeline is this deployment's mistake, not
		// the caller's: the failure type says the compile did not happen, and
		// the message says why, so a caller does not go looking for a bad
		// argument that does not exist.
		return toolError(errorTypeCompileFailed, "compile pipeline is not configured on this server"), nil
	}

	messages := make([]api.Message, len(args.Messages))
	for i, msg := range args.Messages {
		messages[i] = api.Message{Role: msg.Role, Content: msg.Content}
	}

	result, err := s.compiler.CompileContext(ctx, args.SessionID, messages, args.TokenBudget)
	if err != nil {
		// Same treatment the REST handlers give a pipeline failure: the cause
		// goes to the log (stderr, never the JSON-RPC stream) and the caller
		// gets a typed failure without an internal error string in it. An
		// error from here can name a database path or an upstream host, and
		// neither belongs in a model's context.
		slog.Error("MCP compile failed", "error", err)
		return toolError(errorTypeCompileFailed, "the compile pipeline failed; see the synapse MCP server log for the cause"), nil
	}

	payload, err := mcpgo.NewToolResultJSON(newCompileToolResult(result))
	if err != nil {
		return nil, fmt.Errorf("mcp: compile result is not marshallable: %w", err)
	}
	return payload, nil
}

// parseCompileArgs decodes and validates the tool's arguments.
//
// The two rules the brief names (non-empty messages, a session_id) are applied
// through internal/api's own validators rather than reimplemented here, and the
// same goes for the per-message shape check: an editor's compile is rejected by
// the rules an HTTP compile is rejected by, which is the only way the two paths
// can be trusted to accept the same conversations.
func parseCompileArgs(req mcpgo.CallToolRequest) (compileArgs, error) {
	var args compileArgs
	if err := req.BindArguments(&args); err != nil {
		return args, fmt.Errorf("arguments must be an object with messages, session_id, and an optional token_budget: %w", err)
	}

	if len(args.Messages) == 0 {
		return args, errors.New("messages is required and must not be empty")
	}
	if err := api.ValidateSessionID(args.SessionID); err != nil {
		return args, err
	}
	if args.TokenBudget < 0 {
		return args, errors.New("token_budget must not be negative")
	}
	for i, msg := range args.Messages {
		if err := api.ValidateMessageContent(msg.Content); err != nil {
			return args, fmt.Errorf("messages[%d]: %w", i, err)
		}
	}

	return args, nil
}

// Tool error types, carried in the error result's JSON payload.
//
// They exist because a handler cannot choose a JSON-RPC error code: mcp-go
// returns mcp.INTERNAL_ERROR (-32603) for any error a handler returns, so
// "invalid params" is expressed the way the protocol does allow from inside a
// tool -- a CallToolResult with IsError set, whose text names the type. A model
// reading that text can tell a bad call apart from a broken server, which is
// the difference between retrying with better arguments and giving up.
const (
	errorTypeInvalidParams = "invalid_params"
	errorTypeCompileFailed = "compile_failed"
)

// errorEnvelope is the JSON body of a tool error.
type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

// errorDetail is the machine-readable half of a tool error, alongside a message
// meant to be read by a caller.
type errorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// toolError builds an error result of the given type.
//
// The Marshal failure path is unreachable for the strings this package passes
// (a Go string that is not valid UTF-8 would be needed); the fallback exists so
// that theoretical case cannot turn into a nil result at the call site.
func toolError(errorType, message string) *mcpgo.CallToolResult {
	payload, err := mcpgo.NewToolResultJSON(errorEnvelope{Error: errorDetail{Type: errorType, Message: message}})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("%s: %s", errorType, message))
	}
	payload.IsError = true
	return payload
}

// compileToolResult is the payload synapse_compile returns.
//
// The first four fields are the compile's own numbers. trace_id and memories
// are the .clinerules requirement that an MCP response be able to say WHY a
// memory was surfaced: the trace id ties this answer to the same trace the HTTP
// API and the audit ledger record, and each memory carries the four factor
// scores that put it in (or left it out of) the compiled context. Content
// previews are deliberately not repeated here: the memories that were included
// are already in compiled_messages, and the ones that were not are not this
// caller's business.
type compileToolResult struct {
	CompiledMessages []map[string]interface{} `json:"compiled_messages"`
	TokensUsed       int                      `json:"tokens_used"`
	ReductionPct     float64                  `json:"reduction_pct"`
	DetectedIntent   string                   `json:"detected_intent"`
	TraceID          string                   `json:"trace_id"`
	Memories         []compileMemoryScore     `json:"memories"`
}

// compileMemoryScore is one scored memory as the tool reports it.
type compileMemoryScore struct {
	ID                 string  `json:"id"`
	MemoryType         string  `json:"memory_type"`
	ScoreSemantic      float64 `json:"score_semantic"`
	ScoreRecency       float64 `json:"score_recency"`
	ScoreImportance    float64 `json:"score_importance"`
	ScoreTaskAlignment float64 `json:"score_task_alignment"`
	ScoreTotal         float64 `json:"score_total"`
	Included           bool    `json:"included"`
}

// newCompileToolResult flattens a compile result into the tool's payload.
//
// A nil trace is tolerated rather than dereferenced: the pipeline always
// attaches one, but this is the boundary where a future refactor would
// otherwise turn a missing trace into a panic inside a tool call.
func newCompileToolResult(result *compiler.CompileResult) compileToolResult {
	out := compileToolResult{
		CompiledMessages: result.Messages,
		Memories:         []compileMemoryScore{},
	}
	if out.CompiledMessages == nil {
		out.CompiledMessages = []map[string]interface{}{}
	}

	if manifest := result.Trace; manifest != nil {
		out.TokensUsed = manifest.TokensUsed
		out.ReductionPct = manifest.ReductionPct
		out.DetectedIntent = manifest.DetectedIntent
		out.TraceID = manifest.RequestID
		out.Memories = memoryScores(manifest.Memories)
	}

	return out
}

// memoryScores copies the four factor scores out of a trace manifest.
func memoryScores(memories []trace.TraceMemory) []compileMemoryScore {
	scores := make([]compileMemoryScore, len(memories))
	for i, memory := range memories {
		scores[i] = compileMemoryScore{
			ID:                 memory.ID,
			MemoryType:         memory.MemoryType,
			ScoreSemantic:      memory.ScoreSemantic,
			ScoreRecency:       memory.ScoreRecency,
			ScoreImportance:    memory.ScoreImportance,
			ScoreTaskAlignment: memory.ScoreTaskAlignment,
			ScoreTotal:         memory.ScoreTotal,
			Included:           memory.Included,
		}
	}
	return scores
}
