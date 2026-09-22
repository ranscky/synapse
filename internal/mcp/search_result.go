// The result half of synapse_search_memories: the wire shape a search answers
// with, and the mapping from the scorer's own type into it.
//
// Split out of search.go to stay inside this project's 300-line-per-file ceiling,
// on the same seam that put the tool registration, handler, and argument
// validation in search.go: everything here is about what a result looks like,
// and the handler is the only thing that decides how one is produced.
package mcp

import (
	"time"

	"synapse/internal/scorer"
	"synapse/internal/store"
)

// searchToolResult is the payload synapse_search_memories returns.
type searchToolResult struct {
	// TraceID correlates this answer with this process's log line for it.
	TraceID string `json:"trace_id"`
	// Memories is the ranked result set, best first. Always an array, never
	// null: "nothing matched" must not break a client that iterates.
	Memories []searchMemoryScore `json:"memories"`
}

// searchMemoryScore is one scored memory as the tool reports it.
//
// Every field is marshaled unconditionally -- no omitempty anywhere -- because
// the score breakdown IS the answer this tool exists to give: a factor that
// scored 0.0 has to read as 0.0, never as an absent key a caller would have to
// interpret as either "zero" or "this server is too old to say".
type searchMemoryScore struct {
	ID             string  `json:"id"`
	Content        string  `json:"content"`
	MemoryType     string  `json:"memory_type"`
	AgentID        string  `json:"agent_id"`
	CrossAgent     bool    `json:"cross_agent"`
	ConflictStatus string  `json:"conflict_status"`
	CreatedAt      string  `json:"created_at"`
	SessionID      string  `json:"session_id"`
	Visibility     string  `json:"visibility"`
	ScoreS         float64 `json:"score_s"`
	ScoreR         float64 `json:"score_r"`
	ScoreI         float64 `json:"score_i"`
	ScoreT         float64 `json:"score_t"`
	ScoreTotal     float64 `json:"score_total"`
}

// searchMemoryScores flattens the best topK scored memories into the payload.
//
// The scorer already ordered them by total score descending, so this only
// truncates. parseSearchArgs guarantees a topK of at least 1; the result is
// never nil, so an empty set marshals as [] rather than null.
func searchMemoryScores(scored []scorer.ScoredMemory, topK int, localAgentID string) []searchMemoryScore {
	out := make([]searchMemoryScore, 0, len(scored))
	for _, memory := range scored {
		if len(out) >= topK {
			break
		}
		out = append(out, newSearchMemoryScore(memory, localAgentID))
	}
	return out
}

// newSearchMemoryScore copies one scored memory into the wire shape.
//
// cross_agent and the two normalizations follow the trace manifest's rules
// rather than inventing new ones, so one memory reads the same way in a
// synapse_search_memories result and in the trace of the compile that surfaced
// it: a blank agent_id is unattributed, not someone else's; a blank
// conflict_status is "none"; and a blank visibility is the column's own default,
// "org". created_at is emitted in UTC RFC3339Nano so two backends that store the
// same instant (Postgres timestamptz, SQLite text) do not render it differently.
func newSearchMemoryScore(memory scorer.ScoredMemory, localAgentID string) searchMemoryScore {
	conflictStatus := memory.ConflictStatus
	if conflictStatus == "" {
		conflictStatus = store.ConflictStatusNone
	}

	visibility := memory.Visibility
	if visibility == "" {
		visibility = store.VisibilityOrg
	}

	return searchMemoryScore{
		ID:             memory.ID,
		Content:        memory.Content,
		MemoryType:     memory.MemoryType,
		AgentID:        memory.AgentID,
		CrossAgent:     memory.AgentID != "" && memory.AgentID != localAgentID,
		ConflictStatus: conflictStatus,
		CreatedAt:      memory.Timestamp.UTC().Format(time.RFC3339Nano),
		SessionID:      memory.SessionID,
		Visibility:     visibility,
		ScoreS:         memory.ScoreS,
		ScoreR:         memory.ScoreR,
		ScoreI:         memory.ScoreI,
		ScoreT:         memory.ScoreT,
		ScoreTotal:     memory.Total,
	}
}
