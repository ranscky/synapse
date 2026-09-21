// Tokenization and the similarity measure itself, split out of detector.go to keep
// each file inside the project's 300-line cap. Everything here is pure text
// handling: no store types, no policy.
package conflict

import (
	"strings"
	"unicode"
)

// tokenize lowercases content and splits it on every character that is not a
// letter or a digit, which covers whitespace and punctuation in a single pass.
// Order is preserved and duplicates are kept: the negation rule needs token
// positions, so this cannot return a set. Callers that want the set build it with
// tokenSet.
func tokenize(content string) []string {
	var (
		tokens  []string
		current strings.Builder
	)

	for _, r := range strings.ToLower(content) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
			continue
		}
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}

// tokenSet deduplicates tokens into the comparison set of the Jaccard index.
func tokenSet(tokens []string) map[string]bool {
	set := make(map[string]bool, len(tokens))
	for _, tok := range tokens {
		set[tok] = true
	}
	return set
}

// intersectionSize counts the tokens two sets share.
func intersectionSize(a, b map[string]bool) int {
	// Walk the smaller set; the count is the same either way.
	if len(b) < len(a) {
		a, b = b, a
	}
	shared := 0
	for tok := range a {
		if b[tok] {
			shared++
		}
	}
	return shared
}

// jaccardSimilarity is |A∩B| / |A∪B|, with two sets that are both empty defined as
// 0 rather than the NaN that 0/0 would produce -- an empty memory is not "the same
// topic" as another empty one.
func jaccardSimilarity(a, b map[string]bool) float64 {
	shared := intersectionSize(a, b)
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
