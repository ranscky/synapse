//go:build integration

// Phase 29's definition of done: one scenario, two agents, one shared brain, and
// no mocks between them.
//
// Two real edge nodes (their own SQLite stores, their own HTTP and MCP surfaces,
// their own background sync clients) talk to one real control plane over real HTTP
// against a real PostgreSQL. agent_a records a decision, edge_b compiles a fresh
// session and sees it as another agent's memory, agent_b contradicts it, and the
// contradiction is visible where this product puts it: on the plane's own rows, in
// the next Memory Trace with a demoted score, in the ledger, and through the two
// compliance endpoints an enterprise tenant reads.
//
// Three properties of this file are worth naming before the first assertion,
// because each one is a place this test could have lied:
//
//   - The only mock is the model upstream. internal/embedder's ONNX session cannot
//     be loaded per test, so the embedder is a deterministic one -- unit vectors on
//     384 axes, which pgvector stores, indexes, and compares exactly as it compares
//     real ones. Everything else is the production code path.
//   - Every assertion is `require`, which aborts on the first failure. That is the
//     brief's "fail fast", and it is why this file reads as one sequence: a later
//     step's premise is an earlier step's result, so continuing past a failure
//     would only produce noise.
//   - Steps d and e assert what the shipped code does, which differs from the
//     brief's wording in two places. Both are called out verbatim at the step and
//     repeated in PROGRESS.md: the write reply a node gets cannot know about
//     another node's memory (the plane decides that, on the push), and the memory
//     that becomes the superseded candidate is the *first* one written, not the
//     second.
package integration

