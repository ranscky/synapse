package embedder

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// Embedder interface defines the contract for embedding generation
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// onnxSeqLen is the fixed token sequence length used for all ONNX inference
// calls. Text is tokenized, then padded or truncated to exactly this length
// (handled by WordPieceTokenizer.Encode). A fixed length lets the session's
// input/output tensors be allocated once and reused across every Embed()
// call, rather than rebuilding the session per call. 128 is comfortably
// large for chat-message-length text while keeping inference fast; MiniLM's
// real max is 512 if longer context is ever needed.
const onnxSeqLen = 128

// onnxEmbeddingDim is the output dimension of all-MiniLM-L6-v2's
// sentence_embedding output.
const onnxEmbeddingDim = 384

// ONNXEmbedder implements Embedder using real ONNX Runtime inference
// against a local all-MiniLM-L6-v2 model. The session and its input/output
// tensors are created once (in newONNXEmbedder) and reused for every Embed
// call, protected by a mutex since onnxruntime_go sessions are not
// documented as safe for concurrent Run() calls from multiple goroutines.
type ONNXEmbedder struct {
	modelPath string
	tokenizer *WordPieceTokenizer

	mu                  sync.Mutex
	session             *ort.AdvancedSession
	inputIDsTensor      *ort.Tensor[int64]
	attentionMaskTensor *ort.Tensor[int64]
	tokenTypeIDsTensor  *ort.Tensor[int64]
	outputTensor        *ort.Tensor[float32]
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

// onnxEnvInitialized tracks whether ort.InitializeEnvironment has been
// called, since onnxruntime_go's environment is process-global - calling
// InitializeEnvironment more than once (e.g. if multiple ONNXEmbedders are
// created) would be an error.
var (
	onnxEnvOnce sync.Once
	onnxEnvErr  error
)

// NewEmbedder creates an embedder based on configuration
func NewEmbedder(embedderType, openAIAPIKey, onnxModelPath, openAIModel string) (Embedder, error) {
	switch embedderType {
	case "onnx":
		return newONNXEmbedder(onnxModelPath)
	case "openai":
		return NewOpenAIEmbedder(openAIAPIKey, openAIModel), nil
	case "hash":
		return NewHashEmbedder(384), nil // Same dimension as ONNX model
	default:
		slog.Warn("Unknown embedder type, falling back to hash embedder", "type", embedderType)
		return NewHashEmbedder(384), nil
	}
}

// newONNXEmbedder creates a new ONNX embedder with a real, ready-to-use
// inference session. The vocabulary file is expected at "vocab.txt" in the
// same directory as the model file, matching how models/all-MiniLM-L6-v2/
// is organized in this project (model.onnx + vocab.txt together).

// resolveOrtLibPath locates the ONNX Runtime shared library. Resolution
// order: explicit override via SYNAPSE_ORT_LIB_PATH env var, then a
// platform-appropriate filename sitting next to the running executable
// (the bundled-release layout), falling back to just the bare filename so
// onnxruntime_go's own default search (including system lib dirs like
// /usr/local/lib on a dev machine) still applies if neither of the above
// exist.
func resolveOrtLibPath() string {
	if p := os.Getenv("SYNAPSE_ORT_LIB_PATH"); p != "" {
		return p
	}

	names := map[string]string{
		"linux":   "libonnxruntime.so",
		"darwin":  "libonnxruntime.dylib",
		"windows": "onnxruntime.dll",
	}
	libName := names[runtime.GOOS]

	if exe, err := os.Executable(); err == nil {
		if candidate := filepath.Join(filepath.Dir(exe), libName); fileExists(candidate) {
			return candidate
		}
	}

	return libName
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Falls back to the hash embedder (not an error) if the model file is
// missing, preserving the existing graceful-degradation behavior - but
// once the model file IS found, any subsequent failure (bad vocab, broken
// ONNX Runtime install, session creation failure) is a real error, not a
// silent fallback, since at that point something is genuinely broken
// rather than simply "not configured".
func newONNXEmbedder(modelPath string) (Embedder, error) {
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		slog.Warn("ONNX model file not found, falling back to hash embedder", "path", modelPath)
		return NewHashEmbedder(384), nil
	}

	onnxEnvOnce.Do(func() {
		// Only set an explicit path if the library hasn't already been
		// configured - SetSharedLibraryPath must be called before
		// InitializeEnvironment, and InitializeEnvironment is itself
		// guarded to only run once per process via onnxEnvOnce.
		ort.SetSharedLibraryPath(resolveOrtLibPath())
		onnxEnvErr = ort.InitializeEnvironment()
	})
	if onnxEnvErr != nil {
		return nil, fmt.Errorf("failed to initialize ONNX Runtime environment: %w", onnxEnvErr)
	}

	vocabPath := filepath.Join(filepath.Dir(modelPath), "vocab.txt")
	tokenizer, err := NewWordPieceTokenizer(vocabPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load tokenizer vocabulary from %s: %w", vocabPath, err)
	}

	// Different exports of the same model (e.g. across sentence-transformers
	// repo revisions) don't always declare the same input/output names --
	// some omit the optional token_type_ids input entirely. Inspect the
	// actual model instead of assuming a fixed 3-input/1-output shape, so
	// this code tolerates whichever export variant is on disk.
	inputInfo, outputInfo, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect ONNX model input/output info: %w", err)
	}

	hasTokenTypeIDs := false
	for _, in := range inputInfo {
		if in.Name == "token_type_ids" {
			hasTokenTypeIDs = true
			break
		}
	}

	const requiredOutput = "sentence_embedding"
	hasSentenceEmbedding := false
	var outputNames []string
	for _, out := range outputInfo {
		outputNames = append(outputNames, out.Name)
		if out.Name == requiredOutput {
			hasSentenceEmbedding = true
		}
	}
	if !hasSentenceEmbedding {
		return nil, fmt.Errorf("ONNX model at %s does not expose a %q output (found: %v) -- incompatible export variant", modelPath, requiredOutput, outputNames)
	}

	inputShape := ort.NewShape(1, onnxSeqLen)

	// Tensors are created empty here and their contents are overwritten on
	// every Embed() call - this is what lets the session be built once and
	// reused, rather than rebuilt per call.
	inputIDsTensor, err := ort.NewEmptyTensor[int64](inputShape)
	if err != nil {
		return nil, fmt.Errorf("failed to create input_ids tensor: %w", err)
	}

	attentionMaskTensor, err := ort.NewEmptyTensor[int64](inputShape)
	if err != nil {
		inputIDsTensor.Destroy()
		return nil, fmt.Errorf("failed to create attention_mask tensor: %w", err)
	}

	var tokenTypeIDsTensor *ort.Tensor[int64]
	if hasTokenTypeIDs {
		tokenTypeIDsTensor, err = ort.NewEmptyTensor[int64](inputShape)
		if err != nil {
			inputIDsTensor.Destroy()
			attentionMaskTensor.Destroy()
			return nil, fmt.Errorf("failed to create token_type_ids tensor: %w", err)
		}
	}

	outputShape := ort.NewShape(1, onnxEmbeddingDim)
	outputTensor, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		inputIDsTensor.Destroy()
		attentionMaskTensor.Destroy()
		tokenTypeIDsTensor.Destroy()
		return nil, fmt.Errorf("failed to create output tensor: %w", err)
	}

	inputNames := []string{"input_ids", "attention_mask"}
	inputValues := []ort.Value{inputIDsTensor, attentionMaskTensor}
	if hasTokenTypeIDs {
		inputNames = append(inputNames, "token_type_ids")
		inputValues = append(inputValues, tokenTypeIDsTensor)
	}

	session, err := ort.NewAdvancedSession(
		modelPath,
		inputNames,
		[]string{requiredOutput},
		inputValues,
		[]ort.Value{outputTensor},
		nil,
	)
	if err != nil {
		inputIDsTensor.Destroy()
		attentionMaskTensor.Destroy()
		if tokenTypeIDsTensor != nil {
			tokenTypeIDsTensor.Destroy()
		}
		outputTensor.Destroy()
		return nil, fmt.Errorf("failed to create ONNX session: %w", err)
	}

	slog.Info("ONNX embedder initialized with real inference", "model", modelPath, "vocab", vocabPath)

	return &ONNXEmbedder{
		modelPath:           modelPath,
		tokenizer:           tokenizer,
		session:             session,
		inputIDsTensor:      inputIDsTensor,
		attentionMaskTensor: attentionMaskTensor,
		tokenTypeIDsTensor:  tokenTypeIDsTensor,
		outputTensor:        outputTensor,
	}, nil
}

