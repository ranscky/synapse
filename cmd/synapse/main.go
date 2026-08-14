package main

import (
	"bufio"
	"path/filepath"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"synapse/internal/api"
	"synapse/internal/budget"
	"synapse/internal/config"
	"synapse/internal/embedder"
	"synapse/internal/proxy"
	"synapse/internal/session"
	"synapse/internal/store"

	charmlog "github.com/charmbracelet/log"
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

var (
	uiHTML []byte
	uiSessionHTML []byte
)

var (
	configPath     = flag.String("config", "", "Path to configuration file (default: ./synapse.yaml, falling back to the OS-standard config location)")
	upstream       = flag.String("upstream", "", "Override upstream URL")
	port           = flag.String("port", "", "Override port")
	persistTraces  = flag.Bool("persist-traces", false, "Persist memory traces to disk")
)

// resolveConfigPath decides which config file to load when --config wasn't
// given explicitly: check the current directory first (preserves the
// existing standalone-archive workflow), then the OS-standard config
// location (for package-manager installs where cwd is arbitrary). Falls
// through to the OS-standard path either way, since loadConfig already
// handles a missing file gracefully by using defaults.
func resolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat("synapse.yaml"); err == nil {
		return "synapse.yaml"
	}
	return config.DefaultConfigPath()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		initFlags := flag.NewFlagSet("init", flag.ExitOnError)
		initPath := initFlags.String("config", "", "Path to write the scaffolded config (default: OS-standard config location)")
		nonInteractive := initFlags.Bool("yes", false, "Skip interactive prompts and write defaults only (for scripts/CI)")
		initFlags.Parse(os.Args[2:])
		runInitCommand(*initPath, !*nonInteractive)
		return
	}

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

	// Load UI HTML files. Tries the standalone-archive relative layout
	// first (ui/, ../ui/, ../../ui/ -- covers different working
	// directories the binary might be launched from), then a
	// package-manager install layout resolved relative to the actual
	// binary location (e.g. Homebrew's bin/../share/synapse/ui) -- see
	// resolveUIPath.
	uiHTML = loadUIFile("index.html", "Failed to load UI file, UI will not be available")
	uiSessionHTML = loadUIFile("session.html", "Failed to load session UI file, page will not be available")

	// Load configuration
	resolvedConfigPath := resolveConfigPath(*configPath)
	cfg, err := loadConfig(resolvedConfigPath)
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	// Pre-warm the tiktoken encoder now, during startup, so the first real
	// request doesn't pay the one-time BPE table load cost (~180ms observed
	// locally) inside live request latency.
	if _, err := budget.CountTokens("warmup", "cl100k_base"); err != nil {
		slog.Warn("Failed to pre-warm tiktoken encoder", "error", err)
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

	// Apply the configured log level now that config is loaded. The logger
	// itself was created earlier (before config load), so early startup
	// logs have somewhere to go -- this just adjusts its verbosity
	// threshold in place rather than reconstructing the logger.
	prettyLogger.SetLevel(parseLogLevel(cfg.LogLevel))

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

	// If the configured model path isn't found relative to the current
	// directory (the standalone-archive layout), try package-manager and
	// OS-standard fallback locations before giving up.
	if _, err := os.Stat(cfg.ModelPath); os.IsNotExist(err) {
		resolved := false

		// Package-manager layout: bin/ and share/ as siblings under the
		// same prefix (Homebrew, Linuxbrew, and most FHS-style installs).
		// Needed because post_install's inreplace only rewrites model-path
		// the first time it creates a config -- if that config is later
		// deleted and `synapse init` is run manually, the binary has no
		// other way to know it's a package-manager install.
		if exe, eerr := os.Executable(); eerr == nil {
			if resolvedExe, eerr := filepath.EvalSymlinks(exe); eerr == nil {
				candidate := filepath.Join(filepath.Dir(resolvedExe), "..", "share", "synapse", "models", "all-MiniLM-L6-v2", "model.onnx")
				if _, ferr := os.Stat(candidate); ferr == nil {
					slog.Info("Configured model-path not found, using package install location", "configured", cfg.ModelPath, "fallback", candidate)
					cfg.ModelPath = candidate
					resolved = true
				}
			}
		}

		if !resolved {
			if fallback := config.DefaultModelPath(); fallback != "" {
				if _, ferr := os.Stat(fallback); ferr == nil {
					slog.Info("Configured model-path not found, using OS-standard fallback location", "configured", cfg.ModelPath, "fallback", fallback)
					cfg.ModelPath = fallback
				}
			}
		}
	}
	
	// Initialize embedder
	embedderInstance, err := embedder.NewEmbedder(cfg.EmbedderType, cfg.OpenAIAPIKey, cfg.ModelPath, "")
	if err != nil {
		slog.Error("Failed to create embedder", "error", err)
		os.Exit(1)
	}

	// Initialize session manager for trace inspector session inference.
	// 30 min TTL: idle conversations age out of the in-memory map.
	sessionMgr := session.NewManager(30 * time.Minute)


	// Create API server first, so its RecordCompileTime method can be
	// wired into the proxy below.
	apiServer := api.NewAPIServer(storeInstance, embedderInstance, cfg, *persistTraces, sessionMgr)

	// Create proxy with store and embedder
	proxyInstance, err := proxy.NewProxy(cfg.UpstreamURL, storeInstance, embedderInstance, cfg, sessionMgr, apiServer.RecordCompileTime)
	if err != nil {
		slog.Error("Failed to create proxy", "error", err)
		os.Exit(1)
	}
	
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

	// Serve session UI at GET /ui/session
	r.Get("/ui/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(uiSessionHTML)
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

// parseLogLevel converts a config log-level string into charmbracelet/log's
// Level type. Defaults to InfoLevel for empty or unrecognized values, rather
// than erroring, since the logger is already initialized and startup
// shouldn't fail over a typo'd log-level string.
func parseLogLevel(level string) charmlog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return charmlog.DebugLevel
	case "info":
		return charmlog.InfoLevel
	case "warn", "warning":
		return charmlog.WarnLevel
	case "error":
		return charmlog.ErrorLevel
	default:
		return charmlog.InfoLevel
	}
}

