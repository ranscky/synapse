// The compliance report's arithmetic: how the window's facts become the numbers
// a compliance officer reads.
//
// It is split from compliance_report.go -- the endpoint -- along the line that
// decides what is testable in CI: everything in this file is a pure function of
// values that were handed to it, so the percentages, the de-duplication, and the
// cross-agent counting are covered by untagged tests, while the only
// untested-without-a-database half is the SQL that fetched the facts
// (internal/ledger's own, and the tagged integration test end to end).
//
// Two properties are what this file is built around.
//
//   - An unreadable trace is an error, not a smaller report. A row whose
//     trace_json will not parse means the ledger holds something this build
//     cannot read (corruption, or a schema this build predates), and a report
//     that quietly omitted it would under-state a period it is certifying. This
//     is the same fail-closed reading Phase 19's audit endpoint takes; see
//     parseAuditTrace.
//   - Every count that names a memory is a *set*, not a sum, with the two stated
//     exceptions (memories used and cross-agent retrievals are event counts).
//     One memory can be retrieved into many compilations in one period, and
//     "3,000 memories were superseded" when it was three memories in a thousand
//     traces would be a false statement in the direction that matters.
package plane

import (
	"fmt"
	"math"
	"time"

	"synapse/internal/store"
	"synapse/internal/trace"
)

// reportAccumulator collects the per-trace contributions the sections are built
// from. It is a struct rather than a bag of local variables because the walk and
// the fold that follows it read the same fields.
type reportAccumulator struct {
	// memoriesUsed is the sum of the traces' own memories_compiled.
	memoriesUsed int
	// reductionSum/reductionSamples are the ledger-side fallback for the mean
	// reduction: the traces' own recorded values.
	reductionSum     float64
	reductionSamples int
	// typeCounts and includedTotal are the numerators and denominator of the
	// memory-type breakdown: used memories only.
	typeCounts    map[string]int
	includedTotal int
	// crossAgentRetrievals and agents are the Global Brain's two numbers.
	crossAgentRetrievals int
	agents               map[string]struct{}
	// conflicts and superseded are sets of memory ids, so a memory seen in
	// several compilations counts once.
	conflicts  map[string]struct{}
	superseded map[string]struct{}
}

// newReportAccumulator returns an accumulator with every map allocated, so an
// empty period produces empty sections rather than nil ones.
func newReportAccumulator() *reportAccumulator {
	return &reportAccumulator{
		typeCounts: make(map[string]int),
		agents:     make(map[string]struct{}),
		conflicts:  make(map[string]struct{}),
		superseded: make(map[string]struct{}),
	}
}

// buildComplianceReport turns one window's facts into the report both formats
// render. now is passed in rather than read here, so the generated_at a test
// asserts is the one it chose.
//
// The report is returned with ChainIntegrity.EntriesInPeriod filled and its
// verdict untouched: the walk that produces Valid and LastVerifiedAt needs the
// tenant's signing secret, which is the plane's own dependency (see
// LedgerVerifier) and not something this pure function may reach for. The handler
// merges those two fields after this returns.
func buildComplianceReport(tenantID string, window ReportWindow, facts ReportFacts, now time.Time) (ComplianceReport, error) {
	acc := newReportAccumulator()

	for _, row := range facts.Rows {
		manifest, err := parseAuditTrace(row)
		if err != nil {
			// The row's id travels with the failure so the row can be found and
			// repaired. It is a ledger row id of the caller's own tenant -- the
			// same value Phase 19's audit endpoint logs for the same reason --
			// and the payload that failed to parse deliberately does not.
			return ComplianceReport{}, fmt.Errorf("plane: build compliance report: entry %s: %w", row.ID, err)
		}

		acc.addTrace(manifest)
	}

	return ComplianceReport{
		Header: ComplianceReportHeader{
			TenantID: tenantID,
			Period: ComplianceReportPeriod{
				Since: window.Since,
				Until: window.Until,
			},
			GeneratedAt:    now.UTC(),
			SynapseVersion: Version,
		},
		Summary: ComplianceReportSummary{
			TotalCompilations: acc.compilations(facts),
			TotalMemoriesUsed: acc.memoriesUsed,
			AvgReductionPct:   roundTo2(acc.avgReduction(facts)),
		},
		MemoryTypeBreakdown: acc.breakdown(),
		GlobalBrain: ComplianceReportGlobalBrain{
			CrossAgentRetrievals:     acc.crossAgentRetrievals,
			UniqueContributingAgents: len(acc.agents),
		},
		ConflictResolution: ComplianceReportConflictResolution{
			ContradictionsDetected: len(acc.conflicts),
		},
		Supersession: ComplianceReportSupersession{
			MemoriesSuperseded: len(acc.superseded),
		},
		ChainIntegrity: ComplianceReportChainIntegrity{
			EntriesInPeriod: len(facts.Rows),
		},
	}, nil
}

