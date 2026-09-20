//go:build integration

// SECURITY BOUNDARY TEST: cross-tenant isolation.
//
// This is the regression test for the guarantee the whole v2 control plane
// rests on: a tenant's memories live in a schema that belongs to that tenant
// alone, so tenant B cannot reach tenant A's memories through any query. It is
// not a unit test of Search's SQL -- it is the test of the isolation claim
// itself, and it is deliberately built so that it cannot pass vacuously:
//
//   - tenant-alpha's own store must first return exactly the five memories this
//     run wrote -- identified by the team id that is unique to this run, since a
//     fixed slug accumulates rows across runs -- through the very query the
//     isolation assertions use, so an empty tenant-beta result cannot be
//     explained by a broken fixture, a write that never landed, or a query that
//     reaches no row at all.
//   - the query vector is byte-identical to one of tenant-alpha's stored
//     embeddings (L2 distance 0), so if any read path could reach that row,
//     this is the vector that would find it.
//   - the last assertion does not go through Search at all: it counts rows in
//     tenant-beta's own table, proving the table is genuinely empty rather than
//     merely filtered out of the result.
//
// It has been verified to fail when isolation is broken: temporarily pointing
// PGStore.Search at tenant-alpha's table instead of the store's own schema makes
// every tenant-beta assertion below fail. See PROGRESS.md, Phase 6.
//
// Build-tagged integration because a real Postgres is required and
// testcontainers-go is not a dependency of this module, so the test uses the
// database the Phase 4 compose stack publishes (deploy/docker-compose.yml):
//
//	go test ./internal/store/... -run TestCrossTenantIsolation -v -tags integration
//
// The database is a precondition, not an option: an unreachable one fails this
// test instead of skipping it, because a skipped security test reports success
// without having checked anything.
package store

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// isolationTenantAlpha is the tenant whose memories this test writes.
	isolationTenantAlpha = "tenant-alpha"
	// isolationTenantBeta is the tenant that must be unable to reach them.
	isolationTenantBeta = "tenant-beta"

	// isolationReaderAgent is the agent these searches are made as. The claim
	// under test is about schemas rather than scopes, but the scope is spelled
	// out anyway: Phase 10 made a search's result depend on the agent, team, and
	// session it is made with, so a reader has to be named for the query to mean
	// anything at all.
	isolationReaderAgent = "isolation-agent"

	// isolationDefaultDSN is the database the Phase 4 compose stack publishes on
	// host loopback with the credentials in deploy/docker-compose.yml. It is the
	// fallback when SYNAPSE_TEST_DB_DSN is unset, so the command above works
	// as-is against `cd deploy && docker compose up -d db`.
	isolationDefaultDSN = "postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable"

	// isolationEntryCount is how many memories are written to tenant-alpha.
	isolationEntryCount = 5
	// isolationControlTopK bounds the control query. Deliberately generous:
	// tenant-alpha is a fixed slug and accumulates rows across runs, and the
	// control query has to be able to see this run's five whatever is already
	// there.
	isolationControlTopK = 100
	// isolationRandomQueries is how many arbitrary query vectors are tried
	// against tenant-beta on top of the identical-embedding query.
	isolationRandomQueries = 10
	// isolationRandomSeed pins the generator behind those vectors: the values
	// are arbitrary, but a failure has to be reproducible.
	isolationRandomSeed = 20260920

	// isolationPoolTimeout bounds opening the pool and its confirming ping.
	isolationPoolTimeout = 30 * time.Second
)

// isolationPool returns a pool for the test database.
//
// SYNAPSE_TEST_DB_DSN wins when set -- the same variable internal/tenant and the
// untagged PGStore tests use, so this test can be pointed at any instance --
// otherwise the Phase 4 compose database is used. Unlike testPGPool, an
// unusable database is fatal rather than a skip: under the integration tag the
// database is a precondition of the claim being tested.
func isolationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(testEnvDatabaseDSN)
	if dsn == "" {
		dsn = isolationDefaultDSN
	}

	ctx, cancel := context.WithTimeout(context.Background(), isolationPoolTimeout)
	defer cancel()

	pool, err := OpenPGPool(ctx, dsn)
	if err != nil {
		t.Fatalf("cross-tenant isolation needs a reachable postgres (set %s to override the compose default): %v", testEnvDatabaseDSN, err)
	}

	t.Cleanup(pool.Close)

	return pool
}

// isolationRandomEmbedding returns a 384-dim vector with components in [-1, 1).
//
// The values carry no meaning: any valid 384-dim vector would do, because a
// store querying the wrong schema returns rows for all of them. They are
// deterministic so that a failure can be replayed exactly.
func isolationRandomEmbedding(rng *rand.Rand) []float32 {
	vec := make([]float32, EmbeddingDimensions)
	for i := range vec {
		vec[i] = rng.Float32()*2 - 1
	}
	return vec
}

