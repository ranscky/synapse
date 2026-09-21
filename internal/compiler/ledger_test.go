package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"synapse/internal/scorer"
	"synapse/internal/store"
	"synapse/internal/trace"
)

// ledgerQuietWindow is how long a negative case waits for an append that must
// never arrive. A non-enterprise compile spawns no goroutine at all, so the
// window only has to cover scheduler latency, not I/O -- it is not a race, it
// is what "silence" is asserted with in a language where nothing can be
// observed to not happen.
const ledgerQuietWindow = 100 * time.Millisecond

// appendCall is one AppendTrace call, copied out of a sink so an assertion
// never races the goroutine that made it.
type appendCall struct {
	requestID string
	traceJSON string
}

// recordingSink captures every append and never blocks. A buffered channel
// rather than a slice keeps the capture race-free: the goroutine sends, the
// test receives.
type recordingSink struct {
	appends chan appendCall
	err     error
}

func newRecordingSink() *recordingSink {
	return &recordingSink{appends: make(chan appendCall, 4)}
}

func (s *recordingSink) AppendTrace(_ context.Context, requestID, traceJSON string) error {
	s.appends <- appendCall{requestID: requestID, traceJSON: traceJSON}
	return s.err
}

// received returns the next append, failing the test if none arrives.
func (s *recordingSink) received(t *testing.T) appendCall {
	t.Helper()

	select {
	case call := <-s.appends:
		return call
	case <-time.After(2 * time.Second):
		t.Fatalf("no ledger append arrived")
		return appendCall{}
	}
}

// silent fails the test if any append arrives inside the quiet window.
func (s *recordingSink) silent(t *testing.T) {
	t.Helper()

	select {
	case call := <-s.appends:
		t.Fatalf("unexpected ledger append for request %q", call.requestID)
	case <-time.After(ledgerQuietWindow):
	}
}

