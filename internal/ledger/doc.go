// Package ledger is the v2 audit ledger: an append-only, HMAC-signed,
// hash-linked record of what the control plane did with a tenant's memories.
//
// The rows live in synapse_global.ledger -- cross-tenant bookkeeping, which is why
// they are not in a tenant schema. The DDL and the INSERT-only role are
// internal/tenant's migration, applied by RunMigrations on every plane boot, and
// this package reads the schema name and the role name from that package rather
// than repeating them.
//
// Phase 16 scope is the write path. Ledger.Append signs one trace per call and
// chains it to its tenant's previous entry, so every row carries
//
//	prev_hash  = the previous entry's hash_value, or hex(sha256("genesis"))
//	hash_value = hex(HMAC-SHA256(tenant secret, id+tenant_id+request_id+prev_hash+trace_json))
//
// and a later phase can recompute every signature independently from the rows plus
// the tenant's secret. The secret itself never reaches the ledger, and the role the
// write runs as cannot even read the table it writes to: the chain head is read
// before the transaction switches to tenant.LedgerWriterRole, which holds INSERT
// on that one table and nothing else. "Never update, never delete" is therefore a
// database privilege rather than a code convention, and so is "never rewrite".
//
// Files:
//
//	ledger.go  the write path: Ledger, LedgerEntry, Append, and the chain reads
//	chain.go   the hashing primitives: genesis hash, chain message, HMAC signature
//	ledger_test.go, ledger_table_test.go   integration tests (build tag: integration)
//
// The integration tests need a real PostgreSQL: testcontainers-go is not a
// dependency of this module, so they use the database the Phase 4 compose stack
// publishes (deploy/docker-compose.yml), and the signing tests set their own
// SYNAPSE_MASTER_KEY so the commands below need no exported secrets:
//
//	go test ./internal/ledger/... -run TestAppend -v -tags integration
//	go test ./internal/ledger/... -run TestLedgerTablePermissions -v -tags integration
//
// There is no untagged unit test for the write path, because everything it does
// happens inside one transaction against Postgres and a fake pgx.Tx would mostly
// test the fake. What is not database-bound -- AES-GCM wrapping of the tenant
// secret, the master key's shape -- lives in internal/tenant and is unit-tested
// there without a database.
package ledger
