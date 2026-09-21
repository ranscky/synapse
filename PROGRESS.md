# Synapse Context Compiler — Progress Log

One entry per build phase, written before the phase session closes (see
`.clinerules` → Phase discipline). Build and debug phases get separate
sessions, so this file is the handoff between them.

## v1 — shipped at v0.1.15

Standalone proxy: store, embedder, dedup, scorer, budget, compiler, retrieval,
supersession, trace inspector, OpenAI-compatible endpoint. v1 internals are
frozen for the v2 work.

## Phase 0 — pre-v2 cleanup (complete)

Commit `1154486 feat: Phase 0 - pre-v2 cleanup`

- Removed 4 lines of dead code ahead of the v2 work.
- Touched: `internal/api/api.go`, `internal/trace/trace.go`, `openapi.yaml`.
- No v1 behavior change.

## Phase 1 — control plane binary boots (complete)

Scope was deliberately narrow: boot, load config, serve `GET /health`.
No database, no auth, no features.

Commit `feat: Phase 1 - control plane binary boots`

New files:

- `internal/plane/config.go` — `PlaneConfig`, `DefaultConfig`, `LoadConfig`,
  `Validate`, `RedactedFields`, `UnsafePermissions`, plus the
  `DefaultListenAddr` / `MinJWTSecretLen` / `Env*` constants.
- `cmd/plane/main.go` — `--config` / `--port` flags, charmbracelet/log,
  chi v5 with one route (`GET /health`), graceful SIGINT/SIGTERM shutdown.
- `synapse-plane.yaml.example` — all 7 keys, each commented with its purpose
  and the environment variable that overrides it.
- `internal/plane/config_test.go` — defaults, YAML load, env precedence,
  validation, and value-free error assertions.
- `internal/plane/redact_test.go` — secret-redaction assertions and the
  config-permission check. Split out from `config_test.go` to stay under the
  300-line-per-file cap. Together: 13 test funcs plus 13 `TestValidate`
  subtests.
- `PROGRESS.md` — this file.

Decisions made in this phase:

- **Env beats YAML** for `database-dsn`, `jwt-secret`, `admin-token`, and
  `master-key`. An unset *or empty* variable leaves the file value in place,
  so an exported-but-blank variable can never silently disable a secret that
  is configured on disk.
- **`Validate()` returns an error; `cmd/plane` makes it fatal** with
  `os.Exit(1)`. No library code exits the process, so validation is testable.
- **`listen-addr` must be loopback** (`127.*`, `localhost`, `::1`) — the plane
  refuses to boot on `0.0.0.0`, a bare `:9090`, or a LAN address.
- **Required at boot:** `database-dsn`, and `jwt-secret` at ≥ 32 characters.
  `admin-token` and `master-key` are parsed and redacted but deliberately not
  enforced yet, because Phase 1 registers no authenticated route; they become
  required in the phase that adds auth and tenant key wrapping.
- **No new dependencies.** `pgx`, `pgvector-go`, `golang-jwt`, and `mcp-go`
  are *not* yet in `go.mod` and Phase 1 needs none of them. Only the existing
  chi v5, go-yaml v3, charmbracelet/log, and testify are used.
- **Logging secrets is structurally prevented**: `RedactedFields()` is the
  only way config is logged and emits `jwt_secret=set` / `unset`, never a
  value. `Validate()` error strings name keys only.
- `bin/plane`, `plane`, and `synapse-plane.yaml` are gitignored; note that
  `bin/synapse` is still tracked from v1, which is why the plane artifact
  needed an explicit ignore rule.

Verification (real output, this phase):

```text
$ go build ./cmd/plane            # clean
$ go vet ./internal/plane ./cmd/plane
VET OK
$ gofmt -l internal/plane cmd/plane   # empty
$ go test ./internal/plane
ok  	synapse/internal/plane	0.009s

$ ./bin/plane --config synapse-plane.yaml.example
10:58AM WARN plane: Control plane config is readable by other users and holds secrets -- recommended fix: chmod 600 path=synapse-plane.yaml.example current_permissions=0664
10:58AM INFO plane: Control plane config loaded listen_addr=127.0.0.1:9090 database_dsn=set jwt_secret=set admin_token=set master_key=set log_level=info ledger_retention_days=365
10:58AM INFO plane: Synapse Control Plane v2.0.0 listening addr=127.0.0.1:9090

$ curl -sS -i http://127.0.0.1:9090/health
HTTP/1.1 200 OK
Content-Type: application/json
Content-Length: 33

{"status":"ok","version":"2.0.0"}

$ grep -E 'postgres://|placeholder-secret|placeholder-admin|placeholder-master' <full startup log>
NO SECRET VALUES IN LOG
```

Negative paths also verified against the built binary (each exits 1 with a
key-only message): short `jwt-secret`, missing `database-dsn`, missing
`jwt-secret`, non-loopback `listen-addr`. `--port 9199` overrode the
configured port and `/health` answered on 127.0.0.1:9199.

Next phase: not started. Do not add Phase 2 surface here.

## Phase 2 — JWT auth middleware (complete)

Scope was one file plus its test: a chi-compatible middleware that verifies the
tenant JWT and the accessors handlers use to read the verified claims. No route
is wired yet, and no v1 package or `cmd/plane` file was touched.

Commit `feat: Phase 2 - JWT auth middleware`

New files:

- `internal/tenant/auth.go` — `JWTMiddleware(*plane.PlaneConfig) func(http.Handler) http.Handler`,
  the unexported `claims` payload struct (`tenant_id`, `tenant_slug`, `plan`,
  `compliance_tier`, `admin` + `jwt.RegisteredClaims`), five unexported
  `ctxKey` values, `writeUnauthorized`, `bearerToken`, `hmacKeyFunc`,
  `withClaims`, `stringFromCtx`, and the exported accessors
  `TenantIDFromCtx`, `TenantSlugFromCtx`, `PlanFromCtx`,
  `ComplianceTierFromCtx`, `IsAdminFromCtx`. 215 lines.
- `internal/tenant/auth_test.go` — 11 test functions / 29 subtests, all through
  `httptest.NewRequest` + `httptest.NewRecorder` against a chi router that
  installs the middleware via `Use`. 296 lines.

Changed files:

- `go.mod` / `go.sum` — `github.com/golang-jwt/jwt/v5 v5.3.1`, the phase's only
  new dependency (zero transitive deps; its `go.mod` declares `go 1.21`, so the
  module's `go 1.22.5` directive and CI's Go 1.22 matrix are unaffected).
  `go mod tidy` is a no-op afterwards.

Decisions made in this phase:

- **Strict, fail-closed verification.** A request is a 401 when the header is
  absent, the scheme is not `Bearer`, the token is blank or malformed, the
  algorithm is outside `HS256`/`HS384`/`HS512`, the signature does not match,
  the token has expired, the token carries **no `exp`** (`jwt.WithExpirationRequired`),
  the verified token names **no `tenant_id`**, or the middleware was built with
  no secret (`nil` config or empty `JWTSecret`). A signature-valid token that
  names no tenant is a rejection, not a zero value: under schema-per-tenant an
  empty isolation key must never reach a handler.
- **One 401 body for every reason**, `{"error":"unauthorized"}`, so the response
  cannot be used as an oracle to learn which check failed. `Content-Type:
  application/json` and `WWW-Authenticate: Bearer` (RFC 6750 §3) are set.
- **Algorithm confusion is closed twice**: `jwt.WithValidMethods` allow-lists the
  HMAC algorithms, and the key function type-asserts
  `*jwt.SigningMethodHMAC`, so `alg: none` and asymmetric headers can never be
  verified with the shared secret.
- **The signing key is captured once at construction** and never re-read from
  `cfg`, so mutating config after middleware construction cannot change how an
  in-flight token verifies.
- **Secret leakage is structurally impossible**: `internal/tenant` imports no
  logger at all, and rejections never reflect the parser error, the token, or a
  claim value. The only body ever written is the constant above.
- **Claims live under unexported `ctxKey` values**, so no other package can read
  or overwrite a claim with a colliding string key, and the accessors return
  zero values when a request never passed through the middleware.
- **Not wired to a route.** `cmd/plane` still serves only `GET /health`;
  registering a protected route belongs to the phase that adds plane endpoints.
- The middleware deliberately has no unit-test file of its own for parsing: the
  token payloads in the tests are built with `jwt.MapClaims` and literal JSON key
  strings, so a typo in a `claims` struct tag fails the tests rather than passing
  against itself.

Verification (real output, this phase):

```text
$ gofmt -l internal/tenant          # empty
$ go vet ./internal/tenant ./cmd/plane
VET_OK
$ go build ./cmd/plane
BUILD_OK
$ go test ./internal/tenant/... -v
--- PASS: TestJWTMiddlewareValidTokenAttachesClaims (0.00s)
--- PASS: TestJWTMiddlewareLowercaseSchemeAccepted (0.00s)
--- PASS: TestJWTMiddlewareAbsentAdminClaimDefaultsFalse (0.00s)
--- PASS: TestJWTMiddlewareExpiredTokenRejected (0.00s)
--- PASS: TestJWTMiddlewareMissingAuthorizationHeaderRejected (0.00s)
--- PASS: TestJWTMiddlewareWrongSecretRejected (0.00s)
--- PASS: TestJWTMiddlewareRejectsUnusableCredentials (0.00s)
    --- PASS: .../non-bearer_scheme  .../scheme_with_no_separator
    --- PASS: .../bearer_with_empty_token  .../not_a_jwt  .../expired
    --- PASS: .../no_exp_claim  .../empty_tenant_id  .../missing_tenant_id
    --- PASS: .../alg_none
--- PASS: TestJWTMiddlewareFailsClosedWithoutASecret (0.00s)
    --- PASS: .../nil_config  .../empty_secret
--- PASS: TestJWTMiddlewareRejectionsLeakNoCredential (0.00s)
--- PASS: TestClaimHelpersOnEmptyContext (0.00s)
--- PASS: TestBearerToken (0.00s)
    --- PASS: .../canonical  .../lowercase_scheme  .../extra_spacing
    --- PASS: .../empty_header  .../scheme_with_no_separator
    --- PASS: .../empty_token  .../other_scheme
PASS
ok  	synapse/internal/tenant	0.020s

$ go test ./internal/tenant/... -race -count=1
ok  	synapse/internal/tenant	1.036s

$ go test ./internal/plane/...
ok  	synapse/internal/plane	0.009s
```

`git diff --stat` before the commit: `go.mod` (+1 line), `go.sum` (+4 lines),
and the two new `internal/tenant` files. No v1 package, `internal/plane`, or
`cmd/plane` file changed.

Next phase: not started. Registering a protected route — and issuing the tokens
this middleware verifies — belongs to a later phase.

## Phase 3 — tenant provisioning and migrations (complete)

Scope: create `synapse_global` and its tables at plane startup, add the
admin-guarded `POST /v2/tenants`, and make `GET /health` report database state.

Commit `feat: Phase 3 - tenant provisioning and migrations`

New files:

- `internal/tenant/migrations.go` — `SchemaName` and
  `RunMigrations(ctx, *pgxpool.Pool)`: the six documented statements
  (`CREATE SCHEMA IF NOT EXISTS` plus five `CREATE TABLE IF NOT EXISTS`), applied
  in one transaction. 132 lines.
- `internal/tenant/keys.go` — `GenerateAPIKey` (32 bytes from `crypto/rand`,
  hex-encoded), `HashAPIKey` / `VerifyAPIKey` (bcrypt). 52 lines.
- `internal/tenant/token.go` — `TokenTTL` (365 days) and `IssueToken`, which
  builds the *same unexported `claims` struct `auth.go` parses*, HS256.
- `internal/tenant/store.go` — `Store.CreateTenant`: tenants row plus key hash in
  one transaction, duplicate slug detected from SQLSTATE `23505`.
- `internal/tenant/provision.go` — `Provisioner`, the orchestration point
  (generate key → hash → store → issue token) and the place
  `ErrTenantExists` becomes `plane.ErrTenantExists`.
- `internal/plane/handlers.go` — `Version`, the `Database` interface, `Server` /
  `NewServer` / `Routes`, `handleHealth`, `handleCreateTenant`, strict
  `decodeJSON`, and the response/error types.
- `internal/plane/admin.go` — `requireAdmin`, the constant-time, fail-closed
  admin-token check.
- `internal/plane/provision.go` — the plane-side provisioning contract
  (`ProvisionRequest`, `ProvisionResult`, `ErrTenantExists`, `TenantProvisioner`).
- `internal/plane/dsn.go` — `DBHost`, so the fatal startup lines name the target
  without echoing the DSN.
- Tests: `internal/plane/handlers_test.go`, `internal/plane/provision_test.go`,
  `internal/plane/dsn_test.go`, `internal/tenant/keys_test.go`,
  `internal/tenant/token_test.go`, `internal/tenant/migrations_test.go`.

Changed files:

- `cmd/plane/main.go` — after validation: `pgxpool.ParseConfig` →
  `NewWithConfig` → `Ping` → `RunMigrations`, then `plane.NewServer(...).Routes()`
  and `pool.Close()` on the shutdown path. `version` moved into the plane package
  as `plane.Version`.
- `internal/plane/config.go` — package doc refreshed for Phase 3; no behavior
  change (validation rules are exactly as Phase 1 left them).
- `synapse-plane.yaml.example` — the `admin-token` comment now describes the
  route it guards, the exact-match header, and the fail-closed behavior.
- `go.mod` / `go.sum` — `github.com/jackc/pgx/v5 v5.7.4`,
  `golang.org/x/crypto v0.31.0`, plus the indirect graph pgx needs
  (`puddle/v2`, `pgpassfile`, `pgservicefile`, `x/sync`, `x/text`).

Decisions made in this phase:

- **pgx is pinned to v5.7.4, not "latest".** A plain
  `go get github.com/jackc/pgx/v5` resolves to v5.11.0, whose `go.mod` declares
  `go 1.25.0` (v5.8.0 declares 1.24, v5.7.5/6 declare 1.23). Any of those would
  raise this module's `go 1.22.5` directive, breaking `.clinerules` ("Go 1.22+")
  and the CI matrix (`go-version: [1.22]`). v5.7.4 declares `go 1.21` and every
  transitive dependency it needs is ≤ 1.21, so the directive and CI are untouched.
- **`golang.org/x/crypto v0.31.0` is the phase's second new dependency**, needed
  for bcrypt. It is the version pgx v5.7.4 itself requires, and it declares
  `go 1.20`. There is no stdlib bcrypt; `crypto/pbkdf2` is Go 1.24+ and would
  force the same directive bump the pgx pin avoids.
- **`go mod tidy` needed one more pin.** tidy resolves the test-only transitivity
  `pgx → gopkg.in/check.v1 → github.com/kr/pretty → github.com/rogpeppe/go-internal`
  and picked go-internal v1.16.0, which declares `go 1.25`. Pinning it to v1.13.1
  (`go 1.22`) keeps the directive at 1.22.5 and leaves `go mod tidy` a no-op.
- **The provisioning contract lives in `internal/plane`, and that is forced by
  the existing dependency direction.** `internal/tenant` already imports
  `internal/plane` (its middleware takes a `*plane.PlaneConfig`), so `plane`
  importing `tenant` would be a cycle. The consumer therefore declares the
  interface and the request/result types
  (`TenantProvisioner`, `ProvisionRequest`, `ProvisionResult`, `ErrTenantExists`)
  and `tenant.Provisioner` implements them — no adapter wired up in `main`, and
  the handler still depends on an interface rather than a database handle.
- **Migrations run in a single transaction**, so a failure part-way through
  leaves the database untouched instead of a partial schema the next boot would
  build on. There is no advisory lock: migrations run from one controller's
  startup path and `CREATE ... IF NOT EXISTS` is repeatable; two planes booting at
  once is not a supported topology yet.
- **No `pgcrypto`.** `gen_random_uuid()` is core since PostgreSQL 13, so the DDL
  needs no extension and therefore no superuser.
- **The admin token is the exact `Authorization` value**, compared in constant
  time over SHA-256 digests of both sides. `Bearer <token>` is *rejected* on
  purpose: one secret with two accepted spellings is a wider surface for no gain.
  A blank configured token makes the route answer 401 to everyone (fail-closed)
  with a startup warning, rather than making `Validate` fatal — that keeps Phase
  1's config contract and its tests unchanged.
- **`/health` probes the database on every request** and reports
  `db=connected` only when the probe succeeds (200); otherwise
  `{"status":"degraded","db":"disconnected"}` with 503. A health endpoint that
  never fails is not a health endpoint.
- **Slugs are validated exactly as sent** against `^[a-z0-9-]{3,32}$` — no
  trimming or case-folding, because a slug becomes a schema-name component later.
  `plan` defaults to `oss` (the DDL default) and must match `^[a-z0-9_-]{1,32}$`.
  `compliance_tier` is fixed at `team` this phase and is not client-settable.
- **Duplicate slugs are decided by the unique constraint** (SQLSTATE 23505 →
  `ErrTenantExists` → 409), not by a prior `SELECT`, so provisioning is race-free.
- **Client-facing error bodies are constants** — `invalid_body`, `invalid_slug`,
  `invalid_plan`, `unauthorized`, `tenant_exists`, `internal`. The raw database
  error is logged server-side only, because pgx error text quotes the connection
  target (host and user).
- **`tenant_secrets` is created but never written**: sealing a per-tenant secret
  needs the master key, which is a later phase's job.
- **Secrets stay out of the log structurally.** `api_key`, `jwt`, the JWT secret,
  and the admin token appear in no log line; `internal/tenant` still imports no
  logger at all, and the only identifiers permitted into a log line are
  `tenant_id`, `slug`, `plan`, `db_host`, and `schema`.

Verification (real output, this phase):

```text
$ gofmt -l internal/tenant internal/plane cmd/plane      # empty
$ go vet ./internal/tenant ./internal/plane ./cmd/plane  # clean
$ go build ./cmd/plane
BUILD_OK
$ go build ./...
BUILD_ALL_OK

$ go test ./internal/plane/... -count=1
ok  	synapse/internal/plane	0.015s

$ go test ./... -count=1
?   	synapse/cmd/benchmark	[no test files]
?   	synapse/cmd/counttokens	[no test files]
?   	synapse/cmd/mergesessions	[no test files]
?   	synapse/cmd/plane	[no test files]
?   	synapse/cmd/synapse	[no test files]
ok  	synapse/internal/api	0.996s
ok  	synapse/internal/budget	0.345s
ok  	synapse/internal/classifier	0.011s
ok  	synapse/internal/compiler	0.233s
ok  	synapse/internal/config	0.009s
ok  	synapse/internal/dedup	0.010s
ok  	synapse/internal/embedder	4.882s
ok  	synapse/internal/integration	3.181s
ok  	synapse/internal/plane	0.042s
ok  	synapse/internal/proxy	0.532s
?   	synapse/internal/retrieval	[no test files]
ok  	synapse/internal/scorer	0.010s
?   	synapse/internal/session	[no test files]
ok  	synapse/internal/store	1.139s
ok  	synapse/internal/supersession	0.019s
ok  	synapse/internal/tenant	0.786s
ok  	synapse/internal/trace	0.200s
```

The database-backed tests skip unless a target is supplied, then run against a
real PostgreSQL 16.15:

```text
$ SYNAPSE_TEST_DB_DSN='postgres://synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/tenant/... -count=1
--- PASS: TestRunMigrationsIsIdempotentAndCreatesEveryDocumentedTable (0.01s)
--- PASS: TestMigratedConstraintsAreWhatTheHandlerReliesOn (0.02s)
--- PASS: TestProvisionerStoresTheHashAndIssuesAVerifiableToken (0.33s)
--- PASS: TestGenerateAPIKeyIs32RandomBytesHexEncoded (0.00s)
--- PASS: TestHashAPIKeyNeverStoresTheKey (0.38s)
--- PASS: TestHashAPIKeySaltsEveryHash (0.39s)
--- PASS: TestIssueTokenRoundTripsThroughJWTMiddleware (0.00s)
--- PASS: TestIssueTokenClaimsAreTheDocumentedWireContract (0.00s)
--- PASS: TestIssueTokenFailsClosed (0.00s)
--- PASS: (plus every Phase 2 middleware test, unchanged)
ok  	synapse/internal/tenant	1.146s
```

No PostgreSQL and no Docker were available on this machine, so the target was
provided root-free, from Debian packages extracted into `/tmp` (no system
changes, no `sudo`):

```bash
apt-get download postgresql-16 postgresql-client-16 libpq5
for d in *.deb; do dpkg-deb -x "$d" /tmp/scc-pg/root; done
B=/tmp/scc-pg/root/usr/lib/postgresql/16/bin
$B/initdb -D /tmp/scc-pg/data -U synapse -E UTF8 --locale=C --auth=trust
# postgresql.conf: listen_addresses='127.0.0.1', port=5432, jit=off
$B/pg_ctl -D /tmp/scc-pg/data -l /tmp/scc-pg/pg.log start
$B/createdb -h /tmp/scc-pg/sock -U synapse synapse
```

Then the plane, started with env-injected secrets against that database:

```text
$ ./plane --config /tmp/scc-pg/plane.yaml     # listen-addr 127.0.0.1:9090, 0600
11:21AM INFO plane: Control plane config loaded listen_addr=127.0.0.1:9090 database_dsn=set jwt_secret=set admin_token=set master_key=unset log_level=info ledger_retention_days=365
11:21AM INFO plane: migrations complete schema=synapse_global
11:21AM INFO plane: Synapse Control Plane v2.0.0 listening addr=127.0.0.1:9090

$ curl -sS -i http://127.0.0.1:9090/health
HTTP/1.1 200 OK
Content-Type: application/json
Content-Length: 50

{"status":"ok","version":"2.0.0","db":"connected"}

$ curl -sS -i -X POST http://127.0.0.1:9090/v2/tenants \
    -H "Authorization: $SYNAPSE_ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d '{"slug":"my-team","plan":"team"}'
HTTP/1.1 201 Created
Content-Type: application/json
Content-Length: 519

{"tenant_id":"408c4a4a-703f-452f-8087-7a60ac372802","jwt":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ0ZW5hbnRfaWQiOiI0MDhjNGE0YS03MDNmLTQ1MmYtODA4Ny03YTYwYWMzNzI4MDIiLCJ0ZW5hbnRfc2x1ZyI6Im15LXRlYW0iLCJwbGFuIjoidGVhbSIsImNvbXBsaWFuY2VfdGllciI6InRlYW0iLCJhZG1pbiI6ZmFsc2UsInN1YiI6IjQwOGM0YTRhLTcwM2YtNDUyZi04MDg3LTdhNjBhYzM3MjgwMiIsImV4cCI6MTgyMTQzOTI3NywibmJmIjoxNzg5OTAzMjc3LCJpYXQiOjE3ODk5MDMyNzd9.pkepsQ_DFyNUg40UK9JkPnVoSN2ffxuBNuuFIxY6Jzg","api_key":"7bbfe0be8b7a95006a68c4757c5d8322cfb1abda2daecccf66f934786a9c97a8"}

$ curl -sS -i -X POST http://127.0.0.1:9090/v2/tenants \
    -H "Authorization: $SYNAPSE_ADMIN_TOKEN" -H 'Content-Type: application/json' \
    -d '{"slug":"my-team","plan":"team"}'
HTTP/1.1 409 Conflict
Content-Type: application/json
Content-Length: 25

{"error":"tenant_exists"}

$ curl -sS -i -X POST http://127.0.0.1:9090/v2/tenants -d '{"slug":"other-team"}'
HTTP/1.1 401 Unauthorized          {"error":"unauthorized"}
$ curl -sS -i -X POST http://127.0.0.1:9090/v2/tenants \
    -H "Authorization: $SYNAPSE_ADMIN_TOKEN" -d '{"slug":"My Team","plan":"team"}'
HTTP/1.1 400 Bad Request           {"error":"invalid_slug"}
```

The claims in that JWT, decoded, and what the registry row holds:

```text
decoded claims: { "tenant_id": "408c4a4a-...", "tenant_slug": "my-team",
                  "plan": "team", "compliance_tier": "team", "admin": false,
                  "exp": 1821439277, "nbf": 1789903277, "iat": 1789903277 }
lifetime_days: 365.0

synapse_global.tenants
 408c4a4a-703f-452f-8087-7a60ac372802 | my-team | team | team | active

synapse_global.tenant_keys
 my-team | $2a$10$Fax/tbsiKhUKQyF8HUr7muJRCYudbbHmQ1FYYMjOz22ZRcDc0aKIu
select count(*) from synapse_global.tenant_keys where key_hash = '<api_key>';  -> 0

information_schema.tables, schema synapse_global:
 compliance_access_log, tenant_keys, tenant_secrets, tenants, usage_events
```

Secrets in the log, counted over the full startup + request log (each must be 0):

```text
api_key      occurrences: 0
jwt          occurrences: 0
admin_token  occurrences: 0
jwt_secret   occurrences: 0
password     occurrences: 0
```

Negative paths, each exit 1 with no credential in the message:

