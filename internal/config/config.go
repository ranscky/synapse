package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Config represents the application configuration
type Config struct {
	UpstreamURL              string   `yaml:"upstream-url"`
	AllowedUpstreamHosts     []string `yaml:"allowed-upstream-hosts"`
	ListenAddr               string   `yaml:"listen-addr"`
	TokenBudget              int      `yaml:"token-budget"`
	EmbedderType             string   `yaml:"embedder-type"`
	ModelPath                string   `yaml:"model-path"`
	DBPath                   string   `yaml:"db-path"`
	OpenAIAPIKey             string   `yaml:"openai-api-key"`
	WeightSemanticSimilarity float64  `yaml:"weight-semantic-similarity"`
	WeightRecency            float64  `yaml:"weight-recency"`
	WeightImportance         float64  `yaml:"weight-importance"`
	WeightTaskAlignment      float64  `yaml:"weight-task-alignment"`
	DeduplicationThreshold   float64  `yaml:"deduplication-threshold"`
	LogLevel                 string   `yaml:"log-level"`
	RetrievalCandidateK      int      `yaml:"retrieval-candidate-k"`
	SupersessionSimilarityMin float64 `yaml:"supersession-similarity-min"`
	SupersessionSimilarityMax float64 `yaml:"supersession-similarity-max"`
}

// defaultDataDir resolves the stable, per-OS data directory used as the
// parent for both the SQLite database and the OS-standard model fallback
// location. Extracted from the original defaultDBPath so both can share
// the same resolution logic rather than duplicating it.
func defaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		return os.Getenv("APPDATA")
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support")
		}
	default: // linux and other unix-likes
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return xdg
		} else if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share")
		}
	}
	return ""
}

// defaultDBPath resolves a stable, per-OS data directory for the SQLite
// database, so persistence doesn't depend on which folder the binary
// happens to be launched from (a real risk for a distributed release
// binary run from arbitrary locations). Falls back to a bare relative
// filename if the OS data dir can't be resolved for any reason.
func defaultDBPath() string {
	const dbFile = "synapse.db"
	dataDir := defaultDataDir()
	if dataDir == "" {
		return dbFile
	}
	return filepath.Join(dataDir, "synapse", dbFile)
}

// DefaultDBPath resolves the OS-standard database path. Exported so callers
// loading config from a file can apply the same fallback that DefaultConfig
// applies automatically for a blank db-path.
func DefaultDBPath() string {
	return defaultDBPath()
}

// defaultModelPath resolves the OS-standard fallback location for the ONNX
// model. NOT used as DefaultConfig()'s primary ModelPath value -- that
// stays a cwd-relative path so the existing standalone release archive
// (extract, cd in, run ./synapse) keeps working with zero behavior change.
// This is consulted only as a fallback at startup, for package-manager
// installs (Homebrew, etc.) where there's no "models/ next to the binary"
// the way there is in the archive.
func defaultModelPath() string {
	dataDir := defaultDataDir()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "synapse", "models", "all-MiniLM-L6-v2", "model.onnx")
}

// DefaultModelPath exports the OS-standard fallback model location.
func DefaultModelPath() string {
	return defaultModelPath()
}

// defaultConfigPath resolves the OS-standard location for synapse.yaml,
// using Go's own os.UserConfigDir() (XDG_CONFIG_HOME/~/.config on Linux,
// ~/Library/Application Support on macOS, %APPDATA% on Windows) instead of
// hand-rolling per-OS logic a second time.
func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "synapse.yaml"
	}
	return filepath.Join(dir, "synapse", "synapse.yaml")
}

