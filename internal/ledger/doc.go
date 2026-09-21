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
// Phase 16 wrote that path; Phase 17 reads it. Ledger.Verify walks one tenant's
// entries in created_at -- which the write path makes chain order -- and checks
// two independent properties per row: that the recomputed signature equals the
// stored hash_value, and that each row chains from the one before it, with the
// first row chaining from genesis. The mismatch it reports is named and located
// (entry id, that row's created_at), because "your ledger no longer verifies" is
// only actionable with a row attached.
//
// The role that matters here is the opposite one: verification needs the owner's
// connection, since tenant.LedgerWriterRole holds no SELECT at all.
//
// Files:
//
//	ledger.go   the write path: Ledger, LedgerEntry, Append, and the chain reads
//	chain.go    the hashing primitives: genesis hash, chain message, HMAC signature
//	verify.go   the read path: ChainIntegrityResult, verifyQuery, Ledger.Verify
//	ledger_test.go, ledger_table_test.go, verify_test.go, verify_input_test.go
//	            integration tests (build tag: integration)
//
// The integration tests need a real PostgreSQL: testcontainers-go is not a
// dependency of this module, so they use the database the Phase 4 compose stack
// publishes (deploy/docker-compose.yml), and the signing tests set their own
// SYNAPSE_MASTER_KEY so the commands below need no exported secrets:
//
//	go test ./internal/ledger/... -run TestAppend -v -tags integration
//	go test ./internal/ledger/... -run TestVerify -v -tags integration
//	go test ./internal/ledger/... -run TestLedgerTablePermissions -v -tags integration
//
// There is no untagged unit test for either path, because everything they do
// happens against Postgres and a fake pgx.Tx would mostly test the fake. What is
// not database-bound -- AES-GCM wrapping of the tenant secret, the master key's
// shape -- lives in internal/tenant and is unit-tested there without a database,
// and the HTTP half of Verify (the route wiring, the wire shape, the fail-closed
// answers) is unit-tested in internal/plane, where it runs in CI.
package ledger