```text
$ SYNAPSE_DB_DSN='postgres://synapse:REDACTED@127.0.0.1:5433/synapse' ./plane ...
ERRO plane: Failed to connect to the database db_host=127.0.0.1:5433     (exit 1)
$ SYNAPSE_DB_DSN='postgres://synapse:REDACTED@%%%/synapse' ./plane ...
ERRO plane: Failed to parse the database DSN                             (exit 1)

# /health degrades and recovers with the database:
$ pg_ctl stop  -> 503 {"status":"degraded","version":"2.0.0","db":"disconnected"}
$ pg_ctl start -> 200 {"status":"ok","version":"2.0.0","db":"connected"}
```

`git diff --stat` before the commit: 4 new `internal/tenant` files plus
`provision_test.go`, 4 new `internal/plane` files plus 3 test files,
`cmd/plane/main.go`, `internal/plane/config.go` (package doc only),
`synapse-plane.yaml.example`, `PROGRESS.md`, `go.mod`, `go.sum`. No v1 internal
package was touched — `go test ./...` above is the proof.

Next phase: not started. Tenant-scoped data schemas, API-key authentication
(`tenant.VerifyAPIKey` against `synapse_global.tenant_keys`), master-key sealing
for `tenant_secrets`, metering, and the ledger are all still ahead.

Verification (real output, this phase):

```text
$ docker --version
Docker version 29.8.1, build 4a63305
$ docker compose version
Docker Compose version v5.5.1
$ docker ps -a --format '{{.Names}}'     # engine state before the first run
(empty)                                   # 0 images, 0 containers, 0 volumes

$ cd deploy && docker compose config      # offline validation, exit 0
name: deploy
services:
  db:
    image: pgvector/pgvector:pg16
    ports:
      - mode: ingress
        host_ip: 127.0.0.1
        target: 5432
        published: "5432"
  plane:
    build:
      context: /home/ranscky/Dev/synapse
      dockerfile: deploy/Dockerfile.plane
    network_mode: host
```

The cold run, `docker compose up --build` against that empty engine (pull
progress and the initdb banner abridged; everything else is verbatim):

```text
#10 [builder 3/4] COPY . .                     # 1.5s -- .dockerignore kept the context small
#10 DONE 1.5s
#11 [builder 4/4] RUN go build -o /bin/plane ./cmd/plane
#13 naming to docker.io/library/deploy-plane:latest 0.0s done
db-1  | 2026-09-20 12:23:07.100 UTC [1] LOG:  starting PostgreSQL 16.15 (Debian 16.15-1.pgdg12+2) ...
db-1  | 2026-09-20 12:23:07.100 UTC [1] LOG:  listening on IPv4 address "0.0.0.0", port 5432
db-1  | 2026-09-20 12:23:07.412 UTC [1] LOG:  database system is ready to accept connections
 Container deploy-db-1 Healthy
 Container deploy-plane-1 Starting
plane-1  | 12:23PM WARN plane: Control plane config file not found, using defaults and environment path=synapse-plane.yaml
plane-1  | 12:23PM INFO plane: Control plane config loaded listen_addr=127.0.0.1:9090 database_dsn=set jwt_secret=set admin_token=set master_key=set log_level=info ledger_retention_days=365
 Container deploy-plane-1 Started
plane-1  | 12:23PM INFO plane: migrations complete schema=synapse_global
plane-1  | 12:23PM INFO plane: Synapse Control Plane v2.0.0 listening addr=127.0.0.1:9090

$ curl -sS -i http://127.0.0.1:9090/health
HTTP/1.1 200 OK
Content-Type: application/json
Date: Sun, 20 Sep 2026 12:23:27 GMT
Content-Length: 50

{"status":"ok","version":"2.0.0","db":"connected"}
```

Postgres above logs `listening on IPv4 address "0.0.0.0"` *inside its own
container*; on the host only the two loopback endpoints exist:

```text
$ ss -ltn | grep -E ':(5432|9090)'
LISTEN 0 4096 127.0.0.1:9090 0.0.0.0:*
LISTEN 0 4096 127.0.0.1:5432 0.0.0.0:*
```

pgvector in the switched image, with the requested preload flag actually taking
effect (no `could not access file "vector"` anywhere in the log):

```text
$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    "select name, default_version from pg_available_extensions where name='vector'"
  name  | default_version
--------+-----------------
 vector | 0.8.6

$ docker exec deploy-db-1 psql -U synapse -d synapse -tAc 'show shared_preload_libraries'
vector
```

Negative control for the networking decision — the same image, bridge-networked,
with the published port the original compose sketch asked for:

```text
$ docker run -d --rm --name plane-bridge-probe --network deploy_default -p 9099:9099 \
    -e SYNAPSE_DB_DSN='postgres://synapse:synapse@db:5432/synapse?sslmode=disable' \
    -e SYNAPSE_JWT_SECRET='change-me-in-production-must-be-32-chars' \
    deploy-plane:latest --port 9099

$ curl -sS -i http://127.0.0.1:9099/health      # from the host, via the published port
curl: (56) Recv failure: Connection reset by peer

$ docker exec plane-bridge-probe wget -q -O - http://127.0.0.1:9099/health
{"status":"ok","version":"2.0.0","db":"connected"}
```

Same image, same database, same healthy plane — only the host-side published path
fails, because the listener is on the container's own loopback. That is why the
compose service runs with `network_mode: host` instead of a `ports:` mapping.

Provisioning through the deployed stack, so the deployment is proven functional
beyond `/health`:

```text
$ curl -sS -i -X POST http://127.0.0.1:9090/v2/tenants \
    -H 'Authorization: change-me-admin-token' -H 'Content-Type: application/json' \
    -d '{"slug":"compose-team","plan":"team"}'
HTTP/1.1 201 Created
Content-Type: application/json
Content-Length: 526

{"tenant_id":"e2d093ed-84ed-469a-a86e-53d04e456b70","jwt":"eyJhbGciOi...","api_key":"192c98b9..."}

$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    'select id, slug, plan, compliance_tier, status from synapse_global.tenants'
                  id                  |     slug     | plan | compliance_tier | status
--------------------------------------+--------------+------+-----------------+--------
 e2d093ed-84ed-469a-a86e-53d04e456b70 | compose-team | team | team            | active

$ docker exec deploy-db-1 psql -U synapse -d synapse -tAc \
    "select count(*) from synapse_global.tenant_keys where key_hash = '192c98b9...'"
0            # the stored value is the hash; the plaintext key never reaches the table
```

Secrets in the full compose log, each must be 0:

```text
change-me-admin-token                    -> 0
change-me-in-production-must-be-32-chars -> 0
change-me-master-key-32-chars-min        -> 0
postgres://synapse:synapse               -> 0
```

Timings, measured on this machine (4 cores, 7.6G RAM, ~1.4MB/s pull throughput):

```text
cold first run, empty engine:     362s   # ~240MB of images plus Go module downloads
warm docker compose up --build:    23s   # every build step CACHED
warm docker compose up:            12s   # images cached, volume already initialized
```

The two-minute target holds for any run where the three images are already
present (12-23s). It is missed on the very first run here, and the cause is pull
throughput rather than the stack: with the images absent, `--build` spends about
five minutes downloading. `docker compose pull` ahead of time, or a faster link,
is what brings a first-ever run inside two minutes.

Teardown:

```text
$ docker compose down
 Container deploy-plane-1 Removing
 Container deploy-plane-1 Removed
 Container deploy-db-1 Stopping
 Container deploy-db-1 Stopped
 Container deploy-db-1 Removing
 Container deploy-db-1 Removed
 Network deploy_default Removing
 Network deploy_default Removed

$ docker ps -a --format '{{.Names}} {{.Status}}'   # empty
$ docker volume ls                                 # deploy_synapse_pg_data kept (down without -v)
```

`git ls-files --others --exclude-standard` reports exactly `.dockerignore` and
the two files under `deploy/`: no Go source and no v1 package changed.
`CGO_ENABLED=0 go build ./cmd/plane`, `go vet ./cmd/plane ./internal/plane
./internal/tenant` and `go test ./internal/plane/... ./internal/tenant/...`
remain green — the same control the earlier phases used.

## Phase 4 — docker compose self-hosted deployment (complete)

Commit `feat: Phase 4 - docker compose self-hosted deployment`

`cd deploy && docker compose up --build` now brings the v2 plane and its Postgres
up together, and `curl http://127.0.0.1:9090/health` answers from the host. One
new directory plus one new ignore file: no Go file was touched, `internal/plane`
and `internal/tenant` are unchanged, and no v1 internal package was opened.

New files:

- `deploy/docker-compose.yml` — `db` (pgvector/pgvector:pg16, named volume
  `synapse_pg_data`, port published on the host loopback only, `pg_isready`
  healthcheck) and `plane` (built from the repository root, started only after
  `service_healthy`, with the four `SYNAPSE_*` secrets injected from the
  environment).
- `deploy/Dockerfile.plane` — `golang:1.22-alpine` builder stage, `alpine:3.19`
  runtime stage, `CGO_ENABLED=0`.
- `.dockerignore` — at the repository root, because that is the plane image's
  build context.
- `PROGRESS.md` — this entry.

Decisions made in this phase:

- **`postgres:16` cannot satisfy the requested `command:`.** The official image
  installs only `gnupg`, `less`, `ca-certificates`, `wget`, `locales`,
  `libnss-wrapper`, `xz-utils`, `zstd`, `gosu` and `postgresql-16` — no pgvector,
  so `-c shared_preload_libraries=vector` aborts startup with
  `could not access file "vector"`. The base image is therefore
  `pgvector/pgvector:pg16`, which is Postgres 16 plus the prebuilt extension and
  makes the flag valid. Confirmed at runtime below rather than assumed.
- **`CGO_ENABLED=0` is required, not an optimization.** Alpine golang images ship
  no C toolchain, and `./cmd/plane` pulls cgo-requiring standard packages when
  cgo is enabled (`net`, via `net/http`). Reproduced in this repo before the
  Dockerfile was changed:
  `CC=/nonexistent/gcc CGO_ENABLED=1 go build ./cmd/plane` →
  `cgo: C compiler "/nonexistent/gcc" not found`, reported against
  `runtime/cgo`. The plane has no cgo dependency of its own — sqlite and
  onnxruntime live in `internal/store` and `internal/embedder`, which `cmd/plane`
  never imports — so the result is a static binary and the runtime stage needs no
  extra packages.
- **The plane keeps its loopback bind; the compose service uses
  `network_mode: host`.** `PlaneConfig.Validate` refuses any non-loopback
  `listen-addr` and offers no environment override, while a published Docker port
  is DNAT'ed to the container's `eth0` address — never to the container's own
  `127.0.0.1`. Under host networking the plane's loopback *is* the host's
  loopback, so `127.0.0.1:9090` is reachable from the host while the plane still
  never listens on `0.0.0.0`. The db stays on the compose bridge network with
  `127.0.0.1:5432:5432`, and the plane's DSN targets `127.0.0.1:5432` rather than
  the compose DNS name `db` — a host-networked container cannot resolve compose
  service names. The negative control below is the proof, not the argument.
- **`.dockerignore` goes beyond the four requested groups.** `onnx/` does not
  exist in this repo (the v1 model directory is `models/`), so `models/`, `ui/`,
  `testdata/`, `schemas/`, the stale root binaries and `synapse*.yaml` were added
  too: the untrimmed context is ~430M (`.git/` 279M, `models/` 87M, `bin/` 32M).
  Ignoring `synapse-plane.yaml` is also a correctness fix: the plane's default
  config path is `./synapse-plane.yaml` relative to `WORKDIR /app`, so a
  developer's own config file would otherwise be baked into the image and could
  set `listen-addr`.
- **Phase 3's migrations need no `vector` extension.** `RunMigrations` creates
  plain tables, so `/health` does not depend on the extension today; the image
  choice is what makes the requested preload flag valid and what the future
  tenant schemas will need. `EXPOSE 9090` is kept for documentation value even
  though host networking makes it decorative.
- **Known limitation:** `network_mode: host` is Linux-only. Docker Desktop on
  macOS and Windows does not support it; such hosts would need either an explicit
  non-loopback bind (which the plane rejects by design today) or a relay
  container sharing the plane's network namespace.


## Phase 5 — PGStore Write and Search (complete)

Commit `feat: Phase 5 - PGStore Write and Search`

`internal/store` now holds a second backend: `PGStore` talks to one tenant's
Postgres schema through pgvector, and a factory decides which backend a process
gets. `internal/scorer`, `internal/compiler`, `internal/retrieval`,
`internal/dedup`, `internal/proxy`, and `internal/api` are byte-for-byte
unchanged -- the first two because they only ever handled `MemoryEntry` values,
the last two because they already depended on their own narrow interfaces
(`proxy.MemoryStore`, `retrieval.Store`), which `*PGStore` satisfies as-is.

New files:

- `internal/store/pgstore.go` (274 lines) -- `PGStore`, `NewPGStore`, the four
  `Backend` methods, `Close`, plus the `SyncStatus*` / `EmbeddingDimensions`
  constants. Split into four files by concern to stay under the 300-line cap.
- `internal/store/pgmigrate.go` (85 lines) -- the tenant DDL: extension, schema,
  table, HNSW index, `(session_id, created_at DESC)` index, all idempotent and
  run in one transaction.
- `internal/store/pgread.go` (68 lines) -- the shared projection, row iterator,
  and scanner behind `Search`/`GetRecent`.
- `internal/store/factory.go` (116 lines) -- the `Backend` interface,
  `OpenPGPool`, and `NewStoreFromConfig`, the single place that picks a backend.
- `internal/store/pgstore_test.go` (261 lines) -- `TestPGStore` and
  `TestPGStoreRejectsInvalidSlug`, skipped unless `SYNAPSE_TEST_DB_DSN` is set.
- `internal/store/sync_status_test.go` (70 lines) -- the SQLite-side
  `sync_status` migration and round-trip, which needs no database.
- `go.mod` / `go.sum` -- `github.com/pgvector/pgvector-go v0.2.3`.

Changed:

- `internal/store/store.go` -- `MemoryEntry.SyncStatus`, the `sync_status`
  column migration, and `sync_status` added to the Write/Search/GetRecent
  statements so the field actually round-trips on the SQLite side too.
- `internal/config/config.go` -- `ControlPlaneURL` (`control-plane-url`),
  `DatabaseDSN` (`database-dsn`), and the `EnvDatabaseDSN` constant, seeded from
  `SYNAPSE_DB_DSN` in `DefaultConfig`.
- `synapse.yaml.example` -- both new keys, commented out.

Decisions made in this phase:

- **The requested `store.Store` interface did not exist, and could not.** `Store`
  was already a struct wrapping `*sql.DB`, and Go has no overloading, so
  `NewStore(cfg, tenantSlug)` could not coexist with the existing
  `NewStore(dbPath string)`. Resolved as `store.Backend` (exactly the four
  methods the pipeline calls) plus `NewStoreFromConfig`, which touches no v1
  file. The alternative -- renaming the v1 struct to `SQLiteStore` and updating
  eight v1 files plus five of their tests -- was rejected as a v1 change a v2
  feature does not require.
- **The dependency is pinned to `pgvector-go v0.2.3`.** Unpinned, `go get`
  resolves `v0.4.1`, whose `go.mod` says `go 1.25.0`: that would rewrite this
  module's `go 1.22.5` directive and push CI's pinned `go-version: [1.22]`
  matrix onto a downloaded toolchain. `v0.2.3` is the newest release that keeps
  the module at Go 1.22, ships the `pgvector-go/pgx` subpackage
  (`pgxvec.RegisterTypes`), and requires only pgx v5.6.0, so the existing
  v5.7.4 is kept rather than downgraded.
- **A real ordering bug was found by running the test, not by reading.**
  `pgxvec.RegisterTypes` fails the connection outright with
  `vector type not found in the database` when the extension does not exist yet,
  and the extension can only be created by something that already has a
  connection -- so a pool whose `AfterConnect` registers the vector type can
  never be the thing that enables it. `OpenPGPool` now enables the extension on
  a bare, unregistered connection first (`ensureVectorExtension`) and only then
  installs the hook. Confirmed against the live database: `installed_version`
  was NULL before the first run and `vector 0.8.6` after.
- **`Search` keeps the session filter the spec dropped.** Without it, Postgres
  returns other sessions' memories, which contradicts both the SQLite backend
  (`WHERE session_id = ?`) and what `retrieval.Candidates` passes in. An empty
  `sessionID` deliberately means "the whole tenant", so a plane caller can still
  search org-wide. `GetRecent` is always session-scoped.
- **Superseded rows are excluded in SQL.** Not a behavior change for any caller:
  proxy, api, and supersession already skip `SupersededBy != ""`. It means less
  data crosses the wire, and `MarkSuperseded` is immediately visible to reads.
- **An empty query embedding falls back to recency; a wrong-width one errors.**
  `<->` cannot compare against nothing, and retrieval passes an empty embedding
  whenever there is no embedder or no query text, so the fallback is required
  rather than defensive. A wrong width is a caller bug and is reported.
- **PGStore requires uuid ids.** The column is `uuid` and the v1 callers generate
  `req-<nano>` strings, so `Write` reports that plainly instead of inventing an
  id the caller could not later use for supersession.
- **Sanitization is shared, not copied.** `PGStore.Write` calls the same
  `Sanitize` the SQLite backend calls (through a zero-value `Store`, which reads
  no fields), so the two backends cannot drift into two policies -- the
  `[SANITIZED]` assertion in the test is what proves it on the PG path.
- **`SyncStatus` defaults differ per backend on purpose.** A blank status is
  normalized to `local_only` by the SQLite store (a local file has never left
  the machine) and to `synced` by PGStore (a tenant schema is already on the
  plane). Both match their own column defaults, and the migration backfills
  pre-existing SQLite rows as `local_only`.
- **`store.go` was already 504 lines, over the cap before this phase.** Rather
  than grow it further, the v2 work is four new files of at most 274 lines.
  Formatting was left alone: `gofmt -l` flagged `store.go`, `store_test.go`,
  `config.go`, and `config_test.go` at HEAD as well (verified with
  `git show HEAD:<file> | gofmt -l`), so no v1 file was reformatted wholesale;
  the new files are gofmt-clean.
- **The literal `^[a-z0-9_]{3,32}$` slug rule would have rejected every real
  tenant.** The plane issues `^[a-z0-9-]{3,32}$` slugs (hyphens, never
  underscores). PGStore accepts both and folds hyphens to underscores for the
  schema name (`acme-prod` -> `tenant_acme_prod`), which cannot collide.
- **Postgres truncates timestamps to microseconds.** The test compares
  `created_at` at microsecond precision, because that is the column's actual
  resolution; a nanosecond-exact assertion failed against the real database.


Verification (real output, this phase):

The compose stack from Phase 4 was started for this phase (`cd deploy && docker
compose up -d db`: Postgres 16.15 / pgvector 0.8.6, healthy, published on
127.0.0.1:5432). Only the `db` service is needed here; the plane binary is
untouched by this phase.

```text
$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/store/... -run TestPGStore -v
=== RUN   TestPGStore
=== RUN   TestPGStore/search_ranks_the_nearest_memory_first
=== RUN   TestPGStore/search_round-trips_the_stored_fields
=== RUN   TestPGStore/search_is_scoped_to_the_named_session
=== RUN   TestPGStore/search_with_no_session_searches_the_whole_tenant
=== RUN   TestPGStore/search_without_a_query_embedding_falls_back_to_recency
=== RUN   TestPGStore/search_rejects_a_wrong-width_embedding
=== RUN   TestPGStore/get_recent_returns_the_session_newest_first
=== RUN   TestPGStore/a_memory_without_an_embedding_round-trips_as_nil
=== RUN   TestPGStore/content_is_sanitized_before_storage
2026/09/20 12:42:46 WARN Prompt injection detected and neutralized pattern="ignore previous"
2026/09/20 12:42:46 WARN Memory content sanitized due to prompt injection memory_id=0a00f9b0-...
=== RUN   TestPGStore/an_explicit_sync_status_is_stored
=== RUN   TestPGStore/write_rejects_an_id_that_is_not_a_uuid
=== RUN   TestPGStore/tenants_are_isolated_from_each_other
=== RUN   TestPGStore/superseded_memories_drop_out_of_search_and_recent
=== RUN   TestPGStore/mark_superseded_reports_an_unknown_id
--- PASS: TestPGStore (1.82s)
    --- PASS: TestPGStore/search_ranks_the_nearest_memory_first (0.00s)
    --- PASS: TestPGStore/search_round-trips_the_stored_fields (0.00s)
    --- PASS: TestPGStore/search_is_scoped_to_the_named_session (0.01s)
    --- PASS: TestPGStore/search_with_no_session_searches_the_whole_tenant (0.00s)
    --- PASS: TestPGStore/search_without_a_query_embedding_falls_back_to_recency (0.00s)
    --- PASS: TestPGStore/search_rejects_a_wrong-width_embedding (0.00s)
    --- PASS: TestPGStore/get_recent_returns_the_session_newest_first (0.00s)
    --- PASS: TestPGStore/a_memory_without_an_embedding_round-trips_as_nil (0.01s)
    --- PASS: TestPGStore/content_is_sanitized_before_storage (0.01s)
    --- PASS: TestPGStore/an_explicit_sync_status_is_stored (0.01s)
    --- PASS: TestPGStore/write_rejects_an_id_that_is_not_a_uuid (0.00s)
    --- PASS: TestPGStore/tenants_are_isolated_from_each_other (0.18s)
    --- PASS: TestPGStore/superseded_memories_drop_out_of_search_and_recent (0.01s)
    --- PASS: TestPGStore/mark_superseded_reports_an_unknown_id (0.00s)
=== RUN   TestPGStoreRejectsInvalidSlug
--- PASS: TestPGStoreRejectsInvalidSlug (0.02s)
PASS
ok  	synapse/internal/store	1.850s
```

The DoD assertion is the first subtest: five entries written with hardcoded
384-dimension embeddings (entry *i* is a unit vector on axis *i*), a query of
`0.9 * axis3 + 0.1 * axis1`, and entry 3 -- squared L2 distance 0.02, against
1.62 for entry 1 and 1.82 for the rest -- comes back first.

Two failures were hit and fixed before that run, both worth recording:

```text
# 1. extension/reporting order (first run of the test, real output)
store: postgres pool is unreachable: vector type not found in the database
# -> OpenPGPool now enables the extension on a bare connection before the
#    AfterConnect hook registers the vector type. Re-run: pool opens.

# 2. timestamp precision (second run)
expected: ... 12, 42, 36, 38070187 (nanoseconds)
actual  : ... 12, 42, 36, 38070000 (microseconds, the column's resolution)
# -> the assertion compares at microsecond precision, which is what timestamptz
#    actually stores.
```

The objects the migration creates were checked directly in the database rather
than assumed from the DDL text:

```text
$ docker exec deploy-db-1 psql -U synapse -d synapse -tAc \
    "select extname||' '||extversion from pg_extension where extname='vector';"
vector 0.8.6

$ docker exec deploy-db-1 psql -U synapse -d synapse -tAc \
    "select indexdef from pg_indexes where schemaname like 'tenant_%' and tablename='memories';" | head -2
CREATE INDEX idx_memories_embedding_hnsw ON tenant_pgs1789908165340163861.memories
  USING hnsw (embedding vector_l2_ops) WITH (m='16', ef_construction='64')
CREATE INDEX idx_memories_session_created ON tenant_pgs1789908165340163861.memories
  USING btree (session_id, created_at DESC)

# information_schema confirms id uuid DEFAULT gen_random_uuid(), agent_id DEFAULT
# 'default', sync_status DEFAULT 'synced', visibility DEFAULT 'org',
# conflict_status DEFAULT 'none', created_at timestamptz DEFAULT now(),
# embedding vector, and the four nullable text/uuid columns.
```

Both full-suite runs stay green, with and without a database:

```text
$ go test ./...                                     # PG tests skip
ok  synapse/internal/api   ok  synapse/internal/budget   ok  synapse/internal/classifier
ok  synapse/internal/compiler   ok  synapse/internal/config   ok  synapse/internal/dedup
ok  synapse/internal/embedder   ok  synapse/internal/integration   ok  synapse/internal/plane
ok  synapse/internal/proxy   ok  synapse/internal/scorer   ok  synapse/internal/store (5.953s)
ok  synapse/internal/supersession   ok  synapse/internal/tenant   ok  synapse/internal/trace

$ SYNAPSE_TEST_DB_DSN=... go test ./...             # PG tests run
ok  synapse/internal/store (7.147s)
ok  synapse/internal/tenant (3.476s)
... every other package unchanged
```

`gofmt -l` lists no new file, `go vet ./internal/store/... ./internal/config/...`
is clean, and `go build ./...` compiles the whole module -- including
`cmd/synapse`, which still calls `store.NewStore(cfg.DBPath)` directly and is
unchanged, because `control-plane-url` defaults to empty.

Next phase: not started. Do not add Phase 6 surface here.

