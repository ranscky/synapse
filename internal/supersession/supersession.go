
// Package supersession decides whether a newly-written memory should mark
// an existing memory as no longer current -- e.g. "we migrated to MongoDB"
// superseding an earlier "we use PostgreSQL" decision -- rather than the
// two simply sitting side by side as if both were still true.
//
// This is deliberately conservative. False supersession (silently treating
// a fact that's still true as stale) is worse than a duplicate memory
// sitting around doing no harm, so nothing gets marked superseded unless
// every one of several independent signals agrees. See
// FindSupersededCandidate's doc comment for the full gate.
package supersession

import (
	"strings"

	"synapse/internal/store"
)

// eligibleMemoryTypes restricts supersession to memory types with a clean
// "this used to be true, now this is true instead" semantics. Preference,
// context, and error memories are deliberately left out for now -- not
// because they can never be superseded, but because whether that's even
// desirable is a real product question to validate against actual usage,
// not something to assume upfront. Decision and fact are the two
// unambiguous cases: a database choice, or a stated fact, either changed
// or it didn't.
var eligibleMemoryTypes = map[string]bool{
	"decision": true,
	"fact":     true,
}

// contradictionPhrases are explicit, deterministic linguistic signals that
// a new memory intentionally replaces a prior one, rather than merely
// being topically related to it. Two topically-similar facts -- "Postgres
// for billing" and "MongoDB for app data" -- can both be true at once, so
// cosine similarity alone can't distinguish supersession from
// elaboration; something in the text itself has to say "this replaces
// that." This deliberately skips an LLM call too, matching the project's
// existing heuristic-first approach (see store.DetectMemoryType) and
// keeping latency and cost predictable rather than adding a model call to
// every write.
var contradictionPhrases = []string{
	"instead of",
	"instead,",
	"no longer",
	"not anymore",
	"switched to",
	"switched from",
	"migrated to",
	"migrated from",
	"moved from",
	"moved to",
	"replaced",
	"replacing",
	"actually we",
	"actually,",
	"correction:",
	"update:",
	"changed to",
	"changed from",
	"used to",
	"now use",
	"now using",
	"we no longer",
}

// HasContradictionSignal reports whether content contains an explicit
// linguistic marker of replacing prior information, as opposed to merely
// adding to or elaborating on it. Exported so the write path (or a future
// classifier) can check this independently of the full candidate search
// below.
func HasContradictionSignal(content string) bool {
	lower := strings.ToLower(content)
	for _, phrase := range contradictionPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// FindSupersededCandidate looks for an existing memory that newMemory
// should mark as superseded, returning its ID, or "" if nothing qualifies.
//
// All of the following must hold:
//   - newMemory's type is one of eligibleMemoryTypes
//   - newMemory has an embedding to compare with
//   - newMemory's content contains an explicit contradiction signal
//   - a candidate is in the same session as newMemory
//   - a candidate has the same memory type as newMemory
//   - a candidate is not already superseded (SupersededBy == "")
//   - cosine similarity between the two embeddings falls within
//     [minSimilarity, maxSimilarity] -- topically related but not a
//     near-duplicate. Near-duplicates are dedup's job (a higher
//     threshold) and represent restating the same fact, not replacing it.
//
// candidates is not queried fresh here -- callers pass whatever
// same-session candidates they already have on hand (e.g. the pool
// internal/retrieval just fetched for this turn), rather than this
// package issuing its own store query. In practice this works out
// naturally: a message containing a contradiction signal about, say, a
// database choice will itself embed close to prior "database decision"
// memories, so those are exactly the memories semantic retrieval would
// have already surfaced as candidates for this turn.
//
// If more than one existing memory qualifies, only the single most
// similar one is returned -- superseding several memories off one
// heuristic match increases the blast radius of a false positive.
func FindSupersededCandidate(newMemory store.MemoryEntry, candidates []store.MemoryEntry, minSimilarity, maxSimilarity float64) string {
	if !eligibleMemoryTypes[newMemory.MemoryType] {
		return ""
	}
	if len(newMemory.Embedding) == 0 {
		return ""
	}
	if !HasContradictionSignal(newMemory.Content) {
		return ""
	}

	bestID := ""
	bestSimilarity := -2.0 // cosine similarity is bounded to [-1, 1]; anything that qualifies will beat this

	for _, candidate := range candidates {
		if candidate.ID == newMemory.ID {
			continue
		}
		if candidate.SessionID != newMemory.SessionID {
			continue
		}
		if candidate.MemoryType != newMemory.MemoryType {
			continue
		}
		if candidate.SupersededBy != "" {
			continue
		}
		if len(candidate.Embedding) == 0 {
			continue
		}

		sim := store.CosineSimilarity(newMemory.Embedding, candidate.Embedding)
		if sim < minSimilarity || sim > maxSimilarity {
			continue
		}
		if sim > bestSimilarity {
			bestSimilarity = sim
			bestID = candidate.ID
		}
	}

	return bestID
}