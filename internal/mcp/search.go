package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"synapse/internal/api"
	"synapse/internal/classifier"
	"synapse/internal/retrieval"
	"synapse/internal/scorer"
	"synapse/internal/store"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const (
	// searchToolName is the tool an MCP client calls to search the Global
	// Brain. It is plural in the name because a search answers with a ranked
	// set, which may legitimately be empty.
	searchToolName = "synapse_search_memories"

	// searchToolDescription is the text tools/list advertises. It says what
	// this tool answers that a plain memory read does not: not just which
	// memories matched but the four factors that ranked them.
	searchToolDescription = "Search the Global Brain for memories relevant to a query. Returns memories ranked by 4-Factor score with full breakdown - shows WHY each memory was surfaced, not just that it was."

	// defaultSearchTopK is the result count a caller that does not name top_k
	// gets.
	defaultSearchTopK = 10

	// maxSearchTopK caps top_k, at the same ceiling plane's
	// GET /v2/memories/search enforces (internal/plane/search.go): past it a
	// caller is not searching, it is draining the store.
	maxSearchTopK = 500
)

// Embedder is the query-embedding step this tool performs.
//
// It is an interface for the same two reasons Compiler is one: the tool's tests
// need to score without an ONNX session, and the method set is exactly what
// retrieval.Candidates asks a retrieval.Embedder for, so the concrete embedder
// cmd/synapse already holds -- the one the proxy and the compile pipeline share
// -- satisfies it as it stands. A search is therefore embedded by the same model
// that indexed the memories it is compared against; two embedders would make
// cosine similarity meaningless.
type Embedder interface {
	// Embed returns the vector for text.
	Embed(ctx context.Context, text string) ([]float32, error)
}

// searchArgs is the tool's input, decoded from the MCP arguments object.
type searchArgs struct {
	Query      string `json:"query"`
	TopK       int    `json:"top_k"`
	Visibility string `json:"visibility"`
	SessionID  string `json:"session_id"`
}

// registerSearchTool registers synapse_search_memories and its handler.
func (s *Server) registerSearchTool() {
	s.mcp.AddTool(
		mcpgo.NewTool(searchToolName,
			mcpgo.WithDescription(searchToolDescription),
			// A search reads. mcp-go's defaults describe every tool as
			// destructive and not read-only, which is right for synapse_compile
			// (it writes the last user message back) and wrong here -- and a
			// client that gates on these hints would be told the read path is a
			// write path.
			mcpgo.WithReadOnlyHintAnnotation(true),
			mcpgo.WithDestructiveHintAnnotation(false),
			mcpgo.WithOpenWorldHintAnnotation(false),
			mcpgo.WithString("query",
				mcpgo.Required(),
				mcpgo.Description("What to search for. Embedded with this node's own model and compared against stored memories."),
			),
			mcpgo.WithInteger("top_k",
				mcpgo.Description("Maximum number of memories to return, ranked by 4-Factor score. Omit or send 0 for 10."),
			),
			mcpgo.WithString("visibility",
				mcpgo.Description("The visibility scope this search reads at: \"org\" (default) reads org-shared memories only, \"team\" adds this node's team, \"private\" adds this agent's own memories from session_id."),
			),
			mcpgo.WithString("session_id",
				mcpgo.Description("Session to narrow the search to. A standalone node's memories are session-scoped, so naming one is how they are reached there; omit it to search the whole brain, which only a shared/tenant store can do."),
			),
		),
		s.handleSearchMemories,
	)
}

// handleSearchMemories answers a synapse_search_memories call.
//
// The chain is the compile pipeline's retrieve-then-score half, with its write,
// dedup, and budget stages left out: this tool reports what the brain holds and
// why it ranks where it does, and it must never mutate what it was asked to
// read. The scorer is built exactly the way internal/api builds it -- the same
// weights from the same config keys, the same classifier-derived intent and
// confidence -- so a score_s/score_r/score_i/score_t here means what it means in
// a synapse_compile response instead of being a second, subtly different
// ranking.
func (s *Server) handleSearchMemories(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args, err := parseSearchArgs(req)
	if err != nil {
		return toolError(errorTypeInvalidParams, err.Error()), nil
	}
	if s.store == nil || s.embedder == nil {
		// Not the caller's mistake: this deployment was built without the two
		// things a search needs. Reported as a typed failure for the same
		// reason a missing compile pipeline is -- the process must answer
		// instead of dying inside a tool call.
		return toolError(errorTypeSearchFailed, "memory search is not configured on this server"), nil
	}

	// The candidate pool is at least as wide as the answer. The store hands
	// back its nearest poolK by similarity and the 4-Factor total decides which
	// of those are returned -- asking for exactly top_k would let similarity
	// order pre-empt the scoring this tool exists to explain.
	poolK := s.cfg.RetrievalCandidateK
	if poolK < args.TopK {
		poolK = args.TopK
	}

	scope := readerScope(args.Visibility, args.SessionID, s.cfg.AgentID, s.cfg.TeamID)

	retrievalResult, err := retrieval.Candidates(ctx, s.store, s.embedder, s.plane, scope, args.Query, poolK)
	if err != nil {
		// Cause to the log (stderr, never the JSON-RPC stream), typed failure to
		// the caller: a store error can name a database path, and that does not
		// belong in a model's context.
		slog.Error("MCP memory search failed", "error", err)
		return toolError(errorTypeSearchFailed, "the memory search failed; see the synapse MCP server log for the cause"), nil
	}

	// The query is classified the way a conversation's last user turn is, so
	// Task Alignment is computed from this search's own text.
	classifyResult := classifier.Classify(args.Query)
	weights := scorer.GetWeights(
		s.cfg.WeightSemanticSimilarity,
		s.cfg.WeightRecency,
		s.cfg.WeightImportance,
		s.cfg.WeightTaskAlignment,
	)
	scorerInstance := scorer.NewScorer(weights, classifyResult.Intent, float64(classifyResult.Confidence), time.Now())

	// An empty query embedding is scored rather than skipped: retrieval has
	// already fallen back to recency ordering in that case, and the resulting
	// breakdown (score_s at zero, the other three real) says so honestly instead
	// of leaving a caller to guess why every similarity is zero.
	scored := scorerInstance.Score(ctx, retrievalResult.QueryEmbedding, retrievalResult.Candidates)

	traceID := newSearchTraceID()
	memories := searchMemoryScores(scored, args.TopK, s.cfg.AgentID)

	payload, err := mcpgo.NewToolResultJSON(searchToolResult{TraceID: traceID, Memories: memories})
	if err != nil {
		return nil, fmt.Errorf("mcp: search result is not marshallable: %w", err)
	}

	// Counts and identifiers only. A search query is memory content the moment
	// it is embedded, and this process never logs memory content -- nor a
	// caller's question scored against it.
	slog.Info("MCP memory search",
		"trace_id", traceID,
		"visibility", args.Visibility,
		"top_k", args.TopK,
		"candidates", len(retrievalResult.Candidates),
		"memories", len(memories),
	)

	return payload, nil
}

