package embedder

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"math"
	"net/http"
	"os"

	"synapse/internal/config"
)

// Embedder interface defines the contract for embedding generation
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ONNXEmbedder implements Embedder using ONNX runtime
type ONNXEmbedder struct {
	// Simplified for now - will implement proper ONNX integration later
	modelPath string
}

// OpenAIEmbedder implements Embedder using OpenAI API
type OpenAIEmbedder struct {
	apiKey string
	model  string
	client *http.Client
}

// HashEmbedder implements Embedder using deterministic hashing
// Used as fallback when ONNX model is not available
type HashEmbedder struct {
	dimension int
}

// NewEmbedder creates an embedder based on configuration
func NewEmbedder(cfg config.EmbedderConfig) (Embedder, error) {
	switch cfg.Type {
	case "onnx":
		return newONNXEmbedder(cfg.ONNXModelPath)
	case "openai":
		return NewOpenAIEmbedder(cfg.OpenAIAPIKey, cfg.OpenAIModel), nil
	case "hash":
		return NewHashEmbedder(384), nil // Same dimension as ONNX model
	default:
		slog.Warn("Unknown embedder type, falling back to hash embedder", "type", cfg.Type)
		return NewHashEmbedder(384), nil
	}
}

// newONNXEmbedder creates a new ONNX embedder
func newONNXEmbedder(modelPath string) (Embedder, error) {
	// Check if model file exists
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		slog.Warn("ONNX model file not found, falling back to hash embedder", "path", modelPath)
		return NewHashEmbedder(384), nil
	}

	slog.Info("ONNX embedder initialized", "model", modelPath)
	return &ONNXEmbedder{modelPath: modelPath}, nil
}

// Embed implements Embedder interface for ONNXEmbedder
func (e *ONNXEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// TODO: Implement proper ONNX integration
	// This is a simplified placeholder - real implementation would load the ONNX model
	// and run inference to generate embeddings
	
	// For now, return a placeholder embedding
	placeholder := make([]float32, 384)
	for i := range placeholder {
		placeholder[i] = float32(i%100) / 100.0
	}
	
	return placeholder, nil
}

// NewOpenAIEmbedder creates a new OpenAI embedder
func NewOpenAIEmbedder(apiKey, model string) *OpenAIEmbedder {
	return &OpenAIEmbedder{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{},
	}
}

// Embed implements Embedder interface for OpenAIEmbedder
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// TODO: Implement OpenAI API call
	// This would normally make an HTTP request to OpenAI's embedding API
	// For now, return a placeholder embedding
	
	slog.Debug("Using OpenAI embedder (placeholder)", "model", e.model)
	
	placeholder := make([]float32, 384)
	for i := range placeholder {
		placeholder[i] = float32(i%100) / 100.0
	}
	
	return placeholder, nil
}

// NewHashEmbedder creates a new hash-based embedder
func NewHashEmbedder(dimension int) *HashEmbedder {
	return &HashEmbedder{dimension: dimension}
}

// Embed implements Embedder interface for HashEmbedder
func (e *HashEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	// Create deterministic embedding using SHA256 hash
	hash := sha256.Sum256([]byte(text))
	
	embedding := make([]float32, e.dimension)
	for i := 0; i < e.dimension; i++ {
		// Use bytes from hash to generate pseudo-random values
		byteIndex := i % len(hash)
		// Convert byte to float32 in range [-1, 1]
		embedding[i] = (float32(hash[byteIndex]) / 127.5) - 1.0
	}
	
	// Normalize the vector
	return normalizeVector(embedding), nil
}

// normalizeVector normalizes a vector to unit length
func normalizeVector(v []float32) []float32 {
	var sum float32
	for _, val := range v {
		sum += val * val
	}
	
	if sum == 0 {
		return v
	}
	
	magnitude := float32(math.Sqrt(float64(sum)))
	normalized := make([]float32, len(v))
	for i, val := range v {
		normalized[i] = val / magnitude
	}
	
	return normalized
}