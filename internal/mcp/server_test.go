package mcp

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"synapse/internal/config"
	"synapse/internal/store"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// The concrete store cmd/synapse holds must satisfy this package's seam. The
// assertion lives here rather than in server.go so a signature drift fails a
// test file instead of the package main imports.
var _ Store = (*store.Store)(nil)

// newTestServer builds a server whose compile pipeline is a stub.
//
// The transports are what these tests exercise, so the pipeline is never the
// subject: a stub keeps a tools/list or a TCP handshake from depending on a
// store, an embedder, or an ONNX session. The tests that care whether a compile
// really compiled build the real pipeline instead -- see compile_test.go.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(nil, *config.DefaultConfig(), newStubCompiler())
}

// initializeRequest is the handshake every MCP client opens with, in the shape
// mcp-go's own examples use.
func initializeRequest() mcpgo.InitializeRequest {
	var req mcpgo.InitializeRequest
	req.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcpgo.Implementation{Name: "synapse-test", Version: "0.0.0"}
	return req
}

// TestServeStdioListsCompileTool drives the stdio transport itself, but through
// buffers instead of the process's descriptors. It is the automated form of
//
//	echo '{"jsonrpc":"2.0","method":"tools/list","id":1}' | synapse --mcp
//
// including the detail that matters: the request is answered without a preceding
// initialize, which is the shape a hand-typed probe has.
func TestServeStdioListsCompileTool(t *testing.T) {
	srv := newTestServer(t)

	var out bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"tools/list","id":1}` + "\n")

	done := make(chan error, 1)
	go func() { done <- srv.serveStdioWith(context.Background(), in, &out) }()

	select {
	case err := <-done:
		require.NoError(t, err, "EOF on input is how the stdio transport ends")
	case <-time.After(10 * time.Second):
		t.Fatal("serveStdioWith did not return after its input reached EOF")
	}

	require.Contains(t, out.String(), `"tools"`, "the reply must be a tools/list result")
	require.Contains(t, out.String(), compileToolName)
	require.NotContains(t, out.String(), "level=", "nothing but JSON-RPC may reach the stdio stream")
}

// TestServeStdioStopsWhenContextIsCancelled covers the shutdown path main uses:
// the transport is blocked reading a stream that never delivers a line, and the
// context it was started with is cancelled out from under it. It must return
// rather than hold the process open.
func TestServeStdioStopsWhenContextIsCancelled(t *testing.T) {
	srv := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A pipe whose write end is never written to: without the cancellation this
	// read never completes, which is the whole point.
	pr, pw, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pw.Close() })
	t.Cleanup(func() { _ = pr.Close() })

	done := make(chan error, 1)
	go func() { done <- srv.serveStdioWith(ctx, pr, &bytes.Buffer{}) }()

	// Give Listen a moment to register its session and block on the read; then
	// cancel while it is blocked.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled context is a clean stop, not a transport error")
	case <-time.After(10 * time.Second):
		t.Fatal("serveStdioWith did not stop after its context was cancelled")
	}
}

// TestServeRejectsUnknownTransport proves a typo cannot silently pick a
// transport, and pins the loopback-only address rule at the seam that enforces
// it.
func TestServeRejectsUnknownTransport(t *testing.T) {
	srv := newTestServer(t)
	err := srv.Serve(context.Background(), "carrier-pigeon", DefaultTCPPort)
	require.ErrorContains(t, err, "unknown transport")

	addr, err := listenAddr(DefaultTCPPort)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:8765", addr, "the TCP transport must bind loopback, never 0.0.0.0")

	for _, port := range []int{0, -1, 65536, 70000} {
		_, err := listenAddr(port)
		require.Error(t, err, "port %d must be rejected rather than resolved to something else", port)
	}
}

// TestServeTCPBindsLoopbackOnlyAndAnswers is the end-to-end proof for the TCP
// transport: a real listener on a real port, exercised by mcp-go's own
// Streamable HTTP client, plus the negative half -- the same port must not be
// reachable on this host's routable address.
func TestServeTCPBindsLoopbackOnlyAndAnswers(t *testing.T) {
	port := freeLoopbackPort(t)
	srv := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, transportTCP, port) }()
	waitForListener(t, addr)

	// Skipped when this host has no non-loopback IPv4 to aim at: the assertion
	// is that the connection is refused, and there is nothing to refuse on a
	// machine that only has loopback.
	if hostIP := firstNonLoopbackIPv4(t); hostIP != "" {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", hostIP, port), time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("the MCP TCP transport answered on %s -- it must bind loopback only", hostIP)
		}
	}

	c, err := client.NewStreamableHttpClient(fmt.Sprintf("http://%s%s", addr, mcpEndpointPath))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(ctx))

	_, err = c.Initialize(ctx, initializeRequest())
	require.NoError(t, err)

	tools, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	require.NoError(t, err)

	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, compileToolName)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "cancelling the context drains the listener and returns")
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

// freeLoopbackPort returns a port that was free a moment ago. There is an
// inherent race between closing this probe listener and Start binding the same
// port; on a test machine that race does not have a winner, and a lost race
// fails the test loudly rather than silently testing nothing.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())

	return port
}

// waitForListener blocks until addr accepts a connection, or fails the test.
func waitForListener(t *testing.T, addr string) {
	t.Helper()

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 10*time.Second, 20*time.Millisecond, "the TCP transport never listened on %s", addr)
}

// firstNonLoopbackIPv4 returns this host's first routable IPv4 address, or "" if
// it has none. Used only by the negative half of the TCP test.
func firstNonLoopbackIPv4(t *testing.T) string {
	t.Helper()

	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)

	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}
