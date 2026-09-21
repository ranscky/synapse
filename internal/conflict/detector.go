// Package conflict detects contradictions between memories written by different
// agents, in different sessions -- "we decided to use Postgres" recorded on one
// node and "we decided to use MySQL" recorded on another -- and reports the id of
// the memory a new candidate disagrees with.
//
// This is not a replacement for internal/supersession, and the two deliberately do
// not overlap. Supersession resolves a contradiction *inside* one session: it
// requires the same session id, the same memory type, cosine similarity inside a
// tuned band, and an explicit replacement phrase ("switched to", "instead of") in
// the new memory. None of that machinery can see a contradiction across a shared
// control plane. There the two versions of a decision were written by different
// agents, in different sessions, and the newer one carries no phrase saying it
// replaces the older one -- "We decided to use MySQL" is a complete sentence that
// never mentions Postgres. Cross-agent candidates do not even arrive with a
// comparable embedding at hand, so this package works on the text itself: Jaccard
// similarity over token sets to decide whether two memories are about the same
// thing at all, then two narrow signals to decide whether they disagree.
//
// Every rule here is a heuristic and is documented as one. There is no model call,
// matching the project's existing heuristic-first approach (see
// store.DetectMemoryType and supersession.HasContradictionSignal), which keeps the
// cost of running this over a candidate set predictable.
//
// The detector is stateless apart from its threshold, so one instance is safe to
// share between goroutines.
package conflict

import (
	"synapse/internal/store"
)

// DefaultJaccardThreshold is the token-overlap level at which two memories are
// considered to be about the same topic, and therefore worth checking for a
// contradiction. Below it, they are simply different subjects and nothing is
// compared. 0.4 is a starting guess, not a tuned value: the four fixtures in
// detector_test.go that motivated it score 0.667 (a swapped value), 0.333 (added
// detail), 0.000 (unrelated) and 0.250 (an explicit negation), which is why the
// negation rule below has to satisfy this gate on its own rather than be skipped
// by it.
//
// This value must stay in step with config's conflict-jaccard-threshold default.
// The two cannot be shared as one constant: conflict imports store, store imports
// config, so config importing this package would be an import cycle.
const DefaultJaccardThreshold = 0.4

// negationLookahead is how many tokens after a negation token are searched for the
// token it negates. It is not 1 (strict adjacency) because no stemming is done
// here: "We are not using Redis" negates Redis, but the token sitting directly
// after "not" is "using", which does not equal the "use" of "We decided to use
// Redis". A short window sees the real target without reaching across a clause
// boundary in practice -- any token inside it that is a stopword, or that the other
// memory does not contain, is simply skipped.
const negationLookahead = 3

// negationTokens are the words that, when they appear in a candidate memory and
// target a token another memory contains, signal a disagreement rather than an
// elaboration. This is the phase spec's fixed list, kept literal so the behaviour
// stays auditable: it is not a general negation detector and does not try to be
// one.
var negationTokens = map[string]bool{
	"not":      true,
	"no":       true,
	"never":    true,
	"instead":  true,
	"switched": true,
	"dropped":  true,
}

// ContradictionDetector decides whether a candidate memory contradicts one of the
// memories it is compared against, using Jaccard similarity over token sets.
type ContradictionDetector struct {
	jaccardThreshold float64
}

// NewContradictionDetector returns a detector that requires token overlap of at
// least jaccardThreshold before it will look for a contradiction signal. A
// non-positive threshold or one above 1 could never be satisfied meaningfully --
// Jaccard is a ratio in [0,1] -- so both fall back to DefaultJaccardThreshold
// rather than silently disabling detection.
func NewContradictionDetector(jaccardThreshold float64) *ContradictionDetector {
	if jaccardThreshold <= 0 || jaccardThreshold > 1 {
		jaccardThreshold = DefaultJaccardThreshold
	}
	return &ContradictionDetector{jaccardThreshold: jaccardThreshold}
}

// Threshold returns the token-overlap level this detector gates on.
func (d *ContradictionDetector) Threshold() float64 {
	return d.jaccardThreshold
}

// Detect reports whether candidate contradicts one of the existing memories,
// returning that memory's id. It returns (false, "") when nothing in existing
// disagrees with the candidate.
//
// The algorithm, in order:
//
//   - tokenize the candidate and deduplicate into a set, and do the same for each
//     existing memory;
//   - compute the Jaccard index |A∩B| / |A∪B| and skip any memory below the
//     threshold -- a different topic cannot contradict;
//   - for a memory above it, look for either of two signals: an explicit negation
//     in the candidate aimed at a token that memory contains, or the same predicate
//     verb taking a different object.
//
// One deliberate deviation from that order is worth naming outright, because it is
// the difference between detecting an explicit negation and not: a negation aimed
// at a token the other memory is *about* is itself evidence that the two are about
// the same topic, so it satisfies the gate instead of being skipped by it. "We are
// not using Redis" and "We decided to use Redis" share only 2 of 8 tokens (Jaccard
// 0.250), so a strict gate would dismiss that pair as unrelated before the negation
// that makes it a contradiction was ever consulted. See detector_test.go, which
// pins both that number and the resulting behaviour at four thresholds.
//
// existing is scanned in the order given and the first contradiction wins. That is
// deliberately the caller's ordering rather than a relevance computation here:
// callers pass whatever candidate set they already have, and retrieval hands back
// one already ranked by score. Nothing is filtered on AgentID or SessionID -- the
// caller owns supplying a cross-agent, cross-session candidate set, and a memory is
// compared on its text alone, which is also what makes this testable without a
// store. A memory carrying the candidate's own id is skipped.
func (d *ContradictionDetector) Detect(candidate store.MemoryEntry, existing []store.MemoryEntry) (bool, string) {
	orderedA := tokenize(candidate.Content)
	setA := tokenSet(orderedA)
	if len(setA) == 0 {
		return false, ""
	}

	for _, mem := range existing {
		if mem.ID != "" && mem.ID == candidate.ID {
			continue
		}

		orderedB := tokenize(mem.Content)
		setB := tokenSet(orderedB)
		if len(setB) == 0 {
			continue
		}

		negated := negatesTokenIn(orderedA, setB)
		if jaccardSimilarity(setA, setB) < d.jaccardThreshold && !negated {
			continue
		}

		if negated || swapsValueForPredicate(orderedA, orderedB, setB) {
			return true, mem.ID
		}
	}

	return false, ""
}
