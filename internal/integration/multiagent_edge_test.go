//go:build integration

// Phase 29's edge nodes: two in-process Synapse nodes, each with its own local
// store, its own HTTP surface, its own MCP server, and its own background sync
// client pointing at the shared control plane.
//
// They are built the way cmd/synapse builds one -- the same constructors, in the
// same order, with the same wiring decisions -- because the property this phase
// asserts is a property of that composition: two agents that never share a
// database, sharing a brain through the plane. A test double for either node would
// be a test of the double.
//
// Two things differ from the binary, and both are deliberate. The embedder is a
// deterministic one (a real ONNX session cannot be loaded per test), which is why
// every vector here is a unit vector on one of 384 axes: pgvector compares them
// exactly as it compares real ones, so the storage, the search, and the scoring
// are the production ones. And the HTTP listen addresses are httptest's, since
// nothing asserts which port an edge binds.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"synapse/internal/api"
	"synapse/internal/config"
	"synapse/internal/mcp"
	"synapse/internal/proxy"
	"synapse/internal/session"
	"synapse/internal/store"
	"synapse/internal/sync"

	charmlog "github.com/charmbracelet/log"
	"github.com/go-chi/chi/v5"
	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// The MCP tool name this test calls, spelled as a client spells it. It is an
// unexported constant in internal/mcp, which is exactly the point: a tool name is
// part of a client's contract, so pinning the literal here is what would catch a
// rename that broke every editor integration.
const multiagentWriteTool = "synapse_write_memory"

// multiagentEmbedder is the deterministic stand-in for the ONNX model: one unit
// vector per text, on the axis its hash selects.
//
// Deterministic is what matters, not realistic: the same text must embed to the
// same vector on the edge that stored it and on the edge that searches for it,
// because both are the same process here and the plane only ever holds the vector
// the writing node computed. Distinct texts landing on distinct axes is what keeps
// two different memories from looking like duplicates to internal/dedup, and a unit
// vector is what keeps cosine similarity a real number rather than a NaN that
// json.Marshal would reject.
type multiagentEmbedder struct{}

// Embed implements embedder.Embedder and every narrower embedder interface the
// servers take.
func (multiagentEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vector := make([]float32, store.EmbeddingDimensions)

	index := 0
	if text != "" {
		hasher := fnv.New32a()
		_, _ = hasher.Write([]byte(text))
		index = int(hasher.Sum32() % store.EmbeddingDimensions)
	}
	vector[index] = 1

	return vector, nil
}

// multiagentCaptureLogs points the process's default slog logger at a buffer for
// the duration of the test, and returns that buffer.
//
// The three servers log through slog (internal/proxy, internal/api, internal/mcp),
// which is the same default the binary installs charmbracelet/log into, so
// capturing it is how "no credential reached a log line" becomes an assertion
// rather than a reading of the code. The previous default is restored on cleanup,
// so no other test in this package inherits the buffer.
func multiagentCaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(charmlog.NewWithOptions(&logs, charmlog.Options{
		Level:           charmlog.DebugLevel,
		ReportTimestamp: false,
	})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &logs
}

// multiagentFreePort returns a loopback port nothing is listening on. The
// listener is closed before the port is returned, so a caller can bind it; the
// gap is a race in theory and the standard practice in Go tests, and the only
// consequence of losing it is a listen error that names the port.
func multiagentFreePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	return listener.Addr().(*net.TCPAddr).Port
}

// multiagentWaitForPort blocks until addr accepts a connection, so an MCP client
// is never built against a listener that has not started. The MCP transport's
// Start returns as soon as the listener is bound, but the goroutine that runs it
// is not synchronous with the test, which is the whole reason this exists.
func multiagentWaitForPort(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(multiagentWaitTimeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(multiagentPollInterval)
	}

	t.Fatalf("timed out after %s waiting for %s to accept connections", multiagentWaitTimeout, addr)
}

// multiagentEdge is one running in-process Synapse node.
//
// local is the node's own database, held directly so the test can read the queue
// state the flusher sees; everything else reaches the node through its HTTP
// surface or its MCP client, which is how a client would.
type multiagentEdge struct {
	name    string
	baseURL string
	local   *store.Store
	client  *mcpclient.Client
}

