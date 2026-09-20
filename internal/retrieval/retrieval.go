// Package retrieval holds the single candidate-retrieval pipeline shared by
// the live proxy path (internal/proxy) and the /v1/compile playground
// (internal/api). Before this package existed, both call sites duplicated
// their own "get candidates" logic independently -- proxy.go and api.go
// each called store.GetRecent(ctx, sessionID, 20) directly, which is how
// they silently drifted from the store's actual Search() implementation in
// the first place. Centralizing it here means there's exactly one place
// that decides how candidates are retrieved, so the playground and
// production can't diverge again.
package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"synapse/internal/store"
)

// Embedder is the interface for generating text embeddings. Defined here
// (mirroring the local Embedder interface in internal/proxy) rather than
// depending on internal/embedder directly, so this package stays trivially
// mockable in tests and doesn't pull in ONNX/cgo dependencies transitively.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Store is the narrow interface this package depends on -- just semantic
// search, not the full read/write surface. internal/proxy's MemoryStore
// interface and internal/api's *store.Store both already satisfy this with
// no changes needed on either side.
type Store interface {
	Search(ctx context.Context, queryEmbedding []float32, agentID, teamID, sessionID string, topK int) ([]store.MemoryEntry, error)
}

// Scope is who a retrieval is for: the agent and team the caller is acting as,
// and the session it is working in.
//
// It is a type rather than three more string parameters because Candidates
// already takes seven, and three adjacent strings of the same type are exactly
// the shape of a mix-up that still compiles -- swapping agentID and teamID
// would silently widen or narrow what a caller may read. The store's own
// Search keeps its parameters explicit (they are only ever passed straight
// through), but above it they travel together.
//
// The values reach the Postgres backend's visibility predicate unchanged: an
// org-scoped memory is visible to any of them, a team-scoped one only to a
// matching TeamID, and a private one only to the agent that wrote it in the
// session it is working in. On a standalone node (the SQLite backend) they
// change nothing at all, because one file is one agent.
//
// AgentID and TeamID are the caller's own identity and must come from the
// caller's own configuration and from a verified credential -- never from
// anything a request body can name. A request that talks its way into another
// agent's AgentID is reading that agent's private memories.
type Scope struct {
	// AgentID is the agent this retrieval acts as. Empty means an org-only
	// reader: it can reach org-scoped memories and nothing narrower.
	AgentID string
	// TeamID is the team this agent belongs to. Empty matches no team-scoped
	// memory, which is the fail-closed direction.
	TeamID string
	// SessionID is the session the agent is working in. Only private memories
	// are bounded by it.
	SessionID string
}

// PlaneCandidates is the optional control plane candidate source.
//
// It is satisfied structurally by *sync.Syncer -- this package deliberately
// does not import internal/sync (or internal/config), so the edge's HTTP client
// stays a detail of the binary that wires it up, and this package stays trivial
// to test with a double.
//
// A nil source means "no control plane configured" and Candidates then reads
// the local store exactly as it did before this interface existed. That is the
// default for every v1 caller and for every standalone node.
type PlaneCandidates interface {
	// PullCandidates returns the control plane's candidates for this query.
	// It is expected to be bounded by the implementation's own timeout and to
	// report every failure, because the caller's response to an error is to
	// fall back to the local store rather than to fail.
	PullCandidates(ctx context.Context, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error)
}

// Result bundles the retrieved candidates with the query embedding used to
// find them (callers need the embedding again downstream: to score
// candidates against, and to store alongside the new memory entry being
// written for this turn) plus per-stage timings, preserved separately so
// existing store_duration_ms / embed_duration_ms trace fields keep meaning
// what they meant before this pipeline was consolidated.
type Result struct {
	Candidates     []store.MemoryEntry
	QueryEmbedding []float32
	EmbedDuration  time.Duration
	SearchDuration time.Duration
}