## Phase 6 — cross-tenant isolation test (complete)

Commit `feat: Phase 6 - cross-tenant isolation test`

One test file, no production code: `internal/store` gained the regression test for
the guarantee the rest of the v2 plane assumes -- tenant A's memories live in a
schema tenant B cannot reach. `pgstore.go` (md5 identical to HEAD),
`pgmigrate.go`, `pgread.go`, and `factory.go` are unchanged, and no v1 internal
package was opened.

New files:

- `internal/store/isolation_test.go` (212 lines) — `TestCrossTenantIsolation`
  under `//go:build integration`, plus `isolationPool` and
  `isolationRandomEmbedding`.

Changed:

- `PROGRESS.md` — this entry.

The test, in the order it runs:

1. `NewPGStore(pool, "tenant-alpha")` and `NewPGStore(pool, "tenant-beta")` over
   one shared pool, plus
   `require.NotEqual(alpha.schemaName(), beta.schemaName())`.
2. Five `MemoryEntry` values written to tenant-alpha with known hardcoded
   embeddings — entry *i* is the unit vector on axis *i*, the Phase 5 convention
   — in a session id unique to the run.
3. The query vector is byte-identical to tenant-alpha entry 3 (L2 distance 0), so
   if any read path could reach that row, this is the vector that would find it.
4. `beta.Search(query, sessionID, 10)` must return 0, the same query tenant-wide
   (`""`) must return 0, and then 10 deterministic random 384-dim vectors, each
   checked session-scoped and tenant-wide, must return 0.
5. Non-vacuity control: tenant-alpha's own `Search` for that query must return
   exactly the five memories, with entry 3 first.
6. A final assertion that bypasses `Search` entirely: `SELECT count(*) FROM
   tenant_tenant_beta.memories` must be 0, so the empty results come from an
   empty table rather than from a filter that hides rows.

Decisions made in this phase:

- **Live compose Postgres, not testcontainers-go.** testcontainers-go is not a
  dependency of this module and adding one needs confirmation, so the file is
  build-tagged `integration` and uses the database Phase 4 publishes on
  `127.0.0.1:5432`. CI runs `go test -v ./...` with no tags, so the file is
  invisible there: no database is needed, and no existing test changed behavior.
- **The DSN defaults to the compose string**, which is why the DoD command needs
  no environment variable; `SYNAPSE_TEST_DB_DSN` still overrides it, matching
  `internal/tenant` and the Phase 5 tests.
- **An unreachable database is fatal here, not a skip.** A skipped security test
  reports success without having checked anything — the one failure mode a
  boundary test must not have. The build tag is the opt-in.
- **Both stores share one pool.** That removes pool and connection identity as
  variables, leaving the schema each store resolves as the only thing separating
  them.
- **Fixed slugs, per-run session id.** The slugs are the ones the task names;
  note `tenant-alpha` maps to schema `tenant_tenant_alpha`, because `schemaName()`
  prefixes `tenant_`. tenant-alpha therefore accumulates rows across runs, so
  this run's five memories live under their own session id and the "exactly 5"
  control stays exact. tenant-beta is never written to by anything, so its schema
  stays empty and the tenant-wide assertions stay meaningful on every rerun.
- **Leak assertions compare lengths as values** (`assert.Equal(t, 0, len(got))`)
  rather than using `assert.Len`, so a leak reports `expected: 0, actual: 5`
  instead of dumping five entries and five 384-dim embeddings — a security
  assertion's failure output has to be readable.
- **Deterministic "random" vectors.** `math/rand` seeded with a fixed constant:
  the values are arbitrary, but a failure has to be replayable.
- **`pgstore.go` was edited and reverted twice** — once with the wrong schema
  name, which failed as a missing relation rather than as a leak, which is how
  the `tenant_` prefix was confirmed — to prove the test is sensitive to a broken
  read path. `git status` after reverting shows no production change.

Verification (real output, this phase):

The Phase 4 compose database was already up for this phase (`deploy-db-1`,
pgvector/pgvector:pg16, PostgreSQL 16.15, healthy on 127.0.0.1:5432); only the
`db` service is involved, and no v1 package was opened.

```text
$ gofmt -l internal/store/isolation_test.go        # empty
$ go vet -tags integration ./internal/store/...
VET OK

$ go test ./internal/store/... -run TestCrossTenantIsolation -v -tags integration
=== RUN   TestCrossTenantIsolation
=== RUN   TestCrossTenantIsolation/control:_tenant-alpha_reaches_its_own_memories
=== RUN   TestCrossTenantIsolation/tenant-beta_cannot_reach_tenant-alpha's_memories
=== RUN   TestCrossTenantIsolation/tenant-beta_cannot_reach_them_tenant-wide_either
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_1
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_2
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_3
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_4
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_5
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_6
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_7
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_8
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_9
=== RUN   TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_10
=== RUN   TestCrossTenantIsolation/tenant-beta's_table_is_empty_at_the_storage_layer
--- PASS: TestCrossTenantIsolation (0.21s)
    --- PASS: TestCrossTenantIsolation/control:_tenant-alpha_reaches_its_own_memories (0.00s)
    --- PASS: TestCrossTenantIsolation/tenant-beta_cannot_reach_tenant-alpha's_memories (0.00s)
    --- PASS: TestCrossTenantIsolation/tenant-beta_cannot_reach_them_tenant-wide_either (0.00s)
    --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta (0.01s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_1 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_2 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_3 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_4 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_5 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_6 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_7 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_8 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_9 (0.00s)
        --- PASS: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta/query_10 (0.00s)
    --- PASS: TestCrossTenantIsolation/tenant-beta's_table_is_empty_at_the_storage_layer (0.00s)
PASS
ok  	synapse/internal/store	0.212s
```

The DoD also requires that the test fails when isolation is deliberately broken,
so it was broken on purpose: `PGStore.Search` was pointed at
`"tenant_tenant_alpha"."memories"` instead of the store's own schema, which is
exactly the "Search queries the wrong schema" case, and reverted with
`git checkout -- internal/store/pgstore.go` before this commit.

```text
$ git diff internal/store/pgstore.go
-		pgColumns, s.table(), where, len(args)+1)
+		pgColumns, `"tenant_tenant_alpha"."memories"`, where, len(args)+1)

$ go test ./internal/store/... -run TestCrossTenantIsolation -v -tags integration
    isolation_test.go:177:
        Error:   Not equal:
                 expected: 0
                 actual  : 5
        Messages: same session id, same embedding as a tenant-alpha memory: tenant-beta must still find nothing
    isolation_test.go:184:
        Error:   Not equal:
                 expected: 0
                 actual  : 10
        Messages: an empty session widens the search to the whole tenant, which is still only tenant-beta
    # query_1 .. query_10: the same two failures, "expected: 0, actual: 5"
    # session-scoped and "expected: 0, actual: 10" tenant-wide (topK is 10).
--- FAIL: TestCrossTenantIsolation (0.16s)
    --- PASS: TestCrossTenantIsolation/control:_tenant-alpha_reaches_its_own_memories (0.00s)
    --- FAIL: TestCrossTenantIsolation/tenant-beta_cannot_reach_tenant-alpha's_memories (0.00s)
    --- FAIL: TestCrossTenantIsolation/tenant-beta_cannot_reach_them_tenant-wide_either (0.00s)
    --- FAIL: TestCrossTenantIsolation/random_queries_return_nothing_from_tenant-beta (0.01s)
    --- PASS: TestCrossTenantIsolation/tenant-beta's_table_is_empty_at_the_storage_layer (0.00s)
FAIL
FAIL	synapse/internal/store	0.170s
```

Three things about that output are the point of the design: the control still
passes, because tenant-alpha genuinely reads its own schema -- the break is a leak
and not a crash, which is the realistic failure mode; every tenant-beta assertion
trips, so the test is sensitive to exactly the isolation claim it documents; and
the raw row count against `tenant_tenant_beta` still reports 0, because the leak is
read-side, which is what shows the storage-layer assertion measures something the
`Search` assertions cannot.

The untagged suite -- the path CI runs -- is untouched, because the file is not
compiled without the tag:

```text
$ go test ./... -count=1
?   	synapse/cmd/benchmark	[no test files]
?   	synapse/cmd/counttokens	[no test files]
?   	synapse/cmd/mergesessions	[no test files]
?   	synapse/cmd/plane	[no test files]
?   	synapse/cmd/synapse	[no test files]
ok  	synapse/internal/api	1.073s
ok  	synapse/internal/budget	0.430s
ok  	synapse/internal/classifier	0.021s
ok  	synapse/internal/compiler	0.525s
ok  	synapse/internal/config	0.018s
ok  	synapse/internal/dedup	0.014s
ok  	synapse/internal/embedder	4.822s
ok  	synapse/internal/integration	5.388s
ok  	synapse/internal/plane	0.020s
ok  	synapse/internal/proxy	0.206s
?   	synapse/internal/retrieval	[no test files]
ok  	synapse/internal/scorer	0.020s
?   	synapse/internal/session	[no test files]
ok  	synapse/internal/store	2.489s
ok  	synapse/internal/supersession	0.005s
ok  	synapse/internal/tenant	0.791s
ok  	synapse/internal/trace	0.150s
exit=0

$ env -u SYNAPSE_TEST_DB_DSN go test ./internal/store/... -v -count=1
--- SKIP: TestPGStore (0.00s)
--- SKIP: TestPGStoreRejectsInvalidSlug (0.00s)
--- PASS: TestSanitize (0.00s)
--- PASS: TestWriteWithSanitization (0.00s)
--- PASS: TestEmbeddingRoundTrip (0.00s)
--- PASS: TestSearchRanksBySimilarity (0.00s)
--- PASS: TestSchemaMigrationAddsSupersededByColumn (0.80s)
--- PASS: TestSupersededByRoundTrip (0.00s)
--- PASS: TestMarkSuperseded (0.00s)
--- PASS: TestMarkSupersededNonexistentID (0.00s)
--- PASS: TestMarkSupersededEmptyArgs (0.00s)
--- PASS: TestSyncStatusMigrationAndRoundTrip (0.54s)
ok  	synapse/internal/store	1.356s

$ go build ./... && go vet ./internal/store/...
BUILD OK (untagged)
VET OK (untagged)

$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
      go test -tags integration ./internal/store/... -count=1
ok  	synapse/internal/store	1.836s      # Phase 5's tests and this one together
```

The rows this phase wrote are left in place, exactly as Phase 5 left its own
schemas: this run's assertions are scoped to its own session id, and tenant-beta's
schema is empty, so the test stays green on reruns. Checked against the live
database:

```text
$ docker exec deploy-db-1 psql -U synapse -d synapse -tAc \
    "select nspname from pg_namespace where nspname in ('tenant_tenant_alpha','tenant_tenant_beta');"
tenant_tenant_alpha
tenant_tenant_beta
```

Next phase: not started. This phase added no production code; the rest of the v2
surface (sync, conflict, ledger, mcp, metering, billing) is untouched.


## Phase 7 — sync config fields and startup validation (complete)

Commit `feat: Phase 7 - sync config fields and startup validation`

Configuration only. This phase defines the fields the sync protocol will read, and
the one precondition that keeps a half-configured edge node from ever booting.
There is no sync logic, no network call, and no new dependency: nothing here sends
a memory anywhere, and the standalone path behaves exactly as it did in Phase 6.
`internal/store` was not opened at all — see the item 4 decision below for why it
did not need to be.

Changed:

- `internal/config/config.go` — the `EnvPlaneKey` constant, six new `Config`
  fields (`ControlPlaneAPIKey`, `AgentID`, `TeamID`, `DefaultVisibility`,
  `SyncBatchSize`, `SyncIntervalSeconds`), the matching `DefaultConfig()` values,
  and one doc block covering all six. `Validate()` is untouched.
- `cmd/synapse/main.go` — twelve added lines: one comment block, one guard.
  Nothing else in the file was modified.
- `synapse.yaml.example` — the new keys, each with the environment variable that
  supplies it, one comment line per key.
- `PROGRESS.md` — this entry.

The full key set, including the key that already existed:

| yaml key | Field | Default | Notes |
| --- | --- | --- | --- |
| `control-plane-url` | `ControlPlaneURL` | `""` | already present since Phase 5 |
| `control-plane-api-key` | `ControlPlaneAPIKey` | env `SYNAPSE_PLANE_KEY` | secret: never logged |
| `agent-id` | `AgentID` | `""` | required once the URL is set |
| `team-id` | `TeamID` | `""` | optional; empty means tenant-wide |
| `default-visibility` | `DefaultVisibility` | `"org"` | |
| `sync-batch-size` | `SyncBatchSize` | `20` | |
| `sync-interval-seconds` | `SyncIntervalSeconds` | `30` | |

Because `loadConfig` copies `DefaultConfig()` and unmarshals the file over it, a
config that omits the last three keys still gets `"org"` / `20` / `30` — no loader
change was needed to make the defaults reachable from a partial file.

Decisions made in this phase:

- **The guard is in `main.go`, not in `Validate()`.** Right after `loadConfig`, so
  it is the earliest failure the process can have: before the tiktoken warm-up,
  the flag overrides, `store.NewStore`, embedder init, and the port bind. A node
  that is pointed at a plane but never names itself therefore cannot write a local
  row that would later need re-attributing. Keeping it out of `Validate()` also
  keeps the standalone validation contract exactly as it was, and gives the
  precondition a single enforcement point and a single message.
- **`slog.Error` + `os.Exit(1)`, not stdlib `log.Fatal`.** Every other fatal
  startup path in this file already uses that idiom, and `log/slog` is the file's
  logger with charmbracelet/log installed as the default handler. Importing
  `log` would print this one line in a second, unprefixed format. The message
  string itself is verbatim as specified.
- **Item 4 needed no work.** `MemoryEntry.SyncStatus` (`store.go:30`), the
  `SyncStatusLocalOnly` / `SyncStatusSyncPending` / `SyncStatusSynced` constants
  (`pgstore.go:26-28`), the SQLite migration (`store.go:154`), and
  `sync_status_test.go` all landed in Phase 5. `internal/store` was left
  byte-identical, confirmed by `git status`.
- **`ADD COLUMN IF NOT EXISTS` is not SQLite.** The task's SQL is not accepted by
  any SQLite version — verified on this machine (transcript below). Phase 5's
  unconditional `ALTER TABLE memories ADD COLUMN sync_status TEXT NOT NULL DEFAULT
  'local_only'` that swallows only "duplicate column name" is the equivalent, it
  is strictly stronger than the requested nullable form, and
  `TestSyncStatusMigrationAndRoundTrip` covers the migrate-in-place path.
- **Zero-deletion diff.** The new struct fields and the new `DefaultConfig()`
  values are appended past the existing alignment groups with a comment separating
  them, so `gofmt` realigns none of the lines that were already there: +66 / -0.
  `gofmt -l` is dirty repo-wide and was dirty before this phase (e.g.
  `path/filepath` import order in `cmd/synapse/main.go`), so `gofmt -w` was
  deliberately *not* run; `gofmt -d` was used instead to confirm it wants to change
  none of the added lines.
- **`bin/synapse` was left alone.** It is a tracked 16.5 MB v1 build artifact
  (`371186d`), while this project's build convention is
  `go build -o synapse ./cmd/synapse` into the gitignored `/synapse`
  (README.md:157, setup.sh:181, ci.yml:117) — Phase 1 already recorded the wart.
  So the DoD run used `./synapse`, and `bin/synapse` is byte-identical to HEAD
  (md5 matched against `git cat-file blob`). Recommended follow-up, as its own
  `chore:` commit: add `/bin/synapse` to `.gitignore` and `git rm --cached` it.
- **No dependency was added.** The `.clinerules` confirmation rule does not apply
  here; the six fields need nothing beyond stdlib and the go-yaml already in use.

Verification (real output, this phase):

```text
$ go build ./cmd/synapse && go build ./... && go vet ./internal/config/... ./cmd/synapse/...
BUILD OK
VET OK

$ go test ./internal/config/... ./internal/store/... -count=1
ok  	synapse/internal/config	0.004s
ok  	synapse/internal/store	0.875s

$ ./synapse --config /tmp/synapse-phase7/no-agent.yaml
# control-plane-url set, agent-id absent
1:04PM ERRO synapse: agent-id is required when control-plane-url is set
exit=1

$ env -u OPENAI_API_KEY ./synapse --config /tmp/synapse-phase7/with-agent.yaml
# identical config plus agent-id: "edge-01"
1:04PM ERRO synapse: Invalid configuration error="openai-api-key is required when using OpenAI embedder"
exit=1
```

The second run is the negative control, and it is what makes the first one
evidence: with `agent-id` supplied, the guard does not fire and the process moves
on to the next check, so the first failure came from the guard rather than from
something incidental. That config deliberately pairs `embedder-type: "openai"` with
no key, so the next check is a cheap validation error — the control opens no
database, loads no model, and binds no port.

