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
	Search(ctx context.Context, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error)
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

// Candidates runs the retrieval pipeline: embed the query, then semantically
// search the full session memory store for the top candidateK matches.
//
// This replaces the old GetRecent(ctx, sessionID, 20) call that both
// proxy.go and api.go used to make directly. GetRecent narrowed the
// candidate pool to the 20 most recent memories *before* any relevance
// scoring happened, which meant an older memory -- however relevant to the
// current query -- was structurally invisible to the 4-factor scorer no
// matter how it scored, simply because it never made it into the candidate
// set. Search() ranks by embedding similarity across the whole session
// first, so relevance (not recency) decides what enters scoring.
//
// candidateK <= 0 is passed straight through to store.Search, which falls
// back to its own default of 20 -- matching the permissive-zero convention
// already used for config.Config's other numeric fields (see Validate()).
//
// A nil embedder or empty query skips embedding and calls Search with an
// empty query embedding, which store.Search already handles by falling
// back to plain recency ordering -- so behavior degrades the same way the
// old GetRecent(..., 20) path did when there was nothing to embed, rather
// than returning zero candidates outright.
func Candidates(ctx context.Context, st Store, emb Embedder, sessionID string, query string, candidateK int) (Result, error) {
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

	if st == nil {
		return result, nil
	}

	searchStart := time.Now()
	candidates, err := st.Search(ctx, result.QueryEmbedding, sessionID, candidateK)
	result.SearchDuration = time.Since(searchStart)
	if err != nil {
		return result, fmt.Errorf("failed to search memories: %w", err)
	}
	result.Candidates = candidates

	return result, nil
}