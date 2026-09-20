// The edge node's read half of the v2 protocol: GET /v2/memories/search.
//
// Split out of syncer.go rather than appended to it, because syncer.go is the
// push side (client, batching, backlog thresholds, the background flusher) and
// this is a subsystem with its own latency budget and its own failure contract.
// The same split already exists on the plane side, where the sync endpoint lives
// in sync.go instead of handlers.go.
package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"synapse/internal/store"
)

const (
	// searchPath is the control plane route this package pulls candidate
	// memories from.
	searchPath = "/v2/memories/search"

	// pullTimeout bounds one candidate pull, response body included. It is a
	// hard ceiling rather than a target: the compilation path calls this, and
	// when the plane cannot answer inside it the caller falls back to the local
	// store. 200ms is the whole latency budget the pull is allowed, so the local
	// path's sub-50ms compilation target is never at its mercy.
	pullTimeout = 200 * time.Millisecond

	// maxPullBodyBytes bounds a search response body. topK memories carry a
	// 384-float embedding each, so the documented answer is tens of kilobytes of
	// JSON; the ceiling matches the plane's own request limit, which is what
	// keeps neither side able to make the other allocate without bound.
	maxPullBodyBytes = 1 << 20
)

// searchRequest is the GET /v2/memories/search body, in wire order. The query
// embedding is the same 384-float vector the local store would search with, and
// the agent names itself exactly as it does on a push, so a plane can attribute
// a query to the node that made it.
//
// agent_id and team_id here are attribution, not authority: the plane decides
// what this node may read from the claims in the signed token it presents, never
// from this body. They are sent because the documented protocol carries them and
// because a plane may want to corroborate the two -- but a node whose token says
// one agent and whose body says another changes nothing about what it can read.
type searchRequest struct {
	QueryEmbedding []float32 `json:"query_embedding"`
	SessionID      string    `json:"session_id"`
	AgentID        string    `json:"agent_id"`
	TeamID         string    `json:"team_id"`
	TopK           int       `json:"top_k"`
}

// searchResponse is the 200 body: the tenant's memories nearest the query
// embedding. Each one is a full store.MemoryEntry -- embedding and agent_id
// included, because the caller scores and deduplicates candidates with them.
type searchResponse struct {
	Memories []store.MemoryEntry `json:"memories"`
}

// PullCandidates fetches org-scoped candidate memories from the control plane.
//
// This is the read side of the v2 protocol, and unlike Push it is called from
// the compilation path -- so it is bounded by pullTimeout (200ms) and reports
// every failure to its caller rather than retrying. The caller's contract is to
// fall back to the local store when this returns an error, which is what keeps
// compilation fast and correct with a slow, broken, or absent plane. A timeout
// is reported as context.DeadlineExceeded through %w so a caller can tell it
// apart from a refusal without parsing text.
//
// A non-2xx status is an error naming the status and nothing else, because a
// plane error body can quote a database error. The configured credential is
// never logged and never part of the returned error, exactly as in Push.
//
// A missing control plane or credential is a local error, not an
// unauthenticated request on every compile.
func (s *Syncer) PullCandidates(ctx context.Context, queryEmbedding []float32, sessionID string, topK int) ([]store.MemoryEntry, error) {
	if s.cfg.ControlPlaneURL == "" {
		return nil, fmt.Errorf("sync: control-plane-url is required to pull candidates")
	}
	if s.cfg.ControlPlaneAPIKey == "" {
		return nil, fmt.Errorf("sync: control-plane-api-key is required to pull candidates")
	}

	// The caller's context is preserved and the ceiling is layered on top of it,
	// so a request that is already cancelled stays cancelled and the pull can
	// never outlive its own 200ms budget either way.
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()

	body, err := json.Marshal(searchRequest{
		QueryEmbedding: queryEmbedding,
		SessionID:      sessionID,
		AgentID:        s.cfg.AgentID,
		TeamID:         s.cfg.TeamID,
		TopK:           topK,
	})
	if err != nil {
		return nil, fmt.Errorf("sync: marshal search request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.searchEndpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("sync: build search request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.ControlPlaneAPIKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// A timeout arrives here as context.DeadlineExceeded, wrapped by
		// net/http's url.Error, which is what the caller's fallback keys off.
		return nil, fmt.Errorf("sync: search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxPullBodyBytes))
		return nil, fmt.Errorf("sync: search failed status=%d", resp.StatusCode)
	}

	// Decoded from a bounded reader: a plane that streams JSON cannot make the
	// edge allocate without limit inside its own latency budget.
	var decoded searchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPullBodyBytes)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("sync: decode search response: %w", err)
	}

	return decoded.Memories, nil
}

// searchEndpoint is the candidate-pull URL: the configured control plane address
// with the search path appended exactly once, whether or not the operator's URL
// ends in "/".
func (s *Syncer) searchEndpoint() string {
	return strings.TrimRight(s.cfg.ControlPlaneURL, "/") + searchPath
}
