package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
	"synapse/internal/config"
	"synapse/internal/embedder"
	"synapse/internal/proxy"
	"synapse/internal/store"
)

var (
	configPath = flag.String("config", "synapse.yaml", "Path to configuration file")
	targetURL  = flag.String("target", "http://localhost:3000", "Target upstream server URL")
)

func main() {
	flag.Parse()

	// Load configuration
	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		slog.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}

	// Create store
	storeInstance, err := store.NewStore(cfg.Store.DBPath)
	if err != nil {
		slog.Error("Failed to create store", "error", err)
		os.Exit(1)
	}
	defer storeInstance.Close()

	// Create embedder
	embedderInstance, err := embedder.NewEmbedder(cfg.Embedder)
	if err != nil {
		slog.Error("Failed to create embedder", "error", err)
		os.Exit(1)
	}

	// Create proxy
	proxyInstance, err := proxy.NewProxy(*targetURL, storeInstance, embedderInstance)
	if err != nil {
		slog.Error("Failed to create proxy", "error", err)
		os.Exit(1)
	}
	defer proxyInstance.Close()

	// Create router
	r := chi.NewRouter()
	
	// Add health check endpoint
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Register proxy routes
	proxyInstance.RegisterRoutes(r)

	// Start server
	server := &http.Server{
		Addr:    cfg.Proxy.BindAddress,
		Handler: r,
	}

	// Start server in goroutine
	go func() {
		slog.Info("Starting Synapse proxy", "address", cfg.Proxy.BindAddress, "target", *targetURL)
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

	return &cfg, nil
}