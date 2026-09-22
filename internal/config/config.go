package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvDatabaseDSN is the environment variable that supplies the Postgres
// connection string. The same variable is read by the v2 control plane
// (internal/plane); this constant exists so the standalone binary can be
// pointed at a control plane without duplicating the key name or, worse,
// mistyping it somewhere it matters.
const EnvDatabaseDSN = "SYNAPSE_DB_DSN"

// EnvPlaneKey is the environment variable that supplies the control plane API
// key this edge node presents when it syncs. Read at config-load time exactly
// like EnvDatabaseDSN; a control-plane-api-key in the config file still wins.
// It is a secret: never log the value, only whether it is set.
const EnvPlaneKey = "SYNAPSE_PLANE_KEY"

// Config represents the application configuration
type Config struct {
	UpstreamURL              string   `yaml:"upstream-url"`
	AllowedUpstreamHosts     []string `yaml:"allowed-upstream-hosts"`
	ListenAddr               string   `yaml:"listen-addr"`
	TokenBudget              int      `yaml:"token-budget"`
	EmbedderType             string   `yaml:"embedder-type"`
	ModelPath                string   `yaml:"model-path"`
	DBPath                   string   `yaml:"db-path"`
	OpenAIAPIKey             string   `yaml:"openai-api-key"`
	// ControlPlaneURL is the address of a Synapse v2 control plane. Empty --
	// the default -- preserves the standalone SQLite behavior exactly. When it
	// is set, the store factory in internal/store builds a tenant-scoped
	// Postgres backend instead of the local SQLite file.
	ControlPlaneURL string `yaml:"control-plane-url"`
	// DatabaseDSN is the Postgres connection string used when ControlPlaneURL
	// is set; SYNAPSE_DB_DSN supplies it from the environment. It is a secret
	// (it carries the database password) and must never be logged.
	DatabaseDSN string `yaml:"database-dsn"`
	WeightSemanticSimilarity float64  `yaml:"weight-semantic-similarity"`
	WeightRecency            float64  `yaml:"weight-recency"`
	WeightImportance         float64  `yaml:"weight-importance"`
	WeightTaskAlignment      float64  `yaml:"weight-task-alignment"`
	DeduplicationThreshold   float64  `yaml:"deduplication-threshold"`
	LogLevel                 string   `yaml:"log-level"`
	RetrievalCandidateK      int      `yaml:"retrieval-candidate-k"`
	SupersessionSimilarityMin float64 `yaml:"supersession-similarity-min"`
	SupersessionSimilarityMax float64 `yaml:"supersession-similarity-max"`

	// Phase 12 conflict detection (internal/conflict). Both are defined here
	// and read by nothing yet: the detector exists and is tested, but the write
	// path that would call it has not been wired up.
	//
	// They are deliberately separate from the supersession band above, because
	// they solve a different problem. Supersession resolves a contradiction
	// within one session, by cosine similarity plus an explicit replacement
	// phrase; these resolve cross-agent, cross-session ones, by token overlap
	// over the memories' text.
	//
	// ConflictJaccardThreshold's default must stay in step with
	// conflict.DefaultJaccardThreshold. It cannot be that constant: conflict
	// imports store, store imports config, so importing conflict from here
	// would be a cycle -- hence the literal 0.4 in DefaultConfig, and this
	// note as the thing that keeps the two in step.
	ConflictJaccardThreshold float64 `yaml:"conflict-jaccard-threshold"`
	// ConflictScorePenalty is the multiplier a memory flagged as conflicting
	// will take on its score once something consumes it -- 0.5 halves it.
	// Unreferenced until then, like the detector itself.
	ConflictScorePenalty float64 `yaml:"conflict-score-penalty"`

	// v2 sync protocol (Phase 7). Added ahead of the sync work itself: the
	// push loop in a later phase reads these, this phase only defines them.
	// ControlPlaneAPIKey is the credential this node presents to the plane
	// (secret, seeded from SYNAPSE_PLANE_KEY, never logged); AgentID names
	// this node and is required whenever ControlPlaneURL is set --
	// cmd/synapse refuses to boot without it, since a plane cannot attribute
	// a push to an agent that never named itself; TeamID optionally scopes
	// writes to a team; DefaultVisibility is applied to memories written
	// with no visibility of their own; SyncBatchSize is memories per push;
	// SyncIntervalSeconds is the delay between pushes.
	ControlPlaneAPIKey  string `yaml:"control-plane-api-key"`
	AgentID             string `yaml:"agent-id"`
	TeamID              string `yaml:"team-id"`
	DefaultVisibility   string `yaml:"default-visibility"`
	SyncBatchSize       int    `yaml:"sync-batch-size"`
	SyncIntervalSeconds int    `yaml:"sync-interval-seconds"`

	// Phase 22 MCP server (internal/mcp). MCPEnabled is the config file's half
	// of the --mcp flag, so a deployment can turn the server on without a flag;
	// MCPPort > 0 selects the TCP transport (Streamable HTTP on
	// 127.0.0.1:<port>/mcp) and 0 keeps stdio, which is the shape editor MCP
	// clients spawn. Neither field is consulted anywhere in this package: the
	// flag merge and the transport choice happen in cmd/synapse, because they
	// decide what this process runs rather than how it is configured.
	MCPEnabled bool `yaml:"mcp-enabled"`
	MCPPort    int  `yaml:"mcp-port"`
}

// defaultDataDir resolves the stable, per-OS data directory used as the
// parent for both the SQLite database and the OS-standard model fallback
// location. Extracted from the original defaultDBPath so both can share
// the same resolution logic rather than duplicating it.
func defaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		return os.Getenv("APPDATA")
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support")
		}
	default: // linux and other unix-likes
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return xdg
		} else if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share")
		}
	}
	return ""
}

// defaultDBPath resolves a stable, per-OS data directory for the SQLite
// database, so persistence doesn't depend on which folder the binary
// happens to be launched from (a real risk for a distributed release
// binary run from arbitrary locations). Falls back to a bare relative
// filename if the OS data dir can't be resolved for any reason.
func defaultDBPath() string {
	const dbFile = "synapse.db"
	dataDir := defaultDataDir()
	if dataDir == "" {
		return dbFile
	}
	return filepath.Join(dataDir, "synapse", dbFile)
}

// DefaultDBPath resolves the OS-standard database path. Exported so callers
// loading config from a file can apply the same fallback that DefaultConfig
// applies automatically for a blank db-path.
func DefaultDBPath() string {
	return defaultDBPath()
}

// defaultModelPath resolves the OS-standard fallback location for the ONNX
// model. NOT used as DefaultConfig()'s primary ModelPath value -- that
// stays a cwd-relative path so the existing standalone release archive
// (extract, cd in, run ./synapse) keeps working with zero behavior change.
// This is consulted only as a fallback at startup, for package-manager
// installs (Homebrew, etc.) where there's no "models/ next to the binary"
// the way there is in the archive.
func defaultModelPath() string {
	dataDir := defaultDataDir()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "synapse", "models", "all-MiniLM-L6-v2", "model.onnx")
}

// DefaultModelPath exports the OS-standard fallback model location.
func DefaultModelPath() string {
	return defaultModelPath()
}

// defaultConfigPath resolves the OS-standard location for synapse.yaml,
// using Go's own os.UserConfigDir() (XDG_CONFIG_HOME/~/.config on Linux,
// ~/Library/Application Support on macOS, %APPDATA% on Windows) instead of
// hand-rolling per-OS logic a second time.
func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "synapse.yaml"
	}
	return filepath.Join(dir, "synapse", "synapse.yaml")
}

// DefaultConfigPath exports the OS-standard config file location. Used by
// both `synapse init` (to know where to scaffold) and main's config
// resolution (to know where to look when no explicit --config is given
// and nothing's found in the current directory).
func DefaultConfigPath() string {
	return defaultConfigPath()
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	// Try to get OpenAI API key from environment variable
	openAIAPIKey := os.Getenv("OPENAI_API_KEY")
	
	return &Config{
		UpstreamURL:              "", // Must be provided via config or flag
		ListenAddr:               "127.0.0.1:8080",
		TokenBudget:              3000,
		EmbedderType:             "onnx",
		ModelPath:                "models/all-MiniLM-L6-v2/model.onnx",
		DBPath:                   defaultDBPath(),
		OpenAIAPIKey:             openAIAPIKey,
		// No control plane by default: this is what keeps the standalone
		// binary's storage behavior identical to v1.
		ControlPlaneURL: "",
		// Seeded from the environment so container/deployment shapes that
		// inject the DSN (the same variable the control plane reads) work
		// without a config file. A database-dsn key in synapse.yaml still
		// wins, since the loader unmarshals the file over these defaults.
		DatabaseDSN:              os.Getenv(EnvDatabaseDSN),
		WeightSemanticSimilarity: 0.4,
		WeightRecency:            0.1,
		WeightImportance:         0.3,
		WeightTaskAlignment:      0.2,
		DeduplicationThreshold:   0.92, // Default deduplication threshold
		LogLevel:                 "info",
		RetrievalCandidateK:      50, // Candidate pool size pulled from semantic search before scoring
		// These two are a rough starting guess, not a tuned value -- there's
		// no real-usage data behind them yet. The band needs to sit below
		// DeduplicationThreshold (0.92): dedup already catches near-identical
		// restatements above that line, so supersession's job is the "same
		// topic, different answer" zone below it. Revisit once real
		// conversations show whether 0.5-0.90 actually separates
		// "related but different" from "unrelated" and "duplicate" well.
		SupersessionSimilarityMin: 0.5,
		SupersessionSimilarityMax: 0.90,
		// Phase 12 conflict detection. 0.4 is the token overlap level at which
		// two memories are treated as being about the same thing, and therefore
		// worth checking for a contradiction; below it they are simply
		// different subjects and nothing is compared. The detector's own
		// fixture set lands at 0.667 (a swapped value), 0.333 (added detail),
		// 0.000 (unrelated) and 0.250 (an explicit negation) -- which is why a
		// second, threshold-independent signal exists for the last one. Must
		// match conflict.DefaultJaccardThreshold (see the field's comment for
		// why the literal is repeated rather than imported). The penalty is a
		// starting guess like the supersession band: a rough knob to tune once
		// something actually reads it.
		ConflictJaccardThreshold: 0.4,
		ConflictScorePenalty:     0.5,
		// v2 sync protocol (Phase 7). Placed last so the alignment of the
		// groups above is untouched. The API key is seeded from the
		// environment exactly like DatabaseDSN, so a deployment can inject
		// it without a config file; a control-plane-api-key in the file
		// still wins, since the loader unmarshals the file over these
		// defaults. AgentID is deliberately blank: it is the operator's
		// name for this node, and startup fails on a blank one as soon as
		// ControlPlaneURL is set.
		ControlPlaneAPIKey:  os.Getenv(EnvPlaneKey),
		AgentID:             "",
		TeamID:              "",
		DefaultVisibility:   "org",
		SyncBatchSize:       20,
		SyncIntervalSeconds: 30,
		// Phase 22 MCP server: off, and stdio when it is turned on. Both are
		// deliberately the values that leave a config file which never mentions
		// MCP behaving exactly as it did before this phase.
		MCPEnabled: false,
		MCPPort:    0,
	}
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen-addr is required")
	}
	
	// Validate ListenAddr defaults to 127.0.0.1
	if !strings.HasPrefix(c.ListenAddr, "127.0.0.1") && !strings.HasPrefix(c.ListenAddr, "localhost") {
		return fmt.Errorf("listen-addr must default to 127.0.0.1 for security reasons")
	}
	
	if c.EmbedderType == "" {
		return fmt.Errorf("embedder-type is required")
	}
	
	if c.EmbedderType != "onnx" && c.EmbedderType != "openai" {
		return fmt.Errorf("embedder-type must be 'onnx' or 'openai'")
	}
	
	if c.EmbedderType == "openai" && c.OpenAIAPIKey == "" {
		return fmt.Errorf("openai-api-key is required when using OpenAI embedder")
	}
	
	// A negative candidate pool size is nonsensical; zero is left permissive
	// here (same treatment as TokenBudget and the scoring weights above) so
	// that hand-built Config structs in tests don't need to set every field
	// -- callers that actually run retrieval should treat zero/unset as
	// "fall back to the DefaultConfig value" rather than erroring here.
	// "fall back to the DefaultConfig value" rather than erroring here.
	if c.RetrievalCandidateK < 0 {
		return fmt.Errorf("retrieval-candidate-k must not be negative")
	}

	// Only reject the one combination that can never be meaningfully
	// correct -- an empty or inverted band. Values outside [0,1] are left
	// permissive (matching the DeduplicationThreshold precedent above)
	// since cosine similarity can technically go negative for genuinely
	// opposite embeddings, and this is a heuristic knob still being tuned.
	if c.SupersessionSimilarityMin > c.SupersessionSimilarityMax {
		return fmt.Errorf("supersession-similarity-min must not be greater than supersession-similarity-max")
	}

	// A negative overlap threshold can never be met by a ratio in [0,1], so it
	// would silently turn conflict detection off entirely. Zero is left
	// permissive (same treatment as RetrievalCandidateK) so hand-built Config
	// structs in tests don't have to set every knob, and values above 1 are
	// left alone too, matching the DeduplicationThreshold precedent -- this is
	// a heuristic being tuned, not a hard invariant. ConflictScorePenalty is
	// deliberately unvalidated: nothing reads it yet, and there is no single
	// sensible range for a multiplier whose job is still being defined.
	if c.ConflictJaccardThreshold < 0 {
		return fmt.Errorf("conflict-jaccard-threshold must not be negative")
	}

	// Phase 22: a port outside the TCP range can never be bound, so it is
	// rejected here rather than discovered at bind time. Zero stays permissive
	// and means "stdio" -- the same treatment RetrievalCandidateK and the
	// conflict threshold get, so a hand-built Config in a test does not have to
	// set it.
	if c.MCPPort < 0 || c.MCPPort > 65535 {
		return fmt.Errorf("mcp-port must be between 0 and 65535")
	}

	// Upstream URL is required
	if c.UpstreamURL == "" {
		return fmt.Errorf("upstream-url is required")
	}
	
	// Validate upstream URL scheme
	if !strings.HasPrefix(c.UpstreamURL, "http://") && !strings.HasPrefix(c.UpstreamURL, "https://") {
		return fmt.Errorf("upstream-url must start with http:// or https://")
	}
	
	// Validate upstream URL format
	if _, err := url.ParseRequestURI(c.UpstreamURL); err != nil {
		return fmt.Errorf("invalid upstream URL format: %w", err)
	}
	
	// Check upstream URL against allowlist if configured
	if len(c.AllowedUpstreamHosts) > 0 {
		upstreamURL, err := url.Parse(c.UpstreamURL)
		if err != nil {
			return fmt.Errorf("invalid upstream URL: %w", err)
		}
		
		host := upstreamURL.Hostname()
		allowed := false
		
		// Check if host is in allowlist
		for _, allowedHost := range c.AllowedUpstreamHosts {
			if host == allowedHost {
				allowed = true
				break
			}
		}
		
		// Special handling for localhost/127.x.x.x (common for Ollama)
		if !allowed && (host == "localhost" || strings.HasPrefix(host, "127.")) {
			// Log INFO for localhost usage (common for Ollama)
			// This will be handled at startup with proper logging context
		} else if !allowed {
			return fmt.Errorf("upstream host %s is not in allowed list", host)
		}
	}
	
	return nil
}