// Candidates runs the retrieval pipeline: embed the query, then look for the
// top candidateK memory matches.
//
// Where those candidates come from depends on plane. When a control plane is
// configured (a non-nil PlaneCandidates) it is asked first, under its own hard
// timeout: a shared plane holds the org's memories, not just this node's. Any
// failure -- timeout, connection refused, non-2xx, malformed body -- is logged
// as a WARN and the local store is searched instead, so an unreachable plane
// costs one bounded ceiling and never a failed compilation. When plane is nil
// the local store is the only source and the behavior is exactly what it was
// before this parameter existed.
//
// The local path still replaces the old GetRecent(ctx, sessionID, 20) call that
// both proxy.go and api.go used to make directly. GetRecent narrowed the
// candidate pool to the 20 most recent memories *before* any relevance
// scoring happened, which meant an older memory -- however relevant to the
// current query -- was structurally invisible to the 4-factor scorer no
// matter how it scored, simply because it never made it into the candidate
// set. Search() ranks by embedding similarity across the whole session
// first, so relevance (not recency) decides what enters scoring.
//
// scope is who this retrieval is for -- the agent, team, and session the
// candidates are being gathered for (see Scope). It is passed through to the
// local store's Search, which is where the Postgres backend enforces memory
// visibility with it, so a plane-backed or Postgres-backed node only ever
// retrieves what that scope may read: org-scoped memories, its own team's, and
// its own private ones from the session it is in. The SQLite backend ignores
// the agent and team, since a standalone node's file has exactly one reader.
//
// candidateK <= 0 is passed straight through to store.Search, which falls
// back to its own default of 20 -- matching the permissive-zero convention
// already used for config.Config's other numeric fields (see Validate()).
//
// A nil embedder or empty query skips embedding and searches with an
// empty query embedding, which store.Search already handles by falling
// back to plain recency ordering -- so behavior degrades the same way the
// old GetRecent(..., 20) path did when there was nothing to embed, rather
// than returning zero candidates outright. The plane has the same fallback on
// its own side, so an unembeddable query behaves the same way whichever source
// answers it.
func Candidates(ctx context.Context, st Store, emb Embedder, plane PlaneCandidates, scope Scope, query string, candidateK int) (Result, error) {
	var result Result

	if emb != nil && query != "" {
		embedStart := time.Now()
		queryEmbedding, err := emb.Embed(ctx, query)
		result.EmbedDuration = time.Since(embedStart)
		if err != nil {
			return result, fmt.Errorf("failed to generate query embedding: %w", err)
		}
		result.QueryEmbedding = queryEmbedding
	}

	if plane != nil {
		pullStart := time.Now()
		candidates, err := plane.PullCandidates(ctx, result.QueryEmbedding, scope.SessionID, candidateK)
		result.SearchDuration = time.Since(pullStart)

		// No error means the plane answered inside its own ceiling, and its
		// answer is the candidate set -- empty included. The local store is
		// deliberately not merged in: the plane is the org's record, so mixing
		// a node's private rows into it would make the compiled context depend
		// on which node happened to build it.
		if err == nil {
			result.Candidates = candidates
			return result, nil
		}

		// The fallback is silent to the caller and loud in the log: a plane
		// that is down must not fail a compile, but an operator has to be able
		// to see that this node is serving from local memory. The error is
		// logged, which is the only place it is kept -- it carries no
		// credential (see sync.PullCandidates) and the caller must not fail on
		// it.
		slog.Warn("Control plane candidate pull failed, falling back to local search",
			"plane_unavailable", true,
			"fallback", "local",
			"error", err,
		)
	}

	if st == nil {
		return result, nil
	}

	searchStart := time.Now()
	candidates, err := st.Search(ctx, result.QueryEmbedding, scope.AgentID, scope.TeamID, scope.SessionID, candidateK)
	result.SearchDuration = time.Since(searchStart)
	if err != nil {
		return result, fmt.Errorf("failed to search memories: %w", err)
	}
	result.Candidates = candidates

	return result, nil
}