The new yaml tags, defaults, and env seeding, checked by a throwaway test that was
deleted immediately after this run so the committed diff stays at the requested
items (a typo'd yaml tag fails silently — the key simply never maps):

```text
$ go test ./internal/config/ -run TestPhase7SyncFieldsTemporary -v -count=1
=== RUN   TestPhase7SyncFieldsTemporary
    phase7_verify_test.go:31: defaults OK: default-visibility="org" sync-batch-size=20 sync-interval-seconds=30
    phase7_verify_test.go:33: env seeding OK: SYNAPSE_PLANE_KEY -> ControlPlaneAPIKey="env-key"
    phase7_verify_test.go:71: yaml tags OK: agent-id="edge-01" team-id="team-blue" default-visibility="team" sync-batch-size=7 sync-interval-seconds=90 control-plane-api-key="file-key"
--- PASS: TestPhase7SyncFieldsTemporary (0.00s)
ok  	synapse/internal/config	0.004s
```

SQLite's refusal of the SQL spelled in the task, on this machine:

```text
$ sqlite3 :memory: "CREATE TABLE t(a); ALTER TABLE t ADD COLUMN IF NOT EXISTS b TEXT DEFAULT 'x';"
Error: in prepare, near "EXISTS": syntax error
  ALTER TABLE t ADD COLUMN IF NOT EXISTS b TEXT DEFAULT 'x';
                    error here ---^
```

Final tree state:

```text
$ git status --short
 M cmd/synapse/main.go
 M internal/config/config.go
 M synapse.yaml.example

$ git diff --stat
 cmd/synapse/main.go       | 12 ++++++++++++
 internal/config/config.go | 37 +++++++++++++++++++++++++++++++++++++
 synapse.yaml.example      | 17 +++++++++++++++++
 3 files changed, 66 insertions(+)

$ md5sum bin/synapse; git cat-file blob HEAD:bin/synapse | md5sum
36a53be7c56dae45979707be7397fa67  bin/synapse
36a53be7c56dae45979707be7397fa67  -
```

Next phase: the sync protocol itself. These fields are inert until something reads
them — nothing in this phase dials a control plane, and `ControlPlaneURL` still
does exactly what it did in Phase 5: it selects the store backend in
`internal/store/factory.go` and nothing else. Do not add sync, conflict, ledger,
mcp, metering, or billing surface here.

## Phase 8 — syncer Push and background flush (complete)

Commit `feat: Phase 8 - syncer Push and background flush`

The edge node can now send memories to a control plane, and the control plane can
store them: `Syncer.Push` posts one batch to `POST /v2/sync/memories`, a
background flusher drains the local `sync_pending` queue every
`sync-interval-seconds` in batches of `sync-batch-size`, and the plane
acknowledges a batch it has stored into the token's own tenant schema. Nothing in
the compilation hot path calls any of it (Phase 9 marks the rows).

New files:

| File | Contents |
| --- | --- |
| `internal/sync/syncer.go` | `Syncer` (`cfg`, 5s `*http.Client`, interval, batch size, backlog thresholds), `NewSyncer`, `Push`, `pushRequest`, `endpoint`, `sharedSessionID`, `logStart` |
| `internal/sync/background.go` | `PendingStore` interface, `RunBackground`, `flush`, `syncMaxBatchesPerFlush` |
| `internal/sync/syncer_test.go` | 13 tests: envelope/header assertions, non-2xx and transport errors, credential never leaked, batch draining, mark-synced round trip, keep-pending on refusal, backlog warn/abandon, acknowledgement failure, run-and-cancel |
| `internal/store/syncqueue.go` | `Sanitize` (exported wrapper), `PendingSync`, `MarkSynced`, `CountPendingSync`, `DropOldestPendingSync` |
| `internal/store/syncqueue_test.go` | 4 tests against a real SQLite file store |
| `internal/plane/sync.go` | `MemoryWriter` interface, `syncRequest`/`syncResponse`, `handleSyncMemories`, `requireJWT`, `WithTenantSlug`/`TenantSlugFromCtx`, `maxSyncBodyBytes` |
| `internal/plane/sync_test.go` | 6 tests (one with 9 subtests) through the real router and the real tenant JWT verifier |
| `internal/tenant/memorywriter.go` | `MemoryWriter.WriteBatch`, per-tenant `PGStore` cache, `memoryUUID` (UUIDv5), `embeddingWithinColumnWidth` |
| `internal/tenant/memorywriter_test.go` | 3 database-free tests for the id/embedding mapping and the constructor contract |

Changed:

- `internal/plane/handlers.go` — `Server` gains `memories MemoryWriter` and
  `auth func(http.Handler) http.Handler`; `NewServer` takes both (2 new
  parameters); `Routes()` registers the sync route behind `requireJWT`;
  `decodeJSON` is split into a body-limited `decodeJSONLimit` plus the original
  4 KiB wrapper, because a sync batch carries whole memories and a provisioning
  body is two strings.
- `internal/plane/handlers_test.go` — one line (`newRouter` passes `nil, nil`).
- `internal/tenant/auth.go` — `withClaims` also publishes the verified slug
  through `plane.WithTenantSlug` (one line plus a comment). Same value, second
  accessor, no claim re-derivation.
- `cmd/plane/main.go` — wires `tenant.NewMemoryWriter(pool)` and
  `tenant.JWTMiddleware(cfg)` into `plane.NewServer`.
- `cmd/synapse/main.go` — 17 added lines: when `control-plane-url` is set, a
  process-owned context plus `go syncer.RunBackground(syncCtx, storeInstance)`.
  The hot path is untouched.
- `deploy/Dockerfile.plane` — corrected comment: the plane's import graph now
  reaches `internal/store`, whose `go-sqlite3` dependency compiles under
  `CGO_ENABLED=0` through its `!cgo` stub. No SQLite database is ever opened by
  that binary. Verified below.
- `synapse.yaml.example` — the `control-plane-api-key` comment now says what the
  sync route actually verifies today (a tenant JWT).
- `PROGRESS.md` — this entry.

Interface changes (`Push`, `RunBackground` signature, route, response body) match
the phase specification. Nothing in v1 was modified: `git status` lists only the
two new `internal/store/syncqueue*.go` files for that package.



Decisions made in this phase:

- **Backlog overflow is abandoned, never deleted.** Over 10000 pending rows, the
  oldest beyond the limit are marked `local_only` — they stay readable locally,
  stop being retried, and the flusher logs an ERROR naming how many. Deleting
  rows would mean an unreachable plane quietly destroying memories. (`keep` rows
  survive; the SQL enumerates the queue newest-first and skips exactly those,
  which is why the order inside the subquery is `DESC` — the ascending version
  abandons the *newest* rows instead. A unit test caught that before the commit.)
- **`internal/store` gained a new file, not an edit.** The four accessors are
  additive; `store.go`, `pgstore.go`, and the v1 read/write paths are
  byte-identical. The alternative — SQL in `internal/sync` — is impossible: the
  store's `db` handle is unexported.
- **`RunBackground(ctx, PendingStore)` instead of the concrete `*store.Store`.**
  The declared interface is satisfied structurally by `*store.Store`, so the
  specified call site (`go syncer.RunBackground(ctx, store)`) is unchanged, and
  the flusher is testable without a database.
- **No index on `sync_status`.** Adding one means editing the v1 schema
  initialiser; a scan of a local memory table at these sizes is cheap, and it is
  noted in the code for a later phase.
- **The queue read orders by `timestamp, id`.** `time.Now()` collides, and two
  flushes must not disagree about which row is oldest.
- **One batch is one request, and a batch is all-or-nothing on the plane.** The
  edge marks exactly what it pushed as synced on a 2xx; a 200 that quietly
  skipped a memory would turn it into a permanent loss. Write failures answer
  500, the rows stay pending, and the WARN says so.
- **Idempotency is what makes retries safe.** The plane maps a non-uuid edge id
  (the local path names memories `req-<nanos>`) through a fixed UUIDv5 namespace,
  so a re-pushed batch lands on the same rows (`ON CONFLICT (id) DO NOTHING`); a
  push that succeeds but fails to be acknowledged locally costs redundant
  traffic, not duplicate memories. `superseded_by` is mapped the same way.
- **A wrong-width embedding is dropped, not fatal.** The tenant column is
  `vector(384)`; one bad vector would fail the insert and make the batch
  un-pushable forever. The memory is stored without an embedding and a WARN names
  the id and width.
- **Sanitization and storage stay single-sourced.** The plane handler delegates
  to `MemoryWriter.WriteBatch`, whose store-backed implementation calls
  `store.Sanitize` and `store.PGStore.Write`; `sanitized` counts memories whose
  content the sanitizer rewrote. No second pattern list exists anywhere.
- **The sync route fails closed without a middleware.** `requireJWT` answers 401
  for every request when `auth` is nil, the same shape `requireAdmin` uses for an
  unset admin token.
- **The tenant comes from the token only.** The body has no field that could name
  a tenant, and the handler rejects a request whose context carries no slug, so
  schema-per-tenant isolation does not depend on request content.
- **`agent_id` is required (400 `invalid_agent`).** The edge refuses to boot
  without one, so an unattributed push is a contract violation, not a normal path.
- **Backlog thresholds and batch shape are `Syncer` fields,** set from config and
  the package constants in `NewSyncer`, so tests can shrink them without package
  level mutable state. A zero `sync-interval-seconds` falls back to 30s rather
  than panicking `time.NewTicker` inside a goroutine.
- **The first flush happens immediately, then on each tick.** A memory written
  just before startup should not wait out an interval, and a failed batch is
  retried next tick rather than in a loop.
- **A missing `control-plane-api-key` is a local error**, not an unauthenticated
  request every interval.

Limitations this phase deliberately did not close:

- **Nothing marks a memory `sync_pending` yet.** `store.Write` already accepts
  and round-trips the status (Phase 5, `sync_status_test.go`), but the write path
  does not set it — that is Phase 9's work. On a fresh node the flusher therefore
  finds an empty queue; the tests seed the queue directly.
- **The edge's `control-plane-api-key` must be the tenant JWT today.** Phase 3
  issues an opaque API key *and* a JWT; only the JWT is verifiable by a middleware
  that exists, and the specification for this phase said "requires JWT auth
  middleware". Opaque-key verification on the plane is a later phase.
- **Pushed memories are stored with the tenant table's defaults for
  attribution.** `PGStore.Write` predates the sync path and sets neither
  `agent_id` (`'default'`), `team_id`, nor `visibility` (`'org'`); the wire body
  for this phase carries only `session_id`, `agent_id`, and `memories`, and the
  `agent_id` sent is used for logging and validation. Persisting it needs an
  additive write path, which is not this phase's scope.
- **`control-plane-url` is still not format-validated** in `Config.Validate`
  (unchanged from Phase 7). `Push` trims a trailing slash and reports an
  unbuildable URL as a wrapped transport error.

Verification (real output, this phase):

```text
$ gofmt -l internal/sync internal/plane/sync.go internal/plane/sync_test.go \
    internal/tenant/memorywriter.go internal/tenant/memorywriter_test.go \
    internal/store/syncqueue.go internal/store/syncqueue_test.go cmd/plane/main.go
(no output -- every new or touched file is gofmt-clean; the pre-existing
 unformatted v1 files were left alone, confirmed by comparing against HEAD)

$ go vet ./...
(no output)

$ CGO_ENABLED=0 go build -o /dev/null ./cmd/plane && echo 'CGO_ENABLED=0 PLANE BUILD OK'
CGO_ENABLED=0 PLANE BUILD OK

$ go test ./internal/sync/... -count=1 -v
=== RUN   TestNewSyncerUsesConfiguredValuesAndGuardsZeroes
--- PASS: TestNewSyncerUsesConfiguredValuesAndGuardsZeroes (0.00s)
=== RUN   TestPushSendsOneRequestWithEveryMemory
--- PASS: TestPushSendsOneRequestWithEveryMemory (0.00s)
=== RUN   TestPushMixedSessionsSendsAnEmptyEnvelopeSession
--- PASS: TestPushMixedSessionsSendsAnEmptyEnvelopeSession (0.00s)
=== RUN   TestPushRejectsNon2xxWithoutLeakingTheCredential
--- PASS: TestPushRejectsNon2xxWithoutLeakingTheCredential (0.00s)
=== RUN   TestPushWrapsATransportError
--- PASS: TestPushWrapsATransportError (0.00s)
=== RUN   TestPushWithoutCredentialOrEntries
--- PASS: TestPushWithoutCredentialOrEntries (0.00s)
=== RUN   TestFlushMarksEverythingThePlaneAccepted
--- PASS: TestFlushMarksEverythingThePlaneAccepted (0.34s)
=== RUN   TestFlushLeavesMemoriesPendingWhenThePlaneRefuses
--- PASS: TestFlushLeavesMemoriesPendingWhenThePlaneRefuses (0.27s)
=== RUN   TestFlushDrainsInBatchesOfTheConfiguredBatchSize
--- PASS: TestFlushDrainsInBatchesOfTheConfiguredBatchSize (0.30s)
=== RUN   TestFlushAbandonsTheOldestWhenTheBacklogIsOverTheHardLimit
--- PASS: TestFlushAbandonsTheOldestWhenTheBacklogIsOverTheHardLimit (0.35s)
=== RUN   TestFlushWarnsOnALargeBacklogBelowTheHardLimit
--- PASS: TestFlushWarnsOnALargeBacklogBelowTheHardLimit (0.26s)
=== RUN   TestFlushReportsAnAcknowledgementFailure
--- PASS: TestFlushReportsAnAcknowledgementFailure (0.00s)
=== RUN   TestRunBackgroundFlushesUntilTheContextIsCancelled
--- PASS: TestRunBackgroundFlushesUntilTheContextIsCancelled (0.40s)
PASS
ok  	synapse/internal/sync	1.928s

$ go test ./... -count=1
ok  	synapse/internal/api	3.403s
ok  	synapse/internal/budget	0.262s
ok  	synapse/internal/classifier	0.009s
ok  	synapse/internal/compiler	0.248s
ok  	synapse/internal/config	0.006s
ok  	synapse/internal/dedup	0.010s
ok  	synapse/internal/embedder	2.973s
ok  	synapse/internal/integration	5.383s
ok  	synapse/internal/plane	0.051s
ok  	synapse/internal/proxy	0.338s
ok  	synapse/internal/scorer	0.009s
ok  	synapse/internal/store	9.421s
ok  	synapse/internal/supersession	0.005s
ok  	synapse/internal/sync	8.880s
ok  	synapse/internal/tenant	0.798s
ok  	synapse/internal/trace	0.161s

$ go test -race ./internal/sync/... -run 'TestFlush|TestRunBackground' -count=1
ok  	synapse/internal/sync	3.904s
```

The Postgres-backed tests (`internal/store`, `internal/tenant`) skip without
`SYNAPSE_TEST_DB_DSN`, as before: the new plane-side writer is covered by its
database-free mapping tests plus the endpoint tests against a fake writer, so no
database was required for this phase's evidence.

Live end-to-end smoke test — the real `synapse` binary, a real SQLite store with
three rows marked `sync_pending`, and a stub plane on 127.0.0.1:19099:

```text
$ ./synapse-phase8 --config /tmp/phase8.yaml      # control-plane-url: "http://127.0.0.1:19099/"
INFO synapse: sync: background flusher started control_plane_url=http://127.0.0.1:19099/v2/sync/memories agent_id=edge-smoke-1 interval_seconds=2 batch_size=5
WARN synapse: sync: push failed, memories stay pending for the next attempt pending=3 error="sync: push request: Post \"http://127.0.0.1:19099/v2/sync/memories\": dial tcp 127.0.0.1:19099: connect: connection refused"

$ # what the stub plane recorded on the next interval:
path=/v2/sync/memories bearer_scheme=True agent_id=edge-smoke-1 session_id=smoke-session memories=3

$ sqlite3 /tmp/phase8.db "SELECT id, sync_status FROM memories ORDER BY id;"
req-1|synced
req-2|synced
req-3|synced

$ grep -c 'smoke-plane-key-must-not-be-logged' /tmp/phase8-run.log
0

$ # a second run with two more pending memories and log-level debug:
DEBU synapse: sync: pushed memories count=2

$ sqlite3 /tmp/phase8.db "SELECT id, sync_status FROM memories ORDER BY id;"
req-1|synced  req-2|synced  req-3|synced  req-4|synced  req-5|synced
```

That transcript is the retry contract working: the first flush could not reach
the plane, the rows stayed pending, the next interval pushed them once, the stub
saw exactly one batch of three, and the rows read back `synced`. The configured
`control-plane-url` ended in a slash and the request still went to
`/v2/sync/memories` — not `//v2/sync/memories` — and the API key appears nowhere
in the log.

Next phase, as Phase 8 predicted it, was to mark memories `sync_pending` on the
write path. That work was reordered: **Phase 9 turned out to be the read half of
the protocol** (`PullCandidates` and the offline fallback, below), and the
`sync_pending` marking is still unbuilt. Until it lands, this phase's flusher
runs, finds nothing, and sleeps — which is why the endpoint and the queue are
tested by seeding the queue directly.

## Phase 9 — PullCandidates and offline fallback (complete)

Commit `feat: Phase 9 - PullCandidates and offline fallback`

The edge node can now read from the control plane, not just write to it. Before
scoring, `retrieval.Candidates` asks the plane for org-scoped candidates over
`GET /v2/memories/search`, under a hard **200ms** ceiling; if the plane is slow,
refusing, or gone, it logs a WARN and searches local sqlite-vec exactly as it did
in v1. With no `control-plane-url`, nothing about retrieval changes at all.

New files:

| File | Contents |
| --- | --- |
| `internal/sync/pull.go` | `searchPath`, `pullTimeout` (200ms), `maxPullBodyBytes`, `searchRequest`/`searchResponse`, `Syncer.PullCandidates`, `searchEndpoint` |
| `internal/plane/search.go` | `searchRoute`, `MemorySearcher` interface, `searchRequest`/`searchResponse`, `handleSearchMemories`, `maxSearchBodyBytes`, `maxSearchTopK` |
| `internal/sync/syncer_test.go` | 5 new tests plus a search stub (`searchRecord`, `newSearchStub`, `nextSearch`) |
| `internal/retrieval/retrieval_test.go` | 7 tests (one with 3 fallback subtests) and doubles for the store, the embedder, and the plane |
| `internal/plane/search_test.go` | 7 tests (one with 5 auth subtests, one with 6 malformed-body subtests) |

Changed:

- `internal/sync/syncer.go` — package doc corrected: the claim that "nothing in
  this package is called from a request path" is no longer true. The pull
  constants and code live in `pull.go` because `syncer.go` reached 308 lines with
  them inline (the 300-line cap; same split as `plane/handlers.go` → `sync.go`).
- `internal/store/store.go` — `MemoryEntry.AgentID` (`json:"agent_id,omitempty"`).
  Additive, like Phase 5's `SupersededBy`/`SyncStatus`; the SQLite backend has no
  agent column and leaves it empty.
- `internal/store/pgstore.go` — `PGStore.Write` inserts `agent_id`, blank →
  `'default'` (new `defaultAgentID` const), matching the column's own default.
- `internal/store/pgread.go` — `pgColumns` and `scanEntry` carry `agent_id`.
- `internal/store/pgstore_test.go` — new subtest: a named agent round-trips and a
  blank one reads back as `default`.
- `internal/plane/handlers.go` — `Server.searcher`, one more `NewServer`
  parameter, and `GET /v2/memories/search` registered behind `requireJWT`.
- `internal/plane/sync.go` — `handleSyncMemories` sets `memory.AgentID` from the
  envelope (a memory that names its own agent keeps it).
- `internal/tenant/memorywriter.go` — `MemoryWriter.Search` (reuses the
  per-tenant `PGStore` cache `WriteBatch` built), plus a second interface
  assertion so a signature drift is a build failure.
- `internal/retrieval/retrieval.go` — `PlaneCandidates` interface; `Candidates`
  takes the source and asks the plane first; WARN + local fallback on any error.
- `internal/proxy/proxy.go`, `internal/api/api.go` — a `plane` field, a
  `SetPlaneCandidates` setter, and one extra argument at the `Candidates` call
  site. This is the only v1 code this phase edits beyond `internal/retrieval`
  (approved explicitly: `internal/api` stores a concrete `*store.Store`, so no
  design could reach a live `/v1/compile` without one line there).
- `internal/plane/handlers_test.go`, `internal/plane/sync_test.go` — `nil` for the
  new `NewServer` parameter (3 lines).
- `cmd/plane/main.go` — one `tenant.NewMemoryWriter(pool)` serves as both the
  writer and the searcher.
- `cmd/synapse/main.go` — the syncer is hoisted to a variable and installed on
  both servers when `control-plane-url` is set; a standalone node leaves both
  sources nil.
- `synapse.yaml.example` — the credential now covers both routes.
- `PROGRESS.md` — this entry.

Decisions made in this phase:

- **The 200ms ceiling is layered on the caller's context, not substituted for
  it.** `PullCandidates` does `context.WithTimeout(ctx, pullTimeout)`, so a
  request that is already cancelled stays cancelled, and the pull can never
  outlive its own budget. A timeout reaches the caller as
  `context.DeadlineExceeded` through `%w`, so a fallback can distinguish "never
  answered" from "answered 503" with `errors.Is` instead of string matching.
- **The compilation SLA is untouched because the ceiling is the plane's, not the
  compile's.** The local path is the same `store.Search` call it always was, and
  the fallback adds no retry, no second attempt, and no backoff: measured live,
  the offline compile returned in 75ms including the failed pull.
- **A plane answer *is* the candidate set — empty included.** The local store is
  not consulted and the two sets are not merged. A merged set would make the
  compiled context depend on which node happened to build it, and the plane is
  the org's record. (The consequence is recorded under limitations.)
- **A missing URL or credential is a local error, not a request.** Without it,
  every compile on a misconfigured node would make an unauthenticated round trip
  and log a 401 forever; `Push` already made this choice.
- **`agent_id` had to be persisted for the response to mean anything.** The
  tenant column existed (`NOT NULL DEFAULT 'default'`) but `PGStore.Write` never
  set it and the projection never read it, so a search result would have been
  unattributable. A blank value is now written as `'default'` rather than as an
  empty string: the column stays `NOT NULL`, and there is exactly one spelling of
  "unattributed" instead of two.
- **`AgentID` is additive on `store.MemoryEntry`,** following Phase 5's precedent
  for `SupersededBy`/`SyncStatus`. SQLite has no agent column and scans it empty,
  so no v1 file's schema changed and no existing test's expectation moved.
- **`PlaneCandidates` lives in `internal/retrieval` and is satisfied
  structurally** by `*sync.Syncer`. retrieval therefore imports neither
  `internal/sync` nor `internal/config`, stays mockable with a three-field
  double, and the edge's HTTP client remains a detail of the binary that wires
  it up.
- **A setter, not a constructor parameter, on both v1 servers.** `NewProxy` and
  `NewAPIServer` are called from 2 and 13 test sites respectively; a setter means
  none of them changed, and "no plane" is expressed by omission rather than by an
  argument whose nil-ness a caller has to get right.
- **The plane's read half is its own interface and its own `NewServer`
  parameter.** A plane can accept pushes without serving reads, and each endpoint
  then degrades on its own; `cmd/plane` passes one `tenant.MemoryWriter` for both
  roles so the read reuses the writer's per-tenant `PGStore` cache instead of
  opening a second store (and a second pool) per tenant.
- **Validation returns the status the caller can act on.** A wrong-width
  embedding is a 400 (`invalid_embedding`) rather than the 500 the store would
  have produced for a client-side mistake; an empty embedding is *allowed*
  because both backends answer it with recency ordering; `top_k > 500` is a 400
  so no caller can ask one request to drag a whole tenant schema across the wire;
  unknown JSON fields are a 400 as everywhere else in this plane.
- **The fallback is silent to the caller and loud in the log.** The compile never
  fails because of the plane, so `plane_unavailable=true fallback=local` is the
  only observable signal — which is why it is asserted in tests as a literal
  string, and why the error is logged (it carries no credential by construction).
- **`pull.go` and `search.go` exist because of the 300-line cap,** not because
  either half is independently useful: `syncer.go` would have been 308 lines and
  `handlers.go` 300+ with the new code inline. Both splits mirror an existing
  precedent in the same packages.

Limitations this phase deliberately did not close:

- **A reachable plane means local memories are not consulted at all.** This is
  the specified behavior ("if success: use the plane results"), and it has a
  sharp consequence: a node pointed at an *empty* plane compiles with no memories
  even though its own SQLite store has rows. Merging the two sets is a product
  decision, not an oversight, and is left for a phase that can reason about
  ranking across sources.
- **`sync_pending` marking is still unbuilt,** so an edge node cannot populate
  the plane by itself yet. The live verification below therefore seeds the plane
  through `POST /v2/sync/memories` — the same route the edge's own flusher uses.
- **Supersession can select a plane-sourced candidate.** `FindSupersededCandidate`
  runs over whatever candidates retrieval returned, so a plane uuid can reach
  `MarkSuperseded` on the local store, which reports "no memory found with id
  …". That is logged, not fatal, and pre-dates this phase's read path.
- **The plane still verifies a tenant JWT, not the opaque API key**, so
  `control-plane-api-key` must be the jwt returned at provisioning. Both the
  push and the new pull route are behind the same middleware.
- **Success costs latency; the 200ms is only a ceiling on failure.** A local
  plane with one row answered in 123ms total, versus 75ms offline — the compile
  now waits for the network on the happy path, which is the trade the phase
  specifies.
- **The trace carries counts, not provenance.** `MemoryTrace` has no "which side
  answered" field, so the fallback is visible in logs rather than in the trace
  inspector; adding it would mean editing `internal/trace` (v1).
- **`internal/proxy/proxy.go`, `internal/api/api.go`, `cmd/synapse/main.go` (and
  `internal/store/store.go`) remain `gofmt -l` candidates** — as they already
  were at HEAD (73, 383, 94 and 217 diff lines). The added lines follow the
  surrounding style rather than reformatting v1 code wholesale, which is the same
  choice Phase 5 recorded.

Verification (real output, this phase):

```text
$ gofmt -l internal/sync/pull.go internal/sync/syncer.go internal/sync/syncer_test.go \
    internal/plane/search.go internal/plane/search_test.go internal/plane/handlers.go \
    internal/plane/sync.go internal/plane/handlers_test.go internal/plane/sync_test.go \
    internal/retrieval/retrieval.go internal/retrieval/retrieval_test.go \
    internal/tenant/memorywriter.go internal/store/pgread.go internal/store/pgstore.go \
    internal/store/pgstore_test.go internal/plane/provision.go
(no output -- every new file and every line added to a clean file is gofmt-clean)
$ gofmt -l internal/proxy/proxy.go internal/api/api.go cmd/synapse/main.go internal/store/store.go
internal/proxy/proxy.go
internal/api/api.go
cmd/synapse/main.go
internal/store/store.go
(already unformatted at HEAD: 73, 383, 94 and 217 diff lines respectively)

$ go vet ./...
(no output)

$ go test ./internal/sync/... -count=1 -v
--- PASS: TestNewSyncerUsesConfiguredValuesAndGuardsZeroes (0.00s)
--- PASS: TestPushSendsOneRequestWithEveryMemory (0.00s)
--- PASS: TestPushMixedSessionsSendsAnEmptyEnvelopeSession (0.00s)
--- PASS: TestPushRejectsNon2xxWithoutLeakingTheCredential (0.00s)
--- PASS: TestPushWrapsATransportError (0.00s)
--- PASS: TestPushWithoutCredentialOrEntries (0.00s)
--- PASS: TestFlushMarksEverythingThePlaneAccepted (0.30s)
--- PASS: TestFlushLeavesMemoriesPendingWhenThePlaneRefuses (0.25s)
--- PASS: TestFlushDrainsInBatchesOfTheConfiguredBatchSize (0.25s)
--- PASS: TestFlushAbandonsTheOldestWhenTheBacklogIsOverTheHardLimit (0.32s)
--- PASS: TestFlushWarnsOnALargeBacklogBelowTheHardLimit (0.38s)
--- PASS: TestFlushReportsAnAcknowledgementFailure (0.00s)
--- PASS: TestRunBackgroundFlushesUntilTheContextIsCancelled (0.29s)
--- PASS: TestPullCandidatesTimesOutOnASlowPlane (0.25s)
--- PASS: TestPullCandidatesReportsARefusedRequestImmediately (0.00s)
--- PASS: TestPullCandidatesReturnsThePlanesMemories (0.00s)
--- PASS: TestPullCandidatesWithoutACredentialMakesNoRequest (0.00s)
--- PASS: TestPullCandidatesWrapsATransportError (0.00s)
PASS
ok  	synapse/internal/sync	2.037s

$ go test ./internal/retrieval/... ./internal/plane/... -count=1 -v
PASS
ok  	synapse/internal/retrieval	0.018s
--- PASS: TestSearchMemoriesRequiresAVerifiedTenantToken (0.00s)
--- PASS: TestSearchMemoriesFailsClosedWithoutATokenMiddleware (0.00s)
--- PASS: TestSearchMemoriesReadsTheTokensTenant (0.00s)
--- PASS: TestSearchMemoriesAnswersAnEmptyTenantWithAnEmptyArray (0.00s)
--- PASS: TestSearchMemoriesRejectsMalformedRequests (0.00s)
--- PASS: TestSearchMemoriesWithoutASearcherAnswersInternal (0.00s)
--- PASS: TestSearchMemoriesReportsAFailureWithoutLeakingIt (0.00s)
--- PASS: TestSyncMemoriesRequiresAVerifiedTenantToken (0.00s)   (Phase 8, unchanged)
--- PASS: TestSyncMemoriesFailsClosedWithoutATokenMiddleware (0.00s)
--- PASS: TestSyncMemoriesStoresTheBatchInTheTokensTenant (0.00s)
--- PASS: TestSyncMemoriesAcceptsAnEmptyBatch (0.00s)
--- PASS: TestSyncMemoriesRejectsBadInput (0.06s)
--- PASS: TestSyncMemoriesReportsAStorageFailureAsInternal (0.00s)
--- PASS: TestSyncMemoriesWithoutAWriterAnswersInternally (0.00s)
PASS
ok  	synapse/internal/plane	0.246s

$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/store/... ./internal/tenant/... -count=1 -v -run 'TestPGStore|TestRunMigrations'
=== RUN   TestPGStore/an_agent_id_round-trips_and_a_blank_one_becomes_the_column_default
--- PASS: TestPGStore (0.81s)
    --- PASS: TestPGStore/an_agent_id_round-trips_and_a_blank_one_becomes_the_column_default (0.02s)
--- PASS: TestPGStoreRejectsInvalidSlug (0.07s)
PASS
ok  	synapse/internal/store	0.884s
--- PASS: TestRunMigrationsIsIdempotentAndCreatesEveryDocumentedTable (0.04s)
PASS
ok  	synapse/internal/tenant	0.053s

$ go test ./... -count=1
ok  	synapse/internal/api	0.877s
ok  	synapse/internal/budget	0.323s
ok  	synapse/internal/classifier	0.009s
ok  	synapse/internal/compiler	0.318s
ok  	synapse/internal/config	0.006s
ok  	synapse/internal/dedup	0.007s
ok  	synapse/internal/embedder	2.429s
ok  	synapse/internal/integration	2.236s
ok  	synapse/internal/plane	0.053s
ok  	synapse/internal/proxy	0.285s
ok  	synapse/internal/retrieval	0.011s
ok  	synapse/internal/scorer	0.011s
ok  	synapse/internal/store	3.309s
ok  	synapse/internal/supersession	0.005s
ok  	synapse/internal/sync	3.442s
ok  	synapse/internal/tenant	0.783s
ok  	synapse/internal/trace	0.181s
```

Live end-to-end test — real binaries, real Postgres, real traffic. The compose
`db` service from Phase 4 was already up (`docker compose ps`: `db` healthy,
`127.0.0.1:5432->5432`), so only the plane and the edge node were started. The
plane ran with its four secrets from the environment and no config file; the edge
ran with `control-plane-url: http://127.0.0.1:9090`, `control-plane-api-key` set
to the tenant JWT from provisioning, and `agent-id: edge-agent-1`. The edge's
config file was written 0600 and both the JWT and the API key are masked or
absent from everything printed below.

```text
$ SYNAPSE_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
  SYNAPSE_JWT_SECRET='…' SYNAPSE_ADMIN_TOKEN='…' SYNAPSE_MASTER_KEY='…' \
  ./bin/plane > /tmp/synapse-phase9/plane.log 2>&1 &
--- plane.pid=204826
$ curl -s http://127.0.0.1:9090/health
{"status":"ok","version":"2.0.0","db":"connected"}

$ curl -s -X POST http://127.0.0.1:9090/v2/tenants -H 'Authorization: <admin token>' \
      -H 'Content-Type: application/json' -d '{"slug":"phase9-edge"}' -o tenant.json
$ jq '{tenant_id, jwt_len: (.jwt|length), api_key_len: (.api_key|length)}' tenant.json
{ "tenant_id": "8493f5cd-6068-4806-8d88-c3991d386701", "jwt_len": 385, "api_key_len": 64 }

$ ./bin/synapse --config /tmp/synapse-phase9/edge.yaml    # 0600, credential redacted above
INFO synapse: Store initialized db_path=/tmp/synapse-phase9/edge.db
INFO synapse: sync: background flusher started control_plane_url=http://127.0.0.1:9090/v2/sync/memories agent_id=edge-agent-1 interval_seconds=30 batch_size=20
INFO synapse: ONNX embedder initialized with real inference model=/home/ranscky/Dev/synapse/models/all-MiniLM-L6-v2/model.onnx
INFO synapse: Synapse security: proxy bound to 127.0.0.1:8080, upstream 127.0.0.1:11434, …

# --- compile #1: plane reachable, tenant schema empty -------------------------
$ curl -s -X POST http://127.0.0.1:8080/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase9","messages":[{"role":"user","content":"what did we decide about the retrieval pipeline?"}]}' \
    -o compile1.json -w 'HTTP %{http_code} in %{time_total}s\n'
HTTP 200 in 0.304428s
$ jq '.trace | {candidates_retrieved, candidates_after_dedup, tokens_used}' compile1.json
{ "candidates_retrieved": 0, "candidates_after_dedup": 0, "tokens_used": 0 }
$ jq -r '.compiled_messages[] | "[\(.role)] \(.content)"' compile1.json
[user] what did we decide about the retrieval pipeline?
plane.log: INFO plane: Memories searched tenant_slug=phase9-edge agent_id=edge-agent-1 memories=0
   ^ the edge really did ask the plane, and the plane's org-scoped answer was
     empty -- which is the correct first answer on a fresh tenant

# --- seed a memory that exists ONLY on the plane ------------------------------
$ curl -s -X POST http://127.0.0.1:9090/v2/sync/memories -H "Authorization: Bearer $JWT" \
    -H 'Content-Type: application/json' -d @push.json        # 384-float embedding, session sess-phase9
{"written":1,"sanitized":0}
plane.log: INFO plane: Memories synced tenant_slug=phase9-edge agent_id=edge-agent-1 written=1 sanitized=0
```

```text
# --- compile #2: the same request, now that the plane holds an org memory ------
$ curl -s -X POST http://127.0.0.1:8080/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase9","messages":[{"role":"user","content":"what did we decide about the retrieval pipeline?"}]}' \
    -o compile2.json -w 'HTTP %{http_code} in %{time_total}s\n'
HTTP 200 in 0.123633s
$ jq '.trace | {candidates_retrieved, candidates_after_dedup, tokens_used}' compile2.json
{ "candidates_retrieved": 1, "candidates_after_dedup": 1, "tokens_used": 20 }
$ jq -r '.compiled_messages[] | "[\(.role)] \(.content)"' compile2.json
[user] [Memory: decision] PLANE-ONLY-MEMORY: the org decided the retrieval pipeline embeds once per turn.

what did we decide about the retrieval pipeline?
plane.log: INFO plane: Memories searched tenant_slug=phase9-edge agent_id=edge-agent-1 memories=1

# the memory that just entered the compiled context is NOT in local SQLite --
# the only way it could have been compiled is from the plane's answer:
$ python3 -c "import sqlite3;print(sqlite3.connect('/tmp/synapse-phase9/edge.db').execute('select id, substr(content,1,45) from memories').fetchall())"
[('req-1789911788250180931', 'what did we decide about the retrieval p'), ('req-1789911802670566057', 'what did we decide about the retrieval p')]

# --- compile #3: the plane process is killed -----------------------------------
$ kill $(cat plane.pid)
$ curl -s -m 2 http://127.0.0.1:9090/health
connection refused
$ curl -s -X POST http://127.0.0.1:8080/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase9","messages":[{"role":"user","content":"what did we decide about the retrieval pipeline?"}]}' \
    -o compile3.json -w 'HTTP %{http_code} in %{time_total}s\n'
HTTP 200 in 0.076302s
edge.log: WARN synapse: Control plane candidate pull failed, falling back to local search \
          plane_unavailable=true fallback=local \
          error="sync: search request: Get \"http://127.0.0.1:9090/v2/memories/search\": dial tcp 127.0.0.1:9090: connect: connection refused"
$ jq '.trace | {candidates_retrieved, candidates_after_dedup, tokens_used}' compile3.json
{ "candidates_retrieved": 2, "candidates_after_dedup": 1, "tokens_used": 9 }
$ jq -r '.compiled_messages[] | "[\(.role)] \(.content)"' compile3.json
[user] [Memory: context] what did we decide about the retrieval pipeline?

what did we decide about the retrieval pipeline?
```

That transcript is the phase's contract working in both directions:

- **Plane reachable** — the plane's own log line (`Memories searched …
  memories=0`, then `memories=1`) proves the edge asked, and compile #2 compiled a
  memory that exists nowhere in the edge's SQLite file: org-scoped retrieval,
  end to end.
- **Plane killed** — one WARN with `plane_unavailable=true fallback=local`, HTTP
  200 in 76ms, and the local store's own rows (2 retrieved, 1 after dedup, both
  from this session's earlier turns) compiled instead. The compile never failed
  and the credential appears nowhere in the log.
- **Cost of success vs. cost of failure** — 123ms with the plane answering (its
  round trip is inside the compile), 76ms offline, where the refused connection
  costs essentially nothing and the 200ms ceiling never has to fire.

Cleanup after the run: the edge process was stopped, and the JWT-bearing
`tenant.json`/`edge.yaml` were shredded (both were 0600 and never committed).

Next phase: mark memories `sync_pending` on the write path, so an edge node
populates its plane without a hand-pushed batch — the piece Phase 8 expected this
phase to be.


## Phase 10 — Global Brain visibility scopes (complete)

Commit `feat: Phase 10 - Global Brain visibility scopes`

A shared plane is only useful if a node can read the org's memories *and* cannot
read another node's private ones. Before this phase every row in a tenant schema
was reachable by every reader in it: `PGStore.Search` filtered on session and
nothing else, and `PGStore.Write` never named the `visibility` or `team_id`
columns, so every row took the schema's defaults (`'org'`, `NULL`). This phase
makes scope real on both halves of the backend and gives the plane a verified
identity to evaluate it against.

The predicate, in one place (`internal/store/pgvisibility.go`):

```sql
WHERE superseded_by IS NULL AND (
  visibility = 'org'
  OR (visibility = 'team'    AND team_id  = $teamID)
  OR (visibility = 'private' AND agent_id = $agentID AND session_id = $sessionID)
)
```

Every caller value is a bound parameter; `fmt` is used only to number
placeholders. The predicate is applied to *both* reads Search can make — the
HNSW vector path and the no-embedding recency fallback — because "send no
embedding" is a state an attacker can arrange, and a fallback without the
predicate would be a way around it.

New files:

| File | Contents |
| --- | --- |
| `internal/store/pgvisibility.go` | `VisibilityPrivate`/`Team`/`Org`, `visibilityWhere` (the read predicate), `visibilityForWrite` (the write normalization) |
| `internal/store/visibility_test.go` | `TestVisibilityScopes`, `TestVisibilityTeamScopes`, `TestVisibilityWriteNormalization` (+ `idsOf`) |
| `internal/store/pgtest_test.go` | the Postgres fixtures every store test shares (`testEnvDatabaseDSN`, `testPGPool`, `testPGStore`, `embeddingAt`, `testEntry`, `testReaderAgent`) |
| `internal/plane/search_scope_test.go` | `TestSearchMemoriesTakesTheScopeFromTheToken`, `TestSearchMemoriesIgnoresTheBodyScope`, `TestSearchMemoriesRefusesABodyThatNamesAnotherAgent` |
| `internal/plane/provision_scope_test.go` | `TestCreateTenantMintsAnAgentScopedToken`, `TestCreateTenantWithoutAnAgentMintsATenantToken`, `TestCreateTenantRejectsAMalformedIdentity` (+ `claimsFromToken`) |
| `internal/plane/tenants.go` | the provisioning endpoint, split out of `handlers.go` (`createTenantRequest`/`Response`, `handleCreateTenant`, `slugPattern`/`planPattern`/`identityPattern`, the endpoint's body limit and defaults) |

Changed:

- `internal/store/store.go` — `MemoryEntry.Visibility` and `.TeamID` (additive,
  `omitempty`, inert on SQLite: one file is one agent, so a local row is already
  private to its only reader). `(*Store).Search` takes the three scope
  parameters and ignores them, with the reason written down.
- `internal/store/pgstore.go` — `PGStore.Search(ctx, embedding, agentID, teamID,
  currentSessionID, topK)`; `PGStore.Write` inserts `visibility` (blank → the
  column default) and `team_id` (blank → NULL). The old unconditional session
  filter is gone: `currentSessionID` now gates *private* rows only.
- `internal/store/pgread.go` — `pgColumns` carries `visibility` and
  `coalesce(team_id, '')`; `recent` moved here and applies the same predicate.
- `internal/store/factory.go` — `Backend.Search` signature.
- `internal/retrieval/retrieval.go` — `Store.Search`; new exported `Scope`
  (`AgentID`, `TeamID`, `SessionID`) instead of a bare `sessionID`, because seven
  parameters with three adjacent strings is a swapped-argument hazard.
- `internal/proxy/proxy.go`, `internal/api/api.go` — build the scope from their
  own config (`cfg.AgentID`, `cfg.TeamID`) and the request's session. The edge's
  identity is configuration, never a request header or body.
- `internal/sync/pull.go` — `searchRequest` carries `team_id` alongside
  `agent_id`; both are attribution on the wire, never authority.
- `internal/plane/search.go` — `MemorySearcher.Search` takes the scope; the
  handler reads `agentID`/`teamID` from `AgentIDFromCtx`/`TeamIDFromCtx` (the
  verified token) and never from `req.AgentID`. The body's `agent_id` stays
  required and stays in the log line as attribution; a body that names a
  *different* agent than an agent-scoped token is refused with 400
  `invalid_agent`, so a node configured with the wrong `agent-id` finds out
  instead of quietly reading another scope.
- `internal/tenant/memorywriter.go` — passes the scope through to the store.
- `internal/tenant/token.go` — `IssueToken` now takes a `TokenIdentity` struct
  (seven positional strings, two of them a scope, was a mix-up waiting to
  happen); `claims` gains optional `agent_id`/`team_id`, omitted from the payload
  when empty so a tenant-level token is byte-identical to every token this plane
  issued before.
- `internal/tenant/auth.go` — `AgentIDFromCtx`/`TeamIDFromCtx` + ctx keys;
  `withClaims` publishes both through new `plane.WithAgentID`/`WithTeamID`.
- `internal/plane/sync.go` — `WithAgentID`/`AgentIDFromCtx`,
  `WithTeamID`/`TeamIDFromCtx`, documented as verified-claim-only.
- `internal/plane/handlers.go` — `createTenantRequest` accepts optional
  `agent_id`/`team_id`, validated against `^[A-Za-z0-9_.-]{1,64}$`, and passes
  them to the provisioner. This is what makes an agent-scoped token reachable in
  production rather than only in tests. The endpoint itself then moved to
  `internal/plane/tenants.go`: those additions took `handlers.go` to 313 lines,
  past the 300-line ceiling, so it was split the same way `sync.go` and
  `search.go` were — one endpoint per file. `handlers.go` is 179 lines and
  `tenants.go` 156.
- `internal/plane/provision.go`, `internal/tenant/provision.go` — carry
  `AgentID`/`TeamID` from the request into the signed token.
- Test doubles and assertions across `internal/store`, `internal/retrieval`,
  `internal/proxy`, `internal/tenant`, and `internal/plane` moved to the new
  signatures: `fakeStore`/`mockMemoryStore`/`fakeSearcher` record the scope (so a
  pipeline that dropped it would fail a test), `retrieval.Candidates` gained
  `TestCandidatesPassesTheScopeToTheStore` for the deliberately-blank case,
  `token_test.go` pins both halves of the claim contract (present when set,
  absent when not), and `pgstore_test.go`'s fixtures moved to `pgtest_test.go`.

### What the write path guarantees

`visibilityForWrite` is where a stored scope can be made unreachable by mistake,
so every normalization is fail-closed:

- a blank visibility is the schema's own default, `'org'`;
- a `'team'`-scoped memory with no team id is **narrowed to `'private'`** and
  logged — stored as written it would put NULL in `team_id`, which no reader can
  ever match, so the memory would be written successfully and be unreachable
  forever;
- a team id is stored as NULL rather than `''`, so one unteamed agent's row can
  never match another unteamed agent's query;
- any other visibility value is an error, not a stored value: the column has no
  CHECK constraint, and a row the predicate does not recognise is a row nobody
  can read.

### Behavior change worth knowing

**A session no longer bounds an org-scoped search.** Phase 5 documented
`Search`'s session filter as a deliberate difference from SQLite; the scope model
replaces it, because an org-scoped memory reaching only the session it was
written in is not a shared brain. `sessionID` now gates `private` and nothing
else — an agent reaches the org's memories from any session, and its *own*
private memories only from the session that wrote them. Four `TestPGStore`
subtests whose premise was session scoping were rewritten to assert the new
contract, and `internal/store/isolation_test.go`'s fixture now writes team-scoped
memories under a per-run team id (`tenant-alpha` is a fixed slug that accumulates
rows across runs, so an org-scoped fixture can no longer produce an exact count;
a team id unique to the run still does).

### Limitations this phase deliberately did not close

- **`PGStore.GetRecent` applies no visibility predicate.** It is the one read
  that still returns a whole session regardless of scope. Not reachable as a leak
  today: the plane's HTTP surface exposes Search alone, and the v1 endpoints that
  call `GetRecent` (`GET /api/memories`, the sync queue) are wired to the local
  SQLite store — `NewStoreFromConfig`, which can return a PGStore, still has no
  production caller. Closing it needs two more parameters and two more v1
  interfaces, so it is recorded here rather than quietly left out.
- **The edge does not stamp `default-visibility` onto what it writes.**
  `config.DefaultVisibility` is defined and documented but read by nothing, so a
  locally written memory reaches the plane with a blank scope and is stored
  `'org'`. The plane is scope-aware end to end now; having the edge *choose* the
  scope is additive work.
- **No per-agent token reissue.** An existing tenant's token cannot be upgraded
  to an agent-scoped one without provisioning again; there is still no
  reissue/mint endpoint. A tenant-level token remains valid and remains an
  org-only reader.
- **Still no `sync_pending` marking** (unchanged from Phases 8-9): the flusher
  finds an empty queue on a fresh node.
- `openapi.yaml` documents only the v1 endpoints; the v2 plane routes
  (`/v2/tenants`, `/v2/memories/search`) have never been in it, and Phase 10 did
  not add them.

### Verification (real output, this phase)

```text
$ gofmt -l <every file touched>   # each already-dirty v1 file at HEAD excepted
(no newly-dirty file: the pre-existing unformatted v1 files were left alone,
 confirmed by formatting each HEAD revision and comparing)

$ go vet ./...
(no output)

$ go build ./...
BUILD OK
$ CGO_ENABLED=0 go build -o /dev/null ./cmd/plane
CGO_ENABLED=0 PLANE BUILD OK

$ go test ./... -count=1                     # no database: PG tests skip
ok  synapse/internal/api 1.107s       ok  synapse/internal/plane 0.056s
ok  synapse/internal/budget 0.386s    ok  synapse/internal/proxy 0.557s
ok  synapse/internal/classifier ...   ok  synapse/internal/retrieval ...
ok  synapse/internal/compiler ...     ok  synapse/internal/scorer ...
ok  synapse/internal/config ...       ok  synapse/internal/store 3.906s
ok  synapse/internal/dedup ...        ok  synapse/internal/supersession ...
ok  synapse/internal/embedder ...     ok  synapse/internal/sync 3.811s
ok  synapse/internal/integration ...  ok  synapse/internal/tenant 0.796s
                                      ok  synapse/internal/trace 0.194s

$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/store/... -run TestVisibility -v
=== RUN   TestVisibilityScopes
=== RUN   TestVisibilityScopes/control:_the_writer_reads_all_of_its_own_memories
=== RUN   TestVisibilityScopes/agent_b_sees_agent_a's_org_memories_and_none_of_its_private_ones
=== RUN   TestVisibilityScopes/agent_b_in_agent_a's_own_session_still_sees_only_the_org_memories
=== RUN   TestVisibilityScopes/the_no-embedding_fallback_cannot_reach_private_memories
=== RUN   TestVisibilityScopes/a_blank_agent_id_is_an_org-only_reader
--- PASS: TestVisibilityScopes (0.33s)
    --- PASS: .../control:_the_writer_reads_all_of_its_own_memories (0.00s)
    --- PASS: .../agent_b_sees_agent_a's_org_memories... (0.00s)
    --- PASS: .../agent_b_in_agent_a's_own_session... (0.00s)
    --- PASS: .../the_no-embedding_fallback... (0.00s)
    --- PASS: .../a_blank_agent_id_is_an_org-only_reader (0.00s)
=== RUN   TestVisibilityTeamScopes
=== RUN   TestVisibilityTeamScopes/a_teammate_reads_the_team's_memories
=== RUN   TestVisibilityTeamScopes/a_team-scoped_memory_round-trips_its_team_id
=== RUN   TestVisibilityTeamScopes/a_reader_with_no_team_id_reaches_no_team-scoped_memory
=== RUN   TestVisibilityTeamScopes/another_team_reaches_no_team-scoped_memory
--- PASS: TestVisibilityTeamScopes (0.22s)
    --- PASS: .../a_teammate_reads_the_team's_memories (0.00s)
    --- PASS: .../a_team-scoped_memory_round-trips_its_team_id (0.00s)
    --- PASS: .../a_reader_with_no_team_id_reaches_no_team-scoped_memory (0.00s)
    --- PASS: .../another_team_reaches_no_team-scoped_memory (0.00s)
=== RUN   TestVisibilityWriteNormalization
=== RUN   TestVisibilityWriteNormalization/a_blank_visibility_is_stored_as_the_column's_default
=== RUN   TestVisibilityWriteNormalization/an_unknown_visibility_is_refused_rather_than_stored
=== RUN   TestVisibilityWriteNormalization/a_team_scope_with_no_team_id_is_narrowed_to_private
2026/09/20 14:15:40 WARN Team-scoped memory without a team id stored as private
    memory_id=7a2a15ef-db99-42ca-aca6-0b42f5ae8d13 visibility=team
--- PASS: TestVisibilityWriteNormalization (0.40s)
    --- PASS: .../a_blank_visibility... (0.01s)
    --- PASS: .../an_unknown_visibility... (0.00s)
    --- PASS: .../a_team_scope_with_no_team_id... (0.21s)
PASS
ok  synapse/internal/store 0.956s

$ SYNAPSE_TEST_DB_DSN='...' go test ./internal/store/... ./internal/tenant/... ./internal/plane/... -count=1
ok  synapse/internal/store 6.218s
ok  synapse/internal/tenant 1.571s
ok  synapse/internal/plane 0.078s

$ SYNAPSE_TEST_DB_DSN='...' go test ./internal/store/... -run TestCrossTenantIsolation -tags integration
ok  synapse/internal/store 0.148s

# The isolation test was re-verified to fail when isolation is broken, since
# Phase 10 changed its fixture: with PGStore.Search temporarily reading
# tenant_tenant_alpha's table for every store, the control subtest PASSES and
# every tenant-beta assertion FAILS (expected 0, got the leaked rows) -- the ten
# random queries included. The temporary patch was reverted, and `git diff` on
# pgmigrate.go/pgstore.go confirms only the intended changes remain.
```

Phase 10's answer to "why this memory?": the scope a memory was surfaced under is
now part of the result, not just of the query — each `MemoryEntry` carries
`visibility` and `team_id`, so a caller can see whether what it got was org-wide,
its team's, or its own, and a search can only ever return what the caller's
*verified* identity is allowed to read. Surfacing that scope alongside the S/R/I/T
score breakdown and `trace_id` (the MCP work) is what makes the plane auditable
rather than merely opaque.

Next phase: mark memories `sync_pending` on the write path (still the piece
Phase 8 expected Phase 9 to be), or make the edge stamp `default-visibility` onto
what it writes — both are now the missing halves of a pipeline whose ends exist.


## Phase 11 — cross_agent field in the Memory Trace (complete)

Commit `feat: Phase 11 - cross_agent field in Memory Trace`

The scores in a trace say how well a memory ranked. None of them say *whose*
memory it was — and once an edge node pulls candidates from a shared plane, a
memory another agent wrote and a memory this node wrote are indistinguishable in
the compiled output: same four scores, same preview, same entry shape. This phase
adds the two fields that answer that question, and nothing else.

```go
// internal/trace/trace.go
type TraceMemory struct {
	...
	AgentID    string `json:"agent_id"`    // the memory's own agent, as its backend returned it
	CrossAgent bool   `json:"cross_agent"` // that agent is not the one that built this context
}
```

`CrossAgent` is `memory.AgentID != "" && memory.AgentID != localAgentID`, where
`localAgentID` is the compiling node's `config.AgentID`. A blank `AgentID` is
**unattributed, not someone else's**: the local SQLite backend has no agent column
(one file is one agent) and a standalone node has no configured id, so its own
memories must not come out flagged as cross-agent. Neither field carries
`omitempty`, so every entry — included or excluded — says where it stands instead
of leaving "absent" to be read as a third state.

### Where the identity comes from

`cfg.AgentID` lived only in the two v1 callers: `internal/api/api.go`'s
`a.config.AgentID` (already used to build the retrieval scope) and
`internal/proxy/proxy.go`'s `p.config.AgentID`. It is now threaded explicitly as
one new final parameter through `compiler.Compile`,
`compiler.CompileWithContext`, and `trace.NewTraceManifest`. That is three
mechanical call-site additions and no logic change in any v1 file. The
alternative — a package-level default set at boot — was rejected because the
project rule is no global state, and because it would make the meaning of a trace
depend on init order.

| File | Change |
| --- | --- |
| `internal/trace/trace.go` | `TraceMemory.AgentID`/`.CrossAgent`; `NewTraceManifest` takes `localAgentID` and stamps both on every entry it builds |
| `internal/compiler/compiler.go` | `Compile`/`CompileWithContext` take `localAgentID` and forward it to `NewTraceManifest` |
| `internal/api/api.go`, `internal/proxy/proxy.go` | pass `config.AgentID` — one argument each, no other edit |
| `internal/api/integration_test.go` | that test's direct `NewTraceManifest` call passes `""` |
| `schemas/memory-trace.schema.json`, `openapi.yaml` | `agent_id` (string) and `cross_agent` (boolean) on the memory entry, both in `required` |
| `ui/index.html`, `ui/session.html` | a violet `cross-agent: <agent_id>` badge on entries where `cross_agent` is true |

`scorer`, `store`, `dedup`, and `budget` needed no changes at all: `ScoredMemory`
embeds `store.MemoryEntry`, which has carried `AgentID` since Phase 10, and
`Deduplicate`/`Fill` copy whole `ScoredMemory` values — so provenance survives
scoring, dedup, and the token budget intact (asserted in
`TestCompile_TraceAgentProvenance` rather than assumed).

### The two UI files are independent, and both needed the badge

Neither inspector renders a trace entry as generic key/value pairs: each builds
the entry field by field in its own `renderMemoryTrace`, so `agent_id` and
`cross_agent` were invisible in both. Both files got the badge and their own CSS
rule. `session.html`'s entries additionally open a raw JSON dump on click (so
`agent_id` is visible there twice); `index.html`'s playground entries have no such
modal, which is why the badge label carries the agent id rather than only the
words "cross-agent".

### Verification (real output, this phase)

```text
$ gofmt -l <every file touched>
(each of these v1 files is already unformatted at HEAD; the gofmt diff of each
 HEAD revision was compared against the current one and the +/- line counts are
 identical -- trace.go 34/34, compiler.go 4/4, trace_test.go 5/5,
 compiler_test.go 64/64 -- so this phase added no new formatting dirt and
 reformatted nothing pre-existing)

$ go vet ./...
(no output)                                                    EXIT=0

$ go build ./...
BUILD OK
$ CGO_ENABLED=0 go build -o /dev/null ./cmd/plane
CGO_ENABLED=0 PLANE BUILD OK

$ go test ./... -count=1
ok  synapse/internal/api 0.980s          ok  synapse/internal/proxy 0.216s
ok  synapse/internal/budget 0.534s       ok  synapse/internal/retrieval 0.008s
ok  synapse/internal/classifier 0.023s   ok  synapse/internal/scorer 0.008s
ok  synapse/internal/compiler 0.564s     ok  synapse/internal/store 20.853s
ok  synapse/internal/config 0.011s       ok  synapse/internal/supersession 0.005s
ok  synapse/internal/dedup 0.015s        ok  synapse/internal/sync 20.954s
ok  synapse/internal/embedder 3.505s     ok  synapse/internal/tenant 0.802s
ok  synapse/internal/integration 4.787s  ok  synapse/internal/trace 0.174s
ok  synapse/internal/plane 0.083s
                                                               EXIT=0

$ SYNAPSE_TEST_DB_DSN='...' go test ./internal/store/... -run TestVisibility -count=1
--- PASS: TestVisibilityScopes (0.33s)              [4 subtests]
--- PASS: TestVisibilityTeamScopes (0.74s)          [4 subtests]
--- PASS: TestVisibilityWriteNormalization (0.64s)  [3 subtests]
ok  synapse/internal/store 2.729s                              EXIT=0

$ go test ./internal/trace/ -run 'TestNewTraceManifest|TestTraceMemory' -v
--- PASS: TestNewTraceManifest_ExclusionReasons
--- PASS: TestNewTraceManifest_SupersededByOmittedWhenNotSuperseded
--- PASS: TestNewTraceManifest_AgentProvenance (0.00s)
    --- PASS: .../another_agent's_memory_is_cross-agent
    --- PASS: .../this_node's_own_memory_is_not
    --- PASS: .../an_unattributed_memory_is_not_cross-agent
    --- PASS: .../a_standalone_node_reports_nothing_as_cross-agent
    --- PASS: .../a_plane_memory_is_cross-agent_even_for_a_node_with_no_id
--- PASS: TestTraceMemory_ProvenanceAlwaysMarshaled
PASS

$ go test ./internal/compiler/ -run TestCompile_TraceAgentProvenance -v
--- PASS: TestCompile_TraceAgentProvenance (0.14s)
```

Live end-to-end test — real binaries, real Postgres, two edge configs. The
compose `db` service came up healthy; the plane ran with its four secrets from
the environment and no config file; each edge ran with `agent-id`, its own
`control-plane-api-key`, its own `db-path`, and its own loopback port. Both config
files were written 0600 via `umask 077`, and the tenant JWT never appears in
anything printed below.

```text
$ cd deploy && docker compose up -d db      # pgvector/pgvector:pg16
$ docker compose ps db --format '{{.Status}}'
Up 10 seconds (healthy)

$ SYNAPSE_DB_DSN='postgres://...' SYNAPSE_JWT_SECRET='…' SYNAPSE_ADMIN_TOKEN='…' \
  SYNAPSE_MASTER_KEY='…' ./bin/plane > /tmp/synapse-phase11/plane.log 2>&1 &
$ curl -s http://127.0.0.1:9090/health
{"status":"ok","version":"2.0.0","db":"connected"}

# requireAdmin compares the raw Authorization header, so the admin token is sent
# without a "Bearer " prefix here (the tenant JWT is the one that takes Bearer).
$ curl -s -X POST http://127.0.0.1:9090/v2/tenants -H 'Authorization: <admin token>' \
      -H 'Content-Type: application/json' -d '{"slug":"phase11-edge"}' -o tenant.json
$ jq '{tenant_id, jwt_len: (.jwt|length)}' tenant.json
{ "tenant_id": "418f3cfe-22eb-49c7-8070-973bf4fa0db7", "jwt_len": 387 }

# --- edge_a (agent-id: agent_a) writes one session ----------------------------
$ ./bin/synapse --config /tmp/synapse-phase11/edge_a.yaml &
INFO synapse: Store initialized db_path=/tmp/synapse-phase11/edge_a.db
INFO synapse: sync: background flusher started control_plane_url=…/v2/sync/memories \
     agent_id=agent_a interval_seconds=30 batch_size=20
INFO synapse: Starting Synapse proxy address=127.0.0.1:8081 upstream=http://127.0.0.1:11434

$ curl -s -X POST http://127.0.0.1:8081/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase11-a","messages":[{"role":"user","content":"PLANE-ORG-DECISION: we decided the retrieval pipeline embeds the query exactly once per turn."}]}'
HTTP 200
$ jq '.trace | {candidates_retrieved, memories_compiled}' compile_a.json
{ "candidates_retrieved": 0, "memories_compiled": 0 }   # the tenant's schema is still empty

# the memory edge_a just wrote is on local disk only -- nothing marks it
# sync_pending, so the background flusher has nothing to push (see findings)
$ python3 -c "import sqlite3;print(sqlite3.connect('/tmp/synapse-phase11/edge_a.db').execute('select id, session_id, memory_type, sync_status, length(embedding) from memories').fetchall())"
[('req-1789988915117028336', 'sess-phase11-a', 'decision', 'local_only', 1536)]

# --- that exact row goes to the plane through the documented push protocol ----
# (the same body shape and route sync.Syncer.Push uses, envelope naming agent_a;
#  the embedding is read back out of edge_a's own row, 384 floats)
$ curl -s -X POST http://127.0.0.1:9090/v2/sync/memories -H "Authorization: Bearer $JWT" \
    -H 'Content-Type: application/json' -d @push.json
{"written":1,"sanitized":0} HTTP 200
plane.log: INFO plane: Memories synced tenant_slug=phase11-edge agent_id=agent_a written=1 sanitized=0

$ docker compose exec -T db psql -U synapse -d synapse \
    -c "select agent_id, visibility, session_id, left(content,45) from tenant_phase11_edge.memories"
 agent_id | visibility |   session_id   |                    content
----------+------------+----------------+-----------------------------------------------
 agent_a  | org        | sess-phase11-a | PLANE-ORG-DECISION: we decided the retrieval
```

```text
# --- edge_b (agent-id: agent_b) compiles a NEW session ------------------------
$ ./bin/synapse --config /tmp/synapse-phase11/edge_b.yaml &
INFO synapse: sync: background flusher started agent_id=agent_b interval_seconds=30 batch_size=20
INFO synapse: Starting Synapse proxy address=127.0.0.1:8082 upstream=http://127.0.0.1:11434

$ curl -s -X POST http://127.0.0.1:8082/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase11-b","messages":[{"role":"user","content":"what did we decide about the retrieval pipeline?"}]}' \
    -o compile_b.json -w 'HTTP %{http_code} in %{time_total}s\n'
HTTP 200 in 0.136244s
$ jq '.trace | {candidates_retrieved, candidates_after_dedup, memories_compiled, tokens_used,
                memories: [.memories[] | {id, memory_type, agent_id, cross_agent, included}]}' compile_b.json
{
  "candidates_retrieved": 1, "candidates_after_dedup": 1,
  "memories_compiled": 1, "tokens_used": 22,
  "memories": [
    { "id": "84b67539-c16a-563c-914d-2532a39e4d0b", "memory_type": "decision",
      "agent_id": "agent_a", "cross_agent": true, "included": true }
  ]
}
$ jq -r '.compiled_messages[] | "[\(.role)] \(.content)"' compile_b.json
[user] [Memory: decision] PLANE-ORG-DECISION: we decided the retrieval pipeline embeds the query exactly once per turn.

