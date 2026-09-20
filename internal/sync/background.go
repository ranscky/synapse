// The background side of the sync protocol: reading the pending queue, pushing
// it in batches, and acknowledging what the plane accepted. Split out of
// syncer.go to keep both files inside the 300-line ceiling.
package sync

import (
	"context"
	"log/slog"
	"time"

	"synapse/internal/store"
)

// syncMaxBatchesPerFlush bounds the work of one interval. The loop below reads
// after each push, so it terminates on its own when the queue empties; this cap
// only exists so a queue that somehow never drains cannot spin the flusher
// forever inside a single tick. Leftover memories are picked up next interval.
const syncMaxBatchesPerFlush = 50

// PendingStore is the part of the local memory store the background flusher
// needs: read the queue, count it, acknowledge a batch, and abandon the oldest
// overflow. *store.Store satisfies it as-is.
//
// It is declared here, on the consumer side, rather than imported as a concrete
// type so that the flusher can be exercised against a double -- and so that the
// store package stays unaware that anything syncs at all.
type PendingStore interface {
	// PendingSync returns the oldest sync_pending memories, up to limit.
	PendingSync(ctx context.Context, limit int) ([]store.MemoryEntry, error)
	// MarkSynced records that the plane accepted the named memories.
	MarkSynced(ctx context.Context, ids []string) error
	// CountPendingSync reports how large the backlog is.
	CountPendingSync(ctx context.Context) (int, error)
	// DropOldestPendingSync abandons the oldest pending memories beyond keep.
	DropOldestPendingSync(ctx context.Context, keep int) (int, error)
}

// RunBackground flushes the pending queue every interval until ctx is done.
//
// Call it as `go syncer.RunBackground(ctx, store)`: it blocks, and it is the
// only thing in the process that talks to the control plane. It flushes once
// immediately and then on each tick, so a memory written just before the
// goroutine starts does not wait out a whole interval, and a plane that was
// unreachable is retried on the next tick rather than hammered (there is no
// in-tick retry: a failed batch stays pending by design).
//
// A nil store logs and returns instead of panicking inside a goroutine.
func (s *Syncer) RunBackground(ctx context.Context, pending PendingStore) {
	if pending == nil {
		slog.Warn("sync: background flusher not started without a store")
		return
	}

	s.logStart()

	// A flush on startup only, not on a timer: see the doc comment above.
	s.flush(ctx, pending)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("sync: background flusher stopped")
			return
		case <-ticker.C:
			s.flush(ctx, pending)
		}
	}
}

// flush is one interval's work: check the backlog, then drain the queue in
// batches of batchSize until it is empty or the plane refuses.
//
// Every failure path returns rather than retrying in place. The rows are still
// pending, the next interval will try again, and one WARN per interval is a
// readable log while a retry loop is not.
func (s *Syncer) flush(ctx context.Context, pending PendingStore) {
	count, err := pending.CountPendingSync(ctx)
	if err != nil {
		slog.Warn("sync: could not count pending memories", "error", err)
		return
	}

	switch {
	case count > s.backlogMax:
		// Over the hard limit: the oldest overflow leaves the queue as
		// local_only. Nothing is deleted -- an unreachable plane must not be
		// able to destroy memories -- but those rows will never reach the plane,
		// so this is an ERROR, not a warning.
		abandoned, dropErr := pending.DropOldestPendingSync(ctx, s.backlogMax)
		if dropErr != nil {
			slog.Error("sync: could not abandon the oldest pending memories", "error", dropErr)
			return
		}
		slog.Error("sync: backlog over the hard limit -- the oldest pending memories have been abandoned and will never sync",
			"pending", count, "abandoned", abandoned, "limit", s.backlogMax)
	case count > s.backlogWarn:
		slog.Warn("sync backlog high", "pending", count, "limit", s.backlogWarn)
	}

	for batch := 0; batch < syncMaxBatchesPerFlush; batch++ {
		if ctx.Err() != nil {
			return
		}

		entries, err := pending.PendingSync(ctx, s.batchSize)
		if err != nil {
			slog.Warn("sync: could not read pending memories", "error", err)
			return
		}
		if len(entries) == 0 {
			return
		}

		if err := s.Push(ctx, entries); err != nil {
			// The memories are deliberately left pending: the plane did not
			// confirm them, so acknowledging them now would lose them.
			slog.Warn("sync: push failed, memories stay pending for the next attempt", "pending", len(entries), "error", err)
			return
		}

		ids := make([]string, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.ID)
		}

		if err := pending.MarkSynced(ctx, ids); err != nil {
			// Worse than a failed push: the plane has the memories but the
			// local rows still say pending, so they will be pushed again. The
			// plane treats a re-push as a no-op, so the cost is redundant
			// traffic, not duplicate memories.
			slog.Error("sync: the plane accepted a push but the local rows could not be marked synced -- they will be pushed again",
				"pending", len(entries), "error", err)
			return
		}

		slog.Debug("sync: pushed memories", "count", len(entries))

		if len(entries) < s.batchSize {
			return
		}
	}

	slog.Warn("sync: batch limit reached for this interval, the rest syncs on the next tick",
		"batches", syncMaxBatchesPerFlush, "batch_size", s.batchSize)
}
