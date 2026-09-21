// Package plane_test's ledger verification tests: the HTTP half of Phase 17.
//
// They run without PostgreSQL, because the handler's whole job is to choose a
// chain from a verified token, hand it to the verifier, and put the verdict on
// the wire -- the walk itself is internal/ledger's, and its tamper tests are
// integration-tagged. What is checked here is what only this layer can get
// wrong: an unverified request reaching the verifier at all, a result shape that
// does not survive JSON, and a chain chosen from anything other than the token.
package plane_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	charmlog "github.com/charmbracelet/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerVerifyPath is this endpoint's path, written out here rather than shared
// with the package under test so a route registered at the wrong path fails a
// test instead of moving with it.
const ledgerVerifyPath = "/v2/ledger/verify"

// fakeLedgerVerifier records which chain the handler asked about and answers
// with whatever the test scripted. The handler calls it from the test's own
// goroutine (httptest.ResponseRecorder spawns none), so its fields need no
// synchronisation.
type fakeLedgerVerifier struct {
	calls    int
	tenantID string
	result   plane.ChainIntegrityResult
	err      error
}

// VerifyChain implements plane.LedgerVerifier.
func (f *fakeLedgerVerifier) VerifyChain(_ context.Context, tenantID string) (plane.ChainIntegrityResult, error) {
	f.calls++
	f.tenantID = tenantID

	return f.result, f.err
}

// newLedgerRouter builds the plane's routes with the tenant token middleware and
// the given verifier installed, and returns the captured log output so a test
// can assert that no secret reached it.
func newLedgerRouter(t *testing.T, cfg *plane.PlaneConfig, verifier plane.LedgerVerifier) (http.Handler, *bytes.Buffer) {
	t.Helper()

	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	return plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, nil, verifier, tenant.JWTMiddleware(cfg), logger).Routes(), &logs
}