// newMultiagentEdge starts one node: agentID names it (and is what the plane
// stamps on what it pushes), planeURL and jwt are the control plane it syncs with,
// and upstreamURL is the model API its proxy forwards to.
//
// The config is the binary's, one key at a time. sync-interval-seconds is 1 rather
// than the default 30 so the queue drains inside the test's lifetime, and
// default-visibility is org so a memory written without a scope of its own is
// readable by the other agent -- which is what makes a shared brain shared.
func newMultiagentEdge(t *testing.T, agentID, planeURL, jwt, upstreamURL string) *multiagentEdge {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.AgentID = agentID
	cfg.ControlPlaneURL = planeURL
	cfg.ControlPlaneAPIKey = jwt
	cfg.UpstreamURL = upstreamURL
	cfg.DBPath = filepath.Join(t.TempDir(), agentID+".db")
	cfg.DefaultVisibility = store.VisibilityOrg
	cfg.SyncIntervalSeconds = 1
	cfg.SyncBatchSize = 50
	cfg.TokenBudget = 2000

	local, err := store.NewStore(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })

	// The background flusher: the only thing in this node that talks to the plane
	// on its own. It gets the concrete store -- PendingSync and MarkSynced live
	// there and nowhere else.
	syncCtx, cancelSync := context.WithCancel(context.Background())
	t.Cleanup(cancelSync)

	syncer := sync.NewSyncer(*cfg)
	go syncer.RunBackground(syncCtx, local)

	// Phase 29's fix, wired exactly as cmd/synapse wires it: the two paths that
	// store memories hand them to this decorator, so what they write is queued for
	// the plane instead of sitting local_only forever.
	writeBackend := sync.NewPendingWriter(local)

	embedder := multiagentEmbedder{}
	sessionMgr := session.NewManager(30 * time.Minute)

	apiServer := api.NewAPIServer(local, embedder, cfg, false, sessionMgr)
	proxyInstance, err := proxy.NewProxy(cfg.UpstreamURL, writeBackend, embedder, cfg, sessionMgr, apiServer.RecordCompileTime)
	require.NoError(t, err)

	mcpServer := mcp.NewServer(writeBackend, *cfg, apiServer, embedder)

	// One candidate source for all three paths: a memory this node's compile can
	// see must be reachable by its search too.
	apiServer.SetPlaneCandidates(syncer)
	proxyInstance.SetPlaneCandidates(syncer)
	mcpServer.SetPlaneCandidates(syncer)

	router := chi.NewRouter()
	router.Mount("/", apiServer.Router())
	proxyInstance.RegisterRoutes(router)

	vertex := httptest.NewServer(router)
	t.Cleanup(vertex.Close)

	edge := &multiagentEdge{name: agentID, baseURL: vertex.URL, local: local}
	edge.client = newMultiagentMCPClient(t, syncCtx, mcpServer)

	return edge
}

// newMultiagentMCPClient serves the node's MCP tools over their TCP transport and
// returns a client attached to them.
//
// The TCP transport rather than the in-process one because this test lives in
// another package and the in-process client needs the unexported *server.MCPServer:
// the cost is one loopback port per edge, and what it buys is the real transport
// a deployment exposes.
func newMultiagentMCPClient(t *testing.T, ctx context.Context, mcpServer *mcp.Server) *mcpclient.Client {
	t.Helper()

	port := multiagentFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	go func() {
		if err := mcpServer.Serve(ctx, "tcp", port); err != nil {
			slog.Warn("edge MCP server stopped", "addr", addr, "error", err)
		}
	}()

	multiagentWaitForPort(t, addr)

	client, err := mcpclient.NewStreamableHttpClient(fmt.Sprintf("http://%s/mcp", addr))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, client.Start(ctx))

	var initialize mcpgo.InitializeRequest
	initialize.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcpgo.Implementation{Name: "multiagent-integration", Version: "0.0.0"}
	_, err = client.Initialize(ctx, initialize)
	require.NoError(t, err)

	return client
}

// What a node *answers* -- the three client verbs and the two readers that go with
// them -- is in multiagent_tools_test.go, so this file stays about what a node is.

// (The client verbs live in multiagent_tools_test.go.)

// What a node *answers* -- the three client verbs (synapse_write_memory, a
// compile, a proxied turn) and the two readers that go with them (the queue state,
// a trace lookup) -- is in multiagent_tools_test.go, so this file stays about what
// a node *is*: its constructors and its wiring.
