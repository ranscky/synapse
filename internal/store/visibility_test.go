// Phase 10's memory-scope tests: what one caller may read on a shared plane.
//
// Split out of pgstore_test.go to stay inside the 300-line ceiling this project
// holds every file to, and because this is a security boundary rather than a
// description of the store's general behaviour -- the sibling of
// isolation_test.go, which guards the tenant boundary these tests sit inside.
// Tenant isolation says which schema a query reaches; visibility says which rows
// inside that schema are in the result at all.
package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idsOf returns the ids of a result set, for the membership and set assertions
// these tests make. It exists so a failure prints ids rather than whole entries
// with their 384-dim embeddings.
func idsOf(entries []MemoryEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}

	return ids
}

// TestVisibilityScopes is the security test for Phase 10's memory scopes: an
// agent reading a shared plane must reach the org's memories and never another
// agent's private ones.
//
// It is built so it cannot pass vacuously. The same fixture is read first as the
// agent that wrote it -- all 8 rows, private included -- so a later "exactly 5"
// cannot be explained by a write that never landed, a vector that matches
// nothing, or a scope that excludes everything. Every memory shares one session
// and one embedding pool, and the private rows sit in the session they belong
// to, so the only thing separating the 5 org rows from the 3 private ones is the
// reader's identity -- which is exactly the property under test. Matching the
// session on its own is asserted separately, so a session check standing in for
// the agent check would fail here.
func TestVisibilityScopes(t *testing.T) {
	st := testPGStore(t)
	ctx := context.Background()

	const (
		agentA   = "agent_a"
		agentB   = "agent_b"
		sessionA = "session-a-1"
		sessionB = "session-b-1"

		orgCount     = 5
		privateCount = 3
	)

	// Everything is written by agent_a in agent_a's own session: the org rows so
	// that a reader in another session has to reach them, the private rows so
	// that a session filter alone would find them.
	base := time.Now().UTC()
	orgIDs := make([]string, 0, orgCount)
	privateIDs := make([]string, 0, privateCount)

	for i := 0; i < orgCount; i++ {
		entry := testEntry(uuid.NewString(), sessionA, embeddingAt(i, 1))
		entry.AgentID = agentA
		entry.Visibility = VisibilityOrg
		entry.Timestamp = base.Add(time.Duration(i) * time.Second)
		require.NoError(t, st.Write(ctx, entry), "writing org-scoped memory %d", i)
		orgIDs = append(orgIDs, entry.ID)
	}
	for i := 0; i < privateCount; i++ {
		entry := testEntry(uuid.NewString(), sessionA, embeddingAt(orgCount+i, 1))
		entry.AgentID = agentA
		entry.Visibility = VisibilityPrivate
		entry.Timestamp = base.Add(time.Duration(orgCount+i) * time.Second)
		require.NoError(t, st.Write(ctx, entry), "writing private memory %d", i)
		privateIDs = append(privateIDs, entry.ID)
	}

	// Nearest to this query is orgIDs[0]. Every assertion below is about which
	// memories exist for a reader, not about their order, so the choice of
	// vector only has to be a valid one.
	query := embeddingAt(0, 1)

	t.Run("control: the writer reads all of its own memories", func(t *testing.T) {
		got, err := st.Search(ctx, query, agentA, "", sessionA, 20)
		require.NoError(t, err)
		require.Len(t, got, orgCount+privateCount,
			"agent_a must see its own org and private memories; without this the assertions below would prove nothing")
		assert.ElementsMatch(t,
			append(append([]string{}, orgIDs...), privateIDs...), idsOf(got))
	})

	t.Run("agent_b sees agent_a's org memories and none of its private ones", func(t *testing.T) {
		got, err := st.Search(ctx, query, agentB, "", sessionB, 20)
		require.NoError(t, err)
		require.Len(t, got, orgCount, "exactly the org-scoped memories, and nothing narrower")

		ids := idsOf(got)
		assert.ElementsMatch(t, orgIDs, ids,
			"the five org-scoped memories must be reachable from another agent and another session")
		for _, id := range privateIDs {
			assert.NotContains(t, ids, id, "another agent's private memory must never be returned")
		}
		for _, e := range got {
			assert.Equal(t, VisibilityOrg, e.Visibility, "an org-scoped search returns org-scoped memories")
			assert.Equal(t, agentA, e.AgentID, "org memories stay attributed to the agent that wrote them")
		}
	})

	t.Run("agent_b in agent_a's own session still sees only the org memories", func(t *testing.T) {
		got, err := st.Search(ctx, query, agentB, "", sessionA, 20)
		require.NoError(t, err)
		assert.Len(t, got, orgCount,
			"knowing the session is not enough: the agent is the other half of the private predicate")
	})

	t.Run("the no-embedding fallback cannot reach private memories", func(t *testing.T) {
		got, err := st.Search(ctx, nil, agentB, "", sessionB, 20)
		require.NoError(t, err)
		assert.Len(t, got, orgCount,
			"the recency fallback applies the same predicate; otherwise sending no embedding would read the whole tenant")
	})

	t.Run("a blank agent id is an org-only reader", func(t *testing.T) {
		got, err := st.Search(ctx, query, "", "", sessionB, 20)
		require.NoError(t, err)
		assert.Len(t, got, orgCount,
			"no verified agent identity means org scope and nothing narrower, never every agent")
	})
}

