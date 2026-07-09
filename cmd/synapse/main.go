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
	charmlog "github.com/charmbracelet/log"
	"gopkg.in/yaml.v3"
	"synapse/internal/api"
	"synapse/internal/config"
	"synapse/internal/embedder"
	"synapse/internal/proxy"
	"synapse/internal/store"
)

var uiHTML []byte

var (
	configPath     = flag.String("config", "synapse.yaml", "Path to configuration file")
	upstream       = flag.String("upstream", "", "Override upstream URL")
	port           = flag.String("port", "", "Override port")
	persistTraces  = flag.Bool("persist-traces", false, "Persist memory traces to disk")
)

func main() {
	flag.Parse()

	// Replace the default slog text handler with charmbracelet/log's
	// handler for colored, aligned console output. charmbracelet/log
	// implements slog.Handler directly, so every existing slog.Info/
	// Warn/Error call across the codebase gets the new formatting for
	// free -- no changes needed anywhere else.
	prettyLogger := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.Kitchen,
		Prefix:          "synapse",
	})
	slog.SetDefault(slog.New(prettyLogger))

	// Load UI HTML file
	var err error
	uiHTML, err = os.ReadFile("ui/index.html")
	if err != nil {
		// Try alternative path for when running from cmd/synapse directory
		uiHTML, err = os.ReadFile("../../ui/index.html")
		if err != nil {
			// Try path when binary is in project root
			uiHTML, err = os.ReadFile("../ui/index.html")
			if err != nil {
				slog.Warn("Failed to load UI file, UI will not be available", "error", err)
				uiHTML = []byte("<html><body><h1>UI not available</h1></body></html>")
			}
		}
	}

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

	// Initialize store
	storeInstance, err := store.NewStore(cfg.DBPath)
	if err != nil {
		slog.Error("Failed to create store", "error", err)
		os.Exit(1)
	}
	defer storeInstance.Close()

	// Initialize embedder
	embedderInstance, err := embedder.NewEmbedder(cfg.EmbedderType, cfg.OpenAIAPIKey, cfg.ModelPath, "")
	if err != nil {
		slog.Error("Failed to create embedder", "error", err)
		os.Exit(1)
	}

	// Create proxy with store and embedder
	proxyInstance, err := proxy.NewProxy(cfg.UpstreamURL, storeInstance, embedderInstance, cfg)
	if err != nil {
		slog.Error("Failed to create proxy", "error", err)
		os.Exit(1)
	}

	// Create API server
	apiServer := api.NewAPIServer(storeInstance, embedderInstance, cfg, *persistTraces)
	
	// Create router
	r := chi.NewRouter()
	
	// Mount API routes
	r.Mount("/", apiServer.Router())
	
	// Add health check endpoint: GET /health → 200 {"status":"ok","memories_stored":N,"avg_compile_ms":N}
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		// In a real implementation, you'd get actual stats from the store
		// For now, using placeholder values
		memoriesStored := 0
		if storeInstance != nil {
			// This would require adding a method to count memories
			// For now, we'll leave it as 0
		}
		
		response := fmt.Sprintf(`{"status":"ok","memories_stored":%d,"avg_compile_ms":0}`, memoriesStored)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(response))
	})

	// Register proxy routes
	proxyInstance.RegisterRoutes(r)

	// Serve UI at GET /ui
	r.Get("/ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(uiHTML)
	})

	// Add link to UI in startup log
	slog.Info("Trace inspector available at http://" + cfg.ListenAddr + "/ui")

	// Start server
	server := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: r,
	}

	// Log startup security checklist
	upstreamHost := ""
	if parsedURL, err := url.Parse(cfg.UpstreamURL); err == nil {
		upstreamHost = parsedURL.Host
	}
	
	slog.Info("Synapse security: proxy bound to "+cfg.ListenAddr+", upstream "+upstreamHost+", trace persistence "+fmt.Sprintf("%t", *persistTraces)+", header redaction active, injection sanitization active")

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

	// Check config file permissions for security
	if fileInfo, err := os.Stat(path); err == nil {
		// Check if file is world-readable (others have read permission)
		perm := fileInfo.Mode().Perm()
		if perm&0004 != 0 {
			slog.Warn("SECURITY WARNING: Config file is world-readable. Recommended fix: chmod 600 "+path,
				"path", path,
				"current_permissions", fmt.Sprintf("%04o", perm))
		}
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
