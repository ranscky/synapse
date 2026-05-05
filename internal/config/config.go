package config

import (
	"fmt"
	"os"
	"strings"
)

// Config represents the application configuration
type Config struct {
	UpstreamURL              string  `yaml:"upstream-url"`
	ListenAddr               string  `yaml:"listen-addr"`
	TokenBudget              int     `yaml:"token-budget"`
	EmbedderType             string  `yaml:"embedder-type"`
	OpenAIAPIKey             string  `yaml:"openai-api-key"`
	WeightSemanticSimilarity float64 `yaml:"weight-semantic-similarity"`
	WeightRecency            float64 `yaml:"weight-recency"`
	WeightImportance         float64 `yaml:"weight-importance"`
	WeightTaskAlignment      float64 `yaml:"weight-task-alignment"`
	DeduplicationThreshold   float64 `yaml:"deduplication-threshold"`
	LogLevel                 string  `yaml:"log-level"`
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
		OpenAIAPIKey:             openAIAPIKey,
		WeightSemanticSimilarity: 0.4,
		WeightRecency:            0.2,
		WeightImportance:         0.2,
		WeightTaskAlignment:      0.2,
		DeduplicationThreshold:   0.92, // Default deduplication threshold
		LogLevel:                 "info",
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
	
	// Validate upstream URL scheme if provided
	if c.UpstreamURL != "" && !strings.HasPrefix(c.UpstreamURL, "http://") && !strings.HasPrefix(c.UpstreamURL, "https://") {
		return fmt.Errorf("upstream-url must start with http:// or https://")
	}
	
	return nil
}
