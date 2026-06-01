package api

import (
	"net/http"
	"strings"
)

// sanitizeHeaders removes sensitive headers from a map of headers
func sanitizeHeaders(headers map[string][]string) map[string][]string {
	sanitized := make(map[string][]string)
	
	for key, values := range headers {
		// Convert header name to lowercase for case-insensitive comparison
		lowerKey := strings.ToLower(key)
		
		// Skip sensitive headers
		if lowerKey == "authorization" || 
		   lowerKey == "x-api-key" || 
		   lowerKey == "proxy-authorization" ||
		   strings.Contains(lowerKey, "auth") ||
		   strings.Contains(lowerKey, "token") ||
		   strings.Contains(lowerKey, "secret") {
			continue
		}
		
		sanitized[key] = values
	}
	
	return sanitized
}

// getSanitizedHeader gets a header value while redacting sensitive headers in logs
func getSanitizedHeaderValue(header http.Header, key string) string {
	lowerKey := strings.ToLower(key)
	
	// Return empty string for sensitive headers to avoid logging
	if lowerKey == "authorization" || 
	   lowerKey == "x-api-key" || 
	   lowerKey == "proxy-authorization" {
		return "[REDACTED]"
	}
	
	return header.Get(key)
}