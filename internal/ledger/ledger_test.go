//go:build integration

// Signing and chaining tests for the ledger's write path.
//
// The claim under test is this phase's definition of done: the trace every
// Append writes is HMAC-SHA256 signed under the tenant's own secret, and every
// entry is linked to its predecessor, so the signatures can be checked
// independently by anyone who holds that secret and the rows.
//
// What makes these tests evidence rather than a mirror of the implementation is
// that the signature is recomputed here, with crypto/hmac directly, from the raw
// secret and the entry's own fields -- the package's own helper is deliberately
// never called in the assertions. A bug in that helper would have to be
// reproduced here, independently, to pass.
//
// Build-tagged integration because a real Postgres is required and
// testcontainers-go is not a dependency of this module, so the tests use the
// database the Phase 4 compose stack publishes (deploy/docker-compose.yml). The
// database is a precondition, not an option, for the same reason
// ledger_table_test.go's is: a skipped signing test reports success without
// having checked anything. SYNAPSE_MASTER_KEY is set by the tests themselves, so
// the documented command exports nothing:
//
//	go test ./internal/ledger/... -run TestAppend -v -tags integration
package ledger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"synapse/internal/plane"
	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// appendCount is how many entries each test writes and then verifies one by
	// one: enough for a first link, a middle, and a last, and few enough that a
	// failure names the entry it came from.
	appendCount = 10

	// secretBytes is the size of the signing secret the tenant store mints, and
	// the size of the key the negative control below uses.
	secretBytes = 32

	// testMasterKeyHex is 64 hex characters -- 32 bytes, the only shape
	// GenerateAndStoreSecret accepts. Test material: no deployment uses it, and
	// nothing prints it.
	testMasterKeyHex = "1f2e3d4c5b6a79887766554433221100ffeeddccbbaa99887766554433221100"
)

// setMasterKey installs the master key that tenant secrets are wrapped under, for
// the duration of one test. t.Setenv restores whatever was exported afterwards.
func setMasterKey(t *testing.T) {
	t.Helper()

	t.Setenv(plane.EnvMasterKey, testMasterKeyHex)
}

// testChainTenant provisions a tenant nothing else has written to: a registry row
// (tenant_secrets.tenant_id references tenants(id), so the foreign key needs one),
// a wrapped signing secret, and the raw secret back. Every chain a test builds
// therefore starts at genesis.
func testChainTenant(t *testing.T, pool *pgxpool.Pool) (string, []byte) {
	t.Helper()

	ctx := context.Background()

	entropy, err := tenant.GenerateAPIKey()
	require.NoError(t, err)

	tenantID, err := tenant.NewStore(pool).CreateTenant(ctx, tenant.CreateTenantParams{
		Slug:           "ledger-" + entropy[:12],
		Plan:           "oss",
		ComplianceTier: "team",
		KeyHash:        "unused-by-this-test: nothing ever authenticates as this tenant",
	})
	require.NoError(t, err)

	secret, err := tenant.GenerateAndStoreSecret(ctx, pool, tenantID)
	require.NoError(t, err)
	require.Len(t, secret, secretBytes, "the chain key is 32 bytes")

	return tenantID, secret
}

// appendTrace is the trace payload for the nth append: distinct every time, so a
// test that verified the same entry twice by accident would notice.
func appendTrace(n int) string {
	return fmt.Sprintf(`{"seq":%d,"phase":16,"note":"append %d"}`, n, n)
}

// independentSignature recomputes an entry's signature the way the phase brief
// specifies: hex(HMAC-SHA256(secret, id+tenantID+requestID+prevHash+traceJSON)).
// It is written out here rather than calling the package's helper, so the
// assertions are evidence about the implementation instead of a restatement of it.
func independentSignature(secret []byte, entry LedgerEntry) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(entry.ID + entry.TenantID + entry.RequestID + entry.PrevHash + entry.TraceJSON))

	return hex.EncodeToString(mac.Sum(nil))
}

// briefGenesisHash is hex(sha256("genesis")), spelled out here -- the "genesis"
// label included -- so a wrong label inside the package shows up as a failing
// first link instead of being hidden by a shared constant.
func briefGenesisHash() string {
	sum := sha256.Sum256([]byte("genesis"))

	return hex.EncodeToString(sum[:])
}