// parseSearchArgs decodes and validates the tool's arguments.
//
// The rules the REST front end applies to caller text are applied here too
// (api.ValidateMessageContent for the query, api.ValidateSessionID for a named
// session) rather than reimplemented, because .clinerules requires the MCP
// surface to share the REST sanitization pipeline: a query this tool embeds must
// be a query POST /v1/compile would also have accepted.
func parseSearchArgs(req mcpgo.CallToolRequest) (searchArgs, error) {
	var args searchArgs
	if err := req.BindArguments(&args); err != nil {
		return args, fmt.Errorf("arguments must be an object with a query and optional top_k, visibility, and session_id: %w", err)
	}

	if strings.TrimSpace(args.Query) == "" {
		return args, errors.New("query is required and must not be empty")
	}
	if err := api.ValidateMessageContent(args.Query); err != nil {
		return args, fmt.Errorf("query: %w", err)
	}

	if args.TopK < 0 {
		return args, errors.New("top_k must not be negative")
	}
	if args.TopK > maxSearchTopK {
		return args, fmt.Errorf("top_k must not exceed %d", maxSearchTopK)
	}
	if args.TopK == 0 {
		args.TopK = defaultSearchTopK
	}

	if args.Visibility == "" {
		args.Visibility = store.VisibilityOrg
	}
	switch args.Visibility {
	case store.VisibilityOrg, store.VisibilityTeam, store.VisibilityPrivate:
	default:
		return args, fmt.Errorf("visibility must be %q, %q, or %q",
			store.VisibilityOrg, store.VisibilityTeam, store.VisibilityPrivate)
	}

	if args.SessionID != "" {
		if err := api.ValidateSessionID(args.SessionID); err != nil {
			return args, err
		}
	}

	return args, nil
}

// readerScope turns the requested visibility into the reader scope retrieval is
// run under.
//
// visibility decides how much identity the reader presents, and the caller's
// session travels alongside it because a session can only ever narrow a read:
//
//	org     -> {SessionID}: an org-only reader -- no agent and no team are named.
//	           On Postgres the predicate reduces to visibility = 'org': the
//	           session is bound, but it sits in the private branch an unnamed
//	           agent cannot match, so org-shared memories come back regardless of
//	           session, which is what a shared plane means. On the standalone
//	           backend, whose Search is scoped to one session and has no
//	           visibility column at all, the session is the only filter there is
//	           -- naming one is how a local node is searched, not a claim about
//	           visibility.
//	team    -> {TeamID: this node's team, SessionID}: org-shared memories plus
//	           this team's. With no team configured the predicate's team branch
//	           matches no row, which is the fail-closed direction.
//	private -> {AgentID, TeamID, SessionID}: adds this agent's own private
//	           memories from that session. With no session named the private
//	           branch matches nothing, because private memory is session-scoped
//	           by definition.
//
// The agent and team ids always come from this node's own configuration, the
// session from the tool's session_id argument, and nothing from anything else in
// the request: an identity a caller can type is an identity a caller can lie
// about. Each scope is cumulative because the backend's authorization predicate
// is (internal/store/pgvisibility.go): an org-scoped memory is readable by every
// agent in the tenant, so no narrower claim can exclude it -- and no value here
// can widen a read past what the node's own identity already allows.
func readerScope(visibility, sessionID, agentID, teamID string) retrieval.Scope {
	switch visibility {
	case store.VisibilityTeam:
		return retrieval.Scope{TeamID: teamID, SessionID: sessionID}
	case store.VisibilityPrivate:
		return retrieval.Scope{AgentID: agentID, TeamID: teamID, SessionID: sessionID}
	default:
		return retrieval.Scope{SessionID: sessionID}
	}
}

// newSearchTraceID returns the identifier this call's response carries.
//
// .clinerules requires an MCP response to be able to say WHY a memory was
// surfaced, alongside the score breakdown; the trace id is the half that ties
// one answer to one log line, since a search records no trace manifest (traces
// belong to compilations). It is random rather than a timestamp so two searches
// in the same nanosecond cannot share an id, and the fallback exists only so a
// failing entropy source cannot fail a search.
func newSearchTraceID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("search-%d", time.Now().UnixNano())
	}
	return "search-" + hex.EncodeToString(buf[:])
}