// TestVisibilityTeamScopes covers the middle branch of the visibility predicate:
// a team-scoped memory is readable by its own team and by nobody else, and a
// reader with no team id reaches no team-scoped memory at all. That last case is
// the fail-closed direction, and the reason a blank team id is stored as SQL
// NULL rather than as an empty string: NULL matches nothing, so two unteamed
// agents never match each other's rows.
func TestVisibilityTeamScopes(t *testing.T) {
	st := testPGStore(t)
	ctx := context.Background()

	const (
		writerID = "agent_writer"
		readerID = "agent_reader"
		teamOne  = "team-1"
		teamTwo  = "team-2"
		session  = "session-team-scope"

		orgCount     = 2
		teamCount    = 3
		privateCount = 2
	)

	base := time.Now().UTC()
	axis := 0

	// write stores n memories of one scope and returns their ids. Each gets its
	// own axis, so no two rows are equidistant from any query.
	write := func(visibility, team string, n int) []string {
		ids := make([]string, 0, n)
		for i := 0; i < n; i++ {
			entry := testEntry(uuid.NewString(), session, embeddingAt(axis, 1))
			entry.AgentID = writerID
			entry.Visibility = visibility
			entry.TeamID = team
			entry.Timestamp = base.Add(time.Duration(axis) * time.Second)
			require.NoError(t, st.Write(ctx, entry), "writing %s-scoped memory %d", visibility, i)
			ids = append(ids, entry.ID)
			axis++
		}

		return ids
	}

	orgIDs := write(VisibilityOrg, "", orgCount)
	teamIDs := write(VisibilityTeam, teamOne, teamCount)
	privateIDs := write(VisibilityPrivate, "", privateCount)

	query := embeddingAt(0, 1)

	t.Run("a teammate reads the team's memories", func(t *testing.T) {
		got, err := st.Search(ctx, query, readerID, teamOne, "session-elsewhere", 20)
		require.NoError(t, err)
		require.Len(t, got, orgCount+teamCount)

		ids := idsOf(got)
		for _, id := range append(append([]string{}, orgIDs...), teamIDs...) {
			assert.Contains(t, ids, id, "org memories and its own team's memories are readable by a teammate")
		}
		for _, id := range privateIDs {
			assert.NotContains(t, ids, id, "a teammate is not the agent that wrote a private memory")
		}
	})

	t.Run("a team-scoped memory round-trips its team id", func(t *testing.T) {
		got, err := st.Search(ctx, query, readerID, teamOne, "", 20)
		require.NoError(t, err)

		seen := 0
		for _, e := range got {
			if e.Visibility != VisibilityTeam {
				continue
			}
			seen++
			assert.Equal(t, teamOne, e.TeamID, "the scope of a result is part of the result")
		}
		assert.Equal(t, teamCount, seen, "every team-scoped memory reachable here carries its team id")
	})

	t.Run("a reader with no team id reaches no team-scoped memory", func(t *testing.T) {
		got, err := st.Search(ctx, query, readerID, "", "", 20)
		require.NoError(t, err)
		assert.Len(t, got, orgCount,
			"an unteamed reader is not every team: team ids are stored as NULL, which matches no value")
	})

	t.Run("another team reaches no team-scoped memory", func(t *testing.T) {
		got, err := st.Search(ctx, query, readerID, teamTwo, "", 20)
		require.NoError(t, err)
		assert.Len(t, got, orgCount, "a team id only ever widens a search to that one team")
	})
}

// TestVisibilityWriteNormalization covers what a write stores, which is the other
// half of the read predicate: a scope the predicate cannot match would store a
// memory nobody can ever read. Every normalization here makes the stored scope
// narrower than or equal to the caller's request, never wider.
func TestVisibilityWriteNormalization(t *testing.T) {
	st := testPGStore(t)
	ctx := context.Background()
	query := embeddingAt(0, 1)

	t.Run("a blank visibility is stored as the column's default", func(t *testing.T) {
		entry := testEntry(uuid.NewString(), "session-blank", embeddingAt(0, 1))
		require.NoError(t, st.Write(ctx, entry))

		got, err := st.GetRecent(ctx, "session-blank", 10)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, VisibilityOrg, got[0].Visibility)
		assert.Empty(t, got[0].TeamID, "an org-scoped memory carries no team id")
	})

	t.Run("an unknown visibility is refused rather than stored", func(t *testing.T) {
		entry := testEntry(uuid.NewString(), "session-bad-scope", embeddingAt(0, 1))
		entry.Visibility = "world-readable"

		require.Error(t, st.Write(ctx, entry),
			"a scope the read predicate cannot match is a memory nobody can read")

		got, err := st.GetRecent(ctx, "session-bad-scope", 10)
		require.NoError(t, err)
		assert.Empty(t, got, "a refused write must store nothing")
	})

	t.Run("a team scope with no team id is narrowed to private", func(t *testing.T) {
		// A store of its own, because an org-scoped memory is reachable from any
		// session: the subtest above wrote one, and it would otherwise appear in
		// this result set and hide what is being asserted.
		own := testPGStore(t)

		entry := testEntry(uuid.NewString(), "session-narrowed", embeddingAt(0, 1))
		entry.AgentID = "agent_writer"
		entry.Visibility = VisibilityTeam
		// TeamID deliberately left empty: stored as written it would put NULL in
		// team_id, which the predicate matches for nobody.
		require.NoError(t, own.Write(ctx, entry))

		// Its own writer, in its own session, still reaches it...
		mine, err := own.Search(ctx, query, "agent_writer", "", "session-narrowed", 10)
		require.NoError(t, err)
		require.Len(t, mine, 1)
		assert.Equal(t, entry.ID, mine[0].ID)
		assert.Equal(t, VisibilityPrivate, mine[0].Visibility,
			"a team scope with no team is narrowed to private: never widened, never stored unreachable")
		assert.Empty(t, mine[0].TeamID)

		// ...and nobody else does, which is the point of narrowing it.
		theirs, err := own.Search(ctx, query, "agent_other", "", "session-narrowed", 10)
		require.NoError(t, err)
		assert.Empty(t, theirs)
	})
}
