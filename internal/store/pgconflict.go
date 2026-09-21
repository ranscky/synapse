// Conflict marking on the Postgres write path: PGStore.Write consults a
// contradiction detector before it inserts, records the contradiction on both
// memories, and leaves both of them readable.
//
// This is the case internal/supersession cannot see. There the two versions of a
// decision were written in one session, by one agent, and the newer one carries a
// replacement phrase ("switched to", "instead of"). Here they were written by
// different agents in different sessions, and the newer one is a complete sentence
// that never mentions the older one: "We decided to use MySQL" says nothing about
// Postgres. The resolution is deliberately the cautious one rather than
// supersession's:
//
//   - the older memory is marked ConflictStatusSupersededCandidate -- a candidate
//     to be superseded, not superseded. It keeps its content, its embedding and its
//     superseded_by, stays in every read, and is only demoted by the scorer (see
//     scorer.Weights.ConflictScorePenalty). Its name and its direction mirror
//     superseded_by, which is likewise written on the older row and points at the
//     newer one;
//   - the newer memory is marked ConflictStatusConflict, because it is the row that
//     introduced the disagreement;
//   - both rows carry conflict_with_id pointing at the other, so either one, read
//     on its own, names what it disagrees with.
//
// The detector is injected rather than imported: internal/conflict imports this
// package (it takes a MemoryEntry), so this package cannot import it back.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// The three values a memory's conflict_status column can hold. They are the SQL
// literals written and matched on, so they are the only states a row can be in.
const (
	// ConflictStatusNone is the state of every memory no contradiction has been
	// detected for. It is also the column's own DEFAULT.
	ConflictStatusNone = "none"
	// ConflictStatusConflict marks the memory that introduced a contradiction: it
	// was written while a memory it disagrees with was already stored.
	ConflictStatusConflict = "conflict"
	// ConflictStatusSupersededCandidate marks the older memory a newer one
	// contradicts. It is not superseded and is not dropped from any read; it is a
	// candidate to be, which the scorer expresses as a penalty.
	ConflictStatusSupersededCandidate = "superseded_candidate"
)

// conflictCandidateLimit is how many of the tenant's most recent org-scoped
// memories one write is compared against.
const conflictCandidateLimit = 20

// ConflictDetectionBudget is the target for the detection block: one candidate
// query over conflictCandidateLimit memories plus the in-memory comparison over
// what it returned. Exported because it is part of this package's documented
// behaviour rather than an implementation detail -- the write path logs against it
// and the tests measure against it.
//
// It is a target, not a limit: a write is never refused or failed for being slow.
// It deliberately does not bound the whole write either -- an insert's own commit is
// a write-ahead-log fsync (measurably ~11ms on the machine this was built on, and
// the same for an insert with no detector, no embedding and no candidate query),
// which is the database's cost and not this feature's.
const ConflictDetectionBudget = 20 * time.Millisecond

// ConflictDetector is the contradiction check PGStore.Write consults before it
// inserts a memory: Detect reports whether candidate disagrees with one of the
// memories in existing, and returns that memory's id.
//
// The interface is declared here and satisfied by
// internal/conflict.ContradictionDetector through its method set, rather than this
// package importing that one -- conflict imports store, so the reverse would be an
// import cycle. A consumer owning the interface it calls is the shape the rest of
// this project already uses (see plane.MemoryWriter, tenant.Provisioner).
type ConflictDetector interface {
	Detect(candidate MemoryEntry, existing []MemoryEntry) (bool, string)
}

// SetConflictDetector installs the detector Write consults. Until one is
// installed -- the plane's sync path installs one at boot -- Write behaves exactly
// as it did before conflicts existed: every memory is stored with conflict_status
// 'none' and no existing row is ever marked.
//
// It is a setter rather than a NewPGStore parameter so that every existing
// construction path (the store factory, tenant provisioning, the tests) keeps
// compiling, and so that the detector is installed by a package that can name the
// concrete type without this package importing it.
func (s *PGStore) SetConflictDetector(d ConflictDetector) {
	s.detector = d
}

