//go:build integration

// The last four steps of Phase 29's scenario, as functions: what crossed to the
// model upstream (f), what the tenant's audit chain holds (g), and what the two
// compliance surfaces answer (h and i).
//
// They are separated from multiagent_test.go rather than inlined into it for the
// 300-line ceiling this project holds every file to, and along a line that is real:
// each one reads the *consequences* of the scenario the test drives -- an upstream's
// record, the ledger, two read-only endpoints -- where the test's own steps drive
// it. TestMultiAgent still performs them in order and fails fast through them, so a
// failure still names the lettered group it belongs to.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"synapse/internal/plane"
	"synapse/internal/trace"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// multiagentAssertNoCredentialReachedUpstream is step f.
//
// The brief asks that Authorization and x-api-key be absent from everything the
// mock upstream logged, and that is asserted literally: this scenario's client
// sends neither header, so anything that arrived with one was invented by Synapse.
// The rule it enforces is the .clinerules one -- an edge node holds a
// control-plane credential, and spending it on the model API would hand the
// tenant's token to a third party. (The proxy does forward a client's *own*
// Authorization header, which the v1 regression suite asserts as pass-through
// behavior, which is why this scenario simply never sends one.)
//
// The second half is the stronger claim, and the one a name-based check cannot
// make: the tenant's JWT, its API key, and the envelope master key appear nowhere
// in what the upstream received -- headers or bodies -- and nowhere in the two
// nodes' own log output.
func multiagentAssertNoCredentialReachedUpstream(t *testing.T, upstream *multiagentUpstream, tenantInfo multiagentTenantInfo, edgeLogs *bytes.Buffer) {
	t.Helper()

	requests, upstreamLogs := upstream.snapshot()
	require.NotEmpty(t, requests, "the proxied turns must have reached the model upstream")

	for i, request := range requests {
		require.Empty(t, request.headers.Get("Authorization"),
			"request %d (%s) reached the model API with an Authorization header", i, request.path)
		require.Empty(t, request.headers.Get("X-Api-Key"),
			"request %d (%s) reached the model API with an x-api-key header", i, request.path)
	}

	for i, line := range upstreamLogs {
		lower := strings.ToLower(line)
		require.NotContains(t, lower, "authorization", "upstream log %d mentions an Authorization header: %s", i, line)
		require.NotContains(t, lower, "x-api-key", "upstream log %d mentions an x-api-key header: %s", i, line)
	}

	leak := multiagentUpstreamCredentialLeak(t, requests, map[string]string{
		"the tenant's control-plane token": tenantInfo.jwt,
		"the tenant's API key":             tenantInfo.apiKey,
	})
	require.Empty(t, leak, "a Synapse credential reached the model upstream: %s", leak)

	capturedLogs := edgeLogs.String()
	require.NotContains(t, capturedLogs, tenantInfo.jwt, "the control-plane token must never reach a log line")
	require.NotContains(t, capturedLogs, tenantInfo.apiKey, "nor the tenant's API key")
	require.NotContains(t, capturedLogs, multiagentMasterKeyHex, "nor the key that wraps the tenants' secrets")

	t.Logf("STEP f: %d upstream requests, header names on the first: %v -- no Authorization, no x-api-key, no credential",
		len(requests), multiagentHeaderNames(requests[0].headers))
}

// multiagentAssertLedgerHoldsOneEntryPerCompilation is step g, and returns the
// count it observed for the two steps after it.
//
// The appends happen in goroutines the compile path never waits on, so this polls
// rather than reading once: an audit chain that is only written when a request is
// slow enough would be an audit chain nobody can rely on.
func multiagentAssertLedgerHoldsOneEntryPerCompilation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantInfo multiagentTenantInfo, compilations int) int {
	t.Helper()

	multiagentWaitFor(t, "the ledger to hold one entry per compilation", func() bool {
		return multiagentLedgerCount(t, ctx, pool, tenantInfo.tenantID) >= compilations
	})

	entries := multiagentLedgerCount(t, ctx, pool, tenantInfo.tenantID)
	t.Logf("STEP g: %d compilations performed, %d ledger entries", compilations, entries)
	require.GreaterOrEqual(t, entries, compilations,
		"an enterprise tenant's every compilation is appended to its chain")

	return entries
}

// multiagentAssertComplianceSurfaces is steps h and i: the chain verifies, and the
// audit page returns the entries it verified.
//
// Both are reached with the tenant's own token, through the enterprise compliance
// tier it was provisioned with, so this is also the assertion that the tier gate
// admits the tenant it was minted for. The walk re-derives every HMAC from the
// tenant's stored secret, which is what makes it evidence that the appends above
// were signed and chained rather than merely inserted.
func multiagentAssertComplianceSurfaces(t *testing.T, planeURL string, tenantInfo multiagentTenantInfo, planeLogs *bytes.Buffer, compilations int) {
	t.Helper()

	var integrity plane.ChainIntegrityResult
	multiagentPlaneGet(t, planeURL+"/v2/compliance/chain-integrity", tenantInfo.jwt, &integrity)

	require.True(t, integrity.ChainValid, "the tenant's chain must verify: first break %s", integrity.FirstBreakID)
	require.GreaterOrEqual(t, integrity.EntriesChecked, compilations)
	t.Logf("STEP h: chain_valid=%v entries_checked=%d first_break_id=%q",
		integrity.ChainValid, integrity.EntriesChecked, integrity.FirstBreakID)

	// No query is sent, so the page is the default window -- which is why the count
	// is asserted on Total (how many entries the window holds) and on the page it
	// returned together.
	var audit struct {
		Data   []json.RawMessage `json:"data"`
		Total  int               `json:"total"`
		Limit  int               `json:"limit"`
		Offset int               `json:"offset"`
	}
	multiagentPlaneGet(t, planeURL+"/v2/compliance/audit", tenantInfo.jwt, &audit)

	require.GreaterOrEqual(t, audit.Total, compilations,
		"every compilation this scenario performed is in the tenant's audit history")
	require.Len(t, audit.Data, audit.Total, "the window fits in one default page")

	var newest struct {
		TenantID string              `json:"tenant_id"`
		Trace    trace.TraceManifest `json:"trace"`
	}
	require.NoError(t, json.Unmarshal(audit.Data[0], &newest))
	require.Equal(t, tenantInfo.tenantID, newest.TenantID)
	require.NotEmpty(t, newest.Trace.RequestID, "each audit entry carries the trace it signed")
	t.Logf("STEP i: audit returned %d entries (total=%d limit=%d offset=%d); newest trace=%s memories=%d",
		len(audit.Data), audit.Total, audit.Limit, audit.Offset, newest.Trace.RequestID, len(newest.Trace.Memories))

	// The plane's own log output is the last place a presented token could be
	// recorded, and it is the one the two compliance endpoints write through.
	require.NotContains(t, planeLogs.String(), tenantInfo.jwt, "the plane must never log the token it was presented")
	require.NotContains(t, planeLogs.String(), tenantInfo.apiKey)
}