what did we decide about the retrieval pipeline?
plane.log: INFO plane: Memories searched tenant_slug=phase11-edge agent_id="" request_agent_id=agent_b memories=1

# every entry carries both keys, included or not -- the DoD's "on every memory
# entry", asserted against the real response rather than eyeballed:
$ jq 'all(.trace.memories[]; has("agent_id") and has("cross_agent"))' compile_b.json
true
```

```text
# --- the raw entry the session inspector itself receives ----------------------
$ curl -s http://127.0.0.1:8082/api/sessions/69b9b106-c5ec-44a6-95d3-ee4e877b5e6b/trace | jq -c '.memories[0]'
{"id":"84b67539-c16a-563c-914d-2532a39e4d0b","memory_type":"decision",
 "content_preview":"PLANE-ORG-DECISION: we decided the retrieval pipeline embeds the query exactly once per turn.",
 "score_semantic":0.5083690453533551,"score_recency":0.9994540326375881,"score_importance":1,
 "score_task_alignment":0.5,"score_total":0.7032930214051009,"included":true,
 "agent_id":"agent_a","cross_agent":true}

# --- both inspectors actually render it ---------------------------------------
# Each UI's own renderMemoryTrace was extracted from the HTML the server is
# serving and executed against that JSON in node, so this is the markup a browser
# inserts, not a description of it:
ui/session.html  ->  <span class="cross-agent-badge" title="Written by agent agent_a">cross-agent: agent_a</span>
ui/index.html    ->  <span class="cross-agent-badge" title="Written by agent agent_a">cross-agent: agent_a</span>
# and for an entry with agent_id "" / cross_agent false, both emit no badge at all:
ui/session.html  ->  <span class="type-badge type-decision">decision</span> <span class="trace-mem-preview">…</span>
ui/index.html    ->  <span class="type-badge type-decision">decision</span> <span class="trace-mem-preview">…</span>
```

```text
# --- control: the plane is killed, so edge_b compiles from its own SQLite ------
$ kill $(pgrep -f 'bin/plane')
edge_b.log: WARN synapse: Control plane candidate pull failed, falling back to local search
            plane_unavailable=true fallback=local
            error="sync: search request: Get \"http://127.0.0.1:9090/v2/memories/search\": dial tcp 127.0.0.1:9090: connect: connection refused"
