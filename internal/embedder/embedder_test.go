package embedder

import (
	"context"
	"os"
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

func TestONNXEmbedderProducesRealSemanticEmbeddings(t *testing.T) {
	modelPath := "../../models/all-MiniLM-L6-v2/model.onnx"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skip("ONNX model file not present, skipping real inference test")
	}

	embedder, err := NewEmbedder("onnx", "", modelPath, "")
	if err != nil {
		t.Fatalf("failed to create ONNX embedder: %v", err)
	}

	onnxEmbedder, ok := embedder.(*ONNXEmbedder)
	if !ok {
		t.Fatalf("expected *ONNXEmbedder, got %T (model file path may be wrong, fell back to hash embedder)", embedder)
	}
	defer onnxEmbedder.Close()

	ctx := context.Background()

	// Three sentences: the first two are paraphrases of each other (should
	// be highly similar), the third is about a completely unrelated topic
	// (should be much less similar to either). This is the actual proof
	// that real semantic meaning is being captured, not just "some vector
	// comes out" - a broken-but-not-crashing implementation could still
	// produce 384 numbers that mean nothing.
	embedding1, err := onnxEmbedder.Embed(ctx, "The cat sat on the mat.")
	if err != nil {
		t.Fatalf("failed to embed sentence 1: %v", err)
	}

	embedding2, err := onnxEmbedder.Embed(ctx, "A feline was resting on the rug.")
	if err != nil {
		t.Fatalf("failed to embed sentence 2: %v", err)
	}

	embedding3, err := onnxEmbedder.Embed(ctx, "The stock market crashed sharply today.")
	if err != nil {
		t.Fatalf("failed to embed sentence 3: %v", err)
	}

	if len(embedding1) != 384 {
		t.Fatalf("expected embedding length 384, got %d", len(embedding1))
	}

	similarPairSim := cosineSimilarityForTest(embedding1, embedding2)
	dissimilarPairSim := cosineSimilarityForTest(embedding1, embedding3)

	t.Logf("Similarity (cat/mat vs feline/rug, paraphrases):     %.4f", similarPairSim)
	t.Logf("Similarity (cat/mat vs stock market, unrelated):     %.4f", dissimilarPairSim)

	if similarPairSim <= dissimilarPairSim {
		t.Errorf("expected paraphrased sentences to be MORE similar than unrelated ones, but got similar=%.4f <= dissimilar=%.4f",
			similarPairSim, dissimilarPairSim)
	}

	// A reasonably high bar for genuine paraphrase similarity - real
	// sentence embedding models typically score well above 0.5 for clear
	// paraphrases like this pair.
	if similarPairSim < 0.5 {
		t.Errorf("expected paraphrase similarity > 0.5, got %.4f - embeddings may not be semantically meaningful", similarPairSim)
	}
}

// cosineSimilarityForTest is a local copy for test verification purposes -
// deliberately independent of any production cosineSimilarity implementation
// elsewhere in the codebase, so this test can't accidentally pass due to a
// shared bug in both places.
func cosineSimilarityForTest(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (sqrtForTest(normA) * sqrtForTest(normB))
}

func sqrtForTest(x float64) float64 {
	if x == 0 {
		return 0
	}
	z := x
	for i := 0; i < 50; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}