// blockingSink holds every append until it is released, which is how the
// "compilation does not wait for the ledger" property is proved.
type blockingSink struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingSink() *blockingSink {
	return &blockingSink{started: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingSink) AppendTrace(ctx context.Context, _, _ string) error {
	close(s.started)

	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// installLedgerSink wires a sink for one test and clears the process wiring
// afterwards, so a test can never leak ledgering into another test in this
// package.
func installLedgerSink(t *testing.T, sink LedgerSink, plan string) {
	t.Helper()

	SetLedgerSink(sink, plan)
	t.Cleanup(func() { SetLedgerSink(nil, "") })
}

// ledgerCompile runs one compilation with the arguments the ledger hook reads:
// a request id the assertions can name, and one selected memory so the trace
// has a memory in it.
func ledgerCompile(requestID string) *CompileResult {
	mem := scorer.ScoredMemory{
		MemoryEntry: store.MemoryEntry{
			ID:         "mem-1",
			SessionID:  "sess-1",
			Content:    "ledger test memory",
			MemoryType: "fact",
			Timestamp:  time.Now(),
		},
		Total: 0.9,
	}

	return Compile(
		[]scorer.ScoredMemory{mem},
		"ledger test question",
		requestID,
		"generic",
		0.5,
		1,
		1,
		3000,
		0,
		[]scorer.ScoredMemory{mem},
		[]scorer.ScoredMemory{mem},
		"",
	)
}

// TestCompileAppendsTraceForEnterprisePlan is the phase's core contract: one
// enterprise compilation, one append, carrying that compilation's own trace.
func TestCompileAppendsTraceForEnterprisePlan(t *testing.T) {
	sink := newRecordingSink()
	installLedgerSink(t, sink, EnterprisePlan)

	result := ledgerCompile("req-1700000000000000000-1")
	if result == nil || result.Trace == nil {
		t.Fatalf("Compile() returned no trace")
	}

	call := sink.received(t)

	if call.requestID != "req-1700000000000000000-1" {
		t.Errorf("append used request id %q, want the trace's own", call.requestID)
	}

	// The payload must be the trace itself, not a summary of it: the ledger
	// signs these exact bytes.
	var appended trace.TraceManifest
	if err := json.Unmarshal([]byte(call.traceJSON), &appended); err != nil {
		t.Fatalf("appended trace is not the marshalled manifest: %v", err)
	}

	if appended.RequestID != result.Trace.RequestID {
		t.Errorf("appended trace request id = %q, want %q", appended.RequestID, result.Trace.RequestID)
	}
	if appended.MemoriesCompiled != result.Trace.MemoriesCompiled {
		t.Errorf("appended memories_compiled = %d, want %d", appended.MemoriesCompiled, result.Trace.MemoriesCompiled)
	}
	if len(appended.Memories) != len(result.Trace.Memories) {
		t.Errorf("appended %d memories, want %d", len(appended.Memories), len(result.Trace.Memories))
	}
}

// TestCompileSkipsLedgerForNonEnterprisePlans covers the gate. Every case here
// is a plan that must leave the request path exactly as v1 left it.
func TestCompileSkipsLedgerForNonEnterprisePlans(t *testing.T) {
	plans := []string{"oss", "team", "", "Enterprise", "enterprise-plus"}

	for _, plan := range plans {
		t.Run("plan="+plan, func(t *testing.T) {
			sink := newRecordingSink()
			installLedgerSink(t, sink, plan)

			if result := ledgerCompile("req-non-enterprise"); result == nil {
				t.Fatalf("Compile() returned nil")
			}

			sink.silent(t)
		})
	}
}

// TestCompileWithoutLedgerSink is the standalone node: no sink installed at
// all, which is the default this file must not change.
func TestCompileWithoutLedgerSink(t *testing.T) {
	SetLedgerSink(nil, "")
	t.Cleanup(func() { SetLedgerSink(nil, "") })

	if result := ledgerCompile("req-standalone"); result == nil || len(result.Messages) == 0 {
		t.Fatalf("Compile() produced no messages without a ledger sink")
	}
}

// TestSetLedgerSinkIgnoresNilSinkUnderEnterprisePlan covers the other half of
// the gate: an enterprise plan with nothing to append through is still a
// no-op, not a nil dereference.
func TestSetLedgerSinkIgnoresNilSinkUnderEnterprisePlan(t *testing.T) {
	installLedgerSink(t, nil, EnterprisePlan)

	if result := ledgerCompile("req-nil-sink"); result == nil {
		t.Fatalf("Compile() returned nil with a nil sink")
	}
}

// TestCompileDoesNotWaitForLedgerAppend is the 50ms-SLA proof: the append is
// still blocked when Compile returns. The assertion is structural rather than
// timed -- the goroutine is released only after the result has been received.
func TestCompileDoesNotWaitForLedgerAppend(t *testing.T) {
	sink := newBlockingSink()
	installLedgerSink(t, sink, EnterprisePlan)

	compiled := make(chan *CompileResult, 1)
	go func() { compiled <- ledgerCompile("req-blocking-sink") }()

	select {
	case result := <-compiled:
		if result == nil || result.Trace == nil {
			t.Fatalf("Compile() returned no trace")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Compile() waited for the ledger append")
	}

	// The append did run -- it is holding the goroutine open, not skipped --
	// and releasing it now is what lets the test process exit cleanly.
	<-sink.started
	close(sink.release)
}

// TestCompileSurvivesFailedLedgerAppend: an append that fails is the ledger's
// problem, never the caller's. The compiled output must be unchanged.
func TestCompileSurvivesFailedLedgerAppend(t *testing.T) {
	sink := newRecordingSink()
	sink.err = errors.New("ledger: append needs a signing secret")
	installLedgerSink(t, sink, EnterprisePlan)

	result := ledgerCompile("req-failed-append")
	if result == nil || len(result.Messages) == 0 {
		t.Fatalf("Compile() produced no messages when the append failed")
	}

	call := sink.received(t)
	if call.requestID != "req-failed-append" {
		t.Errorf("append used request id %q, want the trace's own", call.requestID)
	}
}
