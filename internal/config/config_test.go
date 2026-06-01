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

