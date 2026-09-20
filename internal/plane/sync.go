// The edge-to-plane sync endpoint: POST /v2/sync/memories.
//
// Split out of handlers.go rather than appended to it, because handlers.go is
// already close to the 300-line ceiling this project holds every file to and
// this endpoint is a whole subsystem's worth of validation and documentation.
package plane

import (
	"context"
	"net/http"

	"synapse/internal/store"
)

// syncRoute is the sync endpoint's path, referenced by both the route
// registration and the handler's own documentation.
const syncRoute = "/v2/sync/memories"

// maxSyncBodyBytes bounds a sync push body. One memory carries a 384-float
// embedding, so a batch of the default 20 is tens of kilobytes of JSON; the
// ceiling is deliberately generous (a megabyte) because refusing a legitimate
// batch would leave the edge retrying it forever, and the count of memories
// inside that ceiling is bounded by arithmetic rather than by a second limit an
// operator could trip over with a large sync-batch-size.
const maxSyncBodyBytes = 1 << 20

// MemoryWriter is everything POST /v2/sync/memories needs from the tenant data
// layer: store one tenant's edge-pushed memories and report what happened.
//
// Like TenantProvisioner, it is declared on the consumer side so the HTTP layer
// depends on a behaviour rather than on a database handle, and so this endpoint
// is testable without PostgreSQL. The implementation lives in internal/tenant
// (the schema-per-tenant owner) and reuses store's sanitization pipeline and
// PostgreSQL store rather than a second insert path.
type MemoryWriter interface {
	// WriteBatch stores every entry in tenantSlug's own schema and reports how
	// many were written and how many had their content rewritten by the
	// sanitizer. It returns an error when the batch could not be stored, and a
	// partial success is idempotent: the same entry pushed again is a no-op.
	WriteBatch(ctx context.Context, tenantSlug string, entries []store.MemoryEntry) (written int, sanitized int, err error)
}

// syncRequest is the POST /v2/sync/memories body, in wire order. It is the
// mirror image of sync.pushRequest on the edge side; the two are the protocol.
type syncRequest struct {
	SessionID string              `json:"session_id"`
	AgentID   string              `json:"agent_id"`
	Memories  []store.MemoryEntry `json:"memories"`
}

// syncResponse is the 200 body: how many memories were stored and how many of
// them had content the sanitizer rewrote. Both counts are of memories, never
// bytes or tokens, so an edge operator can compare them against a batch size.
type syncResponse struct {
	Written   int `json:"written"`
	Sanitized int `json:"sanitized"`
}

// requireJWT wraps next with the plane's tenant token verification, injected at
// construction (see Server.auth).
//
// An unconfigured middleware fails closed: every request is answered with the
// one 401 body and nothing is stored. That keeps a plane that was started
// without wiring a token verifier as unusable rather than open -- the same
// fail-closed shape requireAdmin uses for an unset admin token.
func (s *Server) requireJWT(next http.Handler) http.Handler {
	if s.auth == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
		})
	}

	return s.auth(next)
}

// handleSyncMemories serves POST /v2/sync/memories: validate, sanitize, store.
//
// The tenant comes from the verified token and nowhere else -- the request body
// has no field that could name a tenant -- so a push can only ever write into
// the schema its own credential owns (schema-per-tenant, not a row filter).
//
// agent_id is required: a plane that cannot attribute a memory to the agent that
// produced it should not silently accept one as "default", and the edge refuses
// to boot without an agent id, so this is a contract check rather than a normal
// path.
//
// Successful means every memory in the batch was stored. A batch is
// all-or-nothing from the edge's point of view: the edge marks exactly the batch
// it sent as synced on a 2xx, so a 200 that quietly skipped a memory would turn
// that memory into a permanent loss. Anything that cannot be stored is a 500 and
// the rows stay pending for the next attempt.
//
// Nothing here logs memory content, sanitized or not, nor the presented token,
// the signing secret, or the database error -- a pgx error can quote the
// connection target, and memory content is the user's, not the log's.
func (s *Server) handleSyncMemories(w http.ResponseWriter, r *http.Request) {
	slug := TenantSlugFromCtx(r.Context())
	if slug == "" {
		// Unreachable behind requireJWT, which refuses a token that names no
		// tenant. It is checked anyway so the handler cannot write into an
		// unnamed schema if the middleware is ever rewired.
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req syncRequest
	if !decodeJSONLimit(w, r, &req, maxSyncBodyBytes) {
		return
	}

	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "invalid_agent")
		return
	}

	memories := make([]store.MemoryEntry, 0, len(req.Memories))
	for _, memory := range req.Memories {
		// A memory may arrive without a session of its own; the envelope's
		// session is then the batch's, which is what the edge sends when every
		// memory in it shares one session. Both empty is a memory that could
		// never be retrieved by session, so it is rejected instead of being
		// stored somewhere unreachable.
		if memory.SessionID == "" {
			memory.SessionID = req.SessionID
		}
		if memory.SessionID == "" || memory.ID == "" {
			writeError(w, http.StatusBadRequest, "invalid_memory")
			return
		}

		// The plane is the authority on the plane's own copy: a pushed memory
		// is stored as synced regardless of what the edge's local bookkeeping
		// said about it. Sanitization and storage happen in the writer, which
		// is where the store's one sanitization pipeline lives.
		memory.SyncStatus = store.SyncStatusSynced

		memories = append(memories, memory)
	}

	if s.memories == nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	written, sanitized, err := s.memories.WriteBatch(r.Context(), slug, memories)
	if err != nil {
		if s.logger != nil {
			// Counts and identifiers only: no content, no token, no DSN.
			s.logger.Error("Memory sync write failed",
				"tenant_slug", slug, "agent_id", req.AgentID, "memories", len(memories), "error", err)
		}
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}

	if s.logger != nil {
		s.logger.Info("Memories synced",
			"tenant_slug", slug, "agent_id", req.AgentID, "written", written, "sanitized", sanitized)
	}

	writeJSON(w, http.StatusOK, syncResponse{Written: written, Sanitized: sanitized})
}

// ctxKey is the unexported type behind every context key this package stores,
// so no unrelated package can read or overwrite the tenant slug by reusing a
// string key that happens to match.
type ctxKey int

// tenantSlugKey is where a verified token's tenant slug is parked.
const tenantSlugKey ctxKey = iota

// WithTenantSlug returns a copy of ctx carrying a verified tenant slug.
//
// internal/tenant's JWT middleware calls this with a slug it has already
// verified, because the claims it reads back through its own accessors are
// parked under unexported keys in that package and this one cannot read them.
// The value set here is only ever the verified claim: it is the schema this
// endpoint writes into, so trusting anything else would defeat schema-per-tenant
// isolation entirely.
func WithTenantSlug(ctx context.Context, tenantSlug string) context.Context {
	return context.WithValue(ctx, tenantSlugKey, tenantSlug)
}

// TenantSlugFromCtx returns the verified tenant slug attached by
// WithTenantSlug, or "" when the request never passed through the middleware.
func TenantSlugFromCtx(ctx context.Context) string {
	slug, _ := ctx.Value(tenantSlugKey).(string)

	return slug
}
