//go:build integration

// The probes Phase 29's assertions observe the world with: the bounded wait for a
// background effect, the direct reads of the tenant's own table, and the two
// authenticated GETs the compliance surfaces answer.
//
// Everything here reads state rather than driving it, which is why it is not part
// of the setup or the edge file: an assertion that used a product API to ask
// whether the product worked would be asking the thing under test to be its own
// witness. Rows are read out of PostgreSQL, headers out of the upstream's own
// record, and only the two compliance endpoints -- whose HTTP contract *is* what
// this phase asserts -- are called for their answer.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// multiagentWaitFor polls until cond reports true, and fails the test naming what
// it was waiting for when the deadline passes. A bounded wait is the only honest
// way to observe a background flusher and an asynchronous audit append, and a
// deadline that expires is a failure rather than a skip: the phase's claim is that
// these effects happen.
func multiagentWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(multiagentWaitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(multiagentPollInterval)
	}

	t.Fatalf("timed out after %s waiting for %s", multiagentWaitTimeout, what)
}

// multiagentCount runs a single-row count and returns it, failing on any error:
// these numbers are the assertions' own evidence, so a query that cannot run must
// not be read as zero.
func multiagentCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()

	var count int
	require.NoError(t, pool.QueryRow(ctx, query, args...).Scan(&count))

	return count
}

// multiagentMemoryRow is one tenant memory read straight out of its table: the
// five columns this phase's assertions are about.
type multiagentMemoryRow struct {
	agentID        string
	visibility     string
	conflictStatus string
	conflictWithID string
	content        string
}

// multiagentReadMemory reads one memory by id, and reports whether it is there
// yet: the row arrives when the flusher's push is accepted, so "not yet" is a
// state this test has to be able to observe rather than an error.
func multiagentReadMemory(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, id string) (multiagentMemoryRow, bool) {
	t.Helper()

	row := multiagentMemoryRow{}
	err := pool.QueryRow(ctx,
		`SELECT agent_id, visibility, conflict_status, coalesce(conflict_with_id::text, ''), content
		 FROM `+table+` WHERE id = $1::uuid`, id,
	).Scan(&row.agentID, &row.visibility, &row.conflictStatus, &row.conflictWithID, &row.content)

	if err != nil {
		// pgx reports a missing row as pgx.ErrNoRows, whose text is "no rows in
		// result set"; matching on it keeps this helper free of an import of the
		// pgx error variable for one call site.
		if strings.Contains(err.Error(), "no rows") {
			return multiagentMemoryRow{}, false
		}
		require.NoError(t, err, "reading memory %s", id)
	}

	return row, true
}

// multiagentPlaneGet performs GET with the tenant's own token and decodes the
// body into dst. A non-200 fails the test with the body's first bytes, which is
// the difference between "the gate refused" and "the endpoint broke".
func multiagentPlaneGet(t *testing.T, url, token string, dst any) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s answered %d: %s", url, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, dst), "GET %s body: %s", url, string(body))
}

// multiagentLedgerCount reports how many entries the tenant's audit chain holds.
func multiagentLedgerCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()

	return multiagentCount(t, ctx, pool,
		`SELECT count(*) FROM synapse_global.ledger WHERE tenant_id = $1::uuid`, tenantID)
}

// multiagentUpstreamCredentialLeak reports the first place a Synapse credential
// appears in what the upstream received -- a header value, or the request body --
// or "" when none does.
//
// Both the tenant's control-plane token and its API key are searched for, and the
// search covers every header, not just the two the brief names: an edge that spent
// its plane credential on the model API would be leaking it under whatever name it
// chose, and naming the culprit is what makes the failure readable.
func multiagentUpstreamCredentialLeak(t *testing.T, requests []multiagentUpstreamRequest, secrets map[string]string) string {
	t.Helper()

	for i, request := range requests {
		for name, values := range request.headers {
			for _, value := range values {
				for label, secret := range secrets {
					if secret != "" && strings.Contains(value, secret) {
						return fmt.Sprintf("request %d (%s) sent %s in header %q", i, request.path, label, name)
					}
				}
			}
		}
		for label, secret := range secrets {
			if secret != "" && strings.Contains(request.body, secret) {
				return fmt.Sprintf("request %d (%s) carried %s in its body", i, request.path, label)
			}
		}
	}

	return ""
}
