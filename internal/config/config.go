package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config represents the application configuration
type Config struct {
	Proxy      ProxyConfig      `yaml:"proxy"`
	Embedder   EmbedderConfig   `yaml:"embedder"`
	Store      StoreConfig      `yaml:"store"`
}

// ProxyConfig holds proxy-specific configuration
type ProxyConfig struct {
	BindAddress string `yaml:"bind-address"`
}

// EmbedderConfig holds embedding-related configuration
type EmbedderConfig struct {
	Type        string `yaml:"type"` // "onnx", "openai", "hash"
	ONNXModelPath string `yaml:"onnx-model-path"`
	OpenAIAPIKey  string `yaml:"openai-api-key"`
	OpenAIModel   string `yaml:"openai-model"`
}

// StoreConfig holds memory store configuration
type StoreConfig struct {
	DBPath string `yaml:"db-path"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = "."
	}
	
	defaultDBPath := filepath.Join(homeDir, ".synapse", "memories.db")
	
	return &Config{
		Proxy: ProxyConfig{
			BindAddress: "127.0.0.1:8080",
		},
		Embedder: EmbedderConfig{
			Type:          "onnx",
			ONNXModelPath: "./models/all-MiniLM-L6-v2.onnx",
			OpenAIModel:   "text-embedding-3-small",
		},
		Store: StoreConfig{
			DBPath: defaultDBPath,
		},
	}
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Proxy.BindAddress == "" {
		return fmt.Errorf("proxy.bind-address is required")
	}
	
	if c.Embedder.Type == "" {
		return fmt.Errorf("embedder.type is required")
	}
	
	if c.Embedder.Type == "openai" && c.Embedder.OpenAIAPIKey == "" {
		return fmt.Errorf("embedder.openai-api-key is required when using OpenAI embedder")
	}
	
	if c.Store.DBPath == "" {
		return fmt.Errorf("store.db-path is required")
	}
	
	return nil
}