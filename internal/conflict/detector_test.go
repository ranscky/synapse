// Tests for the cross-agent contradiction detector. The four fixtures the phase
// names are here verbatim, together with the Jaccard index each pair produces, so a
// change to the tokenizer or to the gate shows up as a number moving rather than as
// a test that keeps passing for a different reason.
package conflict

import (
	"math"
	"testing"

	"synapse/internal/store"
)

const (
	// Test 1: same topic, a different value for the same predicate.
	decidedPostgres = "We decided to use Postgres"
	decidedMySQL    = "We decided to use MySQL"
	// Test 2: same topic, added detail, no replacement or negation signal.
	migratePostgres = "We will migrate to Postgres next sprint"
	// Test 3: nothing in common.
	skyIsBlue = "The sky is blue"
	// Test 4: an explicit negation of the other memory's own subject.
	notUsingRedis = "We are not using Redis"
	decidedRedis  = "We decided to use Redis"

	candidateID = "candidate-memory"
	existingID  = "existing-memory"
)

// memory builds the only fields the detector and its helpers read. The search
// scope fields are set to two different values on purpose -- a different session
// and a different agent -- even though nothing filters on them: cross-agent,
// cross-session pairs are the case this package exists for.
func memory(id, sessionID, agentID, content string) store.MemoryEntry {
	return store.MemoryEntry{
		ID:         id,
		SessionID:  sessionID,
		AgentID:    agentID,
		MemoryType: "decision",
		Content:    content,
	}
}

// existingMemory is the one-candidate form almost every test below compares
// against: an agent_a memory, in another session.
func existingMemory(content string) []store.MemoryEntry {
	return []store.MemoryEntry{memory(existingID, "session-a", "agent-a", content)}
}

// detectionFor runs one detection at an explicit threshold.
func detectionFor(threshold float64, candidate string, existing ...store.MemoryEntry) (bool, string) {
	return NewContradictionDetector(threshold).Detect(memory(candidateID, "session-b", "agent-b", candidate), existing)
}

// jaccardOf is the similarity the gate sees, computed the way Detect computes it.
func jaccardOf(a, b string) float64 {
	return jaccardSimilarity(tokenSet(tokenize(a)), tokenSet(tokenize(b)))
}

// TestJaccardIndexOfTheDocumentedFixtures pins the four numbers the gate is built
// around. They are what makes the phase's Test 4 interesting: an explicit negation
// scores lower (0.250) than the default threshold, so the detector cannot rely on
// the gate alone to see it.
func TestJaccardIndexOfTheDocumentedFixtures(t *testing.T) {
	fixtures := []struct {
		name string
		a, b string
		want float64
	}{
		{"same topic, different value", decidedPostgres, decidedMySQL, 0.6667},
		{"same topic, added detail", decidedPostgres, migratePostgres, 0.3333},
		{"different topic", skyIsBlue, decidedMySQL, 0.0},
		{"explicit negation", notUsingRedis, decidedRedis, 0.25},
	}

	for _, f := range fixtures {
		if got := jaccardOf(f.a, f.b); math.Abs(got-f.want) > 0.001 {
			t.Errorf("%s: Jaccard(%q, %q) = %.4f, want %.4f", f.name, f.a, f.b, got, f.want)
		}
	}

	if jaccardOf(notUsingRedis, decidedRedis) >= DefaultJaccardThreshold {
		t.Errorf("negated fixture scored at or above DefaultJaccardThreshold (%v); the documented deviation in Detect would not be needed",
			DefaultJaccardThreshold)
	}
}

// TestDetectSameTopicDifferentValue is the phase's Test 1.
func TestDetectSameTopicDifferentValue(t *testing.T) {
	found, id := detectionFor(DefaultJaccardThreshold, decidedPostgres, existingMemory(decidedMySQL)...)
	if !found {
		t.Fatalf("expected a contradiction between %q and %q", decidedPostgres, decidedMySQL)
	}
	if id != existingID {
		t.Errorf("expected the contradicting memory %q, got %q", existingID, id)
	}
}

