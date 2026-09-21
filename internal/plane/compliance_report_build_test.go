package plane

// Tests for the compliance report's arithmetic: the half of Phase 20 that a
// database is not needed to check, and therefore the half CI can check.
//
// They are an internal test (package plane) because buildComplianceReport is
// unexported -- it is this package's own fold from facts to report, and exporting
// it for a test would be exporting a function whose only caller is the handler next
// door. The SQL that produces the facts is internal/ledger's own test, and the two
// meet end to end in compliance_report_integration_test.go.
import (
	"encoding/json"
	"testing"
	"time"

	"synapse/internal/store"
	"synapse/internal/trace"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reportTestTenant is the tenant every builder test builds a report for.
const reportTestTenant = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

// reportTestTime is the generated_at every test asserts, passed in rather than
// read from the clock so the assertion is about the builder and not about timing.
var reportTestTime = time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)

// traceRow renders one stored ledger row whose trace is the given manifest.
//
// The manifest is marshalled by the test rather than hand-written, so a field
// renamed in internal/trace breaks this file at compile time instead of producing a
// trace that silently parses into zero values.
func traceRow(t *testing.T, manifest trace.TraceManifest) AuditRow {
	t.Helper()

	encoded, err := json.Marshal(manifest)
	require.NoError(t, err)

	return AuditRow{
		ID:        "entry-1",
		TenantID:  reportTestTenant,
		RequestID: manifest.RequestID,
		TraceJSON: string(encoded),
		CreatedAt: reportTestTime,
	}
}

// TestBuildComplianceReportCountsWhatTheTracesSay is the arithmetic contract: the
// volume, the memory-type shares, the Global Brain's two numbers, the conflict
// count, and the supersession count are all read from the traces the window holds,
// and the chain-integrity coverage is the window's own length.
//
// The fixture is deliberately mixed rather than uniform: one memory that was used
// and was another agent's, ones that were used and were not, memories that were not
// used at all (so the breakdown's denominator is observably "used memories", not
// "all memories"), one that introduced a contradiction, and one that was superseded.
func TestBuildComplianceReportCountsWhatTheTracesSay(t *testing.T) {
	first := trace.TraceManifest{
		RequestID:        "req-1",
		MemoriesCompiled: 2,
		ReductionPct:     40,
		Memories: []trace.TraceMemory{
			{ID: "mem-fact-1", MemoryType: "fact", Included: true},
			{ID: "mem-dec-1", MemoryType: "decision", Included: true, CrossAgent: true, AgentID: "agent-b"},
		},
	}

	second := trace.TraceManifest{
		RequestID:        "req-2",
		MemoriesCompiled: 2,
		ReductionPct:     60,
		Memories: []trace.TraceMemory{
			{ID: "mem-conflict", MemoryType: "decision", Included: true,
				CrossAgent: true, AgentID: "agent-c",
				ConflictStatus: store.ConflictStatusConflict, ConflictWithID: "mem-old"},
			{ID: "mem-old", MemoryType: "decision",
				ConflictStatus: store.ConflictStatusSupersededCandidate, ConflictWithID: "mem-conflict",
				ExclusionReason: "conflicting", SupersededBy: "mem-conflict"},
		},
	}

	facts := ReportFacts{Rows: []AuditRow{traceRow(t, first), traceRow(t, second)}}

	report, err := buildComplianceReport(reportTestTenant, ReportWindow{}, facts, reportTestTime)
	require.NoError(t, err)

	assert.Equal(t, reportTestTenant, report.Header.TenantID)
	assert.Equal(t, Version, report.Header.SynapseVersion)
	assert.Equal(t, reportTestTime, report.Header.GeneratedAt)
	assert.Nil(t, report.Header.Period.Since, "an unbounded window stays unbounded in the header")

	// The ledger fallback: no metering rows, so one compilation per entry and the
	// mean of the traces' own reduction.
	assert.Equal(t, 2, report.Summary.TotalCompilations)
	assert.Equal(t, 4, report.Summary.TotalMemoriesUsed, "2 + 2 from the traces' own memories_compiled")
	assert.Equal(t, 50.0, report.Summary.AvgReductionPct, "(40 + 60) / 2")

	// Three used memories: one fact and two decisions. The superseded memory in
	// `second` is excluded from the denominator because it was not used.
	assert.Equal(t, map[string]float64{"decision": 66.67, "fact": 33.33}, report.MemoryTypeBreakdown)

	assert.Equal(t, 2, report.GlobalBrain.CrossAgentRetrievals, "one per cross-agent retrieval event")
	assert.Equal(t, 2, report.GlobalBrain.UniqueContributingAgents, "agent-b and agent-c")

	assert.Equal(t, 1, report.ConflictResolution.ContradictionsDetected,
		"the pair is one detection: the older half is not counted as a second")
	assert.Equal(t, 1, report.Supersession.MemoriesSuperseded)

	assert.Equal(t, 2, report.ChainIntegrity.EntriesInPeriod, "the window's coverage")
	assert.False(t, report.ChainIntegrity.Valid, "the verdict is the handler's to merge, not this function's")
	assert.Equal(t, "", report.Article50Statement, "likewise the statement")
}

