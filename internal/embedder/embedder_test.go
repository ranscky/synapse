package embedder

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashEmbedder(t *testing.T) {
	embedder := NewHashEmbedder(384)

	// Test that the same text produces the same embedding
	text := "Hello, world!"
	embedding1, err := embedder.Embed(context.Background(), text)
	require.NoError(t, err)
	require.Len(t, embedding1, 384)

	embedding2, err := embedder.Embed(context.Background(), text)
	require.NoError(t, err)
	assert.Equal(t, embedding1, embedding2, "Same text should produce identical embeddings")

	// Test that different texts produce different embeddings
	differentText := "Goodbye, world!"
	embedding3, err := embedder.Embed(context.Background(), differentText)
	require.NoError(t, err)
	assert.NotEqual(t, embedding1, embedding3, "Different texts should produce different embeddings")

	// Test that embeddings are normalized (unit vectors)
	var magnitude float32
	for _, val := range embedding1 {
		magnitude += val * val
	}
	magnitude = float32(float64(magnitude))
	assert.InDelta(t, 1.0, magnitude, 0.001, "Embeddings should be normalized to unit vectors")

	// Test dimension consistency
	assert.Len(t, embedding1, 384, "Embedding should have correct dimension")
}

func TestHashEmbedderDeterministic(t *testing.T) {
	// Test that the hash embedder produces deterministic results
	embedder1 := NewHashEmbedder(384)
	embedder2 := NewHashEmbedder(384)

	text := "Test text for deterministic hashing"
	embedding1, err1 := embedder1.Embed(context.Background(), text)
	embedding2, err2 := embedder2.Embed(context.Background(), text)

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.Equal(t, embedding1, embedding2, "Hash embedder should be deterministic")
}

func TestNormalizeVector(t *testing.T) {
	// Test normalization function
	vector := []float32{3.0, 4.0} // Magnitude = 5
	normalized := normalizeVector(vector)
	
	var magnitude float32
	for _, val := range normalized {
		magnitude += val * val
	}
	assert.InDelta(t, 1.0, magnitude, 0.001, "Normalized vector should have unit magnitude")
	
	// Check proportions maintained
	assert.InDelta(t, 3.0/5.0, normalized[0], 0.001, "Proportions should be maintained")
	assert.InDelta(t, 4.0/5.0, normalized[1], 0.001, "Proportions should be maintained")
}