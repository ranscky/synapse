// Tests for the write-path half of conflict detection: which memories a write
// compares against, how many times it asks, and what the block costs. The end-to-end
// behaviour -- both rows marked, the flagged one demoted -- is in conflict_test.go,
// in package store_test, because the real detector and the scorer both import this
// package and a test file in this one cannot import them.
package store

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingDetector records how a write consults a detector, which is the shape this
// phase's "fetch once, do not re-fetch per write" requirement is actually about: a
// detector is handed a candidate slice, so a write that re-queried per candidate
// would show up here as more calls or as smaller slices.
type countingDetector struct {
	calls      int
	candidates []int
}

func (d *countingDetector) Detect(candidate MemoryEntry, existing []MemoryEntry) (bool, string) {
	d.calls++
	d.candidates = append(d.candidates, len(existing))
	return false, ""
}

// candidateFixture writes conflictCandidateLimit+5 org-scoped memories plus three
// memories that must never be candidates, and returns the org-scoped ids oldest
// first together with the ids no write may compare against.
//
// The three excluded memories are written with the newest timestamps in the tenant,
// so a query that lost its predicate would put them at the front of the slice rather
// than off the end of it: an assertion below can then fail for the right reason
// instead of passing because a row was simply too old to be reached.
func candidateFixture(t *testing.T, st *PGStore) (orgIDs, excludedIDs []string) {
	t.Helper()

	ctx := context.Background()
	base := time.Now().UTC()

	const written = conflictCandidateLimit + 5
	orgIDs = make([]string, written)
	for i := range orgIDs {
		orgIDs[i] = uuid.NewString()
		entry := testEntry(orgIDs[i], fmt.Sprintf("session-%d", i), embeddingAt(i%3, 1))
		entry.Timestamp = base.Add(time.Duration(i) * time.Second)
		require.NoError(t, st.Write(ctx, entry))
	}

	newest := base.Add(time.Hour)

	private := testEntry(uuid.NewString(), "session-private", embeddingAt(0, 1))
	private.Visibility = VisibilityPrivate
	private.Timestamp = newest
	require.NoError(t, st.Write(ctx, private))

	teamed := testEntry(uuid.NewString(), "session-team", embeddingAt(0, 1))
	teamed.Visibility = VisibilityTeam
	teamed.TeamID = "team-a"
	teamed.Timestamp = newest
	require.NoError(t, st.Write(ctx, teamed))

	superseded := testEntry(uuid.NewString(), "session-superseded", embeddingAt(0, 1))
	superseded.Timestamp = newest
	require.NoError(t, st.Write(ctx, superseded))
	require.NoError(t, st.MarkSuperseded(ctx, superseded.ID, orgIDs[written-1]))

	return orgIDs, []string{private.ID, teamed.ID, superseded.ID}
}

// TestConflictCandidateFetch pins what the block reads and what that costs: the
// tenant's newest org-scoped, not-yet-superseded memories, capped at the limit, for a
// price inside the same budget the store itself logs against.
func TestConflictCandidateFetch(t *testing.T) {
	st := testPGStore(t)
	ctx := context.Background()
	orgIDs, excludedIDs := candidateFixture(t, st)

	candidates, err := st.conflictCandidates(ctx)
	require.NoError(t, err)

	t.Run("it is the newest org-scoped memories, capped at the limit", func(t *testing.T) {
		require.Len(t, candidates, conflictCandidateLimit,
			"a tenant holding more memories than the limit must still hand over exactly the limit")
		assert.Equal(t, orgIDs[conflictCandidateLimit+4], candidates[0].ID, "newest first")

		for _, c := range candidates {
			assert.Equal(t, VisibilityOrg, c.Visibility, "only org-scoped memories are compared")
			assert.Empty(t, c.SupersededBy)
		}
	})

	t.Run("a private, team-scoped or superseded memory is never a candidate", func(t *testing.T) {
		ids := idsOf(candidates)
		for _, excludedID := range excludedIDs {
			assert.NotContains(t, ids, excludedID,
				"a memory outside the org scope, or already superseded, must never be compared against")
		}
		assert.NotContains(t, ids, orgIDs[0], "the oldest memory falls outside the limit")
	})

	t.Run("the fetch stays inside the detection budget", func(t *testing.T) {
		// A fetch is a plain SELECT: it pays no commit, so unlike a write its cost is
		// stable and a machine having a slow fsync cannot be mistaken here for a slow
		// query. The median is the estimator and the slowest sample is logged, because a
		// single outlier is a scheduling or connection hiccup rather than the query.
		const runs = 20
		spent := make([]time.Duration, runs)
		for i := range spent {
			start := time.Now()
			_, err := st.conflictCandidates(ctx)
			require.NoError(t, err)
			spent[i] = time.Since(start)
		}
		sort.Slice(spent, func(i, j int) bool { return spent[i] < spent[j] })

		t.Logf("conflictCandidates over %d memories: median %s, worst of %d %s (budget %s)",
			len(candidates), spent[runs/2], runs, spent[runs-1], ConflictDetectionBudget)

		assert.Less(t, spent[runs/2], ConflictDetectionBudget,
			"the candidate fetch must stay inside the detection budget")
	})
}

// TestConflictWriteConsultsTheDetectorOncePerWrite is the "cache this slice, do not
// re-fetch per write" requirement: one write runs one comparison, against one slice
// of at most conflictCandidateLimit memories.
func TestConflictWriteConsultsTheDetectorOncePerWrite(t *testing.T) {
	st := testPGStore(t)
	ctx := context.Background()
	candidateFixture(t, st)

	detector := &countingDetector{}
	st.SetConflictDetector(detector)

	require.NoError(t, st.Write(ctx, testEntry(uuid.NewString(), "session-write", embeddingAt(0, 1))))

	require.Equal(t, 1, detector.calls,
		"one write must compare the new memory against the candidate slice exactly once, not once per candidate")
	require.Len(t, detector.candidates, 1)
	assert.Equal(t, conflictCandidateLimit, detector.candidates[0],
		"and it is handed the whole slice, bounded by the limit")
}
