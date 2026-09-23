// The compilation path's half of v2 usage metering: the point where a finished
// compilation's token accounting leaves the request path.
//
// It is ledger.go's twin, deliberately, and shares its reasoning. The
// dependency arrives as an interface declared here rather than as a parameter
// of Compile, because Compile is a v1 free function with twelve positional
// parameters and two frozen callers (internal/proxy's live proxied traffic and
// internal/api's compile path), so there is no Compiler struct to carry a field
// and no call site free to pass another argument. The sink is installed exactly
// once per process, before any router serves, and read on every compilation --
// the same deliberate exception to this project's "no global state" style that
// ledger.go documents.
//
// What it deliberately does not do is what ledger.go does: append from inside
// Compile. The numbers this event exists to carry do not exist at that point.
// Both callers fill in TokensUsed, ReductionPct, and the candidate-pool
// baseline after Compile returns (see trace.NewTraceManifest's doc comment, and
// ledger.go's own note that the ledgered trace therefore carries zeroes), so a
// sink called from inside Compile could only ever write raw_tokens 0 /
// compiled_tokens 0 / reduction_pct 0 -- and the compliance report's average
// context reduction would then be a stored falsehood rather than a number that
// is merely missing. Phase 26 is the "deliberate later-phase decision" that
// note predicted, and RecordUsage is the function those two call sites use.
//
// Nothing here knows about Postgres, tenants, or credentials: the sink is
// implemented in cmd/synapse, the only place that can see both this package and
// internal/metering.
package compiler

import (
	"sync/atomic"
	"time"
)

// usdPerThousandTokens is the blended rate the savings summary is quoted at:
// $0.015 per 1,000 tokens, the figure this phase's brief specifies. It is a
// display rate and not a billing table -- nothing in this process invoices
// anyone -- so it lives next to the only function that computes it, and is
// quoted by the log line and by any future summary rather than written out
// twice.
const usdPerThousandTokens = 0.015

// UsageEvent is one finished compilation's token accounting, as the metering
// table stores it.
//
// A standalone node never builds one: with no sink installed, RecordUsage
// returns on its first pointer load. TenantID is left empty by both v1 call
// sites and filled by the sink itself, because the tenant a node belongs to is
// read from that node's own credential (cmd/synapse) and never from a request.
// SessionID is carried so a usage row can be tied back to a conversation, and
// it is stored and never logged.
type UsageEvent struct {
	// TenantID is the tenant this compilation belongs to.
	TenantID string
	// AgentID is the compiling node's own agent-id (config.AgentID).
	AgentID string
	// SessionID is the session the conversation belonged to.
	SessionID string
	// RawTokens is the retrieved candidate pool's token count: the baseline
	// the reduction is measured against.
	RawTokens int
	// CompiledTokens is the token count of the context the sieve emitted.
	CompiledTokens int
	// ReductionPct is (raw - compiled) / raw * 100, or 0 when there was
	// nothing to reduce.
	ReductionPct float64
	// Model is the operator's label for the upstream model, or "" when the
	// node does not name one.
	Model string
	// CreatedAt is when the compilation finished.
	CreatedAt time.Time
}

// UsageSink records one finished compilation's usage.
//
// It is declared here, on the consumer side, for the same reason LedgerSink is:
// the implementation needs a database pool, and internal/metering must not be
// imported into a package the scoring pipeline depends on. The contract is one
// call wide, and a sink must not block: RecordUsage is called on the request
// path, after the compilation it describes is already complete, and the
// implementation is responsible for making the write asynchronous (see
// metering.Meter.Record).
type UsageSink interface {
	// Record writes event, or drops it. It returns nothing on purpose: a
	// metering failure must have no way to change what a caller receives.
	Record(event UsageEvent)
}

// usageWiring is the process's usage sink. It is one struct rather than a bare
// interface held in the atomic pointer, so that no reader can observe a
// half-installed sink and so that clearing the wiring stores a true nil.
type usageWiring struct {
	sink UsageSink
}

// usage holds the process's usage sink, or nil when this process does not meter
// -- which is the standalone node's shape and the default.
//
// An atomic.Pointer rather than a plain variable for the reason ledger.go gives:
// the write happens once, before serving, and every read is a single load on a
// live request path.
var usage atomic.Pointer[usageWiring]

// SetUsageSink installs the process's usage sink. It must be called before the
// router starts serving; calling it again replaces the previous wiring (which
// is also how a caller clears it, with a nil sink).
//
// A nil sink disables metering for this process entirely: RecordUsage then
// spawns no goroutine and reads no struct, so a standalone node behaves exactly
// as it did before this file existed. There is no plan gate here, and that
// difference from SetLedgerSink is deliberate rather than an omission: usage is
// what the compliance report's compilation count and average reduction are read
// from, so it is not a feature one plan buys.
func SetUsageSink(sink UsageSink) {
	if sink == nil {
		usage.Store(nil)
		return
	}

	usage.Store(&usageWiring{sink: sink})
}

// usageSink returns the sink this process records through, or nil when it does
// not meter.
func usageSink() UsageSink {
	w := usage.Load()
	if w == nil {
		return nil
	}

	return w.sink
}

// RecordUsage hands one finished compilation's token accounting to this
// process's usage sink, or does nothing when none is installed.
//
// It is a free function rather than a method on purpose: the two v1 call sites
// that own the numbers (internal/proxy's HandleMessages and internal/api's
// runCompilePipeline) hold no value that also knows the tenant, and this
// signature keeps each of them to one statement. It never blocks on its own
// account -- one pointer load when unwired, then one call to a sink that is
// required to return immediately -- and it never returns an error, because
// metering must not be able to fail a compilation.
func RecordUsage(event UsageEvent) {
	if sink := usageSink(); sink != nil {
		sink.Record(event)
	}
}

// SavingsUSD is the token saving in dollars, at the blended rate above:
// (raw - compiled) / 1000 * $0.015.
//
// A negative saving is returned as computed rather than clamped: nothing in the
// pipeline is supposed to emit more tokens than the pool it selected from, so a
// negative number is a signal worth seeing in a log line and in a metering row,
// not a value to hide.
func SavingsUSD(rawTokens, compiledTokens int) float64 {
	return float64(rawTokens-compiledTokens) / 1000.0 * usdPerThousandTokens
}
