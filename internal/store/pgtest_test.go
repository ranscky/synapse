// Shared fixtures for the Postgres-backed store tests.
//
// These live in their own file for the same reason the tests themselves are
// split: pgstore_test.go describes the store's behaviour -- its search, its
// reads, its writes -- and the visibility tests in visibility_test.go describe
// who may read what, and neither file should be mostly scaffolding. Everything
// here is used by both.
package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// testEnvDatabaseDSN is the environment variable that points the Postgres-backed
// tests at a real instance, matching internal/tenant's convention:
//
//	SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' go test ./internal/store/...
//
// The Postgres tests skip when it is unset, so the unit suite -- and CI, which
// runs `go test ./...` with no database at all -- stays green anywhere.
const testEnvDatabaseDSN = "SYNAPSE_TEST_DB_DSN"

// testPGPool returns a pool for the database named by testEnvDatabaseDSN, or
// skips the test when it is not configured.
func testPGPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv(testEnvDatabaseDSN)
	if dsn == "" {
		t.Skipf("set %s to run the Postgres-backed tests", testEnvDatabaseDSN)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := OpenPGPool(ctx, dsn)
	require.NoError(t, err, "could not open a pool for %s", testEnvDatabaseDSN)

	t.Cleanup(pool.Close)

	return pool
}

// testPGStore returns a PGStore over a tenant slug no earlier run used, so
// repeated runs cannot see each other's rows. Nothing is deleted afterwards: the
// schema is cheap, and every assertion is scoped to this run's own slug.
func testPGStore(t *testing.T) *PGStore {
	t.Helper()

	st, err := NewPGStore(testPGPool(t), fmt.Sprintf("pgs%d", time.Now().UnixNano()))
	require.NoError(t, err)

	t.Cleanup(func() { _ = st.Close() })

	return st
}

// embeddingAt returns a 384-dim vector that is zero everywhere except index i,
// which is set to v.
//
// The embeddings in these tests are hardcoded through this helper rather than
// written out as literal 384-element slices: the one number that matters per
// entry is its axis, and a wall of zeros would bury it.
func embeddingAt(i int, v float32) []float32 {
	vec := make([]float32, EmbeddingDimensions)
	vec[i] = v
	return vec
}

// testReaderAgent is the agent the assertions in TestPGStore read as.
//
// Every memory those subtests write is org-scoped (a blank Visibility is the
// column's own default, 'org'), so the agent a search is made as cannot change
// which rows come back -- naming one anyway keeps the calls honest about who is
// asking, which is the parameter Phase 10 added. The visibility assertions
// themselves live in TestVisibilityScopes, where the agent does decide.
const testReaderAgent = "test-reader-agent"

// testEntry builds a minimal valid entry for sessionID.
func testEntry(id, sessionID string, embedding []float32) MemoryEntry {
	return MemoryEntry{
		ID:         id,
		SessionID:  sessionID,
		Content:    "memory " + id,
		MemoryType: "fact",
		Importance: 0.5,
		Timestamp:  time.Now().UTC(),
		Embedding:  embedding,
	}
}
