// The write-order half of the conflict path: the same two decisions as
// conflict_test.go's TestConflictMarksBothMemories, written in the opposite order.
//
// This file exists for one reason. The product's rule is directional -- the *older*
// memory becomes the superseded candidate and the newer one is marked conflicting --
// and a directional rule is exactly the kind a test with one fixed write order cannot
// tell apart from its own inverse. The QA checklist states the expectation from the
// other side ("Postgres vs MySQL memory -> MySQL is superseded_candidate"), which is
// what this file settles: it writes MySQL first, so MySQL is the older memory, and
// asserts that MySQL -- and only MySQL -- carries the marker and the halved score.
//
// Between this file and conflict_test.go, both ends are pinned: whichever decision is
// persisted first is the one demoted, and the vendor names in the two fixtures are
// what move, not the rule. The product is not asked to prefer MySQL over Postgres, and
// nothing here would notice if the fixtures were swapped.
//
// It needs a real Postgres like its sibling, and skips when SYNAPSE_TEST_DB_DSN is
// unset (conflictTestTenant documents why that is a skip here and a failure under the
// integration tag):
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
//	  go test ./internal/store/... -run TestConflictDemotesWhicheverDecisionCameFirst -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"synapse/internal/classifier"
	"synapse/internal/config"
	"synapse/internal/conflict"
	"synapse/internal/scorer"
	"synapse/internal/store"
)

// TestConflictDemotesWhicheverDecisionCameFirst is the checklist's conflict scenario:
// the MySQL decision is stored first, the Postgres decision contradicts it second, and
// the MySQL memory -- the older one -- becomes the superseded candidate scored at the
// configured 0.5x penalty, with both versions still present in the read path and the
// demotion visible in the S/R/I/T breakdown a trace carries.
func TestConflictDemotesWhicheverDecisionCameFirst(t *testing.T) {
	pool, table, slug := conflictTestTenant(t)
	ctx := context.Background()

	st, err := store.NewPGStore(pool, slug)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// The config's own threshold and penalty are the source of truth for both halves,
	// so this test fails if either default drifts away from the other.
	cfg := config.DefaultConfig()
	st.SetConflictDetector(conflict.NewContradictionDetector(cfg.ConflictJaccardThreshold))

	// One timestamp for both memories, so recency cannot differ between them either.
	base := time.Now().UTC()

	mysql := conflictMemory(uuid.NewString(), agentA, "session-a", decidedMySQL, base)
	postgres := conflictMemory(uuid.NewString(), agentB, "session-b", decidedPostgres, base)

	// A warm-up write first, so neither measured write pays for a cold connection or a
	// cold write-ahead log.
	require.NoError(t, st.Write(ctx, conflictMemory(uuid.NewString(), "agent_warmup", "session-warmup", warmUpMemory, base)))

	require.NoError(t, st.Write(ctx, mysql))

	firstStatus, firstWith := conflictStatusOf(t, ctx, pool, table, mysql.ID)
	require.Equal(t, store.ConflictStatusNone, firstStatus,
		"the first write has nothing to contradict yet, which is the baseline the flag is measured against")
	require.Empty(t, firstWith)

	require.NoError(t, st.Write(ctx, postgres))

	mysqlStatus, mysqlWith := conflictStatusOf(t, ctx, pool, table, mysql.ID)
	postgresStatus, postgresWith := conflictStatusOf(t, ctx, pool, table, postgres.ID)

	t.Logf("stored first  (%q): conflict_status=%q conflict_with_id=%q", decidedMySQL, mysqlStatus, mysqlWith)
	t.Logf("stored second (%q): conflict_status=%q conflict_with_id=%q", decidedPostgres, postgresStatus, postgresWith)

	assert.Equal(t, store.ConflictStatusSupersededCandidate, mysqlStatus,
		"the decision stored first becomes the candidate to be superseded, whatever vendor it names")
	assert.Equal(t, postgres.ID, mysqlWith, "and it names the decision that contradicted it")
	assert.Equal(t, store.ConflictStatusConflict, postgresStatus,
		"the decision stored second is marked conflicting, because it introduced the disagreement")
	assert.Equal(t, mysql.ID, postgresWith)
}