// getLedgerVerify sends GET /v2/ledger/verify with the given Authorization
// header value, omitted entirely when it is empty.
func getLedgerVerify(router http.Handler, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, ledgerVerifyPath, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

func TestVerifyLedgerRequiresAVerifiedTenantToken(t *testing.T) {
	cfg := newConfig(adminToken)
	verifier := &fakeLedgerVerifier{}
	router, _ := newLedgerRouter(t, cfg, verifier)
	valid := tenantToken(t, cfg)

	// A token this plane did not sign: same shape, same claims, other secret.
	otherPlane := &plane.PlaneConfig{
		ListenAddr: plane.DefaultListenAddr,
		JWTSecret:  strings.Repeat("x", 48),
		AdminToken: adminToken,
	}
	foreign, err := tenant.IssueToken(otherPlane, tenant.TokenIdentity{
		TenantID: testTenantID,
		Slug:     testTenantSlug,
		Plan:     "team",
		Tier:     "team",
	})
	require.NoError(t, err)

	headers := map[string]string{
		"no header":                           "",
		"empty bearer":                        "Bearer ",
		"token without the scheme":            valid,
		"a token signed with another secret":  "Bearer " + foreign,
		"a string that is not a token at all": "Bearer not-a-token",
	}

	for name, header := range headers {
		t.Run(name, func(t *testing.T) {
			rec := getLedgerVerify(router, header)

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
		})
	}

	assert.Zero(t, verifier.calls, "an unverified request verifies nothing")
}

// TestVerifyLedgerAnswersWithTheChainVerdict is the endpoint's contract on both
// outcomes: a valid chain, a broken one, and the fields that carry the
// difference. The tenant the verifier was asked about is asserted too -- it comes
// from the token's own claim, so a handler that read a tenant from anywhere in
// the request would fail here.
func TestVerifyLedgerAnswersWithTheChainVerdict(t *testing.T) {
	checkedAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	breakAt := time.Date(2026, 9, 21, 11, 30, 0, 0, time.UTC)
	breakID := "2b7d5c1e-4b0a-4a3b-8f5e-9d1c0a7b6e34"

	tests := []struct {
		name     string
		result   plane.ChainIntegrityResult
		expected string
	}{
		{
			name:   "the whole chain verifies",
			result: plane.ChainIntegrityResult{EntriesChecked: 10, ChainValid: true, CheckedAt: checkedAt},
			expected: `{"entries_checked":10,"chain_valid":true,` +
				`"first_break_at":"0001-01-01T00:00:00Z","checked_at":"2026-09-21T12:00:00Z"}`,
		},
		{
			name: "a rewritten entry is reported, not an error",
			result: plane.ChainIntegrityResult{
				EntriesChecked: 5, ChainValid: false, FirstBreakID: breakID,
				FirstBreakAt: breakAt, CheckedAt: checkedAt,
			},
			expected: `{"entries_checked":5,"chain_valid":false,` +
				`"first_break_id":"` + breakID + `",` +
				`"first_break_at":"2026-09-21T11:30:00Z","checked_at":"2026-09-21T12:00:00Z"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newConfig(adminToken)
			verifier := &fakeLedgerVerifier{result: tt.result}
			router, _ := newLedgerRouter(t, cfg, verifier)

			rec := getLedgerVerify(router, "Bearer "+tenantToken(t, cfg))

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.JSONEq(t, tt.expected, rec.Body.String())

			assert.Equal(t, 1, verifier.calls)
			assert.Equal(t, testTenantID, verifier.tenantID,
				"the verified token's own tenant id chooses the chain, and nothing in the request can name another")
		})
	}
}

// TestVerifyLedgerReportsAFailedCheckAsInternal: a verifier that could not run is
// a 500 with the one error body, and the underlying error is not reflected -- it
// can name a table, a host, or a connection target, all of which belong in the
// server's log rather than in a client's response.
func TestVerifyLedgerReportsAFailedCheckAsInternal(t *testing.T) {
	cfg := newConfig(adminToken)
	verifier := &fakeLedgerVerifier{err: errors.New("ledger: fetch tenant secret: connection refused to 127.0.0.1:5432")}
	router, logs := newLedgerRouter(t, cfg, verifier)

	rec := getLedgerVerify(router, "Bearer "+tenantToken(t, cfg))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
	assert.Equal(t, 1, verifier.calls)

	assert.Contains(t, logs.String(), "Ledger chain verification failed", "the failure is logged server-side")
	assert.NotContains(t, rec.Body.String(), "127.0.0.1", "the client sees the error body only")
}

// TestVerifyLedgerFailsClosedWithoutAVerifier: a plane started without the
// dependency refuses the route rather than reporting a verdict it has no way to
// reach. The same fail-closed shape the other endpoints use for a nil dependency.
func TestVerifyLedgerFailsClosedWithoutAVerifier(t *testing.T) {
	cfg := newConfig(adminToken)
	router, _ := newLedgerRouter(t, cfg, nil)

	rec := getLedgerVerify(router, "Bearer "+tenantToken(t, cfg))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `{"error":"internal"}`, rec.Body.String())
}

// TestVerifyLedgerRefusesAnUnverifiedRequest is the fail-closed case under the
// middleware rather than under the dependency: a route registered outside
// requireJWT hands the handler no verified tenant, and an empty tenant id must be
// a refusal. Answering for an empty id -- even with a verifier wired -- would be
// the one way this route could be made to check a chain nobody proved a right to.
func TestVerifyLedgerRefusesAnUnverifiedRequest(t *testing.T) {
	var logs bytes.Buffer
	logger := charmlog.NewWithOptions(&logs, charmlog.Options{Level: charmlog.DebugLevel, ReportTimestamp: false})

	cfg := newConfig(adminToken)
	verifier := &fakeLedgerVerifier{}

	router := plane.NewServer(cfg, fakeDB{}, &fakeProvisioner{}, nil, nil, verifier, nil, logger).Routes()

	rec := getLedgerVerify(router, "Bearer "+tenantToken(t, cfg))

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":"unauthorized"}`, rec.Body.String())
	assert.Zero(t, verifier.calls, "a request with no verified tenant verifies nothing")
}