// DefaultConfigPath exports the OS-standard config file location. Used by
// both `synapse init` (to know where to scaffold) and main's config
// resolution (to know where to look when no explicit --config is given
// and nothing's found in the current directory).
func DefaultConfigPath() string {
	return defaultConfigPath()
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	// Try to get OpenAI API key from environment variable
	openAIAPIKey := os.Getenv("OPENAI_API_KEY")
	
	return &Config{
		UpstreamURL:              "", // Must be provided via config or flag
		ListenAddr:               "127.0.0.1:8080",
		TokenBudget:              3000,
		EmbedderType:             "onnx",
		ModelPath:                "models/all-MiniLM-L6-v2/model.onnx",
		DBPath:                   defaultDBPath(),
		OpenAIAPIKey:             openAIAPIKey,
		WeightSemanticSimilarity: 0.4,
		WeightRecency:            0.1,
		WeightImportance:         0.3,
		WeightTaskAlignment:      0.2,
		DeduplicationThreshold:   0.92, // Default deduplication threshold
		LogLevel:                 "info",
		RetrievalCandidateK:      50, // Candidate pool size pulled from semantic search before scoring
		// These two are a rough starting guess, not a tuned value -- there's
		// no real-usage data behind them yet. The band needs to sit below
		// DeduplicationThreshold (0.92): dedup already catches near-identical
		// restatements above that line, so supersession's job is the "same
		// topic, different answer" zone below it. Revisit once real
		// conversations show whether 0.5-0.90 actually separates
		// "related but different" from "unrelated" and "duplicate" well.
		SupersessionSimilarityMin: 0.5,
		SupersessionSimilarityMax: 0.90,
	}
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen-addr is required")
	}
	
	// Validate ListenAddr defaults to 127.0.0.1
	if !strings.HasPrefix(c.ListenAddr, "127.0.0.1") && !strings.HasPrefix(c.ListenAddr, "localhost") {
		return fmt.Errorf("listen-addr must default to 127.0.0.1 for security reasons")
	}
	
	if c.EmbedderType == "" {
		return fmt.Errorf("embedder-type is required")
	}
	
	if c.EmbedderType != "onnx" && c.EmbedderType != "openai" {
		return fmt.Errorf("embedder-type must be 'onnx' or 'openai'")
	}
	
	if c.EmbedderType == "openai" && c.OpenAIAPIKey == "" {
		return fmt.Errorf("openai-api-key is required when using OpenAI embedder")
	}
	
	// A negative candidate pool size is nonsensical; zero is left permissive
	// here (same treatment as TokenBudget and the scoring weights above) so
	// that hand-built Config structs in tests don't need to set every field
	// -- callers that actually run retrieval should treat zero/unset as
	// "fall back to the DefaultConfig value" rather than erroring here.
	// "fall back to the DefaultConfig value" rather than erroring here.
	if c.RetrievalCandidateK < 0 {
		return fmt.Errorf("retrieval-candidate-k must not be negative")
	}

	// Only reject the one combination that can never be meaningfully
	// correct -- an empty or inverted band. Values outside [0,1] are left
	// permissive (matching the DeduplicationThreshold precedent above)
	// since cosine similarity can technically go negative for genuinely
	// opposite embeddings, and this is a heuristic knob still being tuned.
	if c.SupersessionSimilarityMin > c.SupersessionSimilarityMax {
		return fmt.Errorf("supersession-similarity-min must not be greater than supersession-similarity-max")
	}

	// Upstream URL is required
	if c.UpstreamURL == "" {
		return fmt.Errorf("upstream-url is required")
	}
	
	// Validate upstream URL scheme
	if !strings.HasPrefix(c.UpstreamURL, "http://") && !strings.HasPrefix(c.UpstreamURL, "https://") {
		return fmt.Errorf("upstream-url must start with http:// or https://")
	}
	
	// Validate upstream URL format
	if _, err := url.ParseRequestURI(c.UpstreamURL); err != nil {
		return fmt.Errorf("invalid upstream URL format: %w", err)
	}
	
	// Check upstream URL against allowlist if configured
	if len(c.AllowedUpstreamHosts) > 0 {
		upstreamURL, err := url.Parse(c.UpstreamURL)
		if err != nil {
			return fmt.Errorf("invalid upstream URL: %w", err)
		}
		
		host := upstreamURL.Hostname()
		allowed := false
		
		// Check if host is in allowlist
		for _, allowedHost := range c.AllowedUpstreamHosts {
			if host == allowedHost {
				allowed = true
				break
			}
		}
		
		// Special handling for localhost/127.x.x.x (common for Ollama)
		if !allowed && (host == "localhost" || strings.HasPrefix(host, "127.")) {
			// Log INFO for localhost usage (common for Ollama)
			// This will be handled at startup with proper logging context
		} else if !allowed {
			return fmt.Errorf("upstream host %s is not in allowed list", host)
		}
	}
	
	return nil
}