// TestDetectSameTopicNoSignal is the phase's Test 2. Both memories are about the
// same subject and the verdict is still "no contradiction", but for two different
// reasons depending on the threshold, and both are asserted: at the default 0.4 the
// Jaccard gate rejects the pair (0.333), while below it the gate lets the pair
// through and the signal check rejects it -- no negation targets Postgres, and the
// predicates do not match ("use" against "migrate").
func TestDetectSameTopicNoSignal(t *testing.T) {
	for _, threshold := range []float64{0.2, 0.3, DefaultJaccardThreshold, 0.7} {
		found, id := detectionFor(threshold, migratePostgres, existingMemory(decidedPostgres)...)
		if found {
			t.Errorf("threshold %v: added detail was reported as a contradiction with %q", threshold, id)
		}

		// The reverse order is "no contradiction" for the same two reasons, and
		// it is the order a pull from a plane would produce: the older memory
		// arrives as the candidate.
		found, id = detectionFor(threshold, decidedPostgres, existingMemory(migratePostgres)...)
		if found {
			t.Errorf("threshold %v: the reverse order was reported as a contradiction with %q", threshold, id)
		}
	}
}

// TestDetectDifferentTopic is the phase's Test 3: below the threshold nothing is
// compared, so no signal can fire no matter how the two sentences are worded.
func TestDetectDifferentTopic(t *testing.T) {
	for _, threshold := range []float64{0.1, DefaultJaccardThreshold} {
		found, id := detectionFor(threshold, skyIsBlue, existingMemory(decidedMySQL)...)
		if found {
			t.Errorf("threshold %v: %q was reported as contradicting %q (id %q)", threshold, skyIsBlue, decidedMySQL, id)
		}
	}
}

// TestDetectNegationTargetsExistingToken is the phase's Test 4: the pair scores
// 0.250, below the default gate, and is still a contradiction because "not" reaches
// "redis", a token the existing memory is about.
func TestDetectNegationTargetsExistingToken(t *testing.T) {
	found, id := detectionFor(DefaultJaccardThreshold, notUsingRedis, existingMemory(decidedRedis)...)
	if !found {
		t.Fatalf("expected %q to contradict %q", notUsingRedis, decidedRedis)
	}
	if id != existingID {
		t.Errorf("expected the contradicting memory %q, got %q", existingID, id)
	}
}

// TestDetectNegationWithoutTarget: a negation only counts when it reaches a token
// the other memory contains. In this pair "not" is followed by "using" and "redis",
// and neither is a token of the Postgres memory.
func TestDetectNegationWithoutTarget(t *testing.T) {
	found, id := detectionFor(DefaultJaccardThreshold, notUsingRedis, existingMemory(decidedPostgres)...)
	if found {
		t.Errorf("%q was reported as contradicting %q (id %q); its negation has no target there",
			notUsingRedis, decidedPostgres, id)
	}
}

// TestDetectValueSwapOnlyWhenTheObjectDiffers covers both sides of the value-swap
// rule's narrowness: a shared predicate with the same object is not a contradiction
// even when the qualifiers differ, while the same predicate with a different object
// is one.
func TestDetectValueSwapOnlyWhenTheObjectDiffers(t *testing.T) {
	sameObject := []struct{ candidate, existing string }{
		{"We use Postgres for billing", "We use Postgres for analytics"},
		{"We use Postgres for analytics and reporting", "We use Postgres for billing"},
	}
	for _, f := range sameObject {
		if found, id := detectionFor(DefaultJaccardThreshold, f.candidate, existingMemory(f.existing)...); found {
			t.Errorf("%q was reported as contradicting %q (id %q); both pick Postgres, only the qualifier differs",
				f.candidate, f.existing, id)
		}
	}

	diffObject := []struct{ candidate, existing string }{
		{"We use Postgres for billing", "We use MySQL for billing"},
		{"We decided to use a Postgres cluster", "We decided to use MySQL"},
	}
	for _, f := range diffObject {
		found, id := detectionFor(DefaultJaccardThreshold, f.candidate, existingMemory(f.existing)...)
		if !found {
			t.Errorf("expected %q to contradict %q", f.candidate, f.existing)
			continue
		}
		if id != existingID {
			t.Errorf("expected the contradicting memory %q, got %q", existingID, id)
		}
	}
}

