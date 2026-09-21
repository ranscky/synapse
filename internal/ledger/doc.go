// Package ledger is the v2 audit ledger: an append-only record of what the
// control plane did with a tenant's memories, so that a later phase can prove
// the record was not rewritten.
//
// Phase 15 scope is the table and the role that guards it, and nothing else. The
// rows live in synapse_global.ledger (cross-tenant bookkeeping, which is why they
// are not in a tenant schema); the DDL is internal/tenant's migration, applied by
// RunMigrations on every plane boot. The only role allowed to write is
// tenant.LedgerWriterRole, which holds INSERT on that one table and nothing else
// -- no UPDATE, no DELETE, no SELECT -- so "never update, never delete" is a
// database privilege rather than a code convention. There is no signing and no
// hash chain yet: prev_hash and hash_value are still supplied by the caller.
//
// This file exists so the package always has a buildable Go file. Everything else
// in this directory is behind //go:build integration, and a directory whose only
// files are all excluded by build constraints makes `go test ./...` fail outright
// (NoGoError: "build constraints exclude all Go files") rather than skip, which
// would break CI.
//
// The permission guarantee is verified against a real PostgreSQL by
// ledger_table_test.go:
//
//	go test ./internal/ledger/... -run TestLedgerTablePermissions -v -tags integration
package ledger
