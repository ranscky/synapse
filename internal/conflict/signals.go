// The two contradiction signals and the vocabularies they need, split out of
// detector.go to keep each file inside the project's 300-line cap. Every rule here
// is deliberately narrow; each one's doc comment says which failure it prefers.
package conflict

// decisionPredicates are the "same predicate word (verb)" half of the value-swap
// rule. There is no part-of-speech tagger in this project, so a predicate has to be
// recognised rather than parsed; a curated list of verbs that show up in
// decision-shaped memories keeps the rule conservative in the only way available
// here. It can miss a contradiction whose verb is not listed -- the cheaper
// failure, and the same one supersession's doc comment describes -- but it cannot
// invent one out of two sentences that merely share a noun.
var decisionPredicates = map[string]bool{
	"adopt":     true,
	"adopted":   true,
	"choose":    true,
	"chose":     true,
	"chosen":    true,
	"decide":    true,
	"decided":   true,
	"deploy":    true,
	"deployed":  true,
	"drop":      true,
	"dropped":   true,
	"host":      true,
	"hosted":    true,
	"keep":      true,
	"kept":      true,
	"migrate":   true,
	"migrated":  true,
	"pick":      true,
	"picked":    true,
	"prefer":    true,
	"replace":   true,
	"replaced":  true,
	"run":       true,
	"running":   true,
	"select":    true,
	"selected":  true,
	"store":     true,
	"stored":    true,
	"switch":    true,
	"switched":  true,
	"target":    true,
	"targeting": true,
	"use":       true,
	"uses":      true,
	"using":     true,
}

// stopwords are ignored when identifying what a predicate takes as its object and
// what a negation token applies to. They carry no topic information, and skipping
// them is what lets "we use Postgres" and "we use a MySQL cluster" compare Postgres
// against MySQL instead of "Postgres" against "a".
var stopwords = map[string]bool{
	"a":     true,
	"about": true,
	"after": true,
	"all":   true,
	"also":  true,
	"an":    true,
	"and":   true,
	"any":   true,
	"are":   true,
	"as":    true,
	"at":    true,
	"be":    true,
	"but":   true,
	"by":    true,
	"can":   true,
	"for":   true,
	"from":  true,
	"has":   true,
	"have":  true,
	"in":    true,
	"into":  true,
	"is":    true,
	"it":    true,
	"its":   true,
	"of":    true,
	"on":    true,
	"onto":  true,
	"or":    true,
	"our":   true,
	"over":  true,
	"than":  true,
	"that":  true,
	"the":   true,
	"their": true,
	"them":  true,
	"then":  true,
	"there": true,
	"these": true,
	"they":  true,
	"this":  true,
	"those": true,
	"to":    true,
	"up":    true,
	"us":    true,
	"was":   true,
	"we":    true,
	"were":  true,
	"will":  true,
	"with":  true,
	"you":   true,
	"your":  true,
}

// negatesTokenIn reports whether the ordered tokens A contain a negation token that
// targets a token of B within negationLookahead positions. Both ends have to be
// real: the negation word must be one of negationTokens, and what it reaches must
// be a non-stopword that the other memory actually contains, so "we are not sure
// yet" cannot contradict anything on its own. Only A is searched for the negation,
// matching the direction this phase specifies -- a candidate that affirms what an
// existing memory negates is not detected, a known one-sided limitation.
func negatesTokenIn(orderedA []string, setB map[string]bool) bool {
	for i, tok := range orderedA {
		if !negationTokens[tok] {
			continue
		}

		last := i + negationLookahead
		if last >= len(orderedA) {
			last = len(orderedA) - 1
		}
		for j := i + 1; j <= last; j++ {
			target := orderedA[j]
			if stopwords[target] {
				continue
			}
			if setB[target] {
				return true
			}
		}
	}
	return false
}

// swapsValueForPredicate reports whether A and B use the same known predicate verb
// but give it a different direct object -- "we decided to use Postgres" against "we
// decided to use MySQL".
//
// This is the rough approximation the phase calls for, and it is intentionally
// narrow. It compares the *first* occurrence of each shared predicate only, and
// only the first non-stopword token after it, so a memory that lists several values
// ("we use Postgres and we use Redis") is judged on the value it leads with instead
// of firing on whichever of its objects happens to differ. That narrowness is also
// the honest limit of this rule: it does not fire for a pair like "we use Postgres
// for billing" and "we use Postgres for analytics", where both predicates take the
// same object and only the qualifier differs, but it equally misses a contradiction
// expressed with a verb that is not in decisionPredicates.
func swapsValueForPredicate(orderedA, orderedB []string, setB map[string]bool) bool {
	for _, predicate := range sharedPredicates(orderedA, setB) {
		posA := firstIndex(orderedA, predicate)
		posB := firstIndex(orderedB, predicate)
		if posA < 0 || posB < 0 {
			continue
		}

		objectA, okA := objectAfter(orderedA, posA)
		objectB, okB := objectAfter(orderedB, posB)
		if !okA || !okB {
			continue
		}
		if objectA != objectB {
			return true
		}
	}
	return false
}

// sharedPredicates returns the tokens of A that are both a known predicate verb and
// present in B, in the order they appear in A and without duplicates, so the signal
// check stays deterministic.
func sharedPredicates(orderedA []string, setB map[string]bool) []string {
	var (
		seen   = make(map[string]bool, len(orderedA))
		shared []string
	)
	for _, tok := range orderedA {
		if seen[tok] || !decisionPredicates[tok] || !setB[tok] {
			continue
		}
		seen[tok] = true
		shared = append(shared, tok)
	}
	return shared
}

// firstIndex returns the position of target in tokens, or -1 when it is absent.
func firstIndex(tokens []string, target string) int {
	for i, tok := range tokens {
		if tok == target {
			return i
		}
	}
	return -1
}

// objectAfter returns the first non-stopword token following position i, the
// heuristic stand-in for the object of the predicate at i. It reports false at the
// end of a memory: a predicate with no object at all is not evidence of anything,
// so the caller moves on rather than guessing.
func objectAfter(tokens []string, i int) (string, bool) {
	for j := i + 1; j < len(tokens); j++ {
		if stopwords[tokens[j]] {
			continue
		}
		return tokens[j], true
	}
	return "", false
}
