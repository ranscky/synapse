// Tests for the conflict path: one agent's memory contradicting another agent's,
// written into the same tenant and left in the pool on both sides.
//
// This file is in package store_test, not package store, and that is not a style
// choice. internal/conflict and internal/scorer both import internal/store, so a
// test file in package store cannot import either of them -- Go reports an import
// cycle for the test binary before a single test runs. Everything below therefore
// goes through the exported API only: the pool, the store, the detector installed
// on it, and the scorer that demotes what the store marked.
//
// It needs a real Postgres, like this package's other Postgres tests, and skips
// when SYNAPSE_TEST_DB_DSN is unset:
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
//	  go test ./internal/store/... -run TestConflict -v
package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"synapse/internal/classifier"
	"synapse/internal/config"
	"synapse/internal/conflict"
	"synapse/internal/scorer"
	"synapse/internal/store"
)

const (
	// conflictTestDSNEnv is the environment variable the package's own Postgres
	// tests read, under which this one skips.
	conflictTestDSNEnv = "SYNAPSE_TEST_DB_DSN"

	// The phase's two fixtures: one decision, restated by a second agent with a
	// different value for the same predicate.
	decidedPostgres = "We decided to use Postgres"
	decidedMySQL    = "We decided to use MySQL"
	agentA          = "agent_a"
	agentB          = "agent_b"

	// warmUpMemory is written before the two fixtures so that the two writes below are
	// not paying for a cold connection or a cold write-ahead log. It shares no tokens
	// with either fixture, so it cannot be half of a contradiction.
	warmUpMemory = "The build runs on CI"
)

// conflictTestTenant returns a pool and the memories table of a tenant no earlier
// run used, so repeated runs cannot see each other's rows.
//
// The table name is built here rather than asked of the store, because the point of
// reading it directly below is to see what is in the database rather than what the
// store's own projection reports. It is the mapping NewPGStore documents for a slug
// (hyphens folded to underscores, schema "tenant_" + slug).
func conflictTestTenant(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()

	dsn := os.Getenv(conflictTestDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run the Postgres-backed conflict tests", conflictTestDSNEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := store.OpenPGPool(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", conflictTestDSNEnv)
	t.Cleanup(pool.Close)

	slug := fmt.Sprintf("cf%d", time.Now().UnixNano())
	table := "tenant_" + strings.ReplaceAll(slug, "-", "_") + ".memories"

	return pool, table, slug
}

// conflictStatusOf reads the two conflict columns straight out of the tenant table.
// A store's read path could in principle report a status the row does not hold, so
// the assertions that matter are made against the row itself.
func conflictStatusOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, memoryID string) (string, string) {
	t.Helper()

	var status, withID string
	err := pool.QueryRow(ctx,
		`SELECT conflict_status, coalesce(conflict_with_id::text, '') FROM `+table+` WHERE id = $1`,
		memoryID).Scan(&status, &withID)
	require.NoError(t, err, "memory %s must still exist as a row", memoryID)

	return status, withID
}

// conflictMemory builds one of the phase's two memories. Everything the scorer
// reads is identical between the pair -- type, importance, embedding, timestamp --
// and only the content and the agent that wrote it differ, so the single thing that
// can move one memory's Total away from the other's is the conflict penalty.
func conflictMemory(id, agentID, sessionID, content string, timestamp time.Time) store.MemoryEntry {
	embedding := make([]float32, store.EmbeddingDimensions)
	embedding[0] = 1

	return store.MemoryEntry{
		ID:         id,
		SessionID:  sessionID,
		Content:    content,
		MemoryType: "decision",
		Importance: 1.0,
		Embedding:  embedding,
		Timestamp:  timestamp,
		AgentID:    agentID,
	}
}