// Embed implements Embedder interface for ONNXEmbedder using real
// tokenization and real ONNX Runtime inference. Mutex-protected since the
// underlying session and its tensors are shared/reused across calls.
func (e *ONNXEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	inputIDs, attentionMask, tokenTypeIDs := e.tokenizer.Encode(text, onnxSeqLen)

	copy(e.inputIDsTensor.GetData(), inputIDs)
	copy(e.attentionMaskTensor.GetData(), attentionMask)
	if e.tokenTypeIDsTensor != nil {
		copy(e.tokenTypeIDsTensor.GetData(), tokenTypeIDs)
	}

	if err := e.session.Run(); err != nil {
		return nil, fmt.Errorf("ONNX inference failed: %w", err)
	}

	// Copy the output out rather than returning a slice view directly into
	// the tensor's backing memory - that memory gets overwritten on the
	// very next Embed() call, which would corrupt any embedding the caller
	// is still holding onto.
	raw := e.outputTensor.GetData()
	result := make([]float32, len(raw))
	copy(result, raw)

	return result, nil
}

// Close releases the ONNX session and tensors. Should be called when the
// embedder is no longer needed, to avoid leaking native memory.
func (e *ONNXEmbedder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.session != nil {
		e.session.Destroy()
	}
	if e.inputIDsTensor != nil {
		e.inputIDsTensor.Destroy()
	}
	if e.attentionMaskTensor != nil {
		e.attentionMaskTensor.Destroy()
	}
	if e.tokenTypeIDsTensor != nil {
		e.tokenTypeIDsTensor.Destroy()
	}
	if e.outputTensor != nil {
		e.outputTensor.Destroy()
	}
	return nil
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