// resolveUIPath finds a UI asset by trying, in order: the standalone-
// archive relative layout, then a location relative to the executable
// itself matching a package-manager install layout (e.g. Homebrew's
// bin/../share/synapse/ui). Returns "" if the file isn't found under any
// of them.
func resolveUIPath(name string) string {
	candidates := []string{
		filepath.Join("ui", name),
		filepath.Join("..", "ui", name),
		filepath.Join("..", "..", "ui", name),
	}

	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		execDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(execDir, "..", "share", "synapse", "ui", name),
		)
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// loadUIFile loads a UI asset via resolveUIPath, logging a warning and
// returning a placeholder page if it can't be found under any layout.
func loadUIFile(name, warnMsg string) []byte {
	if path := resolveUIPath(name); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
	}
	slog.Warn(warnMsg, "file", name)
	return []byte("<html><body><h1>UI not available</h1></body></html>")
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

	cfg := *config.DefaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Blank db-path in the file means "use the OS-standard default" per the
	// documented config comment -- apply the same fallback DefaultConfig()
	// gets automatically, since yaml.Unmarshal alone won't do it.
	if cfg.DBPath == "" {
		cfg.DBPath = config.DefaultDBPath()
	}

	// If OpenAI API key not in config, try environment variable
	if cfg.OpenAIAPIKey == "" {
		cfg.OpenAIAPIKey = os.Getenv("OPENAI_API_KEY")
	}

	return &cfg, nil
}

// runInitCommand scaffolds a synapse.yaml at the OS-standard config
// location (see config.DefaultConfigPath), for package-manager installs
// where there's no natural "next to the binary" place for it the way
// there is when running from an extracted release archive.
func runInitCommand(explicitPath string, interactive bool) {
	path := explicitPath
	if path == "" {
		path = config.DefaultConfigPath()
	}

	if _, err := os.Stat(path); err == nil {
		fmt.Printf("Config already exists at %s -- not overwriting.\n", path)
		fmt.Printf("Edit it directly, or delete it and re-run `synapse init` to start fresh.\n")
		return
	}

	reader := bufio.NewReader(os.Stdin)

	if interactive {
		fmt.Printf("Create a config file at %s? [Y/n]: ", path)
		input, _ := reader.ReadString('\n')
		if answer := strings.TrimSpace(strings.ToLower(input)); answer == "n" || answer == "no" {
			fmt.Println("Skipped -- no config file created.")
			return
		}
	}

	upstreamURL := defaultOllamaURL
	if interactive {
		upstreamURL = promptProvider(reader)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create config directory %s: %v\n", dir, err)
		os.Exit(1)
	}

	// Keep this in sync with synapse.yaml.example -- duplicated rather than
	// embedded from the repo file, since go:embed can't reach outside this
	// package directory. upstream-url is filled in from the provider
	// prompt above (or the Ollama default in --yes/non-interactive mode).
	template := `# Synapse Context Compiler Configuration
# Generated by ` + "`synapse init`" + `

# Required: Upstream model server URL (must start with http:// or https://)
upstream-url: "` + upstreamURL + `"

# Listen address for the proxy (must default to 127.0.0.1 for security)
listen-addr: "127.0.0.1:8080"

# Maximum tokens in compiled context
token-budget: 3000

# Embedding backend: "onnx" or "openai"
embedder-type: "onnx"

# Path to the ONNX model file (only used when embedder-type is "onnx").
# Leave as the relative default below -- if it's not found next to the
# binary, Synapse automatically falls back to the OS-standard model
# location instead.
model-path: "models/all-MiniLM-L6-v2/model.onnx"

# SQLite database path. Leave blank to use the OS-standard data directory.
db-path: ""

# OpenAI API key (can also be set via OPENAI_API_KEY environment variable)
openai-api-key: ""

# Weights for the 4-factor scoring model
weight-semantic-similarity: 0.4
weight-recency: 0.1
weight-importance: 0.3
weight-task-alignment: 0.2

# Logging level: debug, info, warn, error
log-level: "info"

# Cosine similarity threshold above which two memories are treated as
# duplicates and one is dropped (0.0-1.0)
deduplication-threshold: 0.92
`

	if err := os.WriteFile(path, []byte(template), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write config to %s: %v\n", path, err)
		os.Exit(1)
	}

	fmt.Printf("\nCreated %s\n", path)
	fmt.Printf("upstream-url set to %s\n\n", upstreamURL)

	if !interactive {
		fmt.Println("Next steps:")
		fmt.Println("  1. Edit the file above if needed")
		fmt.Printf("  2. Run: synapse --config %s\n", path)
		return
	}

	fmt.Printf("Then start Synapse: synapse --config %s\n", path)
	printStartupTip(reader)
}