// TestDetectSkipsSelfAndEmptyContent covers the inputs that must not panic or
// produce a match: a memory compared against itself, an empty candidate set, empty
// content on either side. The last case proves an empty entry is skipped rather
// than aborting the scan, so a later memory can still be found.
func TestDetectSkipsSelfAndEmptyContent(t *testing.T) {
	if found, id := detectionFor(DefaultJaccardThreshold, decidedPostgres,
		memory(candidateID, "session-b", "agent-b", decidedMySQL)); found {
		t.Errorf("a memory compared against itself was reported as a contradiction (id %q)", id)
	}

	if found, id := detectionFor(DefaultJaccardThreshold, decidedPostgres); found {
		t.Errorf("an empty candidate set was reported as a contradiction (id %q)", id)
	}

	if found, id := detectionFor(DefaultJaccardThreshold, "", existingMemory(decidedMySQL)...); found {
		t.Errorf("an empty candidate content was reported as a contradiction (id %q)", id)
	}

	entries := []store.MemoryEntry{
		memory("empty-memory", "session-a", "agent-a", "   "),
		memory(existingID, "session-a", "agent-a", decidedMySQL),
	}
	if found, id := detectionFor(DefaultJaccardThreshold, decidedPostgres, entries...); !found || id != existingID {
		t.Errorf("expected the memory after an empty one (%q) to be found, got found=%v id=%q", existingID, found, id)
	}
}

// TestDetectReturnsFirstContradictionInSliceOrder pins the contract that the
// caller's ordering picks the id when more than one memory disagrees, since callers
// pass a set that retrieval has already ranked.
func TestDetectReturnsFirstContradictionInSliceOrder(t *testing.T) {
	entries := []store.MemoryEntry{
		memory("first", "session-a", "agent-a", decidedMySQL),
		memory("second", "session-c", "agent-c", "We use MySQL instead of Postgres"),
	}

	found, id := detectionFor(DefaultJaccardThreshold, decidedPostgres, entries...)
	if !found || id != "first" {
		t.Errorf("expected the first contradiction %q, got found=%v id=%q", "first", found, id)
	}
}

// TestDetectAcrossThresholds is the phase's "run with multiple threshold values and
// assert behaviour changes as expected" requirement, over all four fixtures at
// once. Two behaviours are encoded here and are the reason the table exists rather
// than four separate assertions:
//
//   - the swapped-value pair is lost once the gate rises above its own 0.667, so a
//     stricter threshold costs a real contradiction;
//   - the negated pair is detected at every threshold including 1.0, because its
//     signal is gated on the target token rather than on token overlap.
func TestDetectAcrossThresholds(t *testing.T) {
	thresholds := []float64{0.2, DefaultJaccardThreshold, 0.7, 1.0}

	fixtures := []struct {
		name      string
		candidate string
		existing  string
		want      []bool
	}{
		{"same topic, different value", decidedPostgres, decidedMySQL, []bool{true, true, false, false}},
		{"same topic, added detail", migratePostgres, decidedPostgres, []bool{false, false, false, false}},
		{"different topic", skyIsBlue, decidedMySQL, []bool{false, false, false, false}},
		{"explicit negation", notUsingRedis, decidedRedis, []bool{true, true, true, true}},
	}

	for _, f := range fixtures {
		for i, threshold := range thresholds {
			found, id := detectionFor(threshold, f.candidate, existingMemory(f.existing)...)
			if found != f.want[i] {
				t.Errorf("threshold %v, %s: Detect(%q, %q) = %v (id %q), want %v",
					threshold, f.name, f.candidate, f.existing, found, id, f.want[i])
			}
		}
	}
}

// TestNewContradictionDetectorThreshold is the constructor's contract: a value
// Jaccard can never reach falls back to the default instead of silently disabling
// detection, and every usable value is kept exactly as given.
func TestNewContradictionDetectorThreshold(t *testing.T) {
	for _, invalid := range []float64{0, -1, 1.5, 100} {
		if got := NewContradictionDetector(invalid).Threshold(); got != DefaultJaccardThreshold {
			t.Errorf("NewContradictionDetector(%v).Threshold() = %v, want the default %v", invalid, got, DefaultJaccardThreshold)
		}
	}

	for _, valid := range []float64{0.1, DefaultJaccardThreshold, 0.75, 1.0} {
		if got := NewContradictionDetector(valid).Threshold(); got != valid {
			t.Errorf("NewContradictionDetector(%v).Threshold() = %v, want %v", valid, got, valid)
		}
	}
}
