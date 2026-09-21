// Tests for the tokenization every rule is built on, plus the two-set arithmetic.
// Split from detector_test.go to keep each file inside the project's 300-line cap,
// mirroring how tokens.go is split from detector.go.
package conflict

import (
	"math"
	"reflect"
	"testing"
)

// TestTokenizeSplitsOnWhitespaceAndPunctuation is the step-(a) contract: lowercase,
// split on punctuation as well as spaces, and keep stopwords as tokens (they are
// filtered later, by the signal rules that care, not by the tokenizer).
func TestTokenizeSplitsOnWhitespaceAndPunctuation(t *testing.T) {
	got := tokenize("We decided to use Postgres! (not MySQL, or Redis)")
	want := []string{"we", "decided", "to", "use", "postgres", "not", "mysql", "or", "redis"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenize() = %q, want %q", got, want)
	}
}

// TestTokenizeKeepsOrderAndDuplicates: positions are needed by the negation rule, so
// tokenize must not deduplicate. tokenSet is where deduplication happens, and that
// set is what the gate compares.
func TestTokenizeKeepsOrderAndDuplicates(t *testing.T) {
	got := tokenize("use Postgres, use it twice: use")
	want := []string{"use", "postgres", "use", "it", "twice", "use"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenize() = %q, want %q", got, want)
	}

	if n := len(tokenSet(got)); n != 4 {
		t.Errorf("tokenSet(tokens) has %d members, want 4", n)
	}
}

// TestTokenizeNothingToSplit: an empty or punctuation-only memory produces no
// tokens at all, which is what makes Detect skip it instead of treating it as a
// topic that matches everything.
func TestTokenizeNothingToSplit(t *testing.T) {
	for _, content := range []string{"", "   ", "...", "!!?", "\n\t"} {
		if got := tokenize(content); len(got) != 0 {
			t.Errorf("tokenize(%q) = %q, want no tokens", content, got)
		}
	}
}

// TestJaccardSimilarityEdges pins the measure's boundaries: identical sets, disjoint
// sets, a proper subset, and the empty pair -- which is 0 and not NaN, because the
// gate has to decide something about an empty memory rather than propagate a NaN.
func TestJaccardSimilarityEdges(t *testing.T) {
	set := func(tokens ...string) map[string]bool {
		return tokenSet(tokens)
	}

	cases := []struct {
		name string
		a, b map[string]bool
		want float64
	}{
		{"identical", set("we", "use", "postgres"), set("we", "use", "postgres"), 1.0},
		{"disjoint", set("postgres"), set("mysql"), 0.0},
		{"subset", set("we", "use", "postgres"), set("we", "use", "postgres", "mysql"), 0.75},
		{"both empty", set(), set(), 0.0},
		{"one empty", set("postgres"), set(), 0.0},
	}

	for _, c := range cases {
		if got := jaccardSimilarity(c.a, c.b); math.Abs(got-c.want) > 0.0001 {
			t.Errorf("%s: jaccardSimilarity = %v, want %v", c.name, got, c.want)
		}
		if got := intersectionSize(c.a, c.b); got != intersectionSize(c.b, c.a) {
			t.Errorf("%s: intersectionSize is not symmetric: %d against %d", c.name, got, intersectionSize(c.b, c.a))
		}
	}
}
