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

