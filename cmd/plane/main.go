// Command plane is the Synapse v2 control plane binary.
//
// Phase 3 scope: boot, load and validate configuration, connect to PostgreSQL,
// run the synapse_global migrations, and serve GET /health plus the
// admin-guarded POST /v2/tenants. Tenant-scoped APIs, metering, and billing are
// later phases.
package main

import (
	"context"
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

	"synapse/internal/billing"
	"synapse/internal/conflict"
	"synapse/internal/ledger"
	"synapse/internal/plane"
	"synapse/internal/tenant"

	charmlog "github.com/charmbracelet/log"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultConfigPath is the file consulted when --config is not given.
const defaultConfigPath = "synapse-plane.yaml"

// connectTimeout bounds the startup database connection and migrations. A plane
// that cannot reach its database exits rather than serving requests it cannot
// fulfil.
const connectTimeout = 60 * time.Second

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

	// PostgreSQL is opened, pinged, and migrated before anything is served: a
	// control plane that cannot reach its database has no useful answer for any
	// route, so failing here is cheaper than failing per request later.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseDSN)
	if err != nil {
		// pgx wraps the raw DSN in its parse error and the DSN carries the
		// password, so the error text is deliberately never logged.
		logger.Error("Failed to parse the database DSN")
		os.Exit(1)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), connectTimeout)
	defer cancelStartup()

	pool, err := pgxpool.NewWithConfig(startupCtx, poolCfg)
	if err != nil {
		logger.Error("Failed to open the database pool", "db_host", plane.DBHost(cfg.DatabaseDSN))
		os.Exit(1)
	}
	// pgxpool.Close is idempotent, so the explicit close on the shutdown path
	// and this deferred safety net cannot conflict.
	defer pool.Close()

	// db_host only, never the DSN: the fatal line still names the target without
	// echoing the credential that lives in the connection string.
	if err := pool.Ping(startupCtx); err != nil {
		logger.Error("Failed to connect to the database", "db_host", plane.DBHost(cfg.DatabaseDSN))
		os.Exit(1)
	}

	if err := tenant.RunMigrations(startupCtx, pool); err != nil {
		logger.Error("Failed to run the database migrations", "db_host", plane.DBHost(cfg.DatabaseDSN), "error", err)
		os.Exit(1)
	}
	logger.Info("migrations complete", "schema", tenant.SchemaName)

	if cfg.AdminToken == "" {
		logger.Warn("No admin token configured -- POST /v2/tenants rejects every request until one is set")
	}

	// The sync endpoint's two v2 dependencies are wired from the tenant layer:
	// the store-backed memory writer (schema-per-tenant, reusing the store's
	// sanitization) and the tenant JWT middleware, which the plane cannot import
	// directly (internal/tenant imports this package).
	//
	// Phase 9 adds the read half: one tenant-backed value serves as both the
	// writer and the searcher, so the search endpoint reuses the same per-tenant
	// PGStore cache (and the same connection pool) the write path built.
	memoryStore := tenant.NewMemoryWriter(pool)

	// Phase 13: a memory an agent pushes that contradicts one already stored in the
	// tenant is marked on both sides -- the older row becomes a superseded candidate
	// and the scorer demotes it -- instead of either row being dropped, and the
	// contradiction is visible in the log by id. The threshold is the detector's own
	// documented default; exposing it as a plane config key is a follow-up, since a
	// wrong threshold here only mislabels a memory rather than losing one.
	memoryStore.SetConflictDetector(conflict.NewContradictionDetector(conflict.DefaultJaccardThreshold))

	// Phase 17: the audit endpoint's dependency. GET /v2/ledger/verify walks the
	// calling tenant's signed chain and reports the first break; the adapter
	// fetches that tenant's signing secret and hands it to the walk, and is
	// built here because it is the only place that can see both internal/ledger
	// and internal/plane.
	ledgerVerifier := newLedgerVerifier(pool)

	// Phase 19: the compliance endpoint's dependency. GET /v2/compliance/audit
	// pages one tenant's signed chain and records every read in
	// synapse_global.compliance_access_log; the implementation is the ledger
	// package's own read path, which is also the only place that could not be
	// replaced by a fake in that endpoint's integration test.
	complianceAuditor := ledger.NewAuditor(pool)

	// Phase 27: the billing webhook's dependency. POST /v2/billing/webhook is
	// Stripe's own entry point and carries no token -- the Stripe-Signature
	// header is its authentication -- so what is wired here is the handler
	// itself rather than a middleware. It is built here because this is the only
	// place that can see both internal/billing and the pool the plane already
	// opened, and because internal/billing reads the schema name from
	// internal/tenant, which imports internal/plane: the reverse import would be
	// a cycle, exactly as it would be for the ledger verifier above.
	billingWebhook := billing.WebhookHandler(cfg, pool)

	srv := plane.NewServer(
		cfg,
		pool,
		tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool),
		memoryStore,
		memoryStore,
		ledgerVerifier,
		complianceAuditor,
		tenant.JWTMiddleware(cfg),
		logger,
		billingWebhook,
	)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info(fmt.Sprintf("Synapse Control Plane v%s listening", plane.Version), "addr", cfg.ListenAddr)

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

	pool.Close()

	logger.Info("Control plane stopped")
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
