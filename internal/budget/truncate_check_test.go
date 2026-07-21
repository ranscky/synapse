package budget

import (
	"strings"
	"testing"
)

// TestTruncateToTokensOvershoot checks whether truncateToTokens' word-based
// estimate (1.3 tokens/word average) can produce content whose REAL
// tiktoken-counted length exceeds the target it was aiming for. The
// estimate is an average -- text with unusually long words, heavy
// punctuation, or code-like syntax can tokenize denser than 1.3
// tokens/word, meaning the "truncated" result might still blow the
// budget.Fill() half-budget slot it was supposed to fit into.
//
// Run with: go test -run TestTruncateToTokensOvershoot -v ./internal/budget
func TestTruncateToTokensOvershoot(t *testing.T) {
	testCases := []struct {
		name   string
		text   string
		target int
	}{
		{
			name:   "plain conversational text",
			text:   strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50),
			target: 100,
		},
		{
			name:   "long technical words",
			text:   strings.Repeat("internationalization deduplication authentication configuration serialization ", 30),
			target: 100,
		},
		{
			name:   "code-like content with heavy punctuation",
			text:   strings.Repeat("func (p *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) { ctx := r.Context(); }\n", 20),
			target: 100,
		},
		{
			name:   "very short words",
			text:   strings.Repeat("a i o u to be or not it is a b c d e f g h ", 40),
			target: 100,
		},
		{
			name:   "URLs and identifiers",
			text:   strings.Repeat("https://api.example.com/v1/synapse/session-abc-123-def-456?query=true&limit=20 ", 20),
			target: 100,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			truncated := truncateToTokens(tc.text, tc.target)
			realTokens, err := CountTokens(truncated, "cl100k_base")
			if err != nil {
				t.Fatalf("CountTokens failed: %v", err)
			}

			overshoot := realTokens - tc.target
			overshootPct := float64(overshoot) / float64(tc.target) * 100

			t.Logf("Target: %d tokens | Real (tiktoken) count: %d tokens | Overshoot: %d (%.1f%%)",
				tc.target, realTokens, overshoot, overshootPct)

			// Flag anything that overshoots by more than 10% as worth
			// knowing about. This isn't necessarily catastrophic -- budget.Fill
			// only uses this in the single edge case where the top-ranked
			// candidate alone exceeds the whole budget -- but a large,
			// consistent overshoot would mean that edge case doesn't
			// actually respect the budget it's supposed to.
			if overshootPct > 10.0 {
				t.Logf("WARNING: overshoot exceeds 10%% for %q", tc.name)
			}
		})
	}
}