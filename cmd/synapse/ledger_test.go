package main

import (
	"context"
	"errors"
	"testing"

	"synapse/internal/config"
	"synapse/internal/ledger"
	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerTestTenantID is the tenant these tests pretend the node speaks for: a
// real uuid, because that is the only shape ledger.Append accepts.
const ledgerTestTenantID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

// ledgerTestCredentialSecret signs the credentials these tests mint. Nothing in
// this file verifies it: the point of ParseTokenClaims is that the edge cannot.
const ledgerTestCredentialSecret = "edge-ledger-test-secret-0123456789abcdef"

// fakeSecretStore stands in for the tenant secret store.
type fakeSecretStore struct {
	secret    []byte
	err       error
	tenantIDs []string
}

func (f *fakeSecretStore) GetSecret(_ context.Context, tenantID string) ([]byte, error) {
	f.tenantIDs = append(f.tenantIDs, tenantID)
	return f.secret, f.err
}

// appendArgs is one recorded Append call.
type appendArgs struct {
	tenantID  string
	requestID string
	traceJSON string
	secret    []byte
}

// fakeAppender stands in for the ledger's write path.
type fakeAppender struct {
	args *appendArgs
	err  error
}

func (f *fakeAppender) Append(_ context.Context, tenantID, requestID, traceJSON string, secret []byte) (ledger.LedgerEntry, error) {
	f.args = &appendArgs{tenantID: tenantID, requestID: requestID, traceJSON: traceJSON, secret: secret}
	if f.err != nil {
		return ledger.LedgerEntry{}, f.err
	}

	return ledger.LedgerEntry{ID: "row-1", TenantID: tenantID, RequestID: requestID, TraceJSON: traceJSON}, nil
}

// TestAppendTraceSignsForThisNodesTenant is the sink's core contract: the
// tenant comes from the node's own wiring, the request id is mapped to the uuid
// shape the ledger requires, the payload is the trace untouched, and the secret
// it signs with is the tenant's.
func TestAppendTraceSignsForThisNodesTenant(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	reader := &fakeSecretStore{secret: secret}
	appender := &fakeAppender{}

	sink := enterpriseLedger{reader: reader, appender: appender, tenantID: ledgerTestTenantID}

	const (
		requestID = "req-1758301000123456789-3"
		traceJSON = `{"request_id":"req-1758301000123456789-3","memories_compiled":2}`
	)

	require.NoError(t, sink.AppendTrace(context.Background(), requestID, traceJSON))

	require.NotNil(t, appender.args, "the ledger must have been asked to append")
	assert.Equal(t, ledgerTestTenantID, appender.args.tenantID)
	assert.Equal(t, traceJSON, appender.args.traceJSON, "the payload must reach the ledger byte for byte")
	assert.Equal(t, secret, appender.args.secret)

	// The id the ledger signs is a uuid derived from the edge's own id, not the
	// edge id itself -- ledger.Append rejects anything else, and Phase 18's
	// verification would silently find an empty table if this were passed
	// through unchanged.
	assert.NotEqual(t, requestID, appender.args.requestID)
	assert.Equal(t, ledgerRequestID(requestID), appender.args.requestID)

	parsed, err := uuid.Parse(appender.args.requestID)
	require.NoError(t, err, "the ledger requires a uuid request id")
	// A uuid v5 is deterministic, which is what keeps the row traceable back to
	// the request id the trace JSON still carries.
	assert.Equal(t, uuid.Version(5), parsed.Version())

	assert.Equal(t, []string{ledgerTestTenantID}, reader.tenantIDs, "the secret must be fetched for this node's tenant")
}

// TestAppendTraceFailsClosed covers the two ways a sink call can go wrong. Each
// one must be an error naming its stage, with no row written.
func TestAppendTraceFailsClosed(t *testing.T) {
	traceJSON := `{"request_id":"req-1"}`

	t.Run("no signing secret", func(t *testing.T) {
		reader := &fakeSecretStore{err: tenant.ErrSecretNotFound}
		appender := &fakeAppender{}
		sink := enterpriseLedger{reader: reader, appender: appender, tenantID: ledgerTestTenantID}

		err := sink.AppendTrace(context.Background(), "req-1", traceJSON)
		require.Error(t, err)
		assert.ErrorIs(t, err, tenant.ErrSecretNotFound)
		assert.Contains(t, err.Error(), "fetch tenant secret")
		assert.Nil(t, appender.args, "an unwritable secret must not reach the append")
	})

	t.Run("append fails", func(t *testing.T) {
		appendErr := errors.New("ledger: append needs a signing secret")
		sink := enterpriseLedger{
			reader:   &fakeSecretStore{secret: []byte("k")},
			appender: &fakeAppender{err: appendErr},
			tenantID: ledgerTestTenantID,
		}

		err := sink.AppendTrace(context.Background(), "req-1", traceJSON)
		require.Error(t, err)
		assert.ErrorIs(t, err, appendErr)
		assert.Contains(t, err.Error(), "append compiled trace")
	})

	t.Run("no pool", func(t *testing.T) {
		sink := enterpriseLedger{tenantID: ledgerTestTenantID}

		err := sink.AppendTrace(context.Background(), "req-1", traceJSON)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no database pool")
	})
}

// TestLedgerRequestIDIsDeterministicAndUuidShaped pins the mapping that makes the
// ledger writable at all. Determinism is the load-bearing part: it is what lets a
// verifier recompute a row's request_id from the request id inside the signed
// trace.
func TestLedgerRequestIDIsDeterministicAndUuidShaped(t *testing.T) {
	ids := []string{
		"req-1758301000123456789",   // internal/api's shape
		"req-1758301000123456789-3", // internal/proxy's shape
		ledgerTestTenantID,          // already a uuid: canonicalized, not hashed
		"REQ-1758301000123456789-3", // case must not collapse two ids into one
		"",                          // degenerate input must still produce a uuid
	}

	seen := map[string]string{}
	for _, id := range ids {
		got := ledgerRequestID(id)

		parsed, err := uuid.Parse(got)
		require.NoError(t, err, "ledgerRequestID(%q) is not a uuid", id)
		assert.Equal(t, got, ledgerRequestID(id), "the mapping must be stable for %q", id)
		assert.Equal(t, parsed.String(), got, "the value must be the canonical form Postgres stores")

		if previous, ok := seen[got]; ok {
			t.Errorf("request ids %q and %q mapped to one uuid %q", previous, id, got)
		}
		seen[got] = id
	}

	assert.Equal(t, ledgerTestTenantID, ledgerRequestID(ledgerTestTenantID), "a uuid must be passed through, not hashed into another one")
}

// TestResolveLedgerPlanReadsThisNodesOwnCredential covers the one place a tenant
// is named on the edge side, and its fail-closed answers.
func TestResolveLedgerPlanReadsThisNodesOwnCredential(t *testing.T) {
	t.Run("enterprise credential", func(t *testing.T) {
		token, err := tenant.IssueToken(&plane.PlaneConfig{JWTSecret: ledgerTestCredentialSecret}, tenant.TokenIdentity{
			TenantID: ledgerTestTenantID,
			Slug:     "enterprise-demo",
			Plan:     "enterprise",
			Tier:     "soc2",
		})
		require.NoError(t, err)

		plan, err := resolveLedgerPlan(token)
		require.NoError(t, err)
		assert.Equal(t, ledgerTestTenantID, plan.tenantID)
		assert.Equal(t, "enterprise", plan.plan)
	})

	t.Run("nothing to read", func(t *testing.T) {
		_, err := resolveLedgerPlan("")
		require.Error(t, err)
	})

	t.Run("no tenant claim", func(t *testing.T) {
		// A well-formed token that names no tenant parses fine -- ParseTokenClaims
		// is a reader, not an authenticator -- and must be refused here rather
		// than becoming an append to an empty tenant.
		bare, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"plan": "enterprise"}).
			SignedString([]byte(ledgerTestCredentialSecret))
		require.NoError(t, err)

		plan, err := resolveLedgerPlan(bare)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "names no tenant")
		assert.Empty(t, plan)
	})
}

// TestEnableEnterpriseLedgerRefusesIncompleteConfiguration covers the boot-time
// decision table without a database: every case here must either install nothing
// or return an error, and none of them may panic or reach for a pool it cannot
// open.
func TestEnableEnterpriseLedgerRefusesIncompleteConfiguration(t *testing.T) {
	enterpriseToken := func(t *testing.T) string {
		t.Helper()

		token, err := tenant.IssueToken(&plane.PlaneConfig{JWTSecret: ledgerTestCredentialSecret}, tenant.TokenIdentity{
			TenantID: ledgerTestTenantID,
			Slug:     "enterprise-demo",
			Plan:     "enterprise",
		})
		require.NoError(t, err)

		return token
	}

	t.Run("no credential", func(t *testing.T) {
		cleanup, enabled, err := enableEnterpriseLedger(config.Config{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "control-plane-api-key is required")
		assert.False(t, enabled)
		assert.Nil(t, cleanup)
	})

	t.Run("credential is not a token", func(t *testing.T) {
		cleanup, enabled, err := enableEnterpriseLedger(config.Config{ControlPlaneAPIKey: "not-a-jwt"})
		require.Error(t, err)
		assert.False(t, enabled)
		assert.Nil(t, cleanup)
	})

	t.Run("non-enterprise plan is not an error", func(t *testing.T) {
		token, err := tenant.IssueToken(&plane.PlaneConfig{JWTSecret: ledgerTestCredentialSecret}, tenant.TokenIdentity{
			TenantID: ledgerTestTenantID,
			Slug:     "small-team",
			Plan:     "team",
		})
		require.NoError(t, err)

		// No database-dsn either: a tenant that does not get a ledger must not
		// be refused one, and must not need one configured.
		cleanup, enabled, err := enableEnterpriseLedger(config.Config{ControlPlaneAPIKey: token})
		require.NoError(t, err)
		assert.False(t, enabled)
		assert.Nil(t, cleanup)
	})

	t.Run("enterprise without a dsn", func(t *testing.T) {
		cleanup, enabled, err := enableEnterpriseLedger(config.Config{ControlPlaneAPIKey: enterpriseToken(t)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "database-dsn")
		assert.False(t, enabled)
		assert.Nil(t, cleanup)
	})
}
