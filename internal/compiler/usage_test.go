package compiler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The metering seam's own tests: what RecordUsage does with a sink, what it
// does without one, and the one property the request path depends on -- that
// handing over an event never waits for the sink.
//
// These are unit tests with no build tag, and they use a recording double
// rather than a database, because what is under test here is the wiring and not
// the write: the INSERT is internal/metering's, and its own integration test
// (meter_test.go, build tag integration) is what proves a row reaches Postgres.
// The division matters, because a seam that reported success without a sink
// would be indistinguishable from a working one in an end-to-end test that had
// no sink installed.
//
// The package-level wiring is process-wide, so these tests install a sink,
// assert, and let t.Cleanup clear it. None of them calls t.Parallel().

// recordingUsageSink is a UsageSink that remembers every event it was handed.
type recordingUsageSink struct {
	events []UsageEvent
}

// Record implements UsageSink.
func (s *recordingUsageSink) Record(event UsageEvent) {
	s.events = append(s.events, event)
}

// usageEvent is a fully-populated event, distinct enough that a field swapped
// for another in the sink's translation would show up as a failing assertion
// rather than as two equal zeroes.
func usageEvent() UsageEvent {
	return UsageEvent{
		TenantID:       "6f9c1e5a-3b7d-4c21-9a5e-8d0f4b2c7e31",
		AgentID:        "agent-metering",
		SessionID:      "sess-metering",
		RawTokens:      4000,
		CompiledTokens: 1000,
		ReductionPct:   75,
		Model:          "llama3.1:8b",
		CreatedAt:      time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
	}
}

// installUsageSink installs sink for one test and clears the wiring afterwards,
// so a test that runs later in the same binary cannot inherit it.
func installUsageSink(t *testing.T, sink UsageSink) {
	t.Helper()

	SetUsageSink(sink)
	t.Cleanup(func() { SetUsageSink(nil) })
}

// TestRecordUsageHandsTheEventToTheSink is the plain contract: every field of
// the event the call site built arrives at the sink unaltered, CreatedAt
// included -- the sink is where a tenant id gets filled in, and nothing else
// may be rewritten on the way.
func TestRecordUsageHandsTheEventToTheSink(t *testing.T) {
	sink := &recordingUsageSink{}
	installUsageSink(t, sink)

	want := usageEvent()
	RecordUsage(want)

	if len(sink.events) != 1 {
		t.Fatalf("sink saw %d events, want 1", len(sink.events))
	}

	if got := sink.events[0]; got != want {
		t.Errorf("sink saw %+v, want %+v", got, want)
	}
}

// TestRecordUsageWithoutASinkDoesNothing is the standalone node: no sink
// installed is the default this file must not change, and the call is a no-op.
func TestRecordUsageWithoutASinkDoesNothing(t *testing.T) {
	SetUsageSink(nil)
	t.Cleanup(func() { SetUsageSink(nil) })

	if usageSink() != nil {
		t.Fatal("usageSink() is non-nil after SetUsageSink(nil)")
	}

	// Nothing to observe beyond "this does not panic" -- and that is the
	// assertion: RecordUsage on the v1 request path may not dereference
	// anything when a node does not meter.
	RecordUsage(usageEvent())
}

// TestSetUsageSinkNilClearsTheWiring covers the other half of the no-op: a sink
// that was installed and then cleared must stop receiving events. Without this,
// "a standalone node does not meter" would only be true for a process that
// never had a sink.
func TestSetUsageSinkNilClearsTheWiring(t *testing.T) {
	sink := &recordingUsageSink{}
	installUsageSink(t, sink)

	RecordUsage(usageEvent())
	if len(sink.events) != 1 {
		t.Fatalf("sink saw %d events while installed, want 1", len(sink.events))
	}

	SetUsageSink(nil)
	RecordUsage(usageEvent())

	if len(sink.events) != 1 {
		t.Errorf("sink saw %d events after being cleared, want the 1 it already had", len(sink.events))
	}
}

// TestRecordUsageCallsTheSinkBeforeReturning pins where the asynchrony lives.
//
// RecordUsage is synchronous on purpose: it calls the sink and returns when the
// sink returns. The goroutine and the timeout are the sink implementation's
// (metering.Meter.Record, which is what cmd/synapse installs), not this seam's,
// for two reasons. A goroutine spawned here would be one unaccounted goroutine
// per compilation -- this function has no way to bound or observe it -- and the
// event it captured would be a copy the caller's struct could race, since both
// v1 call sites keep mutating the value they just passed.
//
// So the assertion is the opposite of "does not wait": after RecordUsage
// returns, the sink has already been given the event. A future edit that moved
// the call into a goroutine here would fail this test, which is the point --
// that edit would move the write outside anything that measures or bounds it.
func TestRecordUsageCallsTheSinkBeforeReturning(t *testing.T) {
	sink := &recordingUsageSink{}
	installUsageSink(t, sink)

	RecordUsage(usageEvent())

	if len(sink.events) != 1 {
		t.Fatalf("sink saw %d events after RecordUsage returned, want 1", len(sink.events))
	}
}

// TestSavingsUSDUsesTheBlendedRate pins the arithmetic the savings log line and
// the metering row's own figure are read against: 3,000 tokens saved is
// 3 * $0.015, and a compile that saved nothing is $0.
func TestSavingsUSDUsesTheBlendedRate(t *testing.T) {
	cases := []struct {
		name                      string
		rawTokens, compiledTokens int
		want                      float64
	}{
		{"three thousand tokens saved", 4000, 1000, 0.045},
		{"nothing saved", 1000, 1000, 0},
		{"empty pool", 0, 0, 0},
		{"smaller than a thousandth", 1, 0, 0.000015},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// InDelta rather than ==: the rate is a decimal that is not exactly
			// representable in binary, so an exact-equality assertion here would
			// be pinning a rounding artifact rather than the rate.
			assert.InDelta(t, tc.want, SavingsUSD(tc.rawTokens, tc.compiledTokens), 1e-12)
		})
	}
}
