// The edge node's queue: the decorator that puts a locally written memory on it.
//
// Before this file existed, nothing in the write path ever set a memory's
// sync_status to sync_pending. store.Write normalizes a blank SyncStatus to the
// backend's own default -- local_only on the SQLite backend -- and every writer
// in the process (the proxy's live traffic, internal/api's compile path, and the
// MCP write tool) leaves that field blank, so every memory an edge node stored
// was local_only. PendingSync, CountPendingSync, and DropOldestPendingSync all
// match on sync_pending, so the background flusher's queue was always empty: an
// edge node never pushed anything to its control plane. Phases 8, 9, 10, and 25
// each recorded that as a finding rather than a bug to fix in passing, and Phase
// 29's multi-agent integration test is the phase that could not pass without it
// (PROGRESS.md, "Findings" of Phase 25, item 3).
//
// The fix is a decorator rather than an edit to internal/store for two reasons.
// The v1 write path is frozen: store.Store.Write's normalization is pinned by
// its own tests and documented as the meaning of the three sync_status values, so
// changing it would make a standalone node's rows claim they are waiting for a
// plane that does not exist. And the decision is an edge's, not a store's: whether
// a memory should be queued for a control plane is a property of the process that
// has one configured, which is why the wrapper is installed by cmd/synapse only
// when control-plane-url is set.
//
// It implements store.Backend, which is also the shape internal/proxy's
// MemoryStore and internal/mcp's Store are declared with, so one value can be
// handed to both the proxy and the MCP server while the background flusher keeps
// the concrete *store.Store it needs for PendingSync and MarkSynced.
package sync

import (
	"context"
	"fmt"

	"synapse/internal/store"
)

// PendingWriter wraps a memory backend and marks every memory written through it
// sync_pending, which is what puts it in the queue the background flusher drains
// (see RunBackground).
//
// Only Write is changed. Reads, recency lookups, and supersession marks are
// delegated unchanged, because none of them is the write path: a memory becomes
// a memory when it is stored, and that is the moment it is either promised to the
// plane or not.
//
// A memory that already carries a SyncStatus keeps it. That is what lets a caller
// that knows better -- a re-push, an import, a test seeding synced rows -- state
// its own answer, and it is the same rule store.Write applies: only a blank value
// is normalized, and here the normalization is "pending" rather than "local_only".
type PendingWriter struct {
	inner store.Backend
}

// NewPendingWriter returns a writer that queues for the control plane whatever
// inner stores.
//
// inner is not owned: the caller closes it, exactly as the flusher's own
// PendingStore is left open by whoever opened it. A nil inner is accepted and
// answered with an error on use rather than a panic, matching the rest of this
// package's "a misconfigured node still has to answer" policy.
func NewPendingWriter(inner store.Backend) *PendingWriter {
	return &PendingWriter{inner: inner}
}

// Write stores entry, marked sync_pending unless it already names another state.
//
// The mark is applied to the caller's value before it is handed on, so the row
// the backend stores and the row a later read returns both carry it -- there is
// no window in which the local database holds a memory the flusher cannot see.
func (w *PendingWriter) Write(ctx context.Context, entry store.MemoryEntry) error {
	if w == nil || w.inner == nil {
		return fmt.Errorf("sync: pending writer has no backend to write through")
	}

	if entry.SyncStatus == "" {
		entry.SyncStatus = store.SyncStatusSyncPending
	}

	return w.inner.Write(ctx, entry)
}

// Search returns the topK most similar memories the caller may read. It is the
// backend's own search, unchanged: marking is a write-path concern.
func (w *PendingWriter) Search(ctx context.Context, queryEmbedding []float32, agentID, teamID, sessionID string, topK int) ([]store.MemoryEntry, error) {
	if w == nil || w.inner == nil {
		return nil, fmt.Errorf("sync: pending writer has no backend to search")
	}

	return w.inner.Search(ctx, queryEmbedding, agentID, teamID, sessionID, topK)
}

// GetRecent returns a session's most recent memories, newest first.
func (w *PendingWriter) GetRecent(ctx context.Context, sessionID string, limit int) ([]store.MemoryEntry, error) {
	if w == nil || w.inner == nil {
		return nil, fmt.Errorf("sync: pending writer has no backend to read from")
	}

	return w.inner.GetRecent(ctx, sessionID, limit)
}

// MarkSuperseded records that one memory has replaced another. It is delegated
// because supersession is local bookkeeping: the plane learns about it from the
// superseded_by field on the row it receives, not from a second call.
func (w *PendingWriter) MarkSuperseded(ctx context.Context, oldID, newID string) error {
	if w == nil || w.inner == nil {
		return fmt.Errorf("sync: pending writer has no backend to mark in")
	}

	return w.inner.MarkSuperseded(ctx, oldID, newID)
}

// The decorator has to satisfy the same contract the concrete backend does, or a
// drift in either would be a runtime surprise at the call site instead of a build
// failure here.
var _ store.Backend = (*PendingWriter)(nil)