// TestCrossTenantIsolation is the security boundary test described at the top of
// this file: nothing tenant-alpha writes may be reachable from tenant-beta's
// store.
func TestCrossTenantIsolation(t *testing.T) {
	pool := isolationPool(t)
	ctx := context.Background()

	// Both stores share one pool on purpose. Sharing it removes pool and
	// connection identity as variables, leaving the schema each store resolves
	// as the only thing that separates them -- which is exactly the property
	// under test.
	alpha, err := NewPGStore(pool, isolationTenantAlpha)
	require.NoError(t, err, "tenant-alpha's store must be constructible")

	beta, err := NewPGStore(pool, isolationTenantBeta)
	require.NoError(t, err, "tenant-beta's store must be constructible")

	// Asserted directly so that a failure says which invariant broke. A collapse
	// of the slug -> schema mapping fails the assertions below anyway.
	require.NotEqual(t,
		alpha.schemaName(), beta.schemaName(),
		"two tenants must never resolve to one schema")

	// Unique per run, and the reason the counts below stay exact: tenant-alpha is
	// a fixed slug, so memories accumulate in that schema across runs.
	//
	// This run's memories are written team-scoped under a team id of its own.
	// Before Phase 10 the session id alone was enough to isolate them, but an
	// org-scoped memory is now reachable from any session, so a session-scoped
	// fixture would no longer produce an exact count -- while a team id still
	// does, because no earlier run used this one. Every leak assertion below is
	// then made with the same team id and the same agent, which is what keeps
	// them non-vacuous: a broken schema filter would surface these five rows for
	// the control query and for tenant-beta alike.
	sessionID := uuid.NewString()
	teamID := "isolation-" + uuid.NewString()

	// Entry i is the unit vector on axis i -- the same hardcoded, known
	// embeddings the Phase 5 test uses -- written through the store's own Write
	// path rather than inserted behind it.
	base := time.Now().UTC()
	ids := make([]string, isolationEntryCount)
	for i := range ids {
		ids[i] = uuid.NewString()
		entry := testEntry(ids[i], sessionID, embeddingAt(i, 1))
		entry.AgentID = isolationReaderAgent
		entry.Visibility = VisibilityTeam
		entry.TeamID = teamID
		entry.Timestamp = base.Add(time.Duration(i) * time.Second)
		require.NoError(t, alpha.Write(ctx, entry), "writing tenant-alpha entry %d", i)
	}

	// Byte-identical to tenant-alpha entry 3: L2 distance 0 to a row that is
	// known to exist and to be visible, so nothing can be nearer to it.
	query := embeddingAt(3, 1)

	t.Run("control: tenant-alpha reaches its own memories", func(t *testing.T) {
		got, err := alpha.Search(ctx, query, isolationReaderAgent, teamID, sessionID, isolationControlTopK)
		require.NoError(t, err)

		// Counted by scope rather than by result length. tenant-alpha is a fixed
		// slug with a history: earlier runs -- and, before Phase 10 made a search
		// scope-aware, earlier writes -- left org-scoped rows in it that any
		// reader can still reach, so the raw result is longer than this run's
		// five and its ordering is full of equidistant rows. The team id is
		// unique to this run, so the rows that carry it are exactly the rows
		// written below, and requiring all five of them is what keeps the leak
		// assertions meaningful: they can only fail if this query reaches rows,
		// which it demonstrably does.
		gotIDs := make(map[string]bool, len(got))
		for _, e := range got {
			if e.TeamID == teamID {
				gotIDs[e.ID] = true
			}
		}

		require.Len(t, gotIDs, isolationEntryCount,
			"tenant-alpha must see exactly the memories this run wrote; without this the assertions below would prove nothing")
		for _, id := range ids {
			assert.True(t, gotIDs[id], "tenant-alpha must reach the memory this run wrote (%s)", id)
		}
	})

	t.Run("tenant-beta cannot reach tenant-alpha's memories", func(t *testing.T) {
		got, err := beta.Search(ctx, query, isolationReaderAgent, teamID, sessionID, isolationEntryCount*2)
		require.NoError(t, err)
		// Lengths are compared as values rather than with assert.Len/Empty so a
		// leak reports "expected 0, actual 5" instead of dumping every leaked
		// entry and its 384-dim embedding into the failure output.
		assert.Equal(t, 0, len(got),
			"same session id, same embedding as a tenant-alpha memory: tenant-beta must still find nothing")
	})

	t.Run("tenant-beta cannot reach them tenant-wide either", func(t *testing.T) {
		got, err := beta.Search(ctx, query, isolationReaderAgent, teamID, "", isolationEntryCount*2)
		require.NoError(t, err)
		assert.Equal(t, 0, len(got),
			"an empty session widens the search to the whole tenant, which is still only tenant-beta")
	})

	t.Run("random queries return nothing from tenant-beta", func(t *testing.T) {
		rng := rand.New(rand.NewSource(isolationRandomSeed))
		for i := 0; i < isolationRandomQueries; i++ {
			t.Run(fmt.Sprintf("query %d", i+1), func(t *testing.T) {
				q := isolationRandomEmbedding(rng)

				inSession, err := beta.Search(ctx, q, isolationReaderAgent, teamID, sessionID, isolationEntryCount*2)
				require.NoError(t, err)
				assert.Equal(t, 0, len(inSession), "session-scoped query reached tenant-beta rows")

				tenantWide, err := beta.Search(ctx, q, isolationReaderAgent, teamID, "", isolationEntryCount*2)
				require.NoError(t, err)
				assert.Equal(t, 0, len(tenantWide), "tenant-wide query reached tenant-beta rows")
			})
		}
	})

	t.Run("tenant-beta's table is empty at the storage layer", func(t *testing.T) {
		var rows int
		table := pgx.Identifier{beta.schemaName(), "memories"}.Sanitize()
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&rows))
		assert.Equal(t, 0, rows,
			"the empty search results above must come from an empty table, not from a filter that hides rows")
	})
}
