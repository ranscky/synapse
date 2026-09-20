// Command plane is the Synapse v2 control plane binary.
//
// Phase 1 scope: boot, load and validate configuration, and serve GET /health.
// There is deliberately no database connection, no auth, and no feature
// surface beyond that.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"synapse/internal/plane"

	charmlog "github.com/charmbracelet/log"
	"github.com/go-chi/chi/v5"
)

// version is reported by GET /health and in the startup log line. Both read
// this one constant so they can never disagree.
const version = "2.0.0"

// defaultConfigPath is the file consulted when --config is not given.
const defaultConfigPath = "synapse-plane.yaml"

// healthResponse is the GET /health body, in wire order.
type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

var (
	configPath = flag.String("config", "", "Path to the control plane YAML config (default: ./synapse-plane.yaml)")
	port       = flag.Int("port", 0, "Override the port in listen-addr (0 keeps the configured port)")
)

func main() {
	flag.Parse()

	logger := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.Kitchen,
		Prefix:          "plane",
	})

	resolvedPath := *configPath
	if resolvedPath == "" {
		resolvedPath = defaultConfigPath
	}

	if _, err := os.Stat(resolvedPath); os.IsNotExist(err) {
		logger.Warn("Control plane config file not found, using defaults and environment", "path", resolvedPath)
	}

	cfg, err := plane.LoadConfig(resolvedPath)
	if err != nil {
		logger.Error("Failed to load control plane config", "error", err)
		os.Exit(1)
	}

	if perms := plane.UnsafePermissions(resolvedPath); perms != "" {
		logger.Warn("Control plane config is readable by other users and holds secrets -- recommended fix: chmod 600",
			"path", resolvedPath, "current_permissions", perms)
	}

	if *port != 0 {
		if err := applyPortOverride(cfg, *port); err != nil {
			logger.Error("Invalid --port override", "error", err)
			os.Exit(1)
		}
	}

	// Fatal by design: a missing/short JWT secret or a missing DSN must never
	// reach ListenAndServe. Validate's messages name keys only, never values.
	if err := cfg.Validate(); err != nil {
		logger.Error("Invalid control plane configuration", "error", err)
		os.Exit(1)
	}

	logger.SetLevel(parseLogLevel(cfg.LogLevel))

	// RedactedFields reports each secret as set/unset -- the only form in
	// which config is ever logged by this binary.
	logger.Info("Control plane config loaded", cfg.RedactedFields()...)

	router := chi.NewRouter()
	router.Get("/health", handleHealth)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info(fmt.Sprintf("Synapse Control Plane v%s listening", version), "addr", cfg.ListenAddr)

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logger.Error("Control plane server failed", "error", err)
		os.Exit(1)
	case sig := <-stop:
		logger.Info("Shutting down control plane", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("Control plane shutdown failed", "error", err)
		os.Exit(1)
	}

	logger.Info("Control plane stopped")
}

// handleHealth serves GET /health -- the only route Phase 1 registers: no
// database probe, no auth, no feature surface.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	body, err := json.Marshal(healthResponse{Status: "ok", Version: version})
	if err != nil {
		http.Error(w, `{"status":"error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// A failed write means the client went away; there is nothing useful to
	// do about it on the health path.
	_, _ = w.Write(body)
}

// applyPortOverride replaces the port in cfg.ListenAddr with port, preserving
// the configured host. net.SplitHostPort/JoinHostPort are used instead of
// string surgery so IPv6 hosts survive the rewrite; the result still has to
// pass PlaneConfig.Validate's loopback check afterwards.
func applyPortOverride(cfg *plane.PlaneConfig, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("plane: --port must be between 1 and 65535")
	}

	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("plane: cannot apply --port to listen-addr: %w", err)
	}

	cfg.ListenAddr = net.JoinHostPort(host, strconv.Itoa(port))

	return nil
}

// parseLogLevel converts a config log-level string into charmbracelet/log's
// Level type. Unrecognized values fall back to InfoLevel rather than
// erroring: the plane should not refuse to boot over a typo'd log-level.
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
