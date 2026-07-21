package embedder

import (
	"context"
	"math"
	"testing"
)

// TestEmbedDiscriminativePower isolates the embedder from the rest of the
// pipeline (no scorer, no dedup, no store) and checks a basic sanity
// property: two unrelated sentences should score meaningfully lower on
// cosine similarity than two near-identical sentences. If this fails, the
// bug is in the tokenizer or the ONNX model's pooling -- not in dedup.go,
// which was already verified to do correct pairwise comparison.
//
// Run with: go test -run TestEmbedDiscriminativePower -v ./internal/embedder
func TestEmbedDiscriminativePower(t *testing.T) {
	emb, err := NewEmbedder("onnx", "", "../../models/all-MiniLM-L6-v2/model.onnx", "")
	if err != nil {
		t.Fatalf("failed to create embedder: %v", err)
	}
	if closer, ok := emb.(interface{ Close() error }); ok {
		defer closer.Close()
	}

	ctx := context.Background()

	unrelatedA := "What's a good recipe for jollof rice?"
	unrelatedB := "Your name is Kofi and you are building Synapse."
	nearIdenticalA := "My name is Kofi and I am building a Go reverse proxy called Synapse."
	nearIdenticalB := "Just to confirm, my name is Kofi and I am working on Synapse, a Go reverse proxy."

	vecUnrelatedA, err := emb.Embed(ctx, unrelatedA)
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	vecUnrelatedB, err := emb.Embed(ctx, unrelatedB)
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	vecNearA, err := emb.Embed(ctx, nearIdenticalA)
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	vecNearB, err := emb.Embed(ctx, nearIdenticalB)
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}

	simUnrelated := cosineSim(vecUnrelatedA, vecUnrelatedB)
	simNearIdentical := cosineSim(vecNearA, vecNearB)

	t.Logf("Unrelated pair similarity   (jollof rice vs. Kofi/Synapse):        %.4f", simUnrelated)
	t.Logf("Near-identical pair similarity (Kofi/Synapse paraphrase):          %.4f", simNearIdentical)
	t.Logf("First 8 dims of unrelatedA embedding: %v", vecUnrelatedA[:8])
	t.Logf("First 8 dims of unrelatedB embedding: %v", vecUnrelatedB[:8])

	if simUnrelated > 0.85 {
		t.Errorf("UNRELATED sentences scored %.4f cosine similarity -- expected well below 0.85. "+
			"This points to a tokenizer or ONNX pooling bug, not a dedup.go bug.", simUnrelated)
	}

	if simNearIdentical < simUnrelated {
		t.Errorf("Near-identical pair (%.4f) scored LOWER than the unrelated pair (%.4f) -- "+
			"embeddings are not discriminative at all.", simNearIdentical, simUnrelated)
	}
}

func cosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0.0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i] * b[i])
		normA += float64(a[i] * a[i])
		normB += float64(b[i] * b[i])
	}
	if normA == 0 || normB == 0 {
		return 0.0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}