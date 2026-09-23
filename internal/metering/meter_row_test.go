//go:build integration

// The metering row's own integration tests: what a single row has to contain,
// what the migration guarantees about the table, and what a bad event does.
//
// Split from meter_test.go on the line count, not on the taste: that file owns
// the definition of done -- ten events, ten rows -- and this one owns the row
// itself. Both are integration-tagged and share that file's helpers (meterPool,
// newTenantID, settle, countForTenants), because they are testing one table
// through one type.
package metering

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordStoresEveryColumn reads one row back and asserts each column
// individually, which is the part a fake pool cannot check: the uuid, float8
// and timestamptz conversions, and the nullable model.
//
// It also pins the one field a reader must not be able to learn from anywhere
// else: session_id is stored. That is the deliberate half of this phase's rule
// -- the session is in the database, and it is in no log line and no API
// response -- and asserting it here stops a later "we never return it, so why
// store it" cleanup from silently removing the billing column.
func TestRecordStoresEveryColumn(t *testing.T) {
	pool := meterPool(t)
	meter := NewMeter(pool)

	tenantID := newTenantID()
	created := time.Now().UTC().Truncate(time.Microsecond)
	event := UsageEvent{
		TenantID:       tenantID,
		AgentID:        "agent-metering-columns",
		SessionID:      "sess-metering-columns",
		RawTokens:      4000,
		CompiledTokens: 1000,
		ReductionPct:   75,
		Model:          "llama3.1:8b",
		CreatedAt:      created,
	}
	meter.Record(event)

	// The second event deliberately names no model, so the NULL mapping is
	// asserted against a row rather than only against the helper.
	unlabelledTenantID := newTenantID()
	meter.Record(UsageEvent{
		TenantID:       unlabelledTenantID,
		AgentID:        "agent-metering-unlabelled",
		SessionID:      "sess-metering-unlabelled",
		RawTokens:      10,
		CompiledTokens: 5,
		ReductionPct:   50,
		Model:          "",
		CreatedAt:      time.Now(),
	})

	settle(t, "the metered rows", func() int {
		return countForTenants(t, pool, []string{tenantID, unlabelledTenantID})
	}, 2)

	var row struct {
		TenantID       string
		AgentID        string
		SessionID      string
		RawTokens      int
		CompiledTokens int
		ReductionPct   float64
		Model          *string
		CreatedAt      time.Time
	}

	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT tenant_id::text, agent_id, session_id, raw_tokens, compiled_tokens,
		       reduction_pct, model, created_at
		  FROM `+usageEventsTable+`
		 WHERE tenant_id = $1`, tenantID).Scan(
		&row.TenantID, &row.AgentID, &row.SessionID, &row.RawTokens,
		&row.CompiledTokens, &row.ReductionPct, &row.Model, &row.CreatedAt))

	assert.Equal(t, tenantID, row.TenantID)
	assert.Equal(t, event.AgentID, row.AgentID)
	assert.Equal(t, event.SessionID, row.SessionID)
	assert.Equal(t, event.RawTokens, row.RawTokens)
	assert.Equal(t, event.CompiledTokens, row.CompiledTokens)
	assert.Equal(t, event.ReductionPct, row.ReductionPct)
	require.NotNil(t, row.Model, "a named model must not be stored as NULL")
	assert.Equal(t, event.Model, *row.Model)

	// The stamp is the caller's, not the connection's: within a microsecond of
	// what Record was handed, which no now() default could be.
	assert.WithinDuration(t, created, row.CreatedAt, time.Microsecond)

	var unlabelledModel *string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT model FROM `+usageEventsTable+` WHERE tenant_id = $1`, unlabelledTenantID).Scan(&unlabelledModel))
	assert.Nil(t, unlabelledModel, "an unnamed model is stored as NULL, not as an empty string")
}

// TestUsageEventsTableHasTheReportIndex pins the migration this phase added.
// The claim is not decoration: the compliance report's totals query is a
// (tenant_id, created_at) window scan, and PROGRESS.md's Phase 20 finding 6
// records that adding the index belonged to the phase that started writing rows.
// Asserting it here means the migration and the write path cannot drift.
func TestUsageEventsTableHasTheReportIndex(t *testing.T) {
	pool := meterPool(t)

	var indexDef string
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT indexdef FROM pg_indexes
		 WHERE schemaname = 'synapse_global' AND tablename = 'usage_events' AND indexname = $1`,
		usageEventsIndex).Scan(&indexDef))

	assert.Contains(t, indexDef, "(tenant_id, created_at)")
}

// TestRecordDropsAMalformedTenantID covers the guard that keeps a bad tenant id
// from turning into one failed INSERT per compilation forever: usage_events
// .tenant_id is uuid, so a non-uuid value could never be stored, and the check
// in Record is what turns that into a single warning instead.
//
// What is asserted is the shape a live request path needs -- no panic, no
// goroutine left holding anything -- plus the negative control that follows it:
// a well-formed event still lands. A row count alone could not distinguish "the
// bad event was refused" from "the bad event was never attempted", and that
// distinction is not available from the database, because the value the
// database would reject is exactly the value no query can name.
func TestRecordDropsAMalformedTenantID(t *testing.T) {
	pool := meterPool(t)
	meter := NewMeter(pool)

	require.Error(t, func() error {
		_, err := canonicalID("not-a-uuid")

		return err
	}())

	malformed := newEvent("not-a-uuid", 0)
	meter.Record(malformed)

	// The negative control: the same meter still writes, so the malformed event
	// did not wedge its pool or its goroutine.
	tenantID := newTenantID()
	meter.Record(newEvent(tenantID, 1))

	settle(t, "the one well-formed row", func() int {
		return countForTenants(t, pool, []string{tenantID})
	}, 1)
}