$ curl -s -X POST http://127.0.0.1:8082/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase11-b","messages":[{"role":"user","content":"what did we decide about the retrieval pipeline?"}]}'
HTTP 200 in 0.037175s
$ jq '.trace.memories[] | {memory_type, agent_id, cross_agent, included}' compile_b_offline.json
{ "memory_type": "context", "agent_id": "", "cross_agent": false, "included": true }
```

That last pair is the whole phase in two entries: the same node, the same flag,
`true` for the memory the plane attributed to `agent_a` and `false` for the row
the node wrote itself. A flag that only meant "this memory came from somewhere
else", or only "this memory has an agent id", could not produce both.

### Findings this phase surfaced (not fixed here — this phase is one field)

1. **Nothing marks a locally written memory `sync_pending`**, so
   `RunBackground`'s queue is always empty and the flusher never pushes anything:
   attribution reaches the plane only through a push that something else drives.
   This verification drove it with the documented endpoint, exactly as
   `sync.Syncer.Push` would. Phase 10's closing note already lists this as the next
   phase's work, and this phase's live run is the second time it has come up.
2. **Two agents in one tenant cannot each hold an agent-scoped token.**
   `POST /v2/tenants` with an `agent_id` creates a *new* tenant schema, and no
   route mints a second token for an existing tenant. The run above therefore used
   one tenant-level token (no agent claim) for both edges, which the plane
   deliberately allows to name whichever node presents it
   (`internal/plane/search.go`), while each edge declares its own `agent-id` in
   config. That is exactly the scenario this phase's flag is about — an org memory
   another agent wrote, surfaced to this one — but per-agent credentials inside one
   tenant are still missing.
3. **`memoryUUID` maps edge ids onto the plane.** `internal/tenant/memorywriter.go`
   turns the edge's non-uuid ids (`req-<nano>`) into a deterministic SHA-1 uuid,
   which is why the trace entry above names
   `84b67539-c16a-563c-914d-2532a39e4d0b` rather than `req-1789988915117028336`.
   Worth knowing before correlating a trace entry with a local row.
4. **A hand-built push body must use an RFC3339 timestamp.** The SQLite column
   holds Go's non-RFC3339 `time.Time` string (`2026-09-21 11:08:35.117+00:00`) and
   the plane answers `invalid_body` for it, because its decoder is strict. Not a
   defect in the protocol — the real edge marshals the struct, which produces
   RFC3339 — but it is the trap for anyone scripting the endpoint by hand.

Next phase: mark memories `sync_pending` on the write path, and give an existing
tenant a way to mint per-agent tokens, so attributed memories reach the plane
without a hand-driven push. Those are the two gaps left between "a trace can say
this came from agent_a" and "a node's memories get there on their own".

## Phase 12 — ContradictionDetector (Jaccard) (complete)

Commit `feat: Phase 12 - ContradictionDetector (Jaccard)`

A detector, its tests, and two config knobs. Nothing calls it: no v1 package was
edited, and the only files touched outside the new package are
`internal/config/config.go` (two fields, two defaults, one guard),
`internal/config/config_test.go` (one test) and `synapse.yaml.example` (two
commented keys). `ConflictScorePenalty` is defined and read by nothing yet, on
purpose — wiring is a later phase.

The problem it exists for: `v0.1.15` already resolves contradictions *inside* one
session, and that machinery cannot see across a shared control plane. A plane pull
returns memories another agent wrote, in another session, with an agent id and no
comparable embedding on hand, and the newer version of a decision carries no
replacement phrase at all — "We decided to use MySQL" is a complete sentence that
never mentions Postgres. So this one works on the text:

```go
// internal/conflict/detector.go
type ContradictionDetector struct { jaccardThreshold float64 }

func NewContradictionDetector(jaccardThreshold float64) *ContradictionDetector
func (d *ContradictionDetector) Threshold() float64
func (d *ContradictionDetector) Detect(candidate store.MemoryEntry, existing []store.MemoryEntry) (bool, string)
```

Complementary, not redundant — the two gates are disjoint in what they can see:

| | supersession (v0.1.15) | conflict (this phase) |
| --- | --- | --- |
| Scope | same session, same memory type | any session, any agent |
| Compared with | cosine similarity in a tuned band (0.5–0.90) | Jaccard over token sets (≥ 0.4) |
| Signal | explicit replacement phrase ("switched to") | negation with a target, or same predicate + different object |
| Needs | an embedding on both sides | nothing but content |
| Misses | anything cross-agent | narrow/paraphrased wording |

### The numbers that shaped the design

Four fixtures, tokenized exactly as specified (lowercase, split on whitespace and
punctuation, deduplicate):

| Pair | \|A\| | \|B\| | A∩B | Jaccard | At 0.4 |
| --- | --- | --- | --- | --- | --- |
| "We decided to use Postgres" / "We decided to use MySQL" | 5 | 5 | we, to, use, decided | **0.667** | same topic → value swap → contradiction |
| "We decided to use Postgres" / "We will migrate to Postgres next sprint" | 5 | 7 | we, to, postgres | **0.333** | below gate → no contradiction |
| "The sky is blue" / "We decided to use MySQL" | 4 | 5 | ∅ | **0.000** | below gate → no contradiction |
| "We are not using Redis" / "We decided to use Redis" | 5 | 5 | we, redis | **0.250** | **below gate** |

The last row is the phase's Test 4, and it is the one that forced a design
decision rather than an implementation detail. The spec's step (b) skips any pair
below the threshold, and step (c) is only reached above it — so read literally, at
the specified default of 0.4, Test 4 returns `(false, "")` and fails. Strict
adjacency on the negation rule fails on it too: `not` is followed by `using`, and
`using ≠ use` without stemming, so the token the other memory actually contains is
two positions away.

**Decision (confirmed with the operator before implementation):** a negation aimed
at a token the other memory contains is itself evidence that the two memories are
about the same topic, so it satisfies the step-(b) gate instead of being skipped by
it. "not … Redis" is only meaningful *because* the other memory is about Redis.
Jaccard stays the primary gate; this is the single documented exception to it, and
it is the deviation the test suite pins explicitly
(`TestJaccardIndexOfTheDocumentedFixtures` asserts the 0.250 that makes it
necessary, so if that number ever rises above the threshold the deviation becomes
visible as an unused rule rather than staying silently load-bearing).

### Files

| File | Lines | Contents |
| --- | --- | --- |
| `internal/conflict/detector.go` | 151 | package doc, `DefaultJaccardThreshold`, `negationLookahead`, `negationTokens`, `ContradictionDetector`, `NewContradictionDetector`, `Threshold`, `Detect` |
| `internal/conflict/signals.go` | 212 | the predicate/stopword vocabularies, `negatesTokenIn`, `swapsValueForPredicate`, `sharedPredicates`, `firstIndex`, `objectAfter` |
| `internal/conflict/tokens.go` | 73 | `tokenize`, `tokenSet`, `intersectionSize`, `jaccardSimilarity` |
| `internal/conflict/detector_test.go` | 281 | the four fixtures, the threshold table, the constructor and self/empty contracts |
| `internal/conflict/tokens_test.go` | 77 | step-(a) tokenization, and the measure's boundaries |
| `internal/config/config.go` | +46 | `ConflictJaccardThreshold` / `ConflictScorePenalty`, defaults 0.4 / 0.5, one negative-value guard |
| `internal/config/config_test.go` | +32 | `TestDefaultConfigConflictKnobs` |
| `synapse.yaml.example` | +21 | both keys, each commented |

Three files in the package instead of one because the project caps a file at 300
lines; the split is by concern (policy / vocabularies and signals / text handling),
not by size alone. Same reason `tokens_test.go` exists separately.

Two smaller deviations from the spec's letter, beyond the gate one above, both
documented at their definitions:

- **The negation window is 3 tokens, not strict adjacency.** "not" is followed by
  "using", which does not equal the "use" in the other memory, because nothing here
  stems. Within the window, any token that is a stopword or that the other memory
  does not contain is ignored, so the rule still requires a real target.
- **The value-swap rule is "same predicate, different *direct object*"** — the
  first non-stopword token after the first occurrence of a shared predicate verb
  from a curated list, in each memory. The spec's wording ("same predicate word,
  different object noun") is implemented as written but needs a predicate list and
  a position rule to be checkable at all; this is what keeps `We use Postgres for
  billing` from contradicting `We use Postgres for analytics`, which the looser
  "shared verb plus any differing noun" reading would have flagged.

### Verification

```text
$ gofmt -l internal/conflict          # prints nothing
$ go vet ./internal/conflict/... ./internal/config/...
$ go test ./internal/conflict/... -v -count=1
=== RUN   TestJaccardIndexOfTheDocumentedFixtures
--- PASS: TestJaccardIndexOfTheDocumentedFixtures (0.00s)
=== RUN   TestDetectSameTopicDifferentValue
--- PASS: TestDetectSameTopicDifferentValue (0.00s)
=== RUN   TestDetectSameTopicNoSignal
--- PASS: TestDetectSameTopicNoSignal (0.00s)
=== RUN   TestDetectDifferentTopic
--- PASS: TestDetectDifferentTopic (0.00s)
=== RUN   TestDetectNegationTargetsExistingToken
--- PASS: TestDetectNegationTargetsExistingToken (0.00s)
=== RUN   TestDetectNegationWithoutTarget
--- PASS: TestDetectNegationWithoutTarget (0.00s)
=== RUN   TestDetectValueSwapOnlyWhenTheObjectDiffers
--- PASS: TestDetectValueSwapOnlyWhenTheObjectDiffers (0.00s)
=== RUN   TestDetectSkipsSelfAndEmptyContent
--- PASS: TestDetectSkipsSelfAndEmptyContent (0.00s)
=== RUN   TestDetectReturnsFirstContradictionInSliceOrder
--- PASS: TestDetectReturnsFirstContradictionInSliceOrder (0.00s)
=== RUN   TestDetectAcrossThresholds
--- PASS: TestDetectAcrossThresholds (0.00s)
=== RUN   TestNewContradictionDetectorThreshold
--- PASS: TestNewContradictionDetectorThreshold (0.00s)
=== RUN   TestTokenizeSplitsOnWhitespaceAndPunctuation
--- PASS: TestTokenizeSplitsOnWhitespaceAndPunctuation (0.00s)
=== RUN   TestTokenizeKeepsOrderAndDuplicates
--- PASS: TestTokenizeKeepsOrderAndDuplicates (0.00s)
=== RUN   TestTokenizeNothingToSplit
--- PASS: TestTokenizeNothingToSplit (0.00s)
=== RUN   TestJaccardSimilarityEdges
--- PASS: TestJaccardSimilarityEdges (0.00s)
PASS
ok  	synapse/internal/conflict	0.006s

$ go test ./internal/config/... -v
--- PASS: TestDefaultConfigConflictKnobs (0.00s)
PASS
ok  	synapse/internal/config	0.008s

$ go test ./...
ok  	synapse/internal/api	0.547s
ok  	synapse/internal/budget	0.253s
ok  	synapse/internal/classifier	(cached)
ok  	synapse/internal/compiler	0.255s
ok  	synapse/internal/config	0.005s
ok  	synapse/internal/conflict	(cached)
ok  	synapse/internal/dedup	0.014s
ok  	synapse/internal/embedder	(cached)
ok  	synapse/internal/integration	1.658s
ok  	synapse/internal/plane	0.106s
ok  	synapse/internal/proxy	0.560s
ok  	synapse/internal/retrieval	0.017s
ok  	synapse/internal/scorer	0.013s
ok  	synapse/internal/store	3.429s
ok  	synapse/internal/supersession	0.008s
ok  	synapse/internal/sync	3.717s
ok  	synapse/internal/tenant	0.789s
ok  	synapse/internal/trace	0.173s
```

Two mutations were run to confirm the suite bites rather than passing for its own
reasons. Each was reverted afterwards (files restored from a copy taken before the
run, then the full suite re-run green to prove the restore):

```text
# 1. drop the "negation also satisfies the gate" clause from Detect
--- FAIL: TestDetectNegationTargetsExistingToken
    detector_test.go:139: expected "We are not using Redis" to contradict "We decided to use Redis"
--- FAIL: TestDetectAcrossThresholds
    detector_test.go:259: threshold 0.4, explicit negation: Detect(...) = false (id ""), want true
    detector_test.go:259: threshold 0.7, explicit negation: Detect(...) = false (id ""), want true
    detector_test.go:259: threshold 1, explicit negation: Detect(...) = false (id ""), want true
# 0.2 is absent above on purpose: at 0.2 the Jaccard gate alone lets the pair
# through, which is precisely the behaviour difference the deviation removes.

# 2. make swapsValueForPredicate return false immediately
    detector_test.go:180: expected "We use Postgres for billing" to contradict "We use MySQL for billing"
    detector_test.go:180: expected "We decided to use a Postgres cluster" to contradict "We decided to use MySQL"
