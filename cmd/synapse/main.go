package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
	"synapse/internal/config"
	"synapse/internal/proxy"
)

var (
	configPath = flag.String("config", "synapse.yaml", "Path to configuration file")
	upstream   = flag.String("upstream", "", "Override upstream URL")
	port       = flag.String("port", "", "Override port")
)

func main() {
	flag.Parse()

	// Load configuration
	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// Override with flags if provided
	if *upstream != "" {
		cfg.UpstreamURL = *upstream
	}
	if *port != "" {
		// Update the listen address with the new port
		host := "127.0.0.1"
		if cfg.ListenAddr != "" {
			if h, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
				host = h
			}
		}
		cfg.ListenAddr = host + ":" + *port
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		slog.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}

	// Ensure upstream URL is provided
	if cfg.UpstreamURL == "" {
		slog.Error("Upstream URL is required (provide via config or --upstream flag)")
		os.Exit(1)
	}

	// Validate upstream URL format
	if _, err := url.ParseRequestURI(cfg.UpstreamURL); err != nil {
		slog.Error("Invalid upstream URL format", "error", err)
		os.Exit(1)
	}

	// For Phase 0, we don't need the full proxy with embedder/store
	// Create a simple passthrough proxy
	proxyInstance, err := proxy.NewProxy(cfg.UpstreamURL)
	if err != nil {
		slog.Error("Failed to create proxy", "error", err)
		os.Exit(1)
	}

	// Create router
	r := chi.NewRouter()
	
	// Add health check endpoint: GET /health → 200 {"status":"ok"}
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Register proxy routes
	proxyInstance.RegisterRoutes(r)

	// Start server
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: r,
	}

	// Start server in goroutine
	go func() {
		slog.Info("Starting Synapse proxy", "address", cfg.ListenAddr, "upstream", cfg.UpstreamURL)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("Server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	slog.Info("Shutting down server...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		slog.Error("Server shutdown failed", "error", err)
	}

	slog.Info("Server stopped")
}

// loadConfig loads configuration from YAML file
func loadConfig(path string) (*config.Config, error) {
	// If config file doesn't exist, use defaults
	if _, err := os.Stat(path); os.IsNotExist(err) {
		slog.Warn("Config file not found, using defaults", "path", path)
		return config.DefaultConfig(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// If OpenAI API key not in config, try environment variable
	if cfg.OpenAIAPIKey == "" {
		cfg.OpenAIAPIKey = os.Getenv("OPENAI_API_KEY")
	}

	return &cfg, nil
}
