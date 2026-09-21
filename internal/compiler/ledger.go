// The compilation path's half of the v2 audit ledger: the point where a
// finished compilation is handed to a ledger, off the request path.
//
// The dependency arrives as an interface and is installed once per process
// (SetLedgerSink) rather than as a parameter of Compile, and the reason is
// structural rather than stylistic. Compile is a v1 free function with twelve
// positional parameters and two frozen callers -- internal/proxy's live
// proxied traffic and internal/api's POST /v1/compile -- so there is no
// Compiler struct to carry a field and no call site this phase may edit to
// pass an argument. A package-level wiring is what remains, and it is the one
// deliberate exception to this project's "no global state" style: it is
// written exactly once, before any router serves, and read on every compile.
//
// Nothing here knows about Postgres, tenants, or signing keys. The sink is
// implemented in cmd/synapse, the only place that can see both this package
// and internal/ledger.
package compiler

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"time"

	"synapse/internal/trace"
)

const (
	// EnterprisePlan is the tenant plan a compilation must belong to before its
	// trace is written to the ledger. It is the one place this literal lives:
	// the value is compared against the plan claim of the node's own tenant
	// token (cmd/synapse), never against anything a request carries.
	EnterprisePlan = "enterprise"

	// ledgerAppendTimeout bounds one append, secret fetch included. It is the
	// append's own ceiling rather than the request's: the request is over by
	// the time this runs, so a slow database costs one abandoned goroutine and
	// one log line rather than a failed or slow compilation.
	ledgerAppendTimeout = 5 * time.Second
)

// LedgerSink records one finished compilation's trace in the audit ledger.
//
// It is declared here, on the consumer side, for the same reason
// internal/plane declares its own LedgerVerifier: the implementation needs the
// tenant secret store and the ledger's write path, and neither may be imported
// into a package the scoring pipeline depends on. The contract is deliberately
// one call wide -- a request id and the trace exactly as it was marshalled --
// so a sink cannot reformat the payload it signs: any normalization would
// change the hash the chain stores.
//
// AppendTrace may block; Compile calls it from its own goroutine and never
// waits for it. ctx carries the timeout described above, and a returned error
// is logged by this package and otherwise ignored: an audit write must never
// change what a caller receives, and must never be retried inside this
// process (a retry would race the tenant's own advisory lock for no benefit --
// the next compilation appends after it anyway).
type LedgerSink interface {
	// AppendTrace signs traceJSON and appends it to requestID's tenant chain.
	// requestID is the value from the trace manifest itself.
	AppendTrace(ctx context.Context, requestID, traceJSON string) error
}

// ledgerWiring is the process's ledger sink and the plan that gates it. Both
// halves are one value so a reader can never observe a new sink under the old
// plan.
type ledgerWiring struct {
	sink LedgerSink
	plan string
}

// wiring holds the process's ledger sink, or nil when this process does not
// write a ledger at all -- which is the standalone node's shape, and the
// default.
//
// It is an atomic.Pointer rather than a plain variable so that a reader on a
// request goroutine cannot race a writer on the boot path (or a test), and no
// mutex is needed: the write happens once, before serving, and every read is a
// single load.
var wiring atomic.Pointer[ledgerWiring]

// SetLedgerSink installs the process's ledger sink and the tenant plan that
// gates it. It must be called before the router starts serving; calling it
// again replaces the previous wiring (which is also how a caller clears it,
// with a nil sink).
//
// A nil sink, or a plan that is not EnterprisePlan, disables ledgering for this
// process entirely: Compile then does no marshalling, spawns no goroutine, and
// behaves exactly as it did before this file existed. That is what leaves a
// standalone node and an oss/team tenant on the v1 path with no branch taken
// per compile beyond one pointer load and one string comparison.
func SetLedgerSink(sink LedgerSink, tenantPlan string) {
	if sink == nil {
		wiring.Store(nil)
		return
	}

	wiring.Store(&ledgerWiring{sink: sink, plan: tenantPlan})
}

// ledgerSink returns the sink this process appends through, or nil when it
// does not ledger. The plan check lives here rather than at the call site so
// there is one place that decides what "this compilation gets an audit entry"
// means.
func ledgerSink() LedgerSink {
	w := wiring.Load()
	if w == nil || w.sink == nil || w.plan != EnterprisePlan {
		return nil
	}

	return w.sink
}

// recordTrace appends one finished compilation to the ledger, in a goroutine,
// and returns immediately.
//
// The marshalling is deliberately synchronous and the I/O deliberately is not.
// Both callers mutate the very same *TraceManifest right after Compile returns
// -- they fill in TokensUsed and ReductionPct, which this package cannot know
// (see trace.NewTraceManifest's doc comment) -- so a goroutine that read the
// struct while they wrote it would be a data race. Serializing first takes the
// snapshot off the shared struct, which is why the cost paid on the request
// path is one json.Marshal of a trace that is already in memory.
//
// The trade that buys is documented rather than hidden: the ledgered trace
// carries tokens_used 0 and reduction_pct 0, because both are still unset at
// the moment the trace is complete from this package's point of view. Editing
// the two v1 call sites to append after their own fixups is what would make
// those fields truthful, and that is a deliberate later-phase decision.
//
// The goroutine uses context.Background() rather than the request's context:
// the request's context is cancelled when the response is written, which is
// before this append runs, so deriving from it would abandon every append.
//
// Nothing here is ever logged except the error: no trace payload, no request
// id, no secret. The request id is not a credential, but it is also not needed
// to diagnose a failed append, and the ledger row's own id is what a caller
// would quote.
func recordTrace(manifest *trace.TraceManifest) {
	sink := ledgerSink()
	if sink == nil || manifest == nil {
		return
	}

	traceJSON, err := json.Marshal(manifest)
	if err != nil {
		slog.Error("ledger: failed to marshal trace manifest", "error", err)
		return
	}

	requestID := manifest.RequestID

	go func(traceJSON, requestID string) {
		ctx, cancel := context.WithTimeout(context.Background(), ledgerAppendTimeout)
		defer cancel()

		if err := sink.AppendTrace(ctx, requestID, traceJSON); err != nil {
			slog.Error("ledger: append failed", "error", err)
		}
	}(string(traceJSON), requestID)
}
