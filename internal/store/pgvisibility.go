// Visibility scopes for the Postgres backend: the three values a memory's
// visibility column may hold, the one predicate that enforces them on reads,
// and the normalization that decides what a write stores.
//
// Both halves live in one file on purpose. A read predicate is only correct if
// the write path stores exactly what it can match, and the two drift apart the
// moment they are maintained in different places -- the failure mode being a
// memory that is written successfully and can never be read back by anyone.
package store

import (
	"fmt"
	"log/slog"
)

// The three values MemoryEntry.Visibility ever holds. They are the SQL literals
// the visibility predicate matches against, so they are the only scopes a row
// can be stored under or found under; migration to a fourth would be a
// deliberate change in both places.
const (
	// VisibilityPrivate is readable only by the memory's own agent, and only
	// in the session it was written in.
	VisibilityPrivate = "private"
	// VisibilityTeam is readable by any agent whose team id matches the
	// memory's team id. A memory with no team id is never readable this way,
	// which is why visibilityForWrite refuses to store one.
	VisibilityTeam = "team"
	// VisibilityOrg is readable by every agent in the tenant -- the "global
	// brain" scope, and the memory column's own DEFAULT.
	VisibilityOrg = "org"
)

// visibilityWhere returns the SQL predicate that decides which memories a
// caller may see, and appends the caller's scope values to args.
//
// The predicate is the whole read-side authorization rule:
//
//	visibility = 'org'
//	OR (visibility = 'team'    AND team_id  = <the caller's team id>)
//	OR (visibility = 'private' AND agent_id = <the caller's agent id>
//	                           AND session_id = <the caller's session>)
//
// The caller's own scope is not a filter on the result set: an org-scoped
// memory reaches every agent in the tenant regardless of team or session, which
// is the point of a shared plane. Private is the narrowest branch precisely
// because it is the only one that is bounded by the caller's session.
//
// The scope literals above are the only SQL text this builds; every value from
// the caller is appended to args and reaches PostgreSQL as a parameter, never
// as part of the statement. fmt is used only to number the placeholders, which
// is also what keeps the predicate composable with the query vector that always
// occupies $1.
//
// An empty agentID or teamID fails closed: no row is ever stored with an empty
// agent_id (the column is NOT NULL and PGStore.Write backs a blank one with
// 'default') and a team-scoped row is never stored with a NULL team_id, and SQL
// NULL matches neither an empty string nor anything else, so a caller that names
// no agent or no team is an org-only reader.
func visibilityWhere(args *[]any, agentID, teamID, currentSessionID string) string {
	// Bound in predicate order: team id ($n-2), agent id ($n-1), session
	// ($n). The values are strings and the placeholders are positional, so
	// the order here is what the statement above expects.
	*args = append(*args, teamID, agentID, currentSessionID)
	n := len(*args)

	return fmt.Sprintf(`(
	visibility = 'org'
	OR (visibility = 'team' AND team_id = $%d)
	OR (visibility = 'private' AND agent_id = $%d AND session_id = $%d)
)`, n-2, n-1, n)
}

// visibilityForWrite resolves the scope a memory is stored under, plus the
// value for the team_id column (nil meaning SQL NULL).
//
// Three normalizations, all deliberately fail-closed -- none of them can ever
// make a memory wider than the caller asked for, and every one of them exists
// so a stored row is reachable by exactly the readers its scope names:
//
//   - A blank visibility is the schema's own DEFAULT 'org', the same way a
//     blank sync status is normalized to this backend's default.
//   - A 'team'-scoped memory with no team id is narrowed to 'private' and
//     logged. Storing it as written would put NULL in team_id, which no reader
//     can ever match (SQL NULL is not equal to any value, an empty string
//     included), so the memory would be written successfully and be unreachable
//     forever. Private keeps it readable by the agent and session that wrote it,
//     which is what "not org-wide" most nearly means when no team was named, and
//     the log line names the memory and the misconfiguration without naming its
//     content.
//   - A team id is stored as NULL rather than as an empty string when there is
//     none, so that one unteamed agent's row can never match another unteamed
//     agent's query.
//
// Anything outside the three documented scopes is an error rather than a stored
// value: the column has no CHECK constraint (the tenant DDL is created with
// IF NOT EXISTS, so adding one would not reach an existing schema), and a row
// whose visibility the predicate does not recognise is a row nobody can read.
func visibilityForWrite(memoryID string, entry MemoryEntry) (string, any, error) {
	visibility := entry.Visibility
	if visibility == "" {
		visibility = VisibilityOrg
	}

	switch visibility {
	case VisibilityOrg:
		return VisibilityOrg, nil, nil
	case VisibilityPrivate:
		return VisibilityPrivate, nil, nil
	case VisibilityTeam:
		if entry.TeamID == "" {
			slog.Warn("Team-scoped memory without a team id stored as private",
				"memory_id", memoryID, "visibility", VisibilityTeam)
			return VisibilityPrivate, nil, nil
		}
		return VisibilityTeam, entry.TeamID, nil
	default:
		return "", nil, fmt.Errorf("store: memory visibility %q is not one of %s|%s|%s",
			entry.Visibility, VisibilityPrivate, VisibilityTeam, VisibilityOrg)
	}
}