// TestBuildComplianceReportPrefersMeteringWhenItHasRows pins the source of the two
// metering-facing numbers: when the window holds usage_events rows they are what
// the report says, even though the ledger holds a different count of entries.
//
// The two disagreeing is the point of the fixture. They are different quantities --
// ledger entries are one per appended trace, metering rows are one per compilation
// the node recorded -- and Phase 20's brief asks for a report built from both. A
// test where they agree could not tell which one was read.
func TestBuildComplianceReportPrefersMeteringWhenItHasRows(t *testing.T) {
	manifest := trace.TraceManifest{RequestID: "req-1", MemoriesCompiled: 2, ReductionPct: 0}

	facts := ReportFacts{
		Rows:  []AuditRow{traceRow(t, manifest), traceRow(t, manifest), traceRow(t, manifest)},
		Usage: UsageTotals{Compilations: 9, AvgReductionPct: 41.5, HasRows: true},
	}

	report, err := buildComplianceReport(reportTestTenant, ReportWindow{}, facts, reportTestTime)
	require.NoError(t, err)

	assert.Equal(t, 9, report.Summary.TotalCompilations, "the metering count, not the three ledger entries")
	assert.Equal(t, 41.5, report.Summary.AvgReductionPct, "the metering mean, not the traces' recorded 0")
	assert.Equal(t, 3, report.ChainIntegrity.EntriesInPeriod, "the chain coverage is still the window's")
	assert.Equal(t, 6, report.Summary.TotalMemoriesUsed, "memories used is always the traces' own count")
}

// TestBuildComplianceReportCountsAMemoryOnce is the de-duplication rule: a memory
// that appears in several compilations in one period is one superseded memory and
// one contradiction, not one per appearance.
//
// Without this, a tenant with three memories superseded a thousand times would read
// a thousand -- a number wrong in the direction that makes a report worse than
// useless to the officer reading it.
func TestBuildComplianceReportCountsAMemoryOnce(t *testing.T) {
	manifest := trace.TraceManifest{
		RequestID:        "req-1",
		MemoriesCompiled: 1,
		Memories: []trace.TraceMemory{
			{ID: "mem-sup", MemoryType: "fact", SupersededBy: "mem-new"},
			{ID: "mem-conflict", MemoryType: "fact", ConflictStatus: store.ConflictStatusConflict},
			{ID: "mem-conflict", MemoryType: "fact", ConflictStatus: store.ConflictStatusConflict},
		},
	}

	facts := ReportFacts{Rows: []AuditRow{
		traceRow(t, manifest), traceRow(t, manifest), traceRow(t, manifest),
	}}

	report, err := buildComplianceReport(reportTestTenant, ReportWindow{}, facts, reportTestTime)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Supersession.MemoriesSuperseded)
	assert.Equal(t, 1, report.ConflictResolution.ContradictionsDetected,
		"and a memory listed twice inside one trace is still one detection")
	assert.Equal(t, 3, report.Summary.TotalMemoriesUsed, "the volume, by contrast, is per compilation")
}

// TestBuildComplianceReportRefusesAnUnreadableTrace: a row that will not parse is an
// error and never a smaller report, because a report whose numbers omit a period's
// rows is a wrong answer to a question about that period -- the same fail-closed
// stance Phase 19's audit endpoint takes.
func TestBuildComplianceReportRefusesAnUnreadableTrace(t *testing.T) {
	facts := ReportFacts{Rows: []AuditRow{{
		ID:        "entry-1",
		TenantID:  reportTestTenant,
		TraceJSON: `{"detected_intent":"leak-me`,
	}}}

	report, err := buildComplianceReport(reportTestTenant, ReportWindow{}, facts, reportTestTime)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse stored trace")
	assert.NotContains(t, err.Error(), "leak-me", "the error names the failure, not the payload")
	assert.Equal(t, ComplianceReport{}, report, "no partial report is returned")
}

// TestBuildComplianceReportRendersAnEmptyWindowAsEmptySections: a tenant with
// nothing in the window gets a report that says so -- zeros, an empty breakdown
// object rather than null, and a window that is still the one it asked for.
func TestBuildComplianceReportRendersAnEmptyWindowAsEmptySections(t *testing.T) {
	since := reportTestTime.Add(-24 * time.Hour)
	until := reportTestTime
	window := ReportWindow{Since: &since, Until: &until}

	report, err := buildComplianceReport(reportTestTenant, window, ReportFacts{Rows: []AuditRow{}}, reportTestTime)
	require.NoError(t, err)

	assert.Equal(t, 0, report.Summary.TotalCompilations)
	assert.Equal(t, 0, report.Summary.TotalMemoriesUsed)
	assert.Equal(t, 0.0, report.Summary.AvgReductionPct)
	assert.NotNil(t, report.MemoryTypeBreakdown, "never nil: it marshals as {} and renders as an empty table")
	assert.Empty(t, report.MemoryTypeBreakdown)
	assert.Equal(t, 0, report.GlobalBrain.CrossAgentRetrievals)
	assert.Equal(t, 0, report.GlobalBrain.UniqueContributingAgents)
	assert.Equal(t, 0, report.ChainIntegrity.EntriesInPeriod)

	// The window is echoed exactly as it arrived, so a caller can see the period
	// its own numbers cover.
	require.NotNil(t, report.Header.Period.Since)
	assert.Equal(t, since, *report.Header.Period.Since)
	require.NotNil(t, report.Header.Period.Until)
	assert.Equal(t, until, *report.Header.Period.Until)
}

// TestRoundTo2KeepsReportsComparable: two decimal places, because these numbers are
// both read by people and compared by scripts and float noise would defeat the
// second.
func TestRoundTo2KeepsReportsComparable(t *testing.T) {
	assert.Equal(t, 33.33, roundTo2(100.0/3.0))
	assert.Equal(t, 0.0, roundTo2(0))
	assert.Equal(t, 66.67, roundTo2(66.66666666666666))
}