--- FAIL: TestDetectSkipsSelfAndEmptyContent
--- FAIL: TestDetectReturnsFirstContradictionInSliceOrder
--- FAIL: TestDetectAcrossThresholds
```

`git diff --stat` for the phase is `internal/config/config.go +46`,
`internal/config/config_test.go +32`, `synapse.yaml.example +21`, plus the new
untracked `internal/conflict/`. No v1 package, no `bin/synapse` (it had been
rebuilt locally before this phase and is unrelated to it, so it is left out of the
commit), and no dependency change — `strings` and `unicode` from the stdlib.
`internal/config/config.go` and `internal/config/config_test.go` were already not
`gofmt`-clean before this phase; the added lines are clean, and the pre-existing
regions were deliberately left unreformatted rather than pulled into this diff.

### Known limitations (all deliberate, none silent)

1. **No stemming and no synonyms.** `using`/`use` and `Postgres`/`PostgreSQL` do not
   match; the negation window papers over the first case, nothing handles the second.
2. **Negation is checked in the candidate only** — A negates B. A candidate that
   affirms what an existing memory negates is not detected. Cheap to flip, but
   flipping widens the false-positive blast radius, so it was left as specified.
3. **The value-swap rule is positional.** `We store logs in S3` against `We store
   metrics in S3` is flagged. Conservative in the missed-contradiction direction,
   not in the false-positive one.
4. **A contradiction with no listed verb and no negation is missed**, e.g.
   `We run MySQL` against `We decided to use Postgres` — no shared predicate.
5. **`Detect` returns the first contradiction in slice order**, not the strongest,
   and exposes no score. Ranking stays the caller's job.
6. **Nothing is wired.** No write-path caller, no consumer of
   `ConflictScorePenalty`, no trace field, no API surface: a flagged contradiction
   would currently change nothing a user sees.

### Next phase

Wire it where supersession already runs — on the candidate pool retrieval fetched
for a turn — and settle the three things this phase deliberately left open: whether
a flagged memory is dropped, penalised, or merely labelled; whether the penalty
lands on the older memory or the newer one; and how the conflict reaches the trace,
so a user can see *why* a memory was marked. That last one is the same
differentiation requirement Phase 11 satisfied for agent attribution.

## Phase 13 — conflict detection wired into Write and the scorer (complete)

Commit `feat: Phase 13 - conflict detection wired into Write and scorer penalty`

Phase 12's detector has a caller, `ConflictScorePenalty` has a consumer, and a
contradiction is now recorded on **both** memories instead of either one being
dropped:

- `PGStore.Write` fetches the tenant's most recent org-scoped, not-yet-superseded
  memories **once per write** (one query, limit 20), runs the installed detector over
  that slice in memory, stores the new memory as `conflict` with `conflict_with_id`
  naming the memory it disagrees with, and then marks the memory it contradicts as
  `superseded_candidate` naming the new one. Both rows stay in every read; the scorer
  is what demotes.
- `scorer` multiplies `Total` by `Weights.ConflictScorePenalty` (default 0.5, fed
  from `config.ConflictScorePenalty` by whoever builds the weights) for a memory whose
  `ConflictStatus` is `superseded_candidate`. Nothing else about a score moves: S, R,
  I and T stay the honest per-factor breakdown, so a caller sees the demotion *and*
  the reason for it.

### The one decision the spec left contradictory, and how it was settled

The phase text said two different things, and they cannot both hold:

- step 1c: the **new** memory becomes `conflict`, the memory it contradicts becomes
  `superseded_candidate`;
- step 3: MySQL (the second write) has `superseded_candidate` and Postgres has
  `conflict`, with `MySQL.Total = Postgres.Total * 0.5`.

Those are inverses of each other, because only `superseded_candidate` is penalised.
Phase 12's PROGRESS notes had already flagged the open question ("whether the penalty
lands on the older memory or the newer one"), so it was put to the user rather than
guessed at. The answer taken is **step 1c literal**, on the grounds that:

1. it matches the direction `superseded_by` already has in v1 — the field that means
   "I am on my way out" is written on the *older* row and points at the newer one, and
   `superseded_candidate` is the cautious version of exactly that statement;
2. it makes the penalty break ties toward the freshest claim, which is what
   supersession itself is for — otherwise writing a new decision would immediately
   rank it below the decision it contradicts;
3. `conflict_with_id` then points the same way as `superseded_by` on both rows.

The cost is a documented deviation from step 3's literal text: the test asserts
`Postgres (superseded_candidate).Total == MySQL (conflict).Total * 0.5`, i.e. the
inverse of the line the phase wrote. Flipping it is a two-line change in
`detectConflict` plus the equivalent test swap.

### Wiring, and why the interface is declared in `store`

`internal/conflict` imports `internal/store` (it takes a `MemoryEntry`), so `store`
cannot import `conflict` back. `PGStore` therefore declares the one method it calls —
`ConflictDetector.Detect(candidate, existing) (bool, string)` — which
`*conflict.ContradictionDetector` satisfies structurally, and takes it through
`PGStore.SetConflictDetector`. A consumer owning the interface it calls is the shape
`plane.MemoryWriter` and `tenant.Provisioner` already use, and the setter (rather than
a `NewPGStore` parameter) kept every existing construction path — the store factory,
tenant provisioning, the tests — compiling unchanged.

It is installed in production at `cmd/plane`, because the plane's sync endpoint is the
only path into a tenant's schema: a contradiction is by definition between memories
that arrived from different agents, so the plane is the only place it can be seen.
`tenant.MemoryWriter.SetConflictDetector` forwards it to every store the writer opens,
including the ones it has already opened, so a tenant's store is never briefly live
without it.

### Files

| File | Change |
| --- | --- |
| `internal/store/pgconflict.go` | new: `ConflictStatus*` constants, the `ConflictDetector` interface, `SetConflictDetector`, `conflictCandidates` (one org-scoped live query, limit 20), `detectConflict`, `markSupersededCandidate`, `ConflictDetectionBudget` |
| `internal/store/pgwrite.go` | new: `sanitized` and `MarkSuperseded` moved out of `pgstore.go` to keep it inside the 300-line ceiling |
| `internal/store/pgstore.go` | `Write` runs the detection block before the insert, stores the two new columns, and marks the contradicted memory after it; `detector` field; `MarkSuperseded` moved out |
| `internal/store/pgread.go` | `pgColumns` and `scanEntry` carry `conflict_status` and `conflict_with_id` |
| `internal/store/store.go` | `MemoryEntry.ConflictStatus` / `.ConflictWithID`, documented as written by the Postgres write path |
| `internal/scorer/weights.go` | `Weights.ConflictScorePenalty`, `DefaultConflictScorePenalty = 0.5`, `Weights.conflictPenalty()` |
| `internal/scorer/scorer.go` | 3 lines: scale `Total` when the memory is a superseded candidate |
| `internal/tenant/memorywriter.go` | `SetConflictDetector`, installed on every store `storeFor` opens |
| `cmd/plane/main.go` | one line installing `conflict.NewContradictionDetector(conflict.DefaultJaccardThreshold)` |
| `internal/store/conflict_test.go` | new, package `store_test`: the end-to-end behaviour |
| `internal/store/pgconflict_test.go` | new, package `store`: what the block reads, how often, and what it costs |

No v1 package was touched other than `internal/store` and `internal/scorer`, the two
this phase was allowed to change. `MemoryEntry`'s new fields are additive and
`omitempty`, and the SQLite backend has no column for them, so the standalone edge
node's storage, its JSON, and the sync queue are unaffected.

### Verification

The phase's own definition of done, in full:

```text
$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/store/... -run TestConflict -v
=== RUN   TestConflictCandidateFetch
=== RUN   TestConflictCandidateFetch/it_is_the_newest_org-scoped_memories,_capped_at_the_limit
=== RUN   TestConflictCandidateFetch/a_private,_team-scoped_or_superseded_memory_is_never_a_candidate
=== RUN   TestConflictCandidateFetch/the_fetch_stays_inside_the_detection_budget
    pgconflict_test.go:125: conflictCandidates over 20 memories: median 701.726µs, worst of 20 911.283µs (budget 20ms)
--- PASS: TestConflictCandidateFetch (0.60s)
    --- PASS: TestConflictCandidateFetch/it_is_the_newest_org-scoped_memories,_capped_at_the_limit (0.00s)
    --- PASS: TestConflictCandidateFetch/a_private,_team-scoped_or_superseded_memory_is_never_a_candidate (0.00s)
    --- PASS: TestConflictCandidateFetch/the_fetch_stays_inside_the_detection_budget (0.01s)
=== RUN   TestConflictWriteConsultsTheDetectorOncePerWrite
--- PASS: TestConflictWriteConsultsTheDetectorOncePerWrite (0.57s)
=== RUN   TestConflictMarksBothMemories
2026/09/21 11:48:13 WARN conflict_detected new_id=dc46c769-4d8e-4212-9217-903dc9a116aa conflicts_with=1345b3b4-ceae-45e1-a734-39f2e640dd7b
    conflict_test.go:164: agent_a ("We decided to use Postgres"): conflict_status="superseded_candidate" conflict_with_id="dc46c769-4d8e-4212-9217-903dc9a116aa"
    conflict_test.go:165: agent_b ("We decided to use MySQL"): conflict_status="conflict" conflict_with_id="1345b3b4-ceae-45e1-a734-39f2e640dd7b"
    conflict_test.go:168: whole write that detects: 21.386468ms; the write before it: 11.243041ms
=== RUN   TestConflictMarksBothMemories/the_older_memory_becomes_a_superseded_candidate
=== RUN   TestConflictMarksBothMemories/the_newer_memory_is_marked_as_conflicting
=== RUN   TestConflictMarksBothMemories/both_memories_stay_in_the_pool_and_keep_their_marker
=== RUN   TestConflictMarksBothMemories/the_flagged_memory_scores_the_penalty_times_the_other
    conflict_test.go:221: superseded candidate: Total=0.450000 (S=1.0000 R=1.0000 I=1.0000 T=0.5000)
    conflict_test.go:223: conflicting memory:   Total=0.900000 (S=1.0000 R=1.0000 I=1.0000 T=0.5000)
    conflict_test.go:225: penalty=0.5
=== RUN   TestConflictMarksBothMemories/the_detection_comparison_stays_inside_the_budget
    conflict_test.go:267: one comparison over 20 candidates: 3.458µs (budget 20ms)
--- PASS: TestConflictMarksBothMemories (0.29s)
    --- PASS: TestConflictMarksBothMemories/the_older_memory_becomes_a_superseded_candidate (0.00s)
    --- PASS: TestConflictMarksBothMemories/the_newer_memory_is_marked_as_conflicting (0.00s)
    --- PASS: TestConflictMarksBothMemories/both_memories_stay_in_the_pool_and_keep_their_marker (0.00s)
    --- PASS: TestConflictMarksBothMemories/the_flagged_memory_scores_the_penalty_times_the_other (0.00s)
    --- PASS: TestConflictMarksBothMemories/the_detection_comparison_stays_inside_the_budget (0.00s)
=== RUN   TestConflictWithoutADetectorMarksNothing
--- PASS: TestConflictWithoutADetectorMarksNothing (0.22s)
PASS
ok  	synapse/internal/store	1.682s
```

The whole suite, with and without a database (the Postgres-backed tests skip when the
DSN is unset, so CI with no Postgres stays green):

```text
$ go test ./...          # no DSN: every Postgres-backed test skips
ok  synapse/internal/api         ok  synapse/internal/budget      ok  synapse/internal/classifier
ok  synapse/internal/compiler    ok  synapse/internal/config      ok  synapse/internal/conflict
ok  synapse/internal/dedup       ok  synapse/internal/embedder    ok  synapse/internal/integration
ok  synapse/internal/plane       ok  synapse/internal/proxy       ok  synapse/internal/retrieval
ok  synapse/internal/scorer      ok  synapse/internal/store       ok  synapse/internal/supersession
ok  synapse/internal/sync        ok  synapse/internal/tenant      ok  synapse/internal/trace

$ SYNAPSE_TEST_DB_DSN=... go test ./...
ok  synapse/internal/store	5.685s
ok  synapse/internal/tenant	1.150s
... every other package unchanged
```

`go vet ./...` is clean and `gofmt -l` lists none of the new files. Three mutations plus
one control were run to confirm the new tests bite rather than passing for their own
reasons; each was reverted afterwards and the suite re-run green:

```text
# 1. drop the penalty multiplication from scoreMemory
--- FAIL: TestConflictMarksBothMemories
    --- FAIL: .../the_flagged_memory_scores_the_penalty_times_the_other
        Error: Max difference between 0.8999999625747428 and 0.4499999812873714 allowed is 0.001
        Messages: a superseded candidate's Total must be exactly the penalty times the score of the memory it conflicts with

# 2. mark the contradicted memory 'conflict' instead of 'superseded_candidate'
--- FAIL: .../the_older_memory_becomes_a_superseded_candidate
--- FAIL: .../both_memories_stay_in_the_pool_and_keep_their_marker

# 3. mark the new memory 'superseded_candidate' instead of 'conflict'
--- FAIL: .../the_newer_memory_is_marked_as_conflicting
--- FAIL: .../the_flagged_memory_scores_the_penalty_times_the_other

# 4. (control) no detector installed
--- PASS: TestConflictWithoutADetectorMarksNothing
    both rows read back conflict_status='none'
```

One further mutation attempt is worth naming: removing the marking block from `Write`
does not compile (`conflictingID` declared and not used), which is why mutations 2 and 3
mutate the values instead of the control flow.

### The 20ms budget, and why it is measured in two halves rather than on a write

The phase asks for the detection block to complete in under 20ms. The first attempt
timed the whole detecting write and failed at 65.9ms — but profiling showed the insert
was the cost, not the detection:

```text
$ SYNAPSE_TEST_DB_DSN=... go test ./internal/store -run TestScratchTiming -v   # scratch, removed
round trip 0: 458.713µs            # SELECT 1: sub-millisecond round trips
round trip 9: 190.298µs
insert without embedding 0: 22.237894ms   # no detector, no embedding, still ~11ms
insert without embedding 4: 11.070698ms
insert with embedding 4: 11.07799ms
conflictCandidates (20 rows) run 1: 905.735µs
whole Write with detector run 3: 11.188997ms
```

Every insert costs ~11ms on this machine because it is its own transaction and pays the
write-ahead-log fsync; `SELECT 1` costs 0.19ms. A wall-clock budget on a write is
therefore a test of the database's fsync latency, which is why one run in six measured a
43ms "block" that contained no detection at all.

So the block is measured as the two things it actually is, each where it can be measured
without that noise:

- the **fetch**, in package `store` (`TestConflictCandidateFetch`): a plain SELECT, whose
  median over 20 runs is ~0.7ms and whose worst sample has never exceeded 1.2ms — a margin of
  more than 15× against the 20ms budget,
  asserted on the median with the worst sample logged;
- the **comparison**, in package `store_test` (the budget subtest): the real detector over
  20 candidates, 3–9µs per comparison across runs.

`ConflictDetectionBudget = 20ms` is exported from `internal/store` for exactly this: the
write path logs a `conflict_detection_slow` warning when a block exceeds it, and both
tests measure against the same number rather than two copies of it. The warning is not
enforcement — a slow block still stores the memory — because detection is an annotation on
a row that is otherwise perfectly storable.

### Known limitations (all deliberate, none silent)

1. **The insert and the marking update are not one transaction.** A failure or crash
   between them leaves the new row `conflict` and the older one unmarked. An edge retry
   converges (the insert is idempotent on the id, and detection re-runs), but a push that
   never retries leaves the pair half-marked. Fixing it properly means wrapping the block
   in a transaction, which changes the write path's retry story that Phase 8 built.
2. **The candidate query has no covering index.** `WHERE superseded_by IS NULL AND
   visibility = 'org' ORDER BY created_at DESC LIMIT 20` is a sort over the tenant's live
   org-scoped rows; the tenant DDL's index is on `(session_id, created_at DESC)`. Fine at
   the sizes measured here, worth an index before a tenant holds tens of thousands of
   memories.
3. **Detection is write-time only.** A memory is compared against what existed when it was
   written. Two contradicting memories pushed in the same batch are compared in order, so
   the pair is found, but a contradiction is never revisited later.
4. **The threshold is the detector's default.** `cmd/plane` installs
   `conflict.DefaultJaccardThreshold`; the plane's config has no conflict key yet, so an
   operator cannot tune it there. `config.ConflictJaccardThreshold` (synapse.yaml) is read
   by the standalone binary, which does not write to a tenant schema.
5. **Everything Phase 12 listed about the detector itself still holds** — no stemming, no
   synonyms, negation checked in the candidate only, a positional value-swap rule, and
   first-contradiction-in-slice-order rather than the strongest. A false positive now
   costs a real 0.5× penalty on the older memory, which is the first time it costs
   anything at all.
6. **The conflict reaches the stored row and the plane's JSON, but not the trace or a
   score breakdown.** `GET /v2/memories/search` echoes `store.MemoryEntry`, so
   `conflict_status` and `conflict_with_id` are already visible there for a marked memory.
   What is missing is the *why*: the plane's response carries no S/R/I/T breakdown and no
   trace id, and the edge's own trace has no field for a conflict, so a demoted score
   still cannot be explained to a user. That is the last piece of the differentiation
   story Phase 11 started for agent attribution.

### Next phase

Carry the conflict into the memory trace (id, status, and the id of the memory it
conflicts with) so a user can see why a memory was demoted, and expose it on the plane's
search response alongside the S/R/I/T breakdown. Then the two knobs that are still
unreachable in production — the plane's conflict threshold, and a resolution path for a
flagged contradiction — become worth wiring.


## Phase 14 — conflict fields in Memory Trace (complete)

Commit `feat: Phase 14 - conflict fields in Memory Trace`

`conflict_status` and `conflict_with_id` now appear on **every** memory entry in the
Memory Trace JSON, so the demotion Phase 13's scorer applies carries its reason with it.
Phase 13 signed off by naming this exact gap — "the conflict reaches the stored row and
the plane's JSON, but not the trace or a score breakdown ... a demoted score still cannot
be explained to a user" — and this phase closes the trace half of it.

```json
{
  "id": "bad88243-97b3-5b84-a4cc-15a0785025c4",
  "memory_type": "decision",
  "content_preview": "We decided to use Postgres",
  "score_semantic": 0.4755730099875433,
  "score_recency": 0.9996831933994552,
  "score_importance": 1,
  "score_task_alignment": 0.5,
  "score_total": 0.3450987616674814,
  "included": true,
  "agent_id": "agent_a",
  "cross_agent": true,
  "conflict_status": "superseded_candidate",
  "conflict_with_id": "d786bcc7-90aa-54a7-a074-328c315d43e7"
}
```

That is a real entry from the live run below (nothing trimmed — this is the full JSON of
the memory). It is the whole phase in one object: `agent_a`'s decision (Phase 11's
provenance), a `score_total` that is exactly half its own S/R/I/T breakdown (Phase 13's
penalty: 0.69021415 × 0.5 = 0.34510708), and now the two fields that say *why*.

### One field addition, and the one place it happens

`TraceMemory` is built in exactly one place — `internal/trace/trace.go`, inside
`NewTraceManifest` — and every caller reaches it through that function:
`compiler.Compile` (which `/v1/compile` and `/api/playground/compile` both call), the
live proxy path, the `X-Synapse-Trace` header, and the manifest the session inspector is
served. So the fill is one assignment there, and `internal/compiler` needed no production
change at all: `Compile` only forwards `[]scorer.ScoredMemory`, and `ScoredMemory`
**embeds** `store.MemoryEntry`, which has carried `ConflictStatus`/`ConflictWithID` since
Phase 13 (`pgread.scanEntry` fills them from the two Postgres columns, and the plane's
`/v2/memories/search` response serializes the same struct to the edge). Adding a second
assignment in the compiler would have been dead code whose only possible future was to
drift from the first. The phase's compiler-side proof is therefore a test
(`TestCompile_TraceCarriesConflictFields`, below) rather than a no-op edit.

Two decisions inside the field addition are worth naming:

- **`"none"` is written, not omitted.** A memory whose backend records no conflict at all
  (the local SQLite table has no such column) and a plane memory no detector has looked
  at both arrive with an empty `ConflictStatus`, and the trace normalizes them to `"none"`
  rather than dropping the key. `"none"` is a *statement* — nobody has contradicted this
  memory — whereas an absent key would leave a reader unable to tell that from a node too
  old to report conflicts at all. That is the same reasoning `agent_id`/`cross_agent`
  carry, and for the same reason neither new field has `omitempty`.
- **The status values are referenced, not re-typed.** `store.ConflictStatusNone` is used
  rather than a local literal, because those constants are the SQL literals the write path
  stores, and `internal/scorer` already imports `internal/store` for
  `ConflictStatusSupersededCandidate`. There is no cycle: `store` imports only `config`,
  and `trace` already depended on `store` transitively through `scorer`.
  `conflict_with_id` is passed through verbatim — the store is the only thing that knows
  which side names which — and stays an empty string when there is no counterpart.

### Files

| File | Change |
| --- | --- |
| `internal/trace/trace.go` | `TraceMemory.ConflictStatus` / `.ConflictWithID`, both marshaled unconditionally; the `"none"` normalization and the two assignments inside `NewTraceManifest`; `store` import |
| `internal/trace/trace_test.go` | `TestTraceMemory_ConflictFieldsAlwaysMarshaled`: both keys on every entry, `"none"` when unset, the counterpart id when set |
| `internal/compiler/compiler_test.go` | `TestCompile_TraceCarriesConflictFields`: the compiler → trace hop on a plane-shaped Postgres/MySQL pair, asserting the Phase 11 fields on the same entries are untouched |
| `schemas/memory-trace.schema.json` | both properties (`conflict_status` with its three-value enum, `conflict_with_id` deliberately without `format: uuid`) added to `memories.items.properties` **and** to `items.required` |
| `openapi.yaml` | the same two fields and the same two `required` entries on `TraceMemory` — the published contract for this payload, kept in step exactly as Phase 11 kept `agent_id`/`cross_agent` |
| `ui/session.html` | `.conflict-badge` (amber) and the badge in `renderMemoryTrace` |
| `ui/index.html` | the same badge and rule, written independently — the two inspectors are separate implementations |
| `PROGRESS.md` | this section |

The badge is amber, not the burn orange of the exclusion badge or the violet of
cross-agent, because a flagged memory is neither rejected nor merely someone else's — it
is a claim another memory disagrees with, and a `superseded_candidate` keeps its place in
the pool. Its `title` names the counterpart id, so the tooltip answers "conflicts with
what?" without a click; an entry whose status is `"none"` renders no badge, and the modal
in both inspectors is a `JSON.stringify` dump, so both fields were already visible there
with no change.

### Verification

The phase's two new tests first, then the whole suite:

```text
$ go test ./internal/trace/... ./internal/compiler/... -run Conflict -v
=== RUN   TestTraceMemory_ConflictFieldsAlwaysMarshaled
--- PASS: TestTraceMemory_ConflictFieldsAlwaysMarshaled (0.21s)
PASS
ok  	synapse/internal/trace	0.214s
=== RUN   TestCompile_TraceCarriesConflictFields
--- PASS: TestCompile_TraceCarriesConflictFields (0.19s)
PASS
ok  	synapse/internal/compiler	0.202s

$ go build ./... && go vet ./... && go test ./...
ok  	synapse/internal/api	1.042s
ok  	synapse/internal/budget	(cached)
ok  	synapse/internal/classifier	(cached)
ok  	synapse/internal/compiler	0.260s
ok  	synapse/internal/config	(cached)
ok  	synapse/internal/conflict	(cached)
ok  	synapse/internal/dedup	(cached)
ok  	synapse/internal/embedder	(cached)
ok  	synapse/internal/integration	1.333s
ok  	synapse/internal/plane	(cached)
ok  	synapse/internal/proxy	0.247s
ok  	synapse/internal/retrieval	(cached)
ok  	synapse/internal/scorer	(cached)
ok  	synapse/internal/store	(cached)
ok  	synapse/internal/supersession	(cached)
ok  	synapse/internal/sync	(cached)
ok  	synapse/internal/tenant	(cached)
ok  	synapse/internal/trace	0.193s

$ SYNAPSE_TEST_DB_DSN='postgres://...' go test ./internal/store/... ./internal/tenant/...
ok  	synapse/internal/store	6.741s
ok  	synapse/internal/tenant	1.245s
```

Three mutations, each reverted afterwards, run to confirm the new tests bite:

```text
# 1. normalize to the wrong status instead of "none"
--- FAIL: TestTraceMemory_ConflictFieldsAlwaysMarshaled (0.20s)
    trace_test.go:260: plain-1: conflict_status="conflict" conflict_with_id="", want "none"/""
    trace_test.go:304: plain-1 conflict_status = conflict, want "none"

# 2. stop passing conflict_with_id through
--- FAIL: TestTraceMemory_ConflictFieldsAlwaysMarshaled (0.20s)
    trace_test.go:307: old-decision conflict_with_id = , want "new-decision"
    trace_test.go:307: new-decision conflict_with_id = , want "old-decision"

# 3. the compiler test's fixture loses its superseded_candidate status
--- FAIL: TestCompile_TraceCarriesConflictFields (0.33s)
    compiler_test.go:293: pg-1 conflict_status = conflict, want "superseded_candidate"
```

One mutation is worth naming because it cannot be run: deleting the normalization
outright (`conflictStatus = ""`) does not compile — `"synapse/internal/store" imported and
not used` — which is why mutation 1 substitutes a different status value instead of
removing the line. The assertion is still tested, because a version of the code that both
kept the `store` import and stopped normalizing (mutation 1) fails.

#### Live run: a two-agent contradiction, end to end

Real binaries, real Postgres (the compose `db` service from Phase 4, already healthy on
`127.0.0.1:5432`), real ONNX embeddings. The plane ran with its four secrets from the
environment; two edge nodes (`agent_a` on `:8081`, `agent_b` on `:8082`) pointed at it, each
with its own SQLite file, so the contradiction is between memories that arrived from
different agents through the plane's own sync endpoint. The JWT-bearing `tenant.json` and
both edge configs were written 0600, are never committed, and were shredded at the end.

```text
$ SYNAPSE_DB_DSN='postgres://...' SYNAPSE_JWT_SECRET='…' SYNAPSE_ADMIN_TOKEN='…' \
  SYNAPSE_MASTER_KEY='…' /tmp/synapse-phase14/bin/plane &
$ curl -s http://127.0.0.1:9090/health
{"status":"ok","version":"2.0.0","db":"connected"}

$ curl -s -X POST http://127.0.0.1:9090/v2/tenants -H 'Authorization: <admin token>' \
      -H 'Content-Type: application/json' -d '{"slug":"phase14-edge"}' -o tenant.json
$ jq '{tenant_id, jwt_len: (.jwt|length), api_key_len: (.api_key|length)}' tenant.json
{ "tenant_id": "c2796f14-0e7b-4de8-8599-061d563c27db", "jwt_len": 387, "api_key_len": 64 }

$ /tmp/synapse-phase14/bin/synapse --config /tmp/synapse-phase14/edge_a.yaml &   # agent_a, :8081
INFO synapse: sync: background flusher started agent_id=agent_a interval_seconds=30
INFO synapse: ONNX embedder initialized with real inference model=models/all-MiniLM-L6-v2/model.onnx
$ /tmp/synapse-phase14/bin/synapse --config /tmp/synapse-phase14/edge_b.yaml &   # agent_b, :8082

# --- each agent writes the phase-13 fixture through its own node --------------------
$ curl -s -X POST http://127.0.0.1:8081/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase14-a","messages":[{"role":"user","content":"We decided to use Postgres"}]}'
HTTP 200   # .trace.candidates_retrieved = 0: the tenant's schema is still empty
$ curl -s -X POST http://127.0.0.1:8082/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase14-b","messages":[{"role":"user","content":"We decided to use MySQL"}]}'
HTTP 200

# each node's own row, with the real 384-float embedding the ONNX model produced:
$ python3 -c "...select id, session_id, content, memory_type, sync_status, length(embedding)..."
edge_a: ('req-1789991868659587109', 'sess-phase14-a', 'We decided to use Postgres', 'decision', 'local_only', 1536)
edge_b: ('req-1789991868840546528', 'sess-phase14-b', 'We decided to use MySQL',    'decision', 'local_only', 1536)

