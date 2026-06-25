package classifier

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectedIntent Intent
		minConfidence  float64
		maxConfidence  float64
	}{
		{
			name:           "Debug intent - error message",
			input:          "There was an error in the system traceback",
			expectedIntent: Debug,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Debug intent - bug fix",
			input:          "I need to fix this broken code, there's a bug",
			expectedIntent: Debug,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Plan intent - architecture",
			input:          "Let's design the architecture and plan the roadmap",
			expectedIntent: Plan,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Plan intent - strategy",
			input:          "How should we structure this strategy?",
			expectedIntent: Plan,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Code intent - implementation",
			input:          "Implement the new function and add feature support",
			expectedIntent: Code,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Code intent - refactoring",
			input:          "Refactor this class method to improve performance",
			expectedIntent: Code,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Write intent - documentation",
			input:          "Write documentation and explain the readme file",
			expectedIntent: Write,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Write intent - blog post",
			input:          "Draft a blog post to summarize our findings",
			expectedIntent: Write,
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Generic intent - low confidence",
			input:          "This is just general conversation about random topics",
			expectedIntent: Generic,
			minConfidence:  0.0,
			maxConfidence:  0.05, // Should be below threshold
		},
		{
			name:           "Generic intent - no keywords",
			input:          "Hello world this is a test message",
			expectedIntent: Generic,
			minConfidence:  0.0,
			maxConfidence:  0.05, // Should be below threshold
		},
		{
			name:           "Mixed keywords - tied score, resolved by fixed priority order",
			input:          "Fix the error and implement the function",
			expectedIntent: Debug, // Debug and Code tie 2-2 ("fix"/"error" vs "implement"/"function");
			                       // resolved deterministically by priorityOrder in Classify, which favors Debug
			minConfidence:  0.1,
			maxConfidence:  1.0,
		},
		{
			name:           "Empty input",
			input:          "",
			expectedIntent: Generic,
			minConfidence:  0.0,
			maxConfidence:  0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Classify(tt.input)
			
			assert.Equal(t, tt.expectedIntent, result.Intent, "Intent should match expected")
			
			if tt.minConfidence == tt.maxConfidence && tt.minConfidence == 0.0 {
				assert.Equal(t, 0.0, result.Confidence, "Confidence should be 0.0 for generic/no keywords")
			} else {
				assert.True(t, result.Confidence >= tt.minConfidence, "Confidence should be >= %f, got %f", tt.minConfidence, result.Confidence)
				assert.True(t, result.Confidence <= tt.maxConfidence, "Confidence should be <= %f, got %f", tt.maxConfidence, result.Confidence)
			}
		})
	}
}

func TestClassifyEdgeCases(t *testing.T) {
	// Test with punctuation
	result := Classify("Fix this bug!")
	assert.Equal(t, Debug, result.Intent)
	assert.True(t, result.Confidence > 0.05)

	// Test with mixed case - CODE might win due to higher keyword count
	result = Classify("ERROR in the CODE implementation")
	// Either debug or code is acceptable here since both have keywords
	assert.True(t, result.Intent == Debug || result.Intent == Code)
	assert.True(t, result.Confidence > 0.05)
}
