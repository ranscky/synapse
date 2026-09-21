//go:build integration

// Package plane_test's compliance audit integration test: the whole endpoint over
// a real PostgreSQL -- two tenants provisioned through the plane's own
// provisioner, five HMAC-signed entries appended to one of their chains, and
// GET /v2/compliance/audit read back through the real HTTP surface.
//
// What this is evidence for, and why a test double will not do it: the SQL, the
// tenant scoping inside it, the since boundary's microsecond fidelity, the order
// the index returns rows in, and the access-log rows the endpoint writes. Every
// one of those lives in internal/ledger's implementation or in the table's own
// DDL, so this test wires the real ledger.Auditor rather than a fake -- the
// untagged tests next to it cover what a fake can, which is this endpoint's own
// logic. The setup it shares (the pool, the tenants, the entries, the router) is
// in compliance_setup_test.go.
//
// Build-tagged integration because a real Postgres is required and
// testcontainers-go is not a dependency of this module. The database is a
// precondition, not an option: an unreachable one fails this test instead of
// skipping it, because a skipped audit test reports success without having read a
// single row. SYNAPSE_MASTER_KEY is set by the test itself, so the documented
// command exports nothing else.
package plane_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComplianceAudit is Phase 19's definition of done against a real PostgreSQL:
// an enterprise tenant reads its own five signed entries with their traces parsed
// as objects, a team tenant is refused with the upsell body, a since boundary names
// one entry exactly and returns the two from there on, paging walks the same
// window, and every one of those calls left an access-log row behind.
//
// The tokens are minted by tenant.IssueToken through the provisioner -- the same
// issuer production runs -- so the compliance_tier claim this endpoint gates on
// travelled the way it travels in a deployment: signed by the plane, verified by
// the middleware, and read from the request context by the handler.
func TestComplianceAudit(t *testing.T) {
	pool := compliancePool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))

	// The signing secret is wrapped under this master key, so the ledger's write
	// path can fetch and unwrap it. t.Setenv restores whatever was exported, so
	// this test does not have to be run with anything in its environment.
	t.Setenv(plane.EnvMasterKey, complianceMasterKeyHex)

	cfg := newConfig(adminToken)
	provisioner := tenant.NewProvisioner(cfg, tenant.NewStore(pool), pool)

	enterprise := provisionComplianceTenant(t, ctx, provisioner, "enterprise", "enterprise")
	team := provisionComplianceTenant(t, ctx, provisioner, "team", "team")

	entries := appendComplianceEntries(t, ctx, pool, enterprise.tenantID, complianceAppendCount)
	require.Len(t, entries, complianceAppendCount)

	router, logs := complianceRouter(t, cfg, pool, provisioner)

	// --- the enterprise tenant reads its own chain -----------------------------
	// The unknown parameter is deliberate: it is the smuggling attempt the
	// redaction rule exists to refuse, and the access-log assertions at the end
	// check that it went nowhere.
	rec := complianceRequest(router, enterprise.jwt, "content=must-not-be-recorded")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	body := decodeComplianceBody(t, rec)
	require.Len(t, body.Data, complianceAppendCount, "one row per appended entry")
	assert.Equal(t, complianceAppendCount, body.Total)
	assert.Equal(t, 50, body.Limit, "an unqualified request gets the default page size")
	assert.Equal(t, 0, body.Offset)

	// Newest first, and every row is this tenant's own: the ids, request ids, and
	// hashes are the ones this test appended, read back in reverse.
	var previous time.Time

	for i, entry := range body.Data {
		appended := entries[complianceAppendCount-1-i]
		assert.Equal(t, appended.ID, entry["id"], "entry %d", i)
		assert.Equal(t, enterprise.tenantID, entry["tenant_id"], "entry %d: the chain read is the token's own", i)
		assert.Equal(t, appended.RequestID, entry["request_id"], "entry %d", i)
		assert.Equal(t, appended.HashValue, entry["hash_value"], "entry %d", i)
		assert.Equal(t, appended.PrevHash, entry["prev_hash"], "entry %d", i)

		// The assertion this phase's brief asks for by name: trace_json comes
		// back as a JSON object rather than as the string the column stores.
		stored, ok := entry["trace"].(map[string]any)
		require.True(t, ok, "entry %d: trace must be a JSON object, got %T", i, entry["trace"])
		assert.NotEmpty(t, stored["request_id"], "entry %d: the parsed trace is the row's own", i)
		assert.Equal(t, "debugging", stored["detected_intent"], "entry %d", i)
		assert.Len(t, stored["memories"], 1, "entry %d", i)

		// created_at descends, which is the endpoint's own ORDER BY and the order
		// the chain was written in.
		raw, ok := entry["created_at"].(string)
		require.True(t, ok, "entry %d: created_at must be a string", i)
		stamped, err := time.Parse(time.RFC3339Nano, raw)
		require.NoError(t, err, "entry %d", i)

		if i > 0 {
			assert.True(t, stamped.Before(previous), "entry %d must be older than entry %d", i, i-1)
		}

		previous = stamped
	}

	// --- the team tenant is refused -------------------------------------------
	refused := complianceRequest(router, team.jwt, "")
	require.Equal(t, http.StatusForbidden, refused.Code)
	assert.JSONEq(t, `{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}`,
		refused.Body.String())
	assert.NotContains(t, refused.Body.String(), enterprise.tenantID,
		"a refused caller is told nothing about another tenant")
	assert.NotContains(t, refused.Body.String(), entries[0].ID)

	// --- a since boundary names one entry exactly ------------------------------
	// The entry's own created_at, sent to the microsecond, is an inclusive lower
	// bound that lands on that entry rather than near it: the two newest entries
	// are at or after it and the three older ones are not. A boundary formatted
	// to whole seconds would floor into the previous entry, which is why the
	// endpoint parses a fractional second and this test sends one.
	boundary := entries[complianceAppendCount-2].CreatedAt.UTC().Format(time.RFC3339Nano)
	filtered := complianceRequest(router, enterprise.jwt, "since="+url.QueryEscape(boundary))
	require.Equal(t, http.StatusOK, filtered.Code, filtered.Body.String())

	filteredBody := decodeComplianceBody(t, filtered)
	require.Len(t, filteredBody.Data, 2, "since the second-newest entry's own timestamp, exactly two entries are at or after it")
	assert.Equal(t, 2, filteredBody.Total, "total counts the window, not the page")
	assert.Equal(t, entries[complianceAppendCount-1].ID, filteredBody.Data[0]["id"])
	assert.Equal(t, entries[complianceAppendCount-2].ID, filteredBody.Data[1]["id"])

	// --- paging walks the same window -----------------------------------------
	paged := complianceRequest(router, enterprise.jwt, "limit=2&offset=1")
	require.Equal(t, http.StatusOK, paged.Code, paged.Body.String())

	pagedBody := decodeComplianceBody(t, paged)
	require.Len(t, pagedBody.Data, 2)
	assert.Equal(t, complianceAppendCount, pagedBody.Total, "one entry is skipped, not removed from the window")
	assert.Equal(t, 2, pagedBody.Limit)
	assert.Equal(t, 1, pagedBody.Offset)
	assert.Equal(t, entries[3].ID, pagedBody.Data[0]["id"], "the page starts after the newest entry")
	assert.Equal(t, entries[2].ID, pagedBody.Data[1]["id"])

	// --- every call above is in the access log, and only in hashed form --------
	enterpriseLog := readAccessLog(t, ctx, pool, enterprise.tenantID)
	require.Len(t, enterpriseLog, 3, "three enterprise reads: the chain, the windowed page, and the paged one")

	for i, row := range enterpriseLog {
		assert.Equal(t, complianceAuditPath, row.Endpoint, "row %d", i)
		assert.Equal(t, http.StatusOK, row.Code, "row %d", i)
		assert.Len(t, row.IPHash, 64, "row %d: the address is recorded as a sha256 digest", i)
		assert.NotEqual(t, complianceRemoteAddr, row.IPHash, "row %d: never as the raw address", i)
	}

	assert.Equal(t, enterpriseLog[0].IPHash, enterpriseLog[1].IPHash, "one caller hashes to one value")
	assert.NotContains(t, enterpriseLog[0].Params, "must-not-be-recorded",
		"an unknown parameter is dropped rather than copied into the audit table")
	assert.Contains(t, enterpriseLog[1].Params, "since=", "the window the caller asked for is recorded")
	assert.Contains(t, enterpriseLog[2].Params, "offset=1", "and so is the paging")

	teamLog := readAccessLog(t, ctx, pool, team.tenantID)
	require.Len(t, teamLog, 1, "a refused call is recorded too: the attempt is the audit fact")
	assert.Equal(t, http.StatusForbidden, teamLog[0].Code)
	assert.Equal(t, complianceAuditPath, teamLog[0].Endpoint)

	// --- and nothing secret or content-shaped reached the process log ----------
	assert.NotContains(t, logs.String(), enterprise.jwt, "the presented token is never logged")
	assert.NotContains(t, logs.String(), team.jwt)
	assert.NotContains(t, logs.String(), jwtSecret, "nor the key that signs them")
	assert.NotContains(t, logs.String(), "entry 0", "nor trace content")
}
