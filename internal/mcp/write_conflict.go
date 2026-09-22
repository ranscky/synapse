// The contradiction check synapse_write_memory runs before it stores anything.
//
// Split out of write.go (and out of write_result.go's neighbourhood) because it is
// the one part of this tool that is not about the write at all: it answers a
// question about the memories the node already holds, needs its own rationale for
// living in this layer rather than in the store, and has the best-effort failure
// policy the store's own detection documents. See store/pgconflict.go, which runs
// the same detector for the same reason on the control plane's write path.
package mcp

import (
	"context"
	"log/slog"

	"synapse/internal/conflict"
	"synapse/internal/store"
)

// writeConflictCandidatePool is how many already-stored memories one write is
// compared against. It mirrors store.conflictCandidateLimit, the pool PGStore.Write
// uses for its own detection: that constant is unexported and this package may not
// reach it, so the number is repeated here with the same meaning rather than left to
// drift into whatever the retrieval pool happens to be.
const writeConflictCandidatePool = 20

// conflictingMemoryID runs the project's contradiction detector over the memories
// this node can already read, and returns the id of the one entry disagrees with --
// or "" when nothing does.
//
// It has to run here, and why is not obvious: store.Write reports nothing. Its
// conflict verdict is a label written onto the stored row, and on the Postgres
// backend that label is produced inside Write by a detector only the control plane
// installs (cmd/plane's SetConflictDetector) -- while the local backend has no
// conflict columns to label at all. Nothing in the Backend contract exposes a
// verdict, so running detection here is the only way this tool can answer
// conflict_detected truthfully. What it runs is the same algorithm, at the same
// threshold, from the same config key the plane installs, so a node and its plane
// cannot disagree about what a contradiction is. A zero threshold resolves to
// conflict.DefaultJaccardThreshold, the "unset means default" convention the
// project's other numeric knobs use.
//
// The candidate set is the store's own Search at this node's identity and the
// session the memory is being written into: the same backend the row lands in, under
// the same visibility predicate, which is the pair this detector exists for. The
// control plane is deliberately not consulted -- a write path's detection is best
// effort by policy (pgconflict.go), and a second embedding plus a network dependency
// would buy candidates that are not stored where this row is.
//
// Best effort is exactly that policy: a candidate read that fails is logged and
// treated as "no contradiction", because a write must not gain a new failure mode
// from a feature whose whole purpose is to annotate a row that is otherwise
// perfectly storable.
func (s *Server) conflictingMemoryID(ctx context.Context, entry store.MemoryEntry, embedding []float32) string {
	candidates, err := s.store.Search(ctx, embedding, s.cfg.AgentID, s.cfg.TeamID, entry.SessionID, writeConflictCandidatePool)
	if err != nil {
		// Ids only: what was compared is content, and content is never logged.
		slog.Warn("MCP memory write conflict candidates failed", "memory_id", entry.ID, "error", err)
		return ""
	}

	found, conflictingID := conflict.NewContradictionDetector(s.cfg.ConflictJaccardThreshold).Detect(entry, candidates)
	if !found {
		return ""
	}

	return conflictingID
}