// TestAppend is the phase's definition of done against a real PostgreSQL: ten
// appends for one tenant, every signature recomputed independently and compared
// with both the returned entry and the stored row, every link checked against its
// predecessor, and the first link checked against genesis.
func TestAppend(t *testing.T) {
	pool := ledgerPool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))
	setMasterKey(t)

	tenantID, secret := testChainTenant(t, pool)

	// The wrap round-trips -- what the store handed out is what it stored -- and
	// the database holds ciphertext rather than the key itself.
	fetched, err := tenant.GetSecret(ctx, pool, tenantID)
	require.NoError(t, err)
	assert.Equal(t, secret, fetched, "GetSecret must return the secret GenerateAndStoreSecret minted")

	var storedCiphertext string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT secret_encrypted FROM `+tenant.SchemaName+`.tenant_secrets WHERE tenant_id = $1`,
		tenantID,
	).Scan(&storedCiphertext))
	assert.NotEmpty(t, storedCiphertext)
	assert.NotContains(t, storedCiphertext, hex.EncodeToString(secret),
		"the signing key must be wrapped in the database, never stored in the clear")

	ledger := NewLedger(pool)

	entries := make([]LedgerEntry, 0, appendCount)
	for i := 0; i < appendCount; i++ {
		entry, err := ledger.Append(ctx, tenantID, uuid.NewString(), appendTrace(i), secret)
		require.NoError(t, err, "append %d", i)

		entries = append(entries, entry)
	}
	require.Len(t, entries, appendCount)

	ids := make(map[string]bool, appendCount)
	var lastCreatedAt time.Time

	for i, entry := range entries {
		// --- the signature verifies independently ----------------------------
		computed := independentSignature(secret, entry)
		assert.Equal(t, computed, entry.HashValue,
			"entry %d: Append must return the HMAC of the entry under the tenant's secret", i)
		assert.Len(t, entry.HashValue, 64, "a hex SHA-256 digest is 64 characters")

		// --- and the stored row is the entry ---------------------------------
		var (
			storedHash  string
			storedPrev  string
			storedTrace string
			storedAt    time.Time
		)
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT hash_value, prev_hash, trace_json, created_at FROM `+ledgerTable+` WHERE id = $1`,
			entry.ID,
		).Scan(&storedHash, &storedPrev, &storedTrace, &storedAt))

		assert.Equal(t, computed, storedHash, "entry %d: the row's hash_value must be its signature", i)
		assert.Equal(t, entry.PrevHash, storedPrev, "entry %d: the row must chain where Append said it did", i)
		assert.Equal(t, entry.TraceJSON, storedTrace, "entry %d: the trace must be stored verbatim", i)
		assert.True(t, entry.CreatedAt.Equal(storedAt), "entry %d: created_at must be the row's own", i)

		// --- the chain -------------------------------------------------------
		if i == 0 {
			assert.Equal(t, briefGenesisHash(), entry.PrevHash,
				"the tenant's first entry must chain from hex(sha256(\"genesis\"))")
		} else {
			assert.Equal(t, entries[i-1].HashValue, entry.PrevHash,
				"entry %d must chain from entry %d", i, i-1)
		}

		assert.Equal(t, tenantID, entry.TenantID)
		assert.False(t, ids[entry.ID], "every entry must have its own id")
		assert.True(t, entry.CreatedAt.After(lastCreatedAt),
			"entry %d: created_at must advance, or the next head read could pick the wrong row", i)

		ids[entry.ID] = true
		lastCreatedAt = entry.CreatedAt
	}

	assert.Len(t, ids, appendCount)

	// Negative control: the same recomputation under a different key must not
	// reproduce a signature. Without this, an implementation that signed with
	// something other than the tenant's secret could still look verified.
	wrongSecret := make([]byte, secretBytes)
	assert.NotEqual(t, independentSignature(wrongSecret, entries[0]), entries[0].HashValue,
		"the signature must depend on the tenant's secret")
}

// TestConcurrentAppendStaysLinear is why Append takes a per-tenant advisory lock
// and stamps created_at inside it.
//
// Ten appends for one tenant, started together, must still produce exactly one
// chain: the rows read back in created_at order must each chain from the row
// before them, and the first must chain from genesis. Without the lock, two
// appends read the same head and both chain from it; without the clock_timestamp()
// stamp, the row that ends up newest by created_at can be one that another row has
// already chained from. Either way the walk below breaks -- which is the point,
// and why this test is deterministic rather than a race that only sometimes fails.
func TestConcurrentAppendStaysLinear(t *testing.T) {
	pool := ledgerPool(t)
	ctx := context.Background()

	require.NoError(t, tenant.RunMigrations(ctx, pool))
	setMasterKey(t)

	tenantID, secret := testChainTenant(t, pool)

	ledger := NewLedger(pool)

	// Goroutines and channels, as the house style has it: the appends are
	// independent, so they run independently and their failures are collected
	// rather than raced on.
	results := make(chan error, appendCount)

	for i := 0; i < appendCount; i++ {
		go func(n int) {
			_, err := ledger.Append(ctx, tenantID, uuid.NewString(), appendTrace(n), secret)
			results <- err
		}(i)
	}

	for i := 0; i < appendCount; i++ {
		require.NoError(t, <-results, "concurrent append %d", i)
	}

	// Walk the chain the way a verifier would: in created_at order, which the
	// write path guarantees is chain order.
	rows, err := pool.Query(ctx,
		`SELECT hash_value, prev_hash, created_at FROM `+ledgerTable+` WHERE tenant_id = $1 ORDER BY created_at`,
		tenantID)
	require.NoError(t, err)
	defer rows.Close()

	prev := briefGenesisHash()
	var lastCreatedAt time.Time

	seen := 0
	for rows.Next() {
		var hash, prevHash string
		var createdAt time.Time
		require.NoError(t, rows.Scan(&hash, &prevHash, &createdAt))

		assert.Equal(t, prev, prevHash, "row %d must chain from the row before it", seen)
		assert.True(t, createdAt.After(lastCreatedAt),
			"row %d must be stamped after its predecessor, or \"newest\" is ambiguous", seen)

		prev = hash
		lastCreatedAt = createdAt
		seen++
	}

	require.NoError(t, rows.Err())
	assert.Equal(t, appendCount, seen, "every append must be on the one chain")
}
