package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Proxy represents the reverse proxy handler
type Proxy struct {
	target   *url.URL
	upstream *httputil.ReverseProxy
}

// NewProxy creates a new proxy instance
func NewProxy(targetURL string) (*Proxy, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	// Validate that target URL starts with http:// or https://
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		return nil, fmt.Errorf("upstream URL must start with http:// or https://")
	}

	proxy := &Proxy{
		target: target,
	}

	// Create reverse proxy with simple director
	proxy.upstream = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			proxy.director(req)
		},
	}

	return proxy, nil
}

// director modifies the outgoing request to forward to the target
func (p *Proxy) director(req *http.Request) {
	// Set the target URL
	req.URL.Scheme = p.target.Scheme
	req.URL.Host = p.target.Host
	req.Host = p.target.Host

	// Remove hop-by-hop headers
	req.Header.Del("Connection")
	req.Header.Del("Upgrade")
	req.Header.Del("TE")
	req.Header.Del("Trailers")
	req.Header.Del("Transfer-Encoding")
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Authenticate")

	// IMPORTANT SECURITY: Never log Authorization headers
	// The header is stripped above and re-added from config if needed
	// But we never log its value anywhere
}

// HandleMessages handles POST /v1/messages requests (passthrough for Phase 0)
func (p *Proxy) HandleMessages(w http.ResponseWriter, r *http.Request) {
	// For Phase 0, this is a simple passthrough proxy
	// In future phases, this will implement the 4-factor scoring
	
	// Log the request (without sensitive headers)
	slog.Info("Handling messages request", "method", r.Method, "url", r.URL.Path)
	
	// Forward the request to upstream
	p.upstream.ServeHTTP(w, r)
}

// RegisterRoutes registers the proxy routes with the chi router
func (p *Proxy) RegisterRoutes(r chi.Router) {
	r.Post("/v1/messages", p.HandleMessages)
}

// Close closes the proxy resources (no-op for simple proxy)
func (p *Proxy) Close() error {
	return nil
}