// TestConflictDemotesWhicheverDecisionCameFirstScores covers the second half of the
// same claim: the marker alone would let a scorer that ignored it pass the test above,
// so the demotion is measured too, the way conflict_test.go measures it.
//
// It is a second test rather than more body in the first because the two halves fail
// for unrelated reasons -- a wrong row marked is a write-path bug, a marker that does
// not move the score is a scoring bug -- and a failure should name which.
func TestConflictDemotesWhicheverDecisionCameFirstScores(t *testing.T) {
	// The table name is not needed here: this half asserts through the store's own read
	// path, which is the stronger claim anyway -- a marker the read path does not carry
	// is not worth storing.
	pool, _, slug := conflictTestTenant(t)
	ctx := context.Background()

	st, err := store.NewPGStore(pool, slug)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.DefaultConfig()
	st.SetConflictDetector(conflict.NewContradictionDetector(cfg.ConflictJaccardThreshold))

	base := time.Now().UTC()

	mysql := conflictMemory(uuid.NewString(), agentA, "session-a", decidedMySQL, base)
	postgres := conflictMemory(uuid.NewString(), agentB, "session-b", decidedPostgres, base)

	require.NoError(t, st.Write(ctx, mysql))
	require.NoError(t, st.Write(ctx, postgres))

	mysqlRecent, err := st.GetRecent(ctx, "session-a", 10)
	require.NoError(t, err)
	require.Len(t, mysqlRecent, 1, "a flagged memory is demoted, never dropped")

	postgresRecent, err := st.GetRecent(ctx, "session-b", 10)
	require.NoError(t, err)
	require.Len(t, postgresRecent, 1, "the memory that conflicted is not dropped either")

	// The MySQL memory is the flagged one, read back through the store rather than out
	// of the table, so this test cannot pass on a marker the read path does not carry.
	require.Equal(t, store.ConflictStatusSupersededCandidate, mysqlRecent[0].ConflictStatus)

	// The two fixtures share type, importance, embedding and timestamp, so the four
	// factors agree exactly and the penalty is the only thing that can move one Total
	// away from the other's.
	weights := scorer.GetWeights(
		cfg.WeightSemanticSimilarity,
		cfg.WeightRecency,
		cfg.WeightImportance,
		cfg.WeightTaskAlignment,
	)
	weights.ConflictScorePenalty = cfg.ConflictScorePenalty

	sc := scorer.NewScorer(weights, classifier.Generic, 1.0, time.Now())
	scored := sc.Score(ctx, mysqlRecent[0].Embedding, []store.MemoryEntry{mysqlRecent[0], postgresRecent[0]})
	require.Len(t, scored, 2)

	byID := make(map[string]scorer.ScoredMemory, len(scored))
	for _, s := range scored {
		byID[s.ID] = s
	}

	flagged, plain := byID[mysql.ID], byID[postgres.ID]

	t.Logf("superseded candidate (%q): Total=%.6f (S=%.4f R=%.4f I=%.4f T=%.4f)",
		decidedMySQL, flagged.Total, flagged.ScoreS, flagged.ScoreR, flagged.ScoreI, flagged.ScoreT)
	t.Logf("conflicting memory   (%q): Total=%.6f (S=%.4f R=%.4f I=%.4f T=%.4f)",
		decidedPostgres, plain.Total, plain.ScoreS, plain.ScoreR, plain.ScoreI, plain.ScoreT)
	t.Logf("penalty=%v, so %.6f x %v = %.6f",
		weights.ConflictScorePenalty, plain.Total, weights.ConflictScorePenalty,
		plain.Total*weights.ConflictScorePenalty)

	// The breakdown stays the honest per-factor picture: only Total is penalised, so a
	// trace can show S/R/I/T unchanged next to a lower total and the reason for it.
	assert.InDelta(t, plain.ScoreS, flagged.ScoreS, 1e-9)
	assert.InDelta(t, plain.ScoreR, flagged.ScoreR, 1e-9)
	assert.InDelta(t, plain.ScoreI, flagged.ScoreI, 1e-9)
	assert.InDelta(t, plain.ScoreT, flagged.ScoreT, 1e-9)

	require.Greater(t, plain.Total, 0.0, "the unpenalised total has to be a real number to compare against")
	assert.InDelta(t, flagged.Total, plain.Total*cfg.ConflictScorePenalty, 0.001,
		"the decision stored first must score exactly the penalty times the decision that replaced it")
	assert.Less(t, flagged.Total, plain.Total, "the flag demotes the older memory rather than removing it")
}