// Known upstream defaults for the provider prompt. Anthropic and OpenAI
// point at the real hosted APIs -- Authorization is passed through
// untouched from the client's own request (see proxy.go), so no API key
// is collected or stored here. OpenRouter is OpenAI-compatible, hence the
// same /v1 suffix convention.
const (
	defaultOllamaURL     = "http://localhost:11434"
	defaultOpenAIURL     = "https://api.openai.com/v1"
	defaultAnthropicURL  = "https://api.anthropic.com"
	defaultOpenRouterURL = "https://openrouter.ai/api/v1"
)

// promptProvider asks which upstream model provider Synapse should proxy
// to, and returns the URL to write into upstream-url. Only the URL is
// collected -- Synapse forwards the client's own Authorization header
// upstream rather than storing a key in config (see proxy.go), so there's
// nothing else to ask here. Falls back to the Ollama default (matching
// the previous hardcoded template value) on empty input, an explicit
// skip, or an unrecognized choice.
func promptProvider(reader *bufio.Reader) string {
	fmt.Println()
	fmt.Println("Which upstream provider are you using?")
	fmt.Println("  1. Ollama (local) -- default")
	fmt.Println("  2. OpenAI")
	fmt.Println("  3. Anthropic")
	fmt.Println("  4. OpenRouter")
	fmt.Println("  5. Other -- I'll enter a custom URL")
	fmt.Print("Choice [1-5]: ")

	input, _ := reader.ReadString('\n')
	switch strings.TrimSpace(input) {
	case "2":
		return defaultOpenAIURL
	case "3":
		return defaultAnthropicURL
	case "4":
		return defaultOpenRouterURL
	case "5":
		fmt.Print("Enter upstream URL: ")
		custom, _ := reader.ReadString('\n')
		if url := strings.TrimSpace(custom); url != "" {
			return url
		}
		fmt.Println("No URL entered -- falling back to Ollama default.")
		return defaultOllamaURL
	default: // "1", empty input, or anything unrecognized
		return defaultOllamaURL
	}
}

// printStartupTip surfaces the run-at-login option. Homebrew installs
// already have this solved via brew services, tested end-to-end in the
// tap formula -- so this only asks/explains for that path today rather
// than generating launchd/systemd unit files itself, since non-Homebrew
// installs are a small, unconfirmed slice of the install base and custom
// service-file generation is real surface area not worth adding until
// that gap is confirmed to matter.
func printStartupTip(reader *bufio.Reader) {
	fmt.Println()
	fmt.Print("Run Synapse automatically on login? [y/N]: ")
	input, _ := reader.ReadString('\n')
	if strings.TrimSpace(strings.ToLower(input)) != "y" {
		return
	}

	fmt.Println()
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		fmt.Println("If you installed via Homebrew, run:")
		fmt.Println("  brew services start synapse")
		fmt.Println("(Manual builds: no service file yet -- start Synapse from your shell profile or a systemd/launchd unit for now.)")
	} else {
		fmt.Println("No automated setup for this OS yet -- add Synapse to your startup")
		fmt.Println("applications manually for now.")
	}
}
