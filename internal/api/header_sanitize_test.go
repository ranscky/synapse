package api

import (
	"net/http"
	"reflect"
	"testing"
)

func TestSanitizeHeaders(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string][]string
		expected map[string][]string
	}{
		{
			name: "Remove Authorization header",
			headers: map[string][]string{
				"Authorization": {"Bearer token123"},
				"Content-Type":  {"application/json"},
				"User-Agent":    {"test-agent"},
			},
			expected: map[string][]string{
				"Content-Type": {"application/json"},
				"User-Agent":   {"test-agent"},
			},
		},
		{
			name: "Remove X-API-Key header",
			headers: map[string][]string{
				"X-API-Key":    {"secret-key"},
				"Content-Type": {"application/json"},
			},
			expected: map[string][]string{
				"Content-Type": {"application/json"},
			},
		},
		{
			name: "Remove multiple sensitive headers",
			headers: map[string][]string{
				"Authorization":     {"Bearer token"},
				"X-API-Key":         {"secret"},
				"Proxy-Authorization": {"proxy-auth"},
				"X-Auth-Token":      {"auth-token"},
				"Content-Type":      {"application/json"},
			},
			expected: map[string][]string{
				"Content-Type": {"application/json"},
			},
		},
		{
			name: "No sensitive headers",
			headers: map[string][]string{
				"Content-Type": {"application/json"},
				"User-Agent":   {"test-agent"},
			},
			expected: map[string][]string{
				"Content-Type": {"application/json"},
				"User-Agent":   {"test-agent"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeHeaders(tt.headers)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("sanitizeHeaders() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGetSanitizedHeaderValue(t *testing.T) {
	header := http.Header{
		"Authorization": {"Bearer token123"},
		"Content-Type":  {"application/json"},
		"X-API-Key":     {"secret-key"},
		"User-Agent":    {"test-agent"},
	}

	tests := []struct {
		name     string
		key      string
		expected string
	}{
		{
			name:     "Authorization header",
			key:      "Authorization",
			expected: "[REDACTED]",
		},
		{
			name:     "X-API-Key header",
			key:      "X-API-Key",
			expected: "[REDACTED]",
		},
		{
			name:     "Non-sensitive header",
			key:      "Content-Type",
			expected: "application/json",
		},
		{
			name:     "Case insensitive Authorization",
			key:      "authorization",
			expected: "[REDACTED]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getSanitizedHeaderValue(header, tt.key)
			if result != tt.expected {
				t.Errorf("getSanitizedHeaderValue() = %v, want %v", result, tt.expected)
			}
		})
	}
}