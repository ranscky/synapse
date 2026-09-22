// Package mcp exposes Synapse's memory store over the Model Context Protocol.
//
// Phase 22 shipped the boot path: the two transports, the tool-registration
// seam, and one placeholder tool that proved a request reaches a handler and a
// result comes back. Phase 23 replaces that placeholder with the first real
// tool, synapse_compile, which hands the conversation it was given to the same
// pipeline POST /v1/compile runs -- internal/api's CompileContext -- so an
// editor's compile and an HTTP compile are one compilation rather than two
// implementations that agree until they don't.
//
// Two .clinerules for this package are visible in the code rather than only in
// prose: the compile tool accepts memory content and therefore shares the REST
// write path's sanitization pipeline (it goes through the store's write, which
// sanitizes, and through internal/api's own input validators before that), and
// it returns the per-memory 4-factor score breakdown plus the trace id of what
// it surfaced, so a caller can see why a memory was compiled rather than only
// that it was.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"synapse/internal/config"
	"synapse/internal/store"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

const (
	// DefaultTCPPort is the loopback port the TCP transport binds when a caller
	// asks for TCP. cmd/synapse's --mcp-port flag defaults to it.
	DefaultTCPPort = 8765

	// serverName and serverVersion identify this process to MCP clients. The
	// version is a placeholder: the standalone binary has no version constant of
	// its own to borrow (the only one in the tree, internal/plane.Version, names
	// the control plane, a different component).
	serverName    = "synapse"
	serverVersion = "0.1.0"

	// transportStdio and transportTCP are the only two transport names Serve
	// accepts.
	transportStdio = "stdio"
	transportTCP   = "tcp"

	// loopbackHost is the only host either transport is allowed to bind, and
	// mcpEndpointPath is where the HTTP transport answers on it.
	loopbackHost    = "127.0.0.1"
	mcpEndpointPath = "/mcp"

	// shutdownGrace bounds how long Serve waits for the TCP listener to drain
	// once its context is cancelled.
	shutdownGrace = 5 * time.Second
)

// Store is everything the MCP server needs from storage.
//
// It names store.Backend, the project's own backend contract, rather than the
// concrete *store.Store: the store is this package's one external dependency, so
// it is held as an interface, and *store.Store -- what cmd/synapse holds --
// satisfies it as it stands. No connection is opened or owned here; the field is
// a seam, not a lifetime.
type Store = store.Backend

// Server is Synapse's MCP front end: the tools it exposes and the transports it
// can be reached over.
//
// Like the rest of the v2 wiring it holds no logger. The structured logger is
// installed process-wide by main, and the MCP protocol itself is written to the
// transport, never to a log: in stdio mode a log line on stdout would corrupt
// the JSON-RPC stream, which is why every logger in this process writes to
// stderr and why this type cannot be the exception.
type Server struct {
	store    Store
	cfg      config.Config
	compiler Compiler
	mcp      *mcpserver.MCPServer
}

// NewServer builds the MCP server and registers this phase's tools.
//
// memStore is the storage the tool set is allowed to reach; the compile tool
// does not query it directly -- it goes through pipeline, which reads and
// writes it the same way the HTTP front end does. Both may be nil, and a nil
// pipeline is reported to a caller as an error result rather than as a panic:
// a server built without a compile path still has to answer, because the
// alternative is a process that starts and then dies inside a tool call.
func NewServer(memStore Store, cfg config.Config, pipeline Compiler) *Server {
	s := &Server{store: memStore, cfg: cfg, compiler: pipeline}
	s.mcp = mcpserver.NewMCPServer(serverName, serverVersion, mcpserver.WithToolCapabilities(false))
	s.registerTools()
	return s
}

// registerTools registers every tool this phase ships.
//
// Each tool's definition and handler live in their own file, so this list stays
// a table of what the server offers rather than a place where tool logic
// accumulates: see compile.go for synapse_compile.
func (s *Server) registerTools() {
	s.registerCompileTool()
}

// Serve runs the MCP server over transport until ctx is cancelled.
//
// transport is "stdio" -- JSON-RPC on this process's stdin/stdout, the shape
// editor MCP clients spawn -- or "tcp" -- Streamable HTTP on
// http://127.0.0.1:<port>/mcp. Anything else is an error rather than a
// fallback, so a typo in configuration cannot silently pick a transport.
func (s *Server) Serve(ctx context.Context, transport string, port int) error {
	switch transport {
	case transportStdio:
		return s.serveStdio(ctx)
	case transportTCP:
		return s.serveTCP(ctx, port)
	default:
		return fmt.Errorf("mcp: unknown transport %q (want %q or %q)", transport, transportStdio, transportTCP)
	}
}

// serveStdio serves the MCP protocol on this process's stdin and stdout.
//
// NewStdioServer+Listen rather than server.ServeStdio: ServeStdio installs its
// own signal handler and its own context, so it could never stop for the context
// main cancels. Listen takes both the context and the streams, which is also
// what makes serveStdioWith testable without touching the real process
// descriptors.
func (s *Server) serveStdio(ctx context.Context) error {
	slog.Info("MCP server listening", "transport", transportStdio)
	return s.serveStdioWith(ctx, os.Stdin, os.Stdout)
}

// serveStdioWith is serveStdio with its streams injected, so a test can drive
// the real protocol through buffers.
//
// A cancelled context and an exhausted input are both how this transport is
// meant to end -- mcp-go's Listen returns the context's error when it is
// cancelled, and nil at EOF -- so neither is reported as a failure. Anything
// else is wrapped: that is a transport error the caller should see.
func (s *Server) serveStdioWith(ctx context.Context, in io.Reader, out io.Writer) error {
	err := mcpserver.NewStdioServer(s.mcp).Listen(ctx, in, out)
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("mcp: stdio transport: %w", err)
}

// serveTCP serves the MCP protocol over Streamable HTTP on 127.0.0.1:port.
//
// It returns when ctx is cancelled -- after draining the listener -- or when the
// listener fails, so a caller that ran it in a goroutine learns about a bind
// failure instead of serving nothing.
func (s *Server) serveTCP(ctx context.Context, port int) error {
	addr, err := listenAddr(port)
	if err != nil {
		return err
	}

	httpSrv := mcpserver.NewStreamableHTTPServer(s.mcp, mcpserver.WithEndpointPath(mcpEndpointPath))
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Start(addr) }()

	slog.Info("MCP server listening", "transport", transportTCP, "addr", addr, "path", mcpEndpointPath)

	select {
	case err := <-serveErr:
		// A bind failure never succeeds later, so it is the one case worth
		// reporting; http.ErrServerClosed is Start's normal exit after a
		// shutdown.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("mcp: tcp transport on %s: %w", addr, err)
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("mcp: tcp transport shutdown: %w", err)
	}
	return nil
}

// listenAddr returns the address the TCP transport binds for port.
//
// The host is hard-coded to loopback and no configuration widens it: this server
// has no authentication of its own in this phase, so binding it to a routable
// interface would be a memory-readable-by-anyone surface. A port outside
// 1-65535 is an error rather than a zero value that silently means "pick one",
// because the caller's intent then cannot be recovered from the result.
func listenAddr(port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("mcp: tcp port %d is out of range (1-65535)", port)
	}
	return net.JoinHostPort(loopbackHost, strconv.Itoa(port)), nil
}
