// Package sync is the edge node's push side of the Synapse v2 sync protocol:
// it ships memories the local store has marked sync_pending to a control plane
// over HTTP, in batches, from a background goroutine.
//
// Nothing in this package is called from a request path. The compilation hot
// path never blocks on a network call; the worst a slow or unreachable plane
// can do is make the background flusher log a warning and leave rows pending
// for the next interval (Phase 9 wires the write path to mark them).
package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"synapse/internal/config"
	"synapse/internal/store"
)

const (
	// syncPath is the control plane route this package pushes to.
	syncPath = "/v2/sync/memories"

	// pushTimeout bounds one push, response body included. A plane that cannot
	// answer inside five seconds is treated as unreachable and the batch simply
	// stays pending, which is the whole point of doing this off the request path.
	pushTimeout = 5 * time.Second

	// defaultSyncInterval and defaultSyncBatchSize mirror config.DefaultConfig().
	// They exist to guard a hand-built config (a test, an embedder-style caller)
	// rather than to override an operator: a zero SyncIntervalSeconds would
	// otherwise make time.NewTicker panic on the first flush, in a goroutine,
	// which is about the least debuggable failure this package could have.
	defaultSyncInterval  = 30 * time.Second
	defaultSyncBatchSize = 20

	// syncBacklogWarn and syncBacklogMax are the backlog thresholds: warn over
	// the first, abandon the oldest beyond the second.
	syncBacklogWarn = 1000
	syncBacklogMax  = 10000
)

// Syncer pushes locally written memories to one control plane.
//
// Its dependencies arrive through the constructor and it keeps no state of its
// own beyond them, so a process can hold one Syncer and hand it to a goroutine.
// interval, batchSize, backlogWarn, and backlogMax are fields rather than
// package constants so that a test can shrink them without mutating global
// state; NewSyncer derives them from config plus the defaults above.
type Syncer struct {
	cfg         config.Config
	httpClient  *http.Client
	interval    time.Duration
	batchSize   int
	backlogWarn int
	backlogMax  int
}

// NewSyncer returns a Syncer for cfg. cfg is copied, so a later mutation of the
// caller's Config cannot change where this node pushes or with what credential.
func NewSyncer(cfg config.Config) *Syncer {
	interval := time.Duration(cfg.SyncIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = defaultSyncInterval
	}

	batchSize := cfg.SyncBatchSize
	if batchSize <= 0 {
		batchSize = defaultSyncBatchSize
	}

	return &Syncer{
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: pushTimeout},
		interval:    interval,
		batchSize:   batchSize,
		backlogWarn: syncBacklogWarn,
		backlogMax:  syncBacklogMax,
	}
}

// pushRequest is the POST /v2/sync/memories body, in wire order.
//
// Memories carry their own session_id and are written exactly as received; the
// envelope's session_id is the batch's session when every memory shares one,
// and empty when the batch spans sessions. It is a convenience for a reader
// (and the fallback for a memory with no session of its own), never the thing
// the plane scopes a write to.
type pushRequest struct {
	SessionID string              `json:"session_id"`
	AgentID   string              `json:"agent_id"`
	Memories  []store.MemoryEntry `json:"memories"`
}

// Push sends entries to the control plane as one batch.
//
// The request carries exactly the configured credential in its Authorization
// header and nothing else: that value, and the header built from it, are never
// logged by this package, not even at debug level, and it is not part of the
// returned error (a transport error quotes the URL, which holds no secret).
//
// A 2xx response means the plane accepted the whole batch and the caller may
// mark those memories synced. Any other status is an error naming the status
// and nothing else, because a plane error body can quote a database error. An
// empty batch is a no-op: there is nothing to push and no request to make.
func (s *Syncer) Push(ctx context.Context, entries []store.MemoryEntry) error {
	if len(entries) == 0 {
		return nil
	}

	// Fail with something an operator can act on, rather than sending an
	// unauthenticated request every interval and logging a 401 forever.
	if s.cfg.ControlPlaneAPIKey == "" {
		return fmt.Errorf("sync: control-plane-api-key is required to push memories")
	}

	body, err := json.Marshal(pushRequest{
		SessionID: sharedSessionID(entries),
		AgentID:   s.cfg.AgentID,
		Memories:  entries,
	})
	if err != nil {
		return fmt.Errorf("sync: marshal push request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sync: build push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.ControlPlaneAPIKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sync: push request: %w", err)
	}
	defer resp.Body.Close()

	// Drain so the connection can be reused by the next interval's push. The
	// response body is otherwise unread: the acknowledgement this package
	// depends on is the status code.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("sync: push failed status=%d", resp.StatusCode)
	}

	return nil
}

// endpoint is the push URL: the configured control plane address with the sync
// path appended exactly once, whether or not the operator's URL ends in "/".
func (s *Syncer) endpoint() string {
	return strings.TrimRight(s.cfg.ControlPlaneURL, "/") + syncPath
}

// sharedSessionID returns the session every entry belongs to, or "" when the
// batch spans more than one.
//
// A pending batch is whatever the store's oldest rows happen to be, so a
// multi-session batch is normal rather than an error; reporting "" is the
// honest answer, and the per-memory session_id still tells the plane where each
// one belongs.
func sharedSessionID(entries []store.MemoryEntry) string {
	session := entries[0].SessionID
	for _, entry := range entries[1:] {
		if entry.SessionID != session {
			return ""
		}
	}

	return session
}

// logStart is the one line the flusher writes when it is handed to a
// goroutine, so an operator can see what a node is syncing to and how often.
// It names the endpoint and the agent, never the credential.
func (s *Syncer) logStart() {
	slog.Info("sync: background flusher started",
		"control_plane_url", s.endpoint(),
		"agent_id", s.cfg.AgentID,
		"interval_seconds", int(s.interval.Seconds()),
		"batch_size", s.batchSize,
	)
}