// addTrace folds one compilation's trace into the accumulator.
//
// The three per-memory questions this asks are deliberately different ones, which
// is why they are answered from different fields: was it used (Included, the
// breakdown's denominator), whose was it (CrossAgent/AgentID, the Global Brain's
// contribution), and what did the write path record about it
// (ConflictStatus/SupersededBy, which are properties of the memory rather than of
// this compilation).
func (a *reportAccumulator) addTrace(manifest trace.TraceManifest) {
	a.memoriesUsed += manifest.MemoriesCompiled
	a.reductionSum += manifest.ReductionPct
	a.reductionSamples++

	for _, memory := range manifest.Memories {
		if memory.Included {
			a.typeCounts[memory.MemoryType]++
			a.includedTotal++
		}

		if memory.CrossAgent {
			a.crossAgentRetrievals++

			if memory.AgentID != "" {
				a.agents[memory.AgentID] = struct{}{}
			}
		}

		// The newer side of a contradiction pair is the detection; the older
		// side is the same event seen from the other row, so counting it too
		// would double every detection. store.ConflictStatusConflict is the
		// value the write path records for the row that introduced it, and it
		// is compared rather than re-spelled here so the two cannot drift.
		if memory.ID != "" && memory.ConflictStatus == store.ConflictStatusConflict {
			a.conflicts[memory.ID] = struct{}{}
		}

		if memory.ID != "" && memory.SupersededBy != "" {
			a.superseded[memory.ID] = struct{}{}
		}
	}
}

// compilations answers "how many compilations were there": the metering table's
// own count when it holds rows for the window, and one per signed ledger entry
// when it does not.
//
// The fallback is not a guess. Every ledger row *is* a compilation -- the write
// path appends one per compiled request -- so the ledger count is the same
// quantity measured by the signed artifact rather than by the metering pipeline.
// Nothing writes usage_events yet (the metering phase is unbuilt), so today's
// reports take the fallback; the moment metering lands the metering count wins,
// and the integration test asserts the two can differ so the switch is visible
// rather than implied.
func (a *reportAccumulator) compilations(facts ReportFacts) int {
	if facts.Usage.HasRows {
		return facts.Usage.Compilations
	}

	return len(facts.Rows)
}

// avgReduction answers "how much context did the compiler remove", with the same
// metering-first, ledger-fallback rule as compilations.
//
// The ledger-side mean is the mean of the traces' own reduction_pct. Those values
// are 0 in every ledgered row today, because Phase 18 appends the trace before the
// caller finalises the two token fields -- stated in PROGRESS.md as a fidelity gap
// rather than hidden here, and the second reason the metering table is preferred
// when it has rows: it is the only surface that will carry a real reduction
// without changing a frozen v1 call site.
func (a *reportAccumulator) avgReduction(facts ReportFacts) float64 {
	if facts.Usage.HasRows {
		return facts.Usage.AvgReductionPct
	}

	if a.reductionSamples == 0 {
		return 0
	}

	return a.reductionSum / float64(a.reductionSamples)
}

// breakdown renders the memory-type shares as percentages of the memories that
// were actually used in the period, rounded to two decimals.
//
// The denominator is the count of used memories by type rather than
// memories_compiled, so the shares sum to 100 by construction instead of
// approximately. The map is always non-nil: an empty period has a breakdown of
// nothing, which marshals as {} and renders as an empty table.
func (a *reportAccumulator) breakdown() map[string]float64 {
	shares := make(map[string]float64, len(a.typeCounts))
	if a.includedTotal == 0 {
		return shares
	}

	for memoryType, count := range a.typeCounts {
		shares[memoryType] = roundTo2(float64(count) / float64(a.includedTotal) * 100)
	}

	return shares
}

// roundTo2 rounds to two decimal places. Reports are read by people and compared
// by scripts, and an unrounded mean would carry float noise into both.
func roundTo2(value float64) float64 {
	return math.Round(value*100) / 100
}