// TestConflictMarksBothMemories is the phase's test end to end: agent_a writes a
// decision, agent_b writes a contradictory one, and both rows end up marked -- the
// older as a superseded candidate, the newer as conflicting -- while both stay in
// the pool and the flagged one is demoted to exactly the configured penalty times
// the score of the memory it conflicts with.
func TestConflictMarksBothMemories(t *testing.T) {
	pool, table, slug := conflictTestTenant(t)
	ctx := context.Background()

	st, err := store.NewPGStore(pool, slug)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// The phase-12 config knobs are the source of truth for both halves: the
	// threshold the detector gates on and the penalty the scorer applies, so this
	// test fails if either default drifts away from the other.
	cfg := config.DefaultConfig()
	st.SetConflictDetector(conflict.NewContradictionDetector(cfg.ConflictJaccardThreshold))

	// One timestamp for both memories, so recency cannot differ between them either.
	base := time.Now().UTC()
	postgres := conflictMemory(uuid.NewString(), agentA, "session-a", decidedPostgres, base)
	mysql := conflictMemory(uuid.NewString(), agentB, "session-b", decidedMySQL, base)

	// A warm-up write first, so neither measured write below is paying for a cold
	// connection or a cold write-ahead log.
	require.NoError(t, st.Write(ctx, conflictMemory(uuid.NewString(), "agent_warmup", "session-warmup", warmUpMemory, base)))

	// The first fixture write has nothing to contradict, which is the baseline the flag
	// is measured against: a "conflict" on a memory with nothing to conflict with would
	// make the whole mechanism meaningless.
	started := time.Now()
	require.NoError(t, st.Write(ctx, postgres))
	baseline := time.Since(started)
	status, withID := conflictStatusOf(t, ctx, pool, table, postgres.ID)
	require.Equal(t, store.ConflictStatusNone, status)
	require.Empty(t, withID)

	// The second fixture write is the one that detects.
	started = time.Now()
	require.NoError(t, st.Write(ctx, mysql))
	detected := time.Since(started)

	olderStatus, olderWith := conflictStatusOf(t, ctx, pool, table, postgres.ID)
	newerStatus, newerWith := conflictStatusOf(t, ctx, pool, table, mysql.ID)
	t.Logf("agent_a (%q): conflict_status=%q conflict_with_id=%q", decidedPostgres, olderStatus, olderWith)
	t.Logf("agent_b (%q): conflict_status=%q conflict_with_id=%q", decidedMySQL, newerStatus, newerWith)
	// Evidence, not a measurement of detection: both of these are dominated by the
	// insert's own commit. See the budget subtest for how the block is measured.
	t.Logf("whole write that detects: %s; the write before it: %s", detected, baseline)

	t.Run("the older memory becomes a superseded candidate", func(t *testing.T) {
		assert.Equal(t, store.ConflictStatusSupersededCandidate, olderStatus,
			"the memory the newer one contradicts is the one that is a candidate to be superseded")
		assert.Equal(t, mysql.ID, olderWith, "and it names the memory that contradicts it")
	})

	t.Run("the newer memory is marked as conflicting", func(t *testing.T) {
		assert.Equal(t, store.ConflictStatusConflict, newerStatus,
			"the memory that introduced the contradiction is the one marked conflicting")
		assert.Equal(t, postgres.ID, newerWith)
	})

	// Both rows are read back through the store as well, not only out of the table: a
	// marker that does not survive the read path is not worth storing. GetRecent is
	// session-scoped by contract, so this is one call per session rather than one call
	// for both memories.
	olderRecent, err := st.GetRecent(ctx, "session-a", 10)
	require.NoError(t, err)
	require.Len(t, olderRecent, 1, "a flagged memory is demoted, never dropped")
	newerRecent, err := st.GetRecent(ctx, "session-b", 10)
	require.NoError(t, err)
	require.Len(t, newerRecent, 1, "the memory that conflicted is not dropped either")

	t.Run("both memories stay in the pool and keep their marker", func(t *testing.T) {
		assert.Equal(t, postgres.ID, olderRecent[0].ID)
		assert.Equal(t, store.ConflictStatusSupersededCandidate, olderRecent[0].ConflictStatus)
		assert.Equal(t, mysql.ID, olderRecent[0].ConflictWithID)
		assert.Equal(t, mysql.ID, newerRecent[0].ID)
		assert.Equal(t, store.ConflictStatusConflict, newerRecent[0].ConflictStatus)
		assert.Equal(t, postgres.ID, newerRecent[0].ConflictWithID)
	})

	t.Run("the flagged memory scores the penalty times the other", func(t *testing.T) {
		weights := scorer.GetWeights(
			cfg.WeightSemanticSimilarity,
			cfg.WeightRecency,
			cfg.WeightImportance,
			cfg.WeightTaskAlignment,
		)
		weights.ConflictScorePenalty = cfg.ConflictScorePenalty

		sc := scorer.NewScorer(weights, classifier.Generic, 1.0, time.Now())
		scored := sc.Score(ctx, olderRecent[0].Embedding, []store.MemoryEntry{olderRecent[0], newerRecent[0]})
		require.Len(t, scored, 2)

		byID := make(map[string]scorer.ScoredMemory, len(scored))
		for _, s := range scored {
			byID[s.ID] = s
		}
		flagged, plain := byID[postgres.ID], byID[mysql.ID]

		t.Logf("superseded candidate: Total=%.6f (S=%.4f R=%.4f I=%.4f T=%.4f)",
			flagged.Total, flagged.ScoreS, flagged.ScoreR, flagged.ScoreI, flagged.ScoreT)
		t.Logf("conflicting memory:   Total=%.6f (S=%.4f R=%.4f I=%.4f T=%.4f)",
			plain.Total, plain.ScoreS, plain.ScoreR, plain.ScoreI, plain.ScoreT)
		t.Logf("penalty=%v", weights.ConflictScorePenalty)

		// Identical inputs, so the four factors agree exactly and only Total moves: the
		// penalty is the only difference between these two rows.
		assert.InDelta(t, plain.ScoreS, flagged.ScoreS, 1e-9)
		assert.InDelta(t, plain.ScoreR, flagged.ScoreR, 1e-9)
		assert.InDelta(t, plain.ScoreI, flagged.ScoreI, 1e-9)
		assert.InDelta(t, plain.ScoreT, flagged.ScoreT, 1e-9)

		require.Greater(t, plain.Total, 0.0, "the unpenalised total has to be a real number to compare against")
		assert.InDelta(t, flagged.Total, plain.Total*cfg.ConflictScorePenalty, 0.001,
			"a superseded candidate's Total must be exactly the penalty times the score of the memory it conflicts with")
		assert.Less(t, flagged.Total, plain.Total, "the flag demotes the older memory rather than removing it")
	})

	t.Run("the detection comparison stays inside the budget", func(t *testing.T) {
		// The phase's budget covers the block: the candidate query plus the comparison
		// over the slice it returned. A whole write cannot measure that -- an insert's
		// commit is a write-ahead-log fsync (11ms on the machine this was built on, the
		// same for an insert with no detector and no candidate query at all), which
		// swamps the block and is the database's cost rather than this feature's. The
		// query half is measured against the same budget in
		// TestConflictCandidateFetchStaysWithinBudget, in package store.
		//
		// Here the comparison half is measured with the real detector over a candidate
		// set of exactly the size the query is limited to, 1000 times, so a detector that
		// became expensive shows up as a number rather than as a slower product.
		const candidateCount, runs = 20, 1000

		candidates := make([]store.MemoryEntry, candidateCount)
		for i := range candidates {
			candidates[i] = conflictMemory(uuid.NewString(), "agent_c",
				fmt.Sprintf("session-%d", i), decidedMySQL, base)
		}
		detector := conflict.NewContradictionDetector(conflict.DefaultJaccardThreshold)
		candidate := conflictMemory(uuid.NewString(), agentA, "session-a", decidedPostgres, base)

		started := time.Now()
		for i := 0; i < runs; i++ {
			detector.Detect(candidate, candidates)
		}
		perComparison := time.Since(started) / runs
		t.Logf("one comparison over %d candidates: %s (budget %s)", candidateCount, perComparison, store.ConflictDetectionBudget)

		assert.Less(t, perComparison, store.ConflictDetectionBudget,
			"comparing one memory against a full candidate set must stay inside the detection budget")
	})
}

// TestConflictWithoutADetectorMarksNothing is the other half of the wiring: a store
// nothing installed a detector on must behave exactly as it did before conflicts
// existed, however contradictory the memories written through it are. That is what
// keeps every other store in this project (the standalone edge node's, this
// package's other tests, a plane that never calls SetConflictDetector) from gaining
// a new column value it never asked for.
func TestConflictWithoutADetectorMarksNothing(t *testing.T) {
	pool, table, slug := conflictTestTenant(t)
	ctx := context.Background()

	st, err := store.NewPGStore(pool, slug)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	base := time.Now().UTC()
	postgres := conflictMemory(uuid.NewString(), agentA, "session-a", decidedPostgres, base)
	mysql := conflictMemory(uuid.NewString(), agentB, "session-b", decidedMySQL, base)
	require.NoError(t, st.Write(ctx, postgres))
	require.NoError(t, st.Write(ctx, mysql))

	for _, id := range []string{postgres.ID, mysql.ID} {
		status, withID := conflictStatusOf(t, ctx, pool, table, id)
		assert.Equal(t, store.ConflictStatusNone, status, "no detector means no marking")
		assert.Empty(t, withID)
	}
}