import (
	"context"
	"os"
	"testing"

	"synapse/internal/compiler"
	"synapse/internal/ledger"
	"synapse/internal/plane"
	"synapse/internal/store"
	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMultiAgent is the phase's scenario. Its steps are lettered as the brief
// lettered them, so a failure names the assertion group it belongs to.
func TestMultiAgent(t *testing.T) {
	ctx := context.Background()

	// --- the fixture: one database, one enterprise tenant, one plane ---------
	pool := multiagentPool(t)
	require.NoError(t, tenant.RunMigrations(ctx, pool))

	// The tenant's signing secret is wrapped under this key, so the ledger can
	// append and verify. t.Setenv restores whatever was exported, so this test
	// needs nothing in its environment.
	t.Setenv(plane.EnvMasterKey, multiagentMasterKeyHex)

	dsn := os.Getenv(multiagentDSNEnv)
	if dsn == "" {
		dsn = multiagentDefaultDSN
	}

	cfg := multiagentPlaneConfig(t, dsn)
	tenantInfo := multiagentProvision(t, ctx, cfg, pool)
	table := multiagentTable(tenantInfo.slug)
	t.Logf("STEP 0: provisioned slug=%s tenant_id=%s jwt=%d chars api_key=%d chars (never printed)",
		tenantInfo.slug, tenantInfo.tenantID, len(tenantInfo.jwt), len(tenantInfo.apiKey))

	// The plane is the real router, and it is serving before any edge exists.
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)
	_, planeURL, planeLogs := multiagentStartPlane(t, cfg, pool, provisioner)
	t.Logf("STEP 0: control plane answering at %s over the compose database", planeURL)

	// The edge node's audit sink. compiler.SetLedgerSink is process-global by
	// design (the compile path has no place to pass a tenant), so both edges share
	// this one -- which is correct here, because they are two agents of one tenant.
	// It is cleared on cleanup so no other test in this package inherits it.
	compiler.SetLedgerSink(multiagentLedgerSink{
		chain:    ledger.NewLedger(pool),
		pool:     pool,
		tenantID: tenantInfo.tenantID,
	}, compiler.EnterprisePlan)
	t.Cleanup(func() { compiler.SetLedgerSink(nil, "") })

	edgeLogs := multiagentCaptureLogs(t)
	upstream := multiagentStartUpstream(t)

	edgeA := newMultiagentEdge(t, multiagentAgentA, planeURL, tenantInfo.jwt, upstream.URL())
	edgeB := newMultiagentEdge(t, multiagentAgentB, planeURL, tenantInfo.jwt, upstream.URL())
	t.Logf("STEP 0: %s and %s are up, each with its own store and flusher", edgeA.name, edgeB.name)

	// compilations counts every request this scenario puts through a compile
	// pipeline: the five proxied turns, and the two explicit compiles. Step g and
	// step h assert the ledger against this number, so it is maintained where the
	// requests are made rather than guessed at the end.
	compilations := 0

	// --- a. five messages through edge_a reach the plane ---------------------
	//
	// This is the step the phase could not pass before Phase 29's fix, and the log
	// line says so out loud: a locally written memory is queued (sync_pending) only
	// because sync.PendingWriter stamps it, and the flusher below is what turns that
	// queue into rows in the tenant's schema.
	t.Log("STEP a: five proxied turns through edge_a, then the plane's own table")
	liveTurns := []string{
		"the order handler retries three times before it gives up",
		"the retry loop lives in the handler rather than in the client",
		"the failure only shows up under load",
		"the connection pool was exhausted before the timeout fired",
		"the next thing to look at is the auth service",
	}
	for i, message := range liveTurns {
		edgeA.chat(t, message)
		compilations++
		t.Logf("STEP a: proxied turn %d/%d through %s", i+1, len(liveTurns), edgeA.name)
	}

	t.Logf("STEP a: %s has %d memories queued for the plane", edgeA.name, edgeA.pendingSync(t))

	// The wait is for the last memory of the five, not for the first one: a flusher
	// that has pushed one batch has not yet pushed the rest, and the assertion below
	// is about all five turns.
	multiagentWaitFor(t, "edge_a's memories to reach the plane", func() bool {
		return multiagentCount(t, ctx, pool,
			`SELECT count(*) FROM `+table+` WHERE agent_id = $1`, multiagentAgentA) >= len(liveTurns)
	})

	syncedFromA := multiagentCount(t, ctx, pool,
		`SELECT count(*) FROM `+table+` WHERE agent_id = $1`, multiagentAgentA)
	t.Logf("STEP a: the plane holds %d memories from %s", syncedFromA, multiagentAgentA)
	require.GreaterOrEqual(t, syncedFromA, len(liveTurns),
		"every proxied turn writes a memory, so the plane must hold at least one per turn")

	// --- b. edge_a records the Postgres decision -----------------------------
	//
	// visibility=org is what makes it readable by the other agent at all: a private
	// memory is visible only to the agent that wrote it, in the session it wrote it
	// in, so a cross-agent trace could never contain one.
	t.Log("STEP b: edge_a writes the decision through synapse_write_memory (decision, org)")
	writeA := edgeA.writeMemory(t, multiagentPostgresDecision, "decision", store.VisibilityOrg, multiagentSessionA)

	_, err := uuid.Parse(writeA.ID)
	require.NoError(t, err, "the write must answer with a uuid, got %q", writeA.ID)
	require.False(t, writeA.Sanitized, "this content carries no injection pattern and is inside the byte cap")
	require.False(t, writeA.ConflictDetected, "edge_a's own store holds nothing this decision contradicts")
	t.Logf("STEP b: stored memory %s (sanitized=%v conflict_detected=%v)",
		writeA.ID, writeA.Sanitized, writeA.ConflictDetected)

	multiagentWaitFor(t, "the Postgres decision to reach the plane", func() bool {
		_, ok := multiagentReadMemory(t, ctx, pool, table, writeA.ID)
		return ok
	})

	postgresRow, ok := multiagentReadMemory(t, ctx, pool, table, writeA.ID)
	require.True(t, ok)
	require.Equal(t, multiagentAgentA, postgresRow.agentID, "the plane must attribute the memory to the agent that pushed it")
	require.Equal(t, store.VisibilityOrg, postgresRow.visibility)
	require.Equal(t, store.ConflictStatusNone, postgresRow.conflictStatus)
	require.Equal(t, multiagentPostgresDecision, postgresRow.content)
	require.Empty(t, postgresRow.conflictWithID)
	t.Logf("STEP b: the plane row is agent_id=%s visibility=%s conflict_status=%s",
		postgresRow.agentID, postgresRow.visibility, postgresRow.conflictStatus)

	// --- c. edge_b compiles a fresh session and sees agent_a's memory ---------
	//
	// The session is new and edge_b's own database holds none of agent_a's rows, so
	// the only way this entry can appear is through the control plane: the pull that
	// answered this compile returned another agent's memory, and the trace says so
	// with agent_id and cross_agent.
	t.Log("STEP c: edge_b compiles a fresh session and reads the Memory Trace")
	compileC := edgeB.compile(t, "sess-b-compile-1", multiagentAuthQuestion)
	compilations++

	require.NotNil(t, compileC.Trace)
	require.GreaterOrEqual(t, compileC.Trace.CandidatesRetrieved, 1,
		"the plane holds memories, so the candidate pull must have returned them")
	require.GreaterOrEqual(t, len(compileC.Trace.Memories), 1, "the trace must list the candidates it scored")

	shared, found := multiagentTraceMemory(compileC.Trace, "migrate the auth service")
	require.True(t, found, "the Memory Trace must contain agent_a's decision (trace %s)", compileC.Trace.RequestID)
	require.Equal(t, writeA.ID, shared.ID, "the surfaced memory is the one agent_a wrote")
	require.Equal(t, multiagentAgentA, shared.AgentID, "and the trace names the agent that wrote it")
	require.True(t, shared.CrossAgent, "agent_a is not the node that assembled this context")
	require.Equal(t, store.ConflictStatusNone, shared.ConflictStatus, "nothing has contradicted it yet")
	t.Logf("STEP c: trace %s entry id=%s agent_id=%s cross_agent=%v conflict_status=%s total=%.4f included=%v",
		compileC.Trace.RequestID, shared.ID, shared.AgentID, shared.CrossAgent,
		shared.ConflictStatus, shared.ScoreTotal, shared.Included)

	// --- d. edge_b contradicts it, and the plane records the disagreement -----
	//
	// What the brief asks for here is "conflict_detected in the response", and what
	// the shipped write path can answer is narrower: synapse_write_memory compares
	// the new memory against the memories *this node* can read through its own
	// store, and agent_a's memory is on the plane, not in edge_b's database. So the
	// reply says false -- verified below rather than assumed -- and the contradiction
	// is detected where a cross-agent contradiction can be: by the control plane's
	// write path, on the push that carries this memory to the tenant's schema
	// (internal/store/pgconflict.go). That verdict is asserted directly against the
	// rows, in both directions, because "a conflict was detected" is exactly the
	// claim this step is making.
	t.Log("STEP d: edge_b writes the contradicting MySQL decision")
	writeB := edgeB.writeMemory(t, multiagentMySQLDecision, "decision", store.VisibilityOrg, multiagentSessionB)

	_, err = uuid.Parse(writeB.ID)
	require.NoError(t, err, "the write must answer with a uuid, got %q", writeB.ID)
	require.False(t, writeB.Sanitized)
	require.False(t, writeB.ConflictDetected,
		"this node's own store cannot hold another agent's memory, so its own verdict is negative -- see this step's comment")
	require.Empty(t, writeB.ConflictWithID)
	t.Logf("STEP d: stored memory %s (this node's own conflict_detected=%v); the cross-agent verdict is the plane's",
		writeB.ID, writeB.ConflictDetected)

	// The wait is for both halves of the verdict, not for the first one: the plane
	// inserts the new memory and then labels the older one in a second statement, so
	// seeing the new row marked does not yet mean the pair is complete.
	multiagentWaitFor(t, "the plane to record the contradiction", func() bool {
		challenger, challengerOK := multiagentReadMemory(t, ctx, pool, table, writeB.ID)
		contradicted, contradictedOK := multiagentReadMemory(t, ctx, pool, table, writeA.ID)

		return challengerOK && contradictedOK &&
			challenger.conflictStatus == store.ConflictStatusConflict &&
			contradicted.conflictStatus == store.ConflictStatusSupersededCandidate
	})

	mysqlRow, ok := multiagentReadMemory(t, ctx, pool, table, writeB.ID)
	require.True(t, ok)
	supersededRow, ok := multiagentReadMemory(t, ctx, pool, table, writeA.ID)
	require.True(t, ok)

	require.Equal(t, store.ConflictStatusConflict, mysqlRow.conflictStatus,
		"the memory that introduced the disagreement is the one marked conflict")
	require.Equal(t, writeA.ID, mysqlRow.conflictWithID, "and it names what it disagrees with")
	require.Equal(t, store.ConflictStatusSupersededCandidate, supersededRow.conflictStatus,
		"the memory it disagrees with becomes a candidate to be superseded")
	require.Equal(t, writeB.ID, supersededRow.conflictWithID)
	t.Logf("STEP d: the plane recorded it -- mysql %s status=%s with=%s; postgres %s status=%s with=%s",
		writeB.ID, mysqlRow.conflictStatus, mysqlRow.conflictWithID,
		writeA.ID, supersededRow.conflictStatus, supersededRow.conflictWithID)

	// --- e. the next trace shows both, one of them demoted --------------------
	//
	// The brief's wording for this step has the two memories the other way round:
	// it expects the MySQL memory to be the superseded candidate and to score lower.
	// The shipped semantics are the reverse, deliberately and on the record --
	// pgconflict.go: "the older memory is marked ConflictStatusSupersededCandidate
	// ... the newer memory is marked ConflictStatusConflict" -- and the scorer
	// penalizes only the superseded candidate (scorer.go). So the assertion below is
	// the real property the brief is after: both versions survive in the trace, the
	// contradicted one carries the marker, and the marker is visible as a lower
	// score_total. Asserting the brief's direction instead would require changing
	// which row the product marks, which is a v1 internal and not this phase's to
	// change.
	t.Log("STEP e: a second fresh session through edge_b, after the conflict")
	compileE := edgeB.compile(t, "sess-b-compile-2", multiagentAuthQuestion)
	compilations++

	demoted, found := multiagentTraceMemory(compileE.Trace, "migrate the auth service")
	require.True(t, found, "both versions of the decision must stay in the pool and in the trace")
	challenger, found := multiagentTraceMemory(compileE.Trace, "keep the auth service")
	require.True(t, found, "the memory that contradicted it is in the trace too")

	require.Equal(t, store.ConflictStatusSupersededCandidate, demoted.ConflictStatus,
		"the older memory is the superseded candidate")
	require.Equal(t, writeB.ID, demoted.ConflictWithID)
	require.Equal(t, store.ConflictStatusConflict, challenger.ConflictStatus)
	require.Equal(t, writeA.ID, challenger.ConflictWithID)
	require.Less(t, demoted.ScoreTotal, challenger.ScoreTotal,
		"the superseded candidate's penalty must be visible in the trace it is explained by")

	t.Logf("STEP e: superseded candidate %s conflict=%s with=%s total=%.4f (S=%.3f R=%.3f I=%.3f T=%.3f) included=%v",
		demoted.ID, demoted.ConflictStatus, demoted.ConflictWithID, demoted.ScoreTotal,
		demoted.ScoreSemantic, demoted.ScoreRecency, demoted.ScoreImportance, demoted.ScoreTaskAlignment, demoted.Included)
	t.Logf("STEP e: contradictory memory %s conflict=%s with=%s total=%.4f (S=%.3f R=%.3f I=%.3f T=%.3f) included=%v",
		challenger.ID, challenger.ConflictStatus, challenger.ConflictWithID, challenger.ScoreTotal,
		challenger.ScoreSemantic, challenger.ScoreRecency, challenger.ScoreImportance, challenger.ScoreTaskAlignment, challenger.Included)

	t.Log("STEP f: header names and values the model upstream actually received")
	multiagentAssertNoCredentialReachedUpstream(t, upstream, tenantInfo, edgeLogs)

	t.Log("STEP g: synapse_global.ledger, counted for this tenant")
	ledgerEntries := multiagentAssertLedgerHoldsOneEntryPerCompilation(t, ctx, pool, tenantInfo, compilations)

	multiagentAssertComplianceSurfaces(t, planeURL, tenantInfo, planeLogs, compilations)

	t.Logf("PASS: two agents, one shared brain -- %s's decision reached %s's trace (cross_agent), %s's contradiction was recorded by the plane, and the enterprise tenant's chain holds %d signed entries",
		multiagentAgentA, multiagentAgentB, multiagentAgentB, ledgerEntries)
}