# --- both rows go to the plane through the documented push protocol -----------------
# (postgres first, so the mysql write is the one that detects)
$ curl -s -X POST http://127.0.0.1:9090/v2/sync/memories -H "Authorization: Bearer $JWT" \
    -H 'Content-Type: application/json' -d @push_a.json          # envelope agent_id=agent_a
{"written":1,"sanitized":0} HTTP 200
$ curl -s -X POST http://127.0.0.1:9090/v2/sync/memories -H "Authorization: Bearer $JWT" \
    -H 'Content-Type: application/json' -d @push_b.json          # envelope agent_id=agent_b
{"written":1,"sanitized":0} HTTP 200
plane.log: INFO plane: Memories synced tenant_slug=phase14-edge agent_id=agent_a written=1 sanitized=0
plane.log: WARN conflict_detected new_id=d786bcc7-90aa-54a7-a074-328c315d43e7 \
                                   conflicts_with=bad88243-97b3-5b84-a4cc-15a0785025c4
plane.log: INFO plane: Memories synced tenant_slug=phase14-edge agent_id=agent_b written=1 sanitized=0

# --- what the plane stored ----------------------------------------------------------
$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    "select agent_id, conflict_status, coalesce(conflict_with_id::text,'') as conflict_with_id, \
     left(content, 26) as content from tenant_phase14_edge.memories order by created_at"
 agent_id |   conflict_status    |           conflict_with_id           |          content
----------+----------------------+--------------------------------------+----------------------------
 agent_a  | superseded_candidate | d786bcc7-90aa-54a7-a074-328c315d43e7 | We decided to use Postgres
 agent_b  | conflict             | bad88243-97b3-5b84-a4cc-15a0785025c4 | We decided to use MySQL
```

```text
# --- the compile that has to show it: a third session, asked a question -------------
$ curl -s -X POST http://127.0.0.1:8082/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase14-query","messages":[{"role":"user","content":"what did we decide about the database?"}]}' \
    -o compile_conflict.json -w 'HTTP %{http_code} in %{time_total}s\n'
HTTP 200 in 0.082711s
plane.log: INFO plane: Memories searched tenant_slug=phase14-edge agent_id="" \
           request_agent_id=agent_b memories=2

# both keys on every entry -- asserted, not eyeballed (the DoD's own wording):
$ jq 'all(.trace.memories[]; has("conflict_status") and has("conflict_with_id"))' compile_conflict.json
true

# the entries, exactly as the trace JSON carries them (ids elided only here):
$ jq -c '.trace.memories[]' compile_conflict.json
{"id":"d786bcc7-…","memory_type":"decision","content_preview":"We decided to use MySQL",
 "score_semantic":0.565985208128604,"score_recency":0.9998509403569839,"score_importance":1,
 "score_task_alignment":0.5,"score_total":0.72637917728714,"included":true,"agent_id":"agent_b",
 "cross_agent":false,"conflict_status":"conflict","conflict_with_id":"bad88243-97b3-5b84-a4cc-15a0785025c4"}
{"id":"bad88243-…","memory_type":"decision","content_preview":"We decided to use Postgres",
 "score_semantic":0.4755730099875433,"score_recency":0.9998494888241959,"score_importance":1,
 "score_task_alignment":0.5,"score_total":0.34510707643871846,"included":true,"agent_id":"agent_a",
 "cross_agent":true,"conflict_status":"superseded_candidate","conflict_with_id":"d786bcc7-90aa-54a7-a074-328c315d43e7"}

# and the demoted Total is the penalty times that entry's own S/R/I/T breakdown
# (the edge's configured weights, 0.4/0.1/0.3/0.2):
We decided to use MySQL     conflict_status=conflict             raw=0.72637918 trace_total=0.72637918 ratio=1.0000
We decided to use Postgres  conflict_status=superseded_candidate raw=0.69021415 trace_total=0.34510708 ratio=0.5000
```

Both memories survive dedup (their cosine similarity stays under the 0.92 threshold) and
both are `included`, which is the phase's contract: a flagged memory is demoted, never
dropped. The flagged one is the *older* row and the one marked `conflict` is the *newer*
row — Phase 13's settled direction, now legible in the output instead of only in the
store.

```text
# --- the schema validates the real output -------------------------------------------
$ python3 -c "jsonschema.validate(trace, schema)"
valid against schemas/memory-trace.schema.json
both keys present on all 2 entries
```

```text
# --- the payload the session inspector itself polls ---------------------------------
$ curl -s http://127.0.0.1:8082/api/sessions | jq -c '.[] | {id, message_count}'
{"id":"0c73c7fc-788c-4d27-88f4-db29ed30b63c","message_count":1}
$ curl -s http://127.0.0.1:8082/api/sessions/0c73c7fc-788c-4d27-88f4-db29ed30b63c/trace \
    | jq 'all(.memories[]; has("conflict_status") and has("conflict_with_id"))'
true
# (that trace came from one request through the proxy path -- see the finding below about
#  which path populates the inspector -- and its Postgres entry carries the same
#  conflict_status/conflict_with_id pair, with score_total 0.3450987616674814)

# --- both inspectors actually render it ---------------------------------------------
$ curl -s http://127.0.0.1:8082/ui -o ui_index.html
$ curl -s http://127.0.0.1:8082/ui/session -o ui_session.html
$ grep -c 'conflict-badge' ui_index.html ui_session.html
ui_index.html:2
ui_session.html:2

# Each UI's own renderMemoryTrace, extracted from the HTML the server is serving and run
# against those entries in node, so this is the markup a browser inserts, not a
# description of it:
ui/session.html
conflict              -> <span class="conflict-badge" title="This memory introduced a contradiction, conflicts with bad88243-97b3-5b84-a4cc-15a0785025c4">conflict</span>
superseded_candidate  -> <span class="conflict-badge" title="A newer memory contradicts this one, so the scorer demotes it, conflicts with d786bcc7-90aa-54a7-a074-328c315d43e7">superseded candidate</span>
none                  -> (no conflict badge rendered)
ui/index.html
conflict              -> <span class="conflict-badge" title="This memory introduced a contradiction, conflicts with bad88243-97b3-5b84-a4cc-15a0785025c4">conflict</span>
superseded_candidate  -> <span class="conflict-badge" title="A newer memory contradicts this one, so the scorer demotes it, conflicts with d786bcc7-90aa-54a7-a074-328c315d43e7">superseded candidate</span>
none                  -> (no conflict badge rendered)
```

```text
# --- control: the plane is killed, so edge_b compiles from its own SQLite -------------
$ kill <plane pid>; curl -s -m 2 http://127.0.0.1:9090/health
connection refused
$ curl -s -X POST http://127.0.0.1:8082/v1/compile -H 'Content-Type: application/json' \
    -d '{"session_id":"sess-phase14-b","messages":[{"role":"user","content":"what did we decide about the database?"}]}'
HTTP 200
edge_b.log: WARN Control plane candidate pull failed, falling back to local search \
            plane_unavailable=true fallback=local

# the field is still on the entry -- a local SQLite row, whose table has no conflict
# column at all, reports the normalized "none" rather than leaving the key out:
$ jq 'all(.trace.memories[]; has("conflict_status") and has("conflict_with_id"))' compile_offline.json
true
$ jq -c '.trace.memories[] | {content_preview, agent_id, conflict_status, conflict_with_id}' compile_offline.json
{"content_preview":"We decided to use MySQL","agent_id":"","conflict_status":"none","conflict_with_id":""}
```

Cleanup after the run: plane and both edges stopped, `tenant.json` and both edge configs
shredded (all three were 0600 and never committed), and no build artifact was written into
the repository — the two binaries for this run were built into
`/tmp/synapse-phase14/bin/`, because `bin/synapse` is tracked by git and was already dirty
from an earlier local build that this phase deliberately left untouched.

### Findings this phase surfaced (not fixed here — this phase is one field addition)

1. **Only the proxy path populates the session inspector.** `sessionMgr.SetTrace` is called
   in exactly one place (`internal/proxy/proxy.go`), and `/v1/compile` never calls it, so a
   session that only ever went through the API has no trace for
   `GET /api/sessions/{id}/trace` to serve — which is why the inspector payload above came
   from one request through the proxy route rather than from the `/v1/compile` that produced
   the trace. Pre-existing v1 behaviour, and now a visible gap: the trace that finally
   explains a demotion is the one the inspector cannot show an API-only caller.
2. **`exclusion_reason`'s enum is stale in both schema files.** `schemas/memory-trace.schema.json`
   and `openapi.yaml` both list `["deduped", "budget_exceeded", "null"]`, while the code emits
   `"superseded"` (Phase 5's own trace test asserts it) and omits the key entirely when there
   is no reason. The live trace in this phase happened to contain no superseded memory, which
   is why `jsonschema.validate` passed; one would fail. Left alone deliberately — this phase
   adds two fields and touches nothing else — but it is a one-line fix waiting for a phase
   that owns the schema.
3. **Dedup could have collapsed the pair, and the field would still have been there.** Both
   fixture sentences survived the 0.92 cosine threshold, so both appear as `included`. Two
   nearer-duplicate sentences would have left one labelled `duplicate` instead — and it would
   still have carried its `conflict_status`, because the field is independent of inclusion.
   That is the useful shape, but it means the "demoted and still included" pair above is a
   property of this fixture, not a guarantee.
4. **The plane still cannot explain a demotion to a caller.** The trace now carries the pair,
   but `GET /v2/memories/search` returns `store.MemoryEntry` with no S/R/I/T breakdown and no
   trace id, so the plane side of Phase 13's finding #6 is untouched, as are the two config
   knobs (`conflict-jaccard-threshold` and `conflict-score-penalty`) that still reach nothing
   in production.
5. **`gofmt -l` flags this file exactly as it flagged it before.** The house convention keeps
   the local `synapse/...` imports last inside a single import group, which gofmt wants
   re-sorted, and the repository is uniformly in that state. Running gofmt on `trace.go` here
   would have re-aligned `TraceManifest` as unrelated churn, so the new import was appended in
   the existing style instead.

### Next phase

Give the plane's own surface the same "why": the S/R/I/T breakdown and a trace id on
`GET /v2/memories/search`, so a demoted memory can be explained to a caller that never sees
the edge's trace — which is the shape `.clinerules` already requires of MCP responses
("score breakdown (S/R/I/T) and trace_id"), and therefore the shape the MCP phase will need
from the plane. The two unreachable config knobs
(`conflict-jaccard-threshold`, `conflict-score-penalty`) are the other half of that work.

## Phase 15 — ledger table and INSERT-only role (complete)

Scope was deliberately two things: create `synapse_global.ledger`, and make
"never update, never delete" a database privilege rather than a code convention.
No signing, no hash chain, no reader, and no wiring into the request path — the
table exists, the writer role exists, and the server refuses everything else.

Commit `feat: Phase 15 - ledger table and INSERT-only role`

New files:

- `internal/ledger/doc.go` — package comment only, 24 lines. It states the
  contract (rows in `synapse_global.ledger`, DDL owned by `internal/tenant`'s
  migration, one writer role holding INSERT and nothing else, no signing yet) and
  why an otherwise empty file exists at all: everything else in that directory
  carries `//go:build integration`, and a directory whose only files are all
  excluded by build constraints makes `go test ./...` fail outright with
  `NoGoError: build constraints exclude all Go files` instead of skipping — and
  that is exactly what CI runs (`ci.yml` → `go test -v ./...`, no build tags, no
  database), so the package would have broken CI the moment it was added.
- `internal/ledger/ledger_table_test.go` — 262 lines, `//go:build integration`:
  `TestLedgerTablePermissions` plus four helpers (`ledgerPool`, `hasPrivilege`,
  `runAsLedgerWriter`, `requirePermissionDenied`).

Changed files:

- `internal/tenant/migrations.go` — the exported `LedgerWriterRole` constant and
  seven new migration entries (`ledger`, `ledger_tenant_created_idx`,
  `ledger_writer_role`, `ledger_revoke_public`, `ledger_writer_schema_usage`,
  `ledger_writer_insert`, `ledger_writer_membership`), all inside the existing
  single transaction, all re-runnable. 206 lines, still under the 300-line cap.
- `internal/tenant/migrations_test.go` — `ledger` added to the table list of
  `TestRunMigrationsIsIdempotentAndCreatesEveryDocumentedTable`, which claims to
  check *every* documented table.

No new dependencies: `go.mod` is untouched, and nothing in the v1 internals was
touched either — `internal/tenant` and `internal/ledger` are both v2 packages.

Decisions made in this phase:

- **The index had to be named.** `CREATE INDEX` has no unnamed form: `CREATE
  INDEX IF NOT EXISTS ON synapse_global.ledger (tenant_id, created_at)` is a
  syntax error (`ERROR: syntax error at or near "ON"`), so the SQL as written in
  the phase brief would have aborted the plane's boot rather than creating
  anything. It is now `CREATE INDEX IF NOT EXISTS ledger_tenant_created_idx ON
  synapse_global.ledger (tenant_id, created_at)`, which is genuinely idempotent
  (`NOTICE: relation "ledger_tenant_created_idx" already exists, skipping`).
- **`GRANT USAGE ON SCHEMA synapse_global TO ledger_writer` was added** — one
  grant beyond what the brief listed. Without it `GRANT INSERT` reaches nothing:
  access to any object requires USAGE on its schema, and `synapse_global`'s ACL
  is empty, so PUBLIC holds none (unlike `public`). Verified both ways:
  `INSERT ... → ERROR: permission denied for schema synapse_global`, and with the
  grant → `INSERT 0 1`.
- **The role is granted to `CURRENT_USER`, not to a literal `synapse`.** The
  database user is deployment configuration (`SYNAPSE_DB_DSN`); a hardcoded name
  would abort the boot of any deployment that names its user differently, which is
  the opposite of what a control plane is for. `GRANT <role> TO CURRENT_USER` is
  valid syntax and resolves to exactly the role the brief calls "the application
  user", whoever that is.
- **The permission assertions run as `ledger_writer`, via `SET LOCAL ROLE`.** This
  is the phase's most important caveat rather than a test detail: `synapse` is a
  Postgres **superuser** and owns the ledger table, and a superuser bypasses every
  privilege check while an owner holds all of them — so no GRANT can bind the DSN
  the plane actually uses. The grants bind only an identity whose `current_user`
  is `ledger_writer`, which is precisely why the brief's `GRANT ledger_writer TO
  synapse` matters: that membership is what makes `SET LOCAL ROLE ledger_writer`
  possible, and it is the shape production has to adopt (finding 1). `SET LOCAL`
  rather than `SET ROLE`, inside an explicit transaction, so a pooled connection is
  never handed back still wearing the writer's identity.
- **`INSERT ... RETURNING` is not available to the writer.** `RETURNING` requires
  `SELECT` on the returned columns, so it fails with `permission denied for table`
  even though the INSERT itself is allowed (verified). Success is therefore judged
  by `RowsAffected() == 1` and confirmed by a read-back through the application
  connection — the only identity that can SELECT, because no SELECT was granted to
  the writer on purpose.
- **The probe INSERT is committed; the UPDATE and DELETE probes never are.** A
  rolled-back INSERT would only prove the statement was not refused, so the row is
  committed and then read back. The two refused statements run in transactions that
  are always rolled back, which means an *unexpected* success leaves the
  append-only ledger untouched and still fails the test.

Verification (real output, this phase):

```text
$ gofmt -l internal/ledger internal/tenant          # empty
$ wc -l internal/ledger/*.go internal/tenant/migrations.go
   24 internal/ledger/doc.go
  262 internal/ledger/ledger_table_test.go
  206 internal/tenant/migrations.go
$ go build ./...                                     # clean
$ go vet ./internal/ledger/... ./internal/tenant/...
$ go vet -tags integration ./internal/ledger/...
VET_OK
```

The definition of done, the command exactly as specified:

```text
$ go test ./internal/ledger/... -run TestLedgerTablePermissions -v -tags integration
=== RUN   TestLedgerTablePermissions
--- PASS: TestLedgerTablePermissions (0.10s)
PASS
ok  	synapse/internal/ledger	0.106s
```

UPDATE returns an error, DELETE returns an error, INSERT succeeds — and the test
asserts more than "an error came back". Both refusals are checked for SQLSTATE
`42501` (`insufficient_privilege`) *and* for the words `permission denied`, so a
syntax error, a missing table, or a failed role switch cannot be mistaken for
enforcement; the probes assert `current_user = ledger_writer` before running, so
they cannot be measuring the superuser; and after both refusals the row is
counted again to prove neither statement touched it.

The test was verified to fail when the guarantee is broken, the same way the
Phase 6 isolation test was. `GRANT UPDATE ON synapse_global.ledger TO
ledger_writer`, then re-run:

```text
$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    'GRANT UPDATE ON synapse_global.ledger TO ledger_writer'
GRANT
$ go test ./internal/ledger/... -run TestLedgerTablePermissions -v -tags integration
=== RUN   TestLedgerTablePermissions
    ledger_table_test.go:225:
        	Error:      	Should be false
        	Messages:   	ledger_writer must never hold UPDATE on synapse_global.ledger
--- FAIL: TestLedgerTablePermissions (0.07s)
FAIL	synapse/internal/ledger	0.078s
```

`REVOKE UPDATE ON synapse_global.ledger FROM ledger_writer` and it passes again,
with the ACL back to `{synapse=arwdDxt/synapse,ledger_writer=a/synapse}`. The ACL
assertions are the first line of defence by construction — an over-granted table
trips `has_table_privilege` before the probes run — while the probes cover the
behavioural half that no ACL text check can reach.

A real plane boot, against the Phase 4 compose database, applies the new
migrations through the production path (`cmd/plane/main.go:122` calls
`tenant.RunMigrations` at startup):

```text
$ SYNAPSE_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
  SYNAPSE_JWT_SECRET=... SYNAPSE_ADMIN_TOKEN=... SYNAPSE_MASTER_KEY=... \
  /tmp/phase15/bin/plane --port 9198
12:08PM WARN plane: Control plane config file not found, using defaults and environment path=synapse-plane.yaml
12:08PM INFO plane: Control plane config loaded listen_addr=127.0.0.1:9198 database_dsn=set jwt_secret=set admin_token=set master_key=set log_level=info ledger_retention_days=365
12:08PM INFO plane: migrations complete schema=synapse_global
12:08PM INFO plane: Synapse Control Plane v2.0.0 listening addr=127.0.0.1:9198

$ curl -sS -i http://127.0.0.1:9198/health
HTTP/1.1 200 OK
Content-Length: 50

{"status":"ok","version":"2.0.0","db":"connected"}
```

The untagged suite still runs everywhere CI does, which is the check `doc.go`
exists for:

```text
$ go test ./... -count=1
ok  	synapse/internal/integration	1.792s
?   	synapse/internal/ledger	[no test files]
ok  	synapse/internal/plane	0.048s
ok  	synapse/internal/proxy	0.398s
ok  	synapse/internal/retrieval	0.009s
ok  	synapse/internal/scorer	0.011s
?   	synapse/internal/session	[no test files]
ok  	synapse/internal/store	4.228s
ok  	synapse/internal/supersession	0.008s
ok  	synapse/internal/sync	4.390s
ok  	synapse/internal/tenant	0.788s
ok  	synapse/internal/trace	0.169s

$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/tenant/... -count=1
ok  	synapse/internal/tenant	1.205s
```

And the database's own answer, independent of the test:

```text
$ docker exec deploy-db-1 psql -U synapse -d synapse -c '\du'
   Role name   |                         Attributes
---------------+------------------------------------------------------------
 ledger_writer | Cannot login
 synapse       | Superuser, Create role, Create DB, Replication, Bypass RLS

$ docker exec deploy-db-1 psql -U synapse -d synapse -c '\d synapse_global.ledger'
   Column   |           Type           | Nullable |      Default
------------+--------------------------+----------+-------------------
 id         | uuid                     | not null | gen_random_uuid()
 tenant_id  | uuid                     | not null |
 request_id | uuid                     | not null |
 trace_json | text                     | not null |
 prev_hash  | text                     | not null |
 hash_value | text                     | not null |
 deleted_at | timestamp with time zone |          |
 created_at | timestamp with time zone | not null | now()
Indexes:
    "ledger_pkey" PRIMARY KEY, btree (id)
    "ledger_tenant_created_idx" btree (tenant_id, created_at)

$ select relacl from pg_class where relname = 'ledger';
{synapse=arwdDxt/synapse,ledger_writer=a/synapse}    # a = INSERT; no w (UPDATE), no d (DELETE)

$ select has_schema_privilege('ledger_writer','synapse_global','USAGE'),
         has_table_privilege('ledger_writer','synapse_global.ledger','INSERT'),
         has_table_privilege('ledger_writer','synapse_global.ledger','UPDATE'),
         has_table_privilege('ledger_writer','synapse_global.ledger','DELETE');
 t | t | f | f

$ select request_id, trace_json, hash_value from synapse_global.ledger order by created_at;
              request_id              |                   trace_json                    |       hash_value
--------------------------------------+-------------------------------------------------+------------------------
 2196a41e-9418-44c8-b627-1c6c3040e84d | {"phase":15,"probe":"ledger-table-permissions"} | phase15-unsigned-probe
 93d7d5c1-9b13-4ade-8f50-5a69137e3b60 | {"phase":15,"probe":"ledger-table-permissions"} | phase15-unsigned-probe
 38ea0a7f-54db-4519-be7b-12f61c81b9d7 | {"phase":15,"probe":"ledger-table-permissions"} | phase15-unsigned-probe
 f1567dd5-4344-484c-b874-98c0f160fad9 | {"phase":15,"probe":"ledger-table-permissions"} | phase15-unsigned-probe
```

Cleanup: the plane was built into `/tmp/phase15/bin/` (never into `bin/`, where
`bin/synapse` is tracked and already dirty from an earlier local build) and
stopped — port 9198 closed, no process left. The four rows above are this phase's
committed probe writes; append-only means they stay, which is why they are
labelled `{"phase":15,...}` / `phase15-unsigned-probe` instead of looking like real
ledger entries.

### Findings this phase surfaced (not fixed here — this phase creates a table and a role)

1. **The guarantee binds nobody in production yet.** The plane connects with the
   DSN's user, and on the compose stack that user is `synapse`: a Postgres
   superuser *and* the ledger table's owner. A superuser bypasses every privilege
   check and an owner holds every privilege by default, so `GRANT INSERT` on that
   identity is decorative — the enforcement is real only for a connection whose
   `current_user` is `ledger_writer`. What the migration does provide is the
   missing half: `synapse` is now a member of `ledger_writer`, so the writer can
   `SET LOCAL ROLE ledger_writer` per transaction with no new credentials, and
   that is the shape the first real ledger write has to adopt. The alternative — a
   dedicated `LOGIN` role with its own password and a second DSN — is a
   deployment change, not a migration change.
2. **The writer cannot read `prev_hash`, so the hash chain cannot be built on this
   role as it stands.** No `SELECT` was granted (the strongest reading of "no
   UPDATE, no DELETE"), which means `INSERT ... RETURNING` fails too — that is
   precisely what forced the test to judge its INSERT by row count. Phase 16's
   chaining writer needs one of: `GRANT SELECT` (still no UPDATE, no DELETE), a
   read of the previous hash through the application connection, or a
   `SECURITY DEFINER` function that returns the chain head. Deciding that is the
   signing phase's job; granting it now would have been scope creep.
3. **`REVOKE ALL ... FROM PUBLIC` is documentation, not a closure.** A newly
   created table's default ACL grants PUBLIC nothing (confirmed: the ACL text
   before the revoke listed only the owner), so the statement cannot be observed
   changing anything. It stays because the intent — this table is not public —
   ought to be readable in the DDL rather than inferred from a default.
4. **Concurrent boots now have a role-shaped race.** The `DO` block is
   check-then-create, so two planes starting in the same instant could both see
   the role missing and one could fail with `duplicate_object`. The single-plane
   assumption in `RunMigrations`' doc comment already excludes that topology, so
   nothing regressed — but the failure mode moved from "two `CREATE TABLE IF NOT
   EXISTS`" (harmless) to "one boot aborts". An advisory lock, or an `EXCEPTION
   WHEN duplicate_object` arm in the block, is the fix when a second plane becomes
   supported.
5. **The committed probe row is deliberately undeletable.** One row per test run
   lands in the ledger and, by this phase's own rule, can never be removed — four
   are there now. That is the honest cost of asserting a *committed* INSERT rather
   than a rolled-back one, and the rows are labelled so a future chain reader can
   recognise pre-chain entries instead of failing on them. A database-level
   alternative does not exist: `deleted_at` is the tenant-facing soft delete for
   chain integrity, not a licence for the role to delete anything.

### Next phase

Give the ledger writer the identity the grants actually bind — a write path that
runs as `ledger_writer` (via `SET LOCAL ROLE` through the membership this phase
granted) — and only then add signing and the `prev_hash` chain it depends on,
which needs finding 2 resolved first. Wiring the table up without the role switch
would produce a ledger whose append-only guarantee is real in the ACL and
imaginary in the process.










