// Write-path helpers for the Postgres backend, split out of pgstore.go to keep
// both files inside the 300-line ceiling. Everything here is a step PGStore.Write
// (or its sibling in pgconflict.go) runs against an already-validated entry.
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// sanitized applies the pre-storage pipeline the SQLite backend applies.
//
// A zero-value Store is enough to call Sanitize -- it reads no fields and only
// logs -- and reusing it is what keeps the two backends from drifting into two
// different sanitization policies. Sanitize already strips null bytes,
// neutralizes prompt-injection patterns, and caps content at 2048 bytes on a
// UTF-8 boundary, so no second truncation is needed here.
func sanitized(content string) string {
	var s Store
	return s.Sanitize(content)
}

// MarkSuperseded records that oldID has been superseded by newID.
//
// Like the SQLite backend, a missing oldID is an error rather than a silent
// no-op, so a caller holding a stale id finds out immediately.
//
// This is supersession proper: the old memory leaves every read. The conflict
// path next door deliberately does not do this -- see markSupersededCandidate,
// which marks a row and leaves it in the pool.
func (s *PGStore) MarkSuperseded(ctx context.Context, oldID, newID string) error {
	if oldID == "" || newID == "" {
		return fmt.Errorf("MarkSuperseded requires non-empty oldID and newID")
	}
	if _, err := uuid.Parse(oldID); err != nil {
		return fmt.Errorf("store: memory id %q is not a uuid", oldID)
	}
	if _, err := uuid.Parse(newID); err != nil {
		return fmt.Errorf("store: superseding memory id %q is not a uuid", newID)
	}

	tag, err := s.pool.Exec(ctx, `UPDATE `+s.table()+` SET superseded_by = $1 WHERE id = $2`, newID, oldID)
	if err != nil {
		return fmt.Errorf("store: mark memory superseded: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no memory found with id %s to mark as superseded", oldID)
	}

	return nil
}
