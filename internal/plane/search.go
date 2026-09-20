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
// data layer: read one tenant's memories nearest a query embedding.
//
// Like MemoryWriter it is declared on the consumer side, so this package
// depends on a behaviour rather than on a database handle and the endpoint is
// testable without PostgreSQL. The implementation lives in internal/tenant (the
// schema-per-tenant owner) and is bound to the verified token's slug -- never to
// anything the request body could name.
type MemorySearcher interface {
	// Search returns the topK memories nearest queryEmbedding in tenantSlug's
	// own schema, scoped to sessionID when it is non-empty and spanning the
	// tenant when it is empty.
	Search(ctx context.Context, tenantSlug string, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error)
}

// searchRequest is the GET /v2/memories/search body, in wire order. It is the
// mirror image of sync.searchRequest on the edge side; the two are the protocol.
type searchRequest struct {
	QueryEmbedding []float32 `json:"query_embedding"`
	SessionID      string    `json:"session_id"`
	AgentID        string    `json:"agent_id"`
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
// sync endpoint. The body's agent_id is attribution metadata -- it names which
// node is asking and is never an isolation key: the schema searched is the one
// the signed tenant_slug names.
//
// Nothing about a memory's content, the query embedding, or the token is ever
// logged. The one log line names the slug, the agent, and how many memories came
// back.
func (s *Server) handleSearchMemories(w http.ResponseWriter, r *http.Request) {
	slug := TenantSlugFromCtx(r.Context())
	if slug == "" {
		// Unreachable behind requireJWT, which refuses a token that names no
		// tenant. Checked anyway so a rewired middleware cannot turn this into
		// a search against an unnamed schema.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

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

	memories, err := s.searcher.Search(r.Context(), slug, req.QueryEmbedding, req.SessionID, req.TopK)
	if err != nil {
		if s.logger != nil {
			// Counts and identifiers only: no content, no embedding, no token.
			// A pgx error can quote the connection target, so it is reported
			// server-side and never returned to the client.
			s.logger.Error("Memory search failed",
				"tenant_slug", slug, "agent_id", req.AgentID, "top_k", req.TopK, "error", err)
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
			"tenant_slug", slug, "agent_id", req.AgentID, "memories", len(memories))
	}

	writeJSON(w, http.StatusOK, searchResponse{Memories: memories})
}
