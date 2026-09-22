package config

import (
	"testing"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		expectError bool
		errorMsg    string
	}{
		{
			name: "Valid config with upstream URL",
			config: &Config{
				UpstreamURL:  "http://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
			},
			expectError: false,
		},
		{
			name: "Missing upstream URL",
			config: &Config{
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
			},
			expectError: true,
			errorMsg:    "upstream-url is required",
		},
		{
			name: "Invalid upstream URL scheme",
			config: &Config{
				UpstreamURL:  "ftp://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
			},
			expectError: true,
			errorMsg:    "upstream-url must start with http:// or https://",
		},
		{
			name: "Valid config with allowlist",
			config: &Config{
				UpstreamURL:          "http://localhost:11434",
				AllowedUpstreamHosts: []string{"localhost", "example.com"},
				ListenAddr:           "127.0.0.1:8080",
				EmbedderType:         "onnx",
			},
			expectError: false,
		},
		{
			name: "Upstream host not in allowlist",
			config: &Config{
				UpstreamURL:          "http://blocked.com:11434",
				AllowedUpstreamHosts: []string{"allowed.com", "example.com"},
				ListenAddr:           "127.0.0.1:8080",
				EmbedderType:         "onnx",
			},
			expectError: true,
			errorMsg:    "upstream host blocked.com is not in allowed list",
		},
				{
			name: "Localhost allowed by default",
			config: &Config{
				UpstreamURL:          "http://localhost:11434",
				AllowedUpstreamHosts: []string{"allowed.com"},
				ListenAddr:           "127.0.0.1:8080",
				EmbedderType:         "onnx",
			},
			expectError: false,
		},
		{
			name: "Negative retrieval candidate K rejected",
			config: &Config{
				UpstreamURL:         "http://localhost:11434",
				ListenAddr:          "127.0.0.1:8080",
				EmbedderType:        "onnx",
				RetrievalCandidateK: -1,
			},
			expectError: true,
			errorMsg:    "retrieval-candidate-k must not be negative",
		},
				{
			name: "Zero retrieval candidate K is permissive",
			config: &Config{
				UpstreamURL:  "http://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
			},
			expectError: false,
		},
		{
			name: "Inverted supersession similarity band rejected",
			config: &Config{
				UpstreamURL:               "http://localhost:11434",
				ListenAddr:                "127.0.0.1:8080",
				EmbedderType:              "onnx",
				SupersessionSimilarityMin: 0.9,
				SupersessionSimilarityMax: 0.5,
			},
			expectError: true,
			errorMsg:    "supersession-similarity-min must not be greater than supersession-similarity-max",
		},
		{
			name: "Equal supersession similarity band is permissive",
			config: &Config{
				UpstreamURL:               "http://localhost:11434",
				ListenAddr:                "127.0.0.1:8080",
				EmbedderType:              "onnx",
				SupersessionSimilarityMin: 0.7,
				SupersessionSimilarityMax: 0.7,
			},
			expectError: false,
		},
		{
			name: "Zero-value supersession band is permissive",
			config: &Config{
				UpstreamURL:  "http://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
			},
			expectError: false,
		},
		{
			name: "MCP port above the TCP range rejected",
			config: &Config{
				UpstreamURL:  "http://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
				MCPPort:      70000,
			},
			expectError: true,
			errorMsg:    "mcp-port must be between 0 and 65535",
		},
		{
			name: "Negative MCP port rejected",
			config: &Config{
				UpstreamURL:  "http://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
				MCPPort:      -1,
			},
			expectError: true,
			errorMsg:    "mcp-port must be between 0 and 65535",
		},
		{
			name: "Zero MCP port is permissive",
			config: &Config{
				UpstreamURL:  "http://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
			},
			expectError: false,
		},
		{
			name: "MCP enabled on the default TCP port validates",
			config: &Config{
				UpstreamURL:  "http://localhost:11434",
				ListenAddr:   "127.0.0.1:8080",
				EmbedderType: "onnx",
				MCPEnabled:   true,
				MCPPort:      8765,
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if err.Error() != tt.errorMsg {
					t.Errorf("Expected error '%s' but got '%s'", tt.errorMsg, err.Error())
				}
						} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestDefaultConfigRetrievalCandidateK(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.RetrievalCandidateK != 50 {
		t.Errorf("Expected default RetrievalCandidateK of 50, got %d", cfg.RetrievalCandidateK)
	}
	if err := cfg.Validate(); err != nil {
		// DefaultConfig alone won't pass Validate() since UpstreamURL is
		// required and left blank -- but confirm the error is that, and not
		// something related to the new field.
		if err.Error() != "upstream-url is required" {
			t.Errorf("Unexpected validation error against DefaultConfig: %v", err)
		}
	}
}

func TestDefaultConfigSupersessionSimilarityBand(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SupersessionSimilarityMin != 0.5 {
		t.Errorf("Expected default SupersessionSimilarityMin of 0.5, got %v", cfg.SupersessionSimilarityMin)
	}
	if cfg.SupersessionSimilarityMax != 0.90 {
		t.Errorf("Expected default SupersessionSimilarityMax of 0.90, got %v", cfg.SupersessionSimilarityMax)
	}
	if cfg.SupersessionSimilarityMin > cfg.SupersessionSimilarityMax {
		t.Errorf("Default supersession similarity band is inverted: min=%v max=%v", cfg.SupersessionSimilarityMin, cfg.SupersessionSimilarityMax)
	}
}

// TestDefaultConfigConflictKnobs covers the Phase 12 conflict detection knobs.
// The threshold is asserted against the literal 0.4 that
// internal/conflict's DefaultJaccardThreshold also carries: config cannot
// import that package (conflict -> store -> config would be a cycle), so this
// test is the only place the two copies are checked against each other's
// documented value.
func TestDefaultConfigConflictKnobs(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ConflictJaccardThreshold != 0.4 {
		t.Errorf("Expected default ConflictJaccardThreshold of 0.4, got %v", cfg.ConflictJaccardThreshold)
	}
	if cfg.ConflictScorePenalty != 0.5 {
		t.Errorf("Expected default ConflictScorePenalty of 0.5, got %v", cfg.ConflictScorePenalty)
	}

	// A hand-built config leaves the knobs at their zero value, which
	// Validate must not treat as negative.
	handBuilt := &Config{
		UpstreamURL:  "http://localhost:11434",
		ListenAddr:   "127.0.0.1:8080",
		EmbedderType: "onnx",
	}
	if err := handBuilt.Validate(); err != nil {
		t.Errorf("Expected a zero-valued ConflictJaccardThreshold to pass validation, got %v", err)
	}

	negative := *handBuilt
	negative.ConflictJaccardThreshold = -0.1
	if err := negative.Validate(); err == nil {
		t.Errorf("Expected a negative ConflictJaccardThreshold to fail validation")
	}
}

// TestDefaultConfigMCPKnobs covers the Phase 22 MCP fields: off, and stdio (port
// 0) when they are left alone. Those two values are what keep a config file that
// never mentions MCP behaving exactly as it did before this phase.
func TestDefaultConfigMCPKnobs(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MCPEnabled {
		t.Errorf("Expected the MCP server to default to disabled")
	}
	if cfg.MCPPort != 0 {
		t.Errorf("Expected the default MCPPort to be 0 (stdio), got %d", cfg.MCPPort)
	}
}
