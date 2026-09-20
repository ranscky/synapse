// The tenant-scoped candidate search endpoint: GET /v2/memories/search.
//
// Split out of handlers.go for the same reason the sync endpoint lives in
// sync.go: handlers.go holds the server, the routes, and the provisioning
// endpoint, and it is already close to the 300-line ceiling this project holds
// every file to. The sync endpoint is the write half of the edge protocol; this
// is the read half, and the two mirror each other deliberately -- same tenant
// source, same body limits, same refusal to leak a database error.
package plane

import (
	"context"
	"net/http"

	"synapse/internal/store"
)

// searchRoute is the candidate search endpoint's path, referenced by both the
// route registration and the handler's own documentation.
const searchRoute = "/v2/memories/search"

const (
	// maxSearchBodyBytes bounds a search request body. The documented body is
	// one 384-float query embedding plus three short strings -- a few kilobytes
	// -- so this ceiling is generous by two orders of magnitude. It exists so a
	// client cannot make the plane allocate by declaring an enormous embedding,
	// the same way maxSyncBodyBytes bounds a push.
	maxSearchBodyBytes = 256 << 10

	// maxSearchTopK caps top_k. An edge asks for one session's candidate pool,
	// which is bounded by its own retrieval-candidate-k; anything beyond this
	// is a client bug, and answering it would let one caller pull an entire
	// tenant schema in a single request.
	maxSearchTopK = 500
)

// MemorySearcher is everything GET /v2/memories/search needs from the tenant
// data layer: read one tenant's memories nearest a query embedding, narrowed to
// what the caller is allowed to see.
//
// Like MemoryWriter it is declared on the consumer side, so this package
// depends on a behaviour rather than on a database handle and the endpoint is
// testable without PostgreSQL. The implementation lives in internal/tenant (the
// schema-per-tenant owner) and is bound to the verified token's slug -- never to
// anything the request body could name.
type MemorySearcher interface {
	// Search returns the topK memories nearest queryEmbedding in tenantSlug's
	// own schema that the scope agentID/teamID/sessionID may read: org-scoped
	// memories from any agent, team-scoped ones from agentID's own team, and
	// agentID's private ones from sessionID. An empty agentID is an org-only
	// reader, which is the fail-closed default for a token that names no agent.
	//
	// tenantSlug, agentID, and teamID all come from the verified token; only
	// the embedding, the session, and topK come from the request.
	Search(ctx context.Context, tenantSlug string, queryEmbedding []float32, agentID, teamID, sessionID string, topK int) ([]store.MemoryEntry, error)
}

// searchRequest is the GET /v2/memories/search body, in wire order. It is the
// mirror image of sync.searchRequest on the edge side; the two are the protocol.
//
// agent_id and team_id are attribution: they name the node that asked, and the
// plane logs them. Neither is ever an isolation input -- what this request may
// read comes from the verified token's own claims (see handleSearchMemories),
// because a body is exactly the part of a request an attacker writes.
type searchRequest struct {
	QueryEmbedding []float32 `json:"query_embedding"`
	SessionID      string    `json:"session_id"`
	AgentID        string    `json:"agent_id"`
	TeamID         string    `json:"team_id"`
	TopK           int       `json:"top_k"`
}

// searchResponse is the 200 body: the matched memories, each serialized as the
// full store.MemoryEntry the store returned. agent_id and embedding are part of
// that answer on purpose -- the edge scores candidates against the query
// embedding and attributes them to the agent that pushed them.
type searchResponse struct {
	Memories []store.MemoryEntry `json:"memories"`
}

// handleSearchMemories serves GET /v2/memories/search: validate, search, answer.
//
// The tenant comes from the verified token and nowhere else, exactly as on the
// sync endpoint -- and so does the caller's visibility scope. agentID and teamID
// are read from the token's own claims (AgentIDFromCtx, TeamIDFromCtx) and are
// what the store's visibility predicate is evaluated against; the body's
// agent_id and team_id are attribution only, logged alongside, and never passed
// to the data layer. A body is the one part of a request a caller controls
// completely, so treating it as an isolation input would mean any node could
// read any other node's private memories by typing its name.
//
// A token that names no agent is an org-only reader: it reaches org-scoped
// memories and nothing narrower. That is the fail-closed default, and it is what
// keeps every tenant token issued before agent-scoped tokens existed working
// exactly as it did.
//
// Nothing about a memory's content, the query embedding, or the token is ever
// logged. The one log line names the slug, the verified agent (when the token
// has one), the agent the body claimed, and how many memories came back.
func (s *Server) handleSearchMemories(w http.ResponseWriter, r *http.Request) {
	slug := TenantSlugFromCtx(r.Context())
	if slug == "" {
		// Unreachable behind requireJWT, which refuses a token that names no
		// tenant. Checked anyway so a rewired middleware cannot turn this into
		// a search against an unnamed schema.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// The verified scope. An empty agentID is not an error: it is the org-only
	// reader a tenant token without an agent claim is.
	agentID := AgentIDFromCtx(r.Context())
	teamID := TeamIDFromCtx(r.Context())

	var req searchRequest
	if !decodeJSONLimit(w, r, &req, maxSearchBodyBytes) {
		return
	}

	// The agent names itself for the same reason it does on a push: a plane
	// that cannot attribute a request cannot meter or audit it. Required, so an
	// unattributed query is a contract violation rather than a normal path.
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "invalid_agent")
		return
	}

	// A token that names an agent is that agent's credential, so a request
	// claiming to be someone else is a misconfiguration worth refusing loudly.
	// A token with no agent claim is not agent-scoped, so its body may name
	// whatever node is presenting it -- several agents can legitimately share
	// one tenant token, and each of them is then an org-only reader.
	if agentID != "" && req.AgentID != agentID {
		writeError(w, http.StatusBadRequest, "invalid_agent")
		return
	}

	if req.TopK > maxSearchTopK {
		writeError(w, http.StatusBadRequest, "invalid_top_k")
		return
	}

	// A wrong-width embedding is reported as a client error here rather than
	// left to the store, which would answer 500 for something the caller can
	// fix. An empty embedding is legitimate: the store falls back to recency
	// ordering for it, which is what the edge sends when it has nothing to
	// embed.
	if len(req.QueryEmbedding) != 0 && len(req.QueryEmbedding) != store.EmbeddingDimensions {
		writeError(w, http.StatusBadRequest, "invalid_embedding")
		return
	}

	if s.searcher == nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	// The scope is the token's, not the body's: agentID and teamID above.
	memories, err := s.searcher.Search(r.Context(), slug, req.QueryEmbedding, agentID, teamID, req.SessionID, req.TopK)
	if err != nil {
		if s.logger != nil {
			// Counts and identifiers only: no content, no embedding, no token.
			// A pgx error can quote the connection target, so it is reported
			// server-side and never returned to the client.
			s.logger.Error("Memory search failed",
				"tenant_slug", slug, "agent_id", agentID, "request_agent_id", req.AgentID,
				"top_k", req.TopK, "error", err)
		}
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	if memories == nil {
		// A nil slice marshals as null, and the published contract is always an
		// array: "no memories yet" must not break a client that iterates.
		memories = []store.MemoryEntry{}
	}

	if s.logger != nil {
		s.logger.Info("Memories searched",
			"tenant_slug", slug, "agent_id", agentID, "request_agent_id", req.AgentID,
			"memories", len(memories))
	}

	writeJSON(w, http.StatusOK, searchResponse{Memories: memories})
}