// conflictCandidates returns the tenant's most recent org-scoped memories, which
// is the candidate set every write is checked against.
//
// Org-scoped on purpose: a private memory belongs to the one session and agent
// that wrote it, and a team-scoped one to its team, so a contradiction with either
// is not a contradiction the tenant's other agents can act on -- and comparing
// against them would let one session's private text decide how another session's
// memory is stored. Superseded rows are excluded (they are already out of every
// read) and the newest come first, so when more than one memory disagrees the
// detector settles on the newest, matching how a caller's own candidate set is
// ranked.
//
// One query per Write call, and that is the point of the slice: the detector walks
// its candidate set in memory, so the block costs a single round trip no matter how
// many memories it compares.
func (s *PGStore) conflictCandidates(ctx context.Context) ([]MemoryEntry, error) {
	query := fmt.Sprintf(`SELECT %s FROM %s
WHERE superseded_by IS NULL AND visibility = 'org'
ORDER BY created_at DESC
LIMIT $1`, pgColumns, s.table())

	return s.queryEntries(ctx, query, conflictCandidateLimit)
}

// detectConflict runs the installed detector over the tenant's recent org-scoped
// memories for the memory about to be inserted, and reports how the new row should
// be stored.
//
// Three results: the conflict_status to insert, the conflict_with_id to insert
// (nil when there is none), and the id of the existing memory that has to be
// marked afterwards -- empty whenever nothing has to be marked, which covers both
// the no-detector case and the no-contradiction case.
//
// Detection is best effort and never fails the write. There is no error to return
// from Detect itself, and a failed candidate read is logged and treated as "no
// contradiction": the write path must not gain a new failure mode from a feature
// whose whole purpose is to annotate a row that is otherwise perfectly storable.
func (s *PGStore) detectConflict(ctx context.Context, entry MemoryEntry) (string, any, string) {
	if s.detector == nil {
		return ConflictStatusNone, nil, ""
	}

	// The block this phase budgeted for: one query, then a comparison over the slice
	// it returned. Timed only so that a regression is observable -- see
	// ConflictDetectionBudget.
	started := time.Now()

	existing, err := s.conflictCandidates(ctx)
	if err != nil {
		// Ids only: a memory's content never reaches a log line.
		slog.Warn("conflict_candidate_read_failed", "memory_id", entry.ID, "error", err)
		return ConflictStatusNone, nil, ""
	}

	found, conflictingID := s.detector.Detect(entry, existing)

	// Ids and a duration only: what was compared, and how long it took, never what it
	// said.
	if spent := time.Since(started); spent > ConflictDetectionBudget {
		slog.Warn("conflict_detection_slow", "memory_id", entry.ID,
			"duration", spent, "candidates", len(existing), "budget", ConflictDetectionBudget)
	}

	if !found || conflictingID == "" {
		return ConflictStatusNone, nil, ""
	}

	return ConflictStatusConflict, conflictingID, conflictingID
}

// markSupersededCandidate records that the older memory conflictingID is a
// candidate to be superseded by newID, the memory just inserted. The row keeps its
// content, its embedding and its superseded_by: this is a label plus the id it
// disagrees with, not a supersession, so the memory stays in every read.
//
// A missing row is an error rather than a silent no-op, matching MarkSuperseded:
// the id came from a row this call just read, so zero rows affected means either
// the row went away underneath it or the id was never one -- either way a caller
// holding a stale id finds out.
func (s *PGStore) markSupersededCandidate(ctx context.Context, conflictingID, newID string) error {
	if _, err := uuid.Parse(conflictingID); err != nil {
		return fmt.Errorf("store: conflicting memory id %q is not a uuid", conflictingID)
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE `+s.table()+` SET conflict_status = $1, conflict_with_id = $2 WHERE id = $3`,
		ConflictStatusSupersededCandidate, newID, conflictingID)
	if err != nil {
		return fmt.Errorf("store: mark memory as a superseded candidate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no memory found with id %s to mark as a superseded candidate", conflictingID)
	}

	return nil
}
