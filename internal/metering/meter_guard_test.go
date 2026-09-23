// The metering package's own guards, with no build tag: the two properties a
// caller on the compilation path depends on -- that an unmetered or unreachable
// meter is inert, and that Record never puts a write in front of a caller.
//
// These run without a database because they are about what happens *before* a
// database is reached. The write itself, and the row it produces, are
// meter_test.go's subject (build tag integration).
package metering

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordWithNoPoolIsInert covers the states a node can be in without a
// database: no meter at all, and a meter with no pool. Both are ordinary -- a
// standalone node holds neither, and cmd/synapse holds a meter only when it
// could open a pool -- so Record has to be a no-op rather than a panic on a
// live request path.
func TestRecordWithNoPoolIsInert(t *testing.T) {
	event := UsageEvent{TenantID: uuid.NewString(), RawTokens: 10, CompiledTokens: 5}

	NewMeter(nil).Record(event)

	var meter *Meter
	meter.Record(event)
}

// TestRecordReturnsBeforeTheWriteCompletes is the request path's requirement:
// the INSERT must not be in front of a compilation.
//
// The address is TEST-NET-1 (RFC 5737), which is guaranteed not to be a
// database, so the connection attempt is still outstanding when the assertion
// runs -- a synchronous write would therefore block here for its full
// three-second timeout, not for a millisecond. That is what makes this evidence
// rather than a timing coincidence, and the bound is deliberately loose (one
// second against a three-second timeout) so a loaded machine cannot make it
// flaky.
func TestRecordReturnsBeforeTheWriteCompletes(t *testing.T) {
	const unreachable = "postgres://synapse:synapse@192.0.2.1:5432/synapse?sslmode=disable"

	pool, err := pgxpool.New(context.Background(), unreachable)
	require.NoError(t, err, "opening a pool is lazy, so an unreachable address must not fail here")
	t.Cleanup(pool.Close)

	meter := NewMeter(pool)

	start := time.Now()
	meter.Record(UsageEvent{TenantID: uuid.NewString(), RawTokens: 10, CompiledTokens: 5})
	elapsed := time.Since(start)

	assert.Less(t, elapsed, time.Second,
		"Record blocked for %s: metering must never slow a compilation", elapsed)

	// The write is still outstanding when this assertion runs -- that is the
	// whole point -- and it fails when its own ceiling expires. Waiting for that
	// here, rather than letting t.Cleanup close the pool underneath it, costs
	// the timeout once and buys a test whose output is its own: a goroutine
	// failing after this test returned would print "metering: record failed"
	// inside whichever test happened to run next, which reads like that test's
	// failure.
	time.Sleep(recordTimeout + 200*time.Millisecond)
}

// TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse pins the guard that keeps a
// non-uuid tenant id from becoming a failed INSERT per compilation. The
// uppercase input is deliberate: it is the same tenant, and it must come back
// in the one canonical form Postgres stores, exactly as internal/ledger
// canonicalizes the ids it writes.
func TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse(t *testing.T) {
	got, err := canonicalID("6F9C1E5A-3B7D-4C21-9A5E-8D0F4B2C7E31")
	require.NoError(t, err)
	assert.Equal(t, "6f9c1e5a-3b7d-4c21-9a5e-8d0f4b2c7e31", got)

	for _, bad := range []string{"", "not-a-uuid", "1234", "6f9c1e5a-3b7d-4c21-9a5e-8d0f4b2c7e3"} {
		t.Run("refuses "+bad, func(t *testing.T) {
			_, err := canonicalID(bad)

			require.Error(t, err)

			// The value is deliberately not echoed -- and an empty needle is
			// contained in every string, so that case asserts only the error.
			if bad != "" {
				assert.NotContains(t, err.Error(), bad, "the refusing error must not echo the value it refused")
			}
		})
	}
}

// TestNullableTextMapsNothingToNull pins the one mapping between the event and
// the column: usage_events.model is nullable, and an operator who named no
// model recorded no model -- not an empty one.
func TestNullableTextMapsNothingToNull(t *testing.T) {
	assert.Nil(t, nullableText(""))
	assert.Equal(t, "llama3.1:8b", nullableText("llama3.1:8b"))
}
