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










## Phase 16 — HMAC signing and ledger Append (complete)

Scope was the ledger's write path, and only that: `Ledger.Append` signs one trace
per call under the tenant's own secret and chains it to that tenant's previous
entry, and `internal/tenant` mints and wraps the secret that signs it. No reader,
no verification endpoint, no HTTP route, and no wiring into the request path — the
plane still appends nothing, so every call in this phase comes from a test.

Commit `feat: Phase 16 - HMAC signing and ledger Append`

New files:

- `internal/tenant/secrets.go` — 263 lines: `GenerateAndStoreSecret`, `GetSecret`,
  `ErrSecretNotFound`, `ErrSecretExists`, and the four helpers they are built from
  (`masterKeyFromEnv`, `sealSecret`, `openSecret`, `newGCM`, `canonicalTenantID`).
  32 random bytes from `crypto/rand`, wrapped with AES-256-GCM under
  `SYNAPSE_MASTER_KEY` (hex, exactly 32 bytes, read from the environment per call),
  stored as `base64(nonce ‖ ciphertext)` in `tenant_secrets.secret_encrypted`, with
  the tenant id authenticated as additional data.
- `internal/tenant/secrets_test.go` — 184 lines, untagged and database-free, so
  `go test ./...` in CI runs it: the master key's accepted shape (and that a
  rejected value is never echoed), the seal/open round trip, a fresh nonce per
  seal, and the four ways `openSecret` must refuse (another tenant, another master
  key, an altered ciphertext, a truncated envelope).
- `internal/ledger/ledger.go` — 289 lines: `Ledger`, `LedgerEntry`, `NewLedger`,
  `Append`, and the transaction's reads (`chainHead`, `readCreatedAt`, `lockChain`,
  `newEntry`, `canonicalID`).
- `internal/ledger/chain.go` — 56 lines: the hashing primitives — `genesisHash`
  (`hex(sha256("genesis"))`), `chainMessage`, `signature` (HMAC-SHA256), `hexDigest`.
- `internal/ledger/ledger_test.go` — 281 lines, `//go:build integration`:
  `TestAppend` and `TestConcurrentAppendStaysLinear`, plus `setMasterKey`,
  `testChainTenant`, `appendTrace`, and two helpers that recompute the brief's
  signature and the genesis hash *in the test*, independently of the package.

Changed files:

- `internal/ledger/doc.go` — 24 → 42 lines. Its Phase 15 text ("there is no signing
  and no hash chain yet: prev_hash and hash_value are still supplied by the caller")
  and its reason for existing (a directory whose only files carry `//go:build
  integration` fails `go test ./...` outright) both stopped being true, so it now
  states the chain contract, the file layout, and the verification commands.
- `internal/ledger/ledger_table_test.go` — 262 → 264 lines, and nothing about its
  assertions changed: the local `ledgerTable` constant was deleted because
  `ledger.go` now defines that name in the same package, and the permission probes
  use the write path's own constant. Phase 15's "holds INSERT and nothing else"
  assertions are untouched and still pass.

No new dependencies: `crypto/hmac`, `crypto/aes`, `crypto/cipher`, `encoding/base64`,
and `encoding/hex` are stdlib, and `google/uuid` plus pgx v5 were already required.
No v1 internal package was touched — `internal/tenant` and `internal/ledger` are both
v2.

Decisions made in this phase:

- **The brief's step 1 could not run as the writer role, so an append uses two
  identities in one transaction.** `SELECT` the chain head is impossible for
  `ledger_writer` (Phase 15 grants it INSERT and nothing else — `ERROR: permission
  denied for table ledger`, re-confirmed here), and `INSERT ... RETURNING` fails for
  the same reason. So `Append` reads the head on the pool's own connection *first*,
  then runs `SET LOCAL ROLE ledger_writer` and the `INSERT`, then commits. The
  alternative — writing as the owner and leaving the grant decorative — is the trade
  Phase 15's finding 2 said not to make, and granting the writer SELECT would have
  rewritten that phase's own assertion. `SET LOCAL`, not `SET ROLE`, so a pooled
  connection is never handed back still wearing the writer's identity.
- **The advisory lock is necessary but not sufficient, and the difference is
  `now()`.** `now()` is the transaction's *start* time, so two appends whose
  transactions began in one order but acquired the tenant lock in the other would
  store `created_at` in the opposite order to the chain — and the chain head is the
  newest row by `created_at`, so the next append would chain from the wrong row.
  `created_at` is stamped with `clock_timestamp()` inside the lock instead. This was
  measured, not argued: with the lock kept and `created_at` coming from `now()`,
  `TestConcurrentAppendStaysLinear` fails 3 of 5 runs (and with the lock removed
  too, 1 of 1).
- **`created_at` is stamped strictly after the head.** `GREATEST(clock_timestamp(),
  head_created_at + interval '1 microsecond')`, with the head's `created_at` read
  along with its hash and a NULL for a tenant's first entry. A *tie* in `created_at`
  makes "newest" ambiguous, and a tie is not hypothetical: one of the deliberately
  `now()`-stamped experiment tenants has two rows sharing a timestamp
  (`distinct_created_at=9` for 10 rows), which is what two concurrent transactions
  starting inside one clock tick look like. Under the lock the new row is therefore
  always strictly newer than the head, whatever the system clock's granularity does —
  a coarse clock or a backwards step cannot reorder or tie a chain.
- **The secret is refused rather than overwritten.** `GenerateAndStoreSecret` uses
  `ON CONFLICT (tenant_id) DO NOTHING` and answers a second call with
  `ErrSecretExists`. An upsert would have been friendlier and wrong: every entry is
  signed with the secret that existed when it was written, `ledger` has no
  key-version column, so replacing the row makes every older signature in that
  tenant's chain unverifiable. Rotation needs key versioning first.
- **The tenant id is GCM additional data, and both ids are canonicalized before
  hashing.** The first stops a ciphertext transplanted into another tenant's row
  from silently becoming that tenant's signing key; the second makes an entry's
  signature reproducible from the stored row, because Postgres renders the uuid the
  same way `uuid.Parse().String()` does.
- **`created_at` is read back after the COMMIT rather than predicted.** The writer
  cannot use `RETURNING`, and the value is computed by the database, so `Append`
  returns the row it wrote instead of a client-side guess. The cost is one extra
  statement, and the failure it creates is recorded as a finding below.
- **The ledger package holds no logger.** It deals in HMAC keys and whole traces, so
  "never log a secret" is structural: there is nowhere in the type to log one. The
  returned `LedgerEntry.ID` is the safe thing for a caller to log.
- **`SYNAPSE_MASTER_KEY` comes from the environment only**, never from the plane
  YAML the way `jwt-secret` and `admin-token` do: the key that unwraps every tenant's
  signing secret should not sit on disk next to the database credentials. Errors name
  the variable and never its value, and the raw secret, the ciphertext, and the key
  appear in no log line, no error, and no return value other than the secret the
  caller is handed once.

### Verification

Every command below was run from the repository root with the Phase 4 compose
database up (`deploy/docker-compose.yml` → `pgvector/pgvector:pg16`, healthy on
127.0.0.1:5432) and **nothing exported**: `SYNAPSE_TEST_DB_DSN` is unset (the tests
fall back to the compose DSN, exactly as Phase 15's do) and `SYNAPSE_MASTER_KEY` is
set by the tests themselves with `t.Setenv`, so the phase's definition-of-done
command needs no exported secrets.

```text
$ gofmt -l internal/ledger internal/tenant      # prints nothing
$ go build ./...                                # prints nothing
$ go vet ./internal/tenant/... ./internal/ledger/...         # prints nothing
$ go vet -tags integration ./internal/ledger/...             # prints nothing
```

The definition of done, with nothing exported:

```text
$ go test ./internal/ledger/... -run TestAppend -v -count=1 -tags integration
=== RUN   TestAppend
--- PASS: TestAppend (0.30s)
PASS
ok  	synapse/internal/ledger	0.306s
```

`TestAppend` asserts, over ten entries: the signature recomputed in the test from
the raw secret and the entry's own fields equals both the returned `HashValue` and
the stored `hash_value`; the row's `prev_hash`, `trace_json`, and `created_at` are
the ones the returned entry reports; `entry[0].PrevHash == hex(sha256("genesis"))`;
`entry[i].PrevHash == entry[i-1].HashValue`; every id is distinct; `created_at`
advances; and a different key does not reproduce the signature — the negative
control that keeps the rest evidence rather than a tautology.

The whole package, including the Phase 15 permission test as a regression check:

```text
$ go test ./internal/ledger/... -count=1 -v -tags integration
=== RUN   TestLedgerTablePermissions
--- PASS: TestLedgerTablePermissions (0.07s)
=== RUN   TestAppend
--- PASS: TestAppend (0.17s)
=== RUN   TestConcurrentAppendStaysLinear
--- PASS: TestConcurrentAppendStaysLinear (0.20s)
PASS
ok  	synapse/internal/ledger	0.443s
```

The secrets path, including `internal/tenant`'s existing database-backed tests with
the DSN set, and the new untagged crypto tests that CI does run:

```text
$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/tenant/... -count=1
ok  	synapse/internal/tenant	1.198s

$ go test ./internal/tenant/... -run 'TestMasterKey|TestSealSecret|TestOpenSecret|TestCanonicalTenantID' -count=1
ok  	synapse/internal/tenant	0.007s

$ go test ./internal/tenant/... -run 'TestMasterKey|TestSealSecret|TestOpenSecret|TestCanonicalTenantID' -v
--- PASS: TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue (0.00s)
    --- PASS: TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue/accepted (0.00s)
    --- PASS: TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue/empty (0.00s)
    --- PASS: TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue/not_hex (0.00s)
    --- PASS: TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue/too_short (0.00s)
    --- PASS: TestMasterKeyFromEnvAcceptsOnlyA32ByteHexValue/too_long (0.00s)
--- PASS: TestSealSecretRoundTripsAndHidesThePlaintext (0.00s)
--- PASS: TestSealSecretUsesAFreshNonce (0.00s)
--- PASS: TestOpenSecretRejectsAnythingButTheOriginalCiphertext (0.00s)
    --- PASS: .../another_tenant (0.00s)
    --- PASS: .../another_master_key (0.00s)
    --- PASS: .../altered_ciphertext (0.00s)
    --- PASS: .../not_base64 (0.00s)
    --- PASS: .../too_short_for_a_nonce (0.00s)
--- PASS: TestCanonicalTenantIDMatchesWhatPostgresStores (0.00s)
```

What the tests wrote, walked back out of the database itself — run *after* the test
process had exited, because a walk started while the tests were still writing sees a
half-built chain (which cost one confusing reading here):

```text
$ psql -c "<per-tenant walk of the two newest test tenants>"
1  ledger-5294a71b3fc1  rows=10  distinct_created_at=10  tied_rows=0  broken_links=0  first_link_is_genesis=1
2  ledger-ba9264f19dee  rows=10  distinct_created_at=10  tied_rows=0  broken_links=0  first_link_is_genesis=1
```

`broken_links` is `lag(hash_value) OVER (PARTITION BY tenant_id ORDER BY created_at)
<> prev_hash`, which is the same walk a verifier performs, and `first_link_is_genesis`
checks `prev_hash = aeebad4a796fcc2e15dc4c6061b45ed9b373f26adfc798ca7d2d8cc58182718e`
— `hex(sha256("genesis"))` computed outside the process (`printf genesis | sha256sum`).

Three probes back the two ordering claims, each against the real server:

```text
# now() is frozen at the transaction's start; clock_timestamp() is not:
$ psql -c "BEGIN; SELECT now(); SELECT pg_sleep(0.15); SELECT now(), clock_timestamp(); ROLLBACK;"
at BEGIN: now()=2026-09-21 12:23:34.103374+00
150ms later inside the same txn: now()=2026-09-21 12:23:34.103374+00  clock_timestamp()=2026-09-21 12:23:34.271141+00

# GREATEST(..., head + 1 microsecond) is strictly later than the head even when the
# head sits an hour in the future (what a backwards clock step looks like), and a
# NULL head -- a tenant's first entry -- falls through to the clock:
$ psql -c "WITH head AS (SELECT (now() + interval '1 hour') AS head_at) SELECT ..."
head=2026-09-21 13:27:09.944475+00  new_stamp=2026-09-21 13:27:09.944476+00
strictly_later_than_head=true  null_head_ignored=true
```

Both ordering safeguards were falsified before being kept. Each variant was patched
into a copy of `ledger.go`, measured, and then restored — the checksum
(`327a44573e0c4e75aeb7120554b703dab7801a091b711c334cabc7d1c7fefb0e`) was verified
identical before and after every experiment, and `gofmt -l` was re-run afterwards:

```text
# lock kept, created_at from now() -- i.e. the brief as written:
$ go test ./internal/ledger/... -run TestConcurrentAppendStaysLinear -count=5 -tags integration
--- FAIL: TestConcurrentAppendStaysLinear (0.19s)
        Messages:  	row 4 must chain from the row before it
--- FAIL: TestConcurrentAppendStaysLinear (0.18s)
        Messages:  	row 2 must chain from the row before it
        Messages:  	row 3 must chain from the row before it
--- FAIL: TestConcurrentAppendStaysLinear (0.17s)
        Messages:  	row 1 must chain from the row before it
        Messages:  	row 2 must chain from the row before it
        Messages:  	row 3 must chain from the row before it
FAIL

# no lock, created_at from now():
$ go test ./internal/ledger/... -run TestConcurrentAppendStaysLinear -count=1 -v -tags integration
        Error:     	Not equal:
        expected: "b76a24728d4267c24de9b1aee4a84966e925dfbc2e1dbeb50e28da7524ad362a"
        actual  : "31bf3cd0251550b4b7a29e40dfb5e3924a5a77e3b3968e2de4ab3c486aefb2e3"
        Messages:  	row 2 must chain from the row before it
FAIL

# restored, and the package green again:
$ go test ./internal/ledger/... -run 'TestAppend|TestConcurrentAppendStaysLinear|TestLedgerTablePermissions' -tags integration
ok  	synapse/internal/ledger	0.407s
```

Two rows in that second transcript share predecessor `31bf3cd0…`, and two entries
chaining from one row is precisely what a forked chain is. The first transcript is
the subtler result: the lock *was* held, so the appends were serialized, and the
chain still forked — because `created_at` (transaction start) disagreed with the
order the lock was acquired in.

### Findings this phase surfaced (not fixed here — this phase is the write path)

1. **Nothing in production calls either function yet.** `GenerateAndStoreSecret` is
   called by the tests and by nothing else, and no code path constructs a `Ledger`.
   The write path is complete and exercised, but until provisioning mints a secret
   and the plane appends a trace, the ledger is still empty in a running deployment —
   the same shape Phase 15 left the page in, one layer up.
2. **There is no key versioning, so rotation is refused rather than implemented.**
   Every entry is signed with the secret that existed when it was written and the
   `ledger` table has no column naming that key, so replacing a tenant's secret makes
   its earlier signatures unverifiable. `ErrSecretExists` is the honest answer until a
   phase adds a key id to the row (or to the message).
3. **The writer cannot read what it writes.** The chain head and the row's
   `created_at` both have to be read through the application connection, which is why
   an `Append` costs six round trips. A verification phase inherits the same
   constraint: checking a chain requires the owner's connection, not the writer role.
4. **An `Append` can fail after its row is committed.** If the post-COMMIT
   `created_at` read-back fails, `Append` returns an error for a row that is already
   in the ledger — so a caller must not blindly retry, because the retry would append
   a *second* entry for the same trace. The alternative (returning a client-side
   timestamp) was rejected as a lie about what was stored; the trade is recorded here
   instead.
5. **The falsification experiments are permanent.** Four test tenants now carry
   deliberately forked chains in the development database, and the append-only rule
   means they cannot be removed by anything short of dropping the table — the same
   honest cost Phase 15 hit with its committed probe row. They are only distinguishable
   by their `ledger-…` slugs, which is a reminder that this table is a poor place to
   experiment in any deployment that matters.
6. **Phase 15's five probe rows are still not chain material.** They carry
   `hash_value = 'phase15-unsigned-probe'` and random `tenant_id`s that have no
   `tenant_secrets` row, so they do not sit inside any tenant's chain and a per-tenant
   walk never meets them. A walk over the whole table would, and would fail on them —
   the label is the only thing that distinguishes them.
7. **The membership grant is now load-bearing in production.** `Append`'s first
   statement after the head read is `SET LOCAL ROLE ledger_writer`, so a deployment
   whose `SYNAPSE_DB_DSN` user is not a member of that role fails every append with
   `permission denied to set role`. The compose stack is fine because
   `RunMigrations` grants the role to `CURRENT_USER`; a manually prepared database
   that skipped the migration would not be.
8. **CI does not exercise any of this.** Every test in `internal/ledger` carries
   `//go:build integration`, so `go test ./...` (what `ci.yml` runs, with no database)
   compiles the package and runs nothing in it. The parts that are not
   database-bound — the AES-GCM wrapping, the master key's shape — were deliberately
   put in `internal/tenant` with untagged tests so something in this phase is covered
   where CI looks.
9. **`created_at` now means "this tenant's insertion order", not "the wall clock".**
   It is `clock_timestamp()` whenever the clock moves between two appends, and is
   pushed one microsecond past the head only when the clock does not (a coarse tick,
   or a step backwards). That is the right trade for a hash-chained ledger — order is
   the property being protected — but a report that reads `created_at` as pure
   wall-clock time should know the difference.
10. **`hashtext` is a 32-bit tenant key.** Two tenants can share an advisory lock and
    serialize against each other; the cost is throughput, never correctness, and the
    fix (a lock table, or `pg_advisory_xact_lock` on two int4 keys derived from the
    uuid) is not needed at this scale.

### Next phase

Wire the write path into the product, in the order the dependencies demand: mint a
tenant secret when a tenant is provisioned (returning it to the caller once, the way
the API key already is), then sign and append a ledger entry from the plane for each
compiled request, and only then build the verification side — a path that reads a
tenant's rows and recomputes every signature and link, which is the artifact the
whole table exists for. Key versioning belongs with that work, because a verifier is
what makes rotation safe to offer. The S/R/I/T breakdown and `trace_id` on the
plane's own memory-search surface — the shape `.clinerules` already requires of MCP
responses — is the other queued item, from Phase 14's findings.

## Phase 17 — chain integrity Verify (complete)

Scope was the ledger's read path and the one route that exposes it:
`Ledger.Verify` walks one tenant's rows in chain order, recomputes every signature
under that tenant's own secret, checks every link, and names the first entry that
does not hold up; `GET /v2/ledger/verify` returns that finding as JSON, tenant-scoped
by the verified token. Nothing was added to the write path, no v1 package was
touched, and no request path signs anything yet — so in a running deployment the
endpoint has nothing to verify until provisioning mints a secret (finding 1).

Commit `feat: Phase 17 - chain integrity Verify`

New files:

- `internal/ledger/verify.go` — 158 lines: `ChainIntegrityResult` (an alias, see
  decision 1), `verifyQuery`, and `Ledger.Verify` — fail-closed guards, a streamed
  walk in `created_at` order, a per-row signature check and link check, and a named,
  located first break. It runs on the pool's own connection, never as
  `ledger_writer`: that role holds no SELECT at all (Phase 16 finding 3, inherited).
- `internal/ledger/verify_test.go` — 262 lines, `//go:build integration`:
  `TestVerify`, the three tamper tests (`TestVerifyDetectsTamperedTrace`,
  `TestVerifyDetectsRewrittenSignature`, `TestVerifyDetectsRemovedRow`),
  `TestVerifyDetectsTruncatedGenesis`, and the negative control
  (`TestVerifyRejectsAnotherSecret`), plus the `chainedEntries` and `tamper` helpers.
  Tampering is done with SQL against the real table, through a connection whose
  privilege is asserted first.
- `internal/ledger/verify_input_test.go` — 81 lines, same tag: the two cases that are
  about *not* answering — `TestVerifyEmptyChain` and `TestVerifyRejectsUnusableInput`.
  Split out because `verify_test.go` hit the 300-line ceiling.
- `internal/plane/ledger.go` — 136 lines: `ledgerVerifyRoute`, the plane-owned
  `ChainIntegrityResult` and `LedgerVerifier` (decision 1), and `handleVerifyLedger`.
- `internal/plane/ledger_test.go` — 222 lines, no build tag, so CI runs it:
  `TestVerifyLedgerRequiresAVerifiedTenantToken`,
  `TestVerifyLedgerAnswersWithTheChainVerdict` (both outcomes, asserted as JSON),
  `TestVerifyLedgerReportsAFailedCheckAsInternal`,
  `TestVerifyLedgerFailsClosedWithoutAVerifier`, and
  `TestVerifyLedgerRefusesAnUnverifiedRequest`.
- `cmd/plane/ledger.go` — 63 lines: `ledgerVerifier`, the adapter that fetches a
  tenant's secret with `tenant.GetSecret` and hands it to `Ledger.Verify`. It is the
  only file that can see both internal/ledger and internal/plane.

Changed files:

- `internal/plane/handlers.go` — 178 → 201 lines: the `ledger LedgerVerifier` field,
  the `NewServer` parameter and its documentation, and the route registration
  (`router.With(s.requireJWT).Get(ledgerVerifyRoute, s.handleVerifyLedger)`).
- `internal/plane/sync.go` — 242 → 273 lines: `tenantIDKey` and
  `WithTenantID`/`TenantIDFromCtx`, the fourth verified claim this package republishes,
  next to the three that were already there.
- `internal/tenant/auth.go` — one line in `withClaims` (`plane.WithTenantID(ctx,
  c.TenantID)`) plus its comment. The tenant's uuid is already in the signed token;
  this is what makes it readable by an endpoint that cannot import internal/tenant.
- `cmd/plane/main.go` — 8 lines: `newLedgerVerifier(pool)`, passed as `NewServer`'s
  sixth argument.
- `internal/plane/handlers_test.go`, `sync_test.go`, `search_test.go` — one extra
  `nil` at each of the four `NewServer` call sites. No assertion changed.
- `internal/ledger/doc.go` — Phase 17's contract, the file list, and the `-run
  TestVerify` command.

No new dependencies: `crypto/hmac` is stdlib and was already used by the write path.
No v1 internal package was touched — `internal/ledger`, `internal/plane`,
`internal/tenant`, and `cmd/plane` are all v2.

Decisions made in this phase:

- **The brief's result type could not be declared where the brief put it, and
  declaring it twice was the one thing not to do.** `internal/plane` cannot import
  `internal/ledger`: the ledger imports `internal/tenant` (schema name, writer role,
  and now `GetSecret`) and `internal/tenant` imports the plane (the verified-claim
  accessors `JWTMiddleware` publishes), so the import is a cycle —
  `go list -deps ./internal/ledger` prints `synapse/internal/plane` today. The type is
  therefore declared in the plane, where its only consumer is, and `internal/ledger`
  aliases it: `type ChainIntegrityResult = plane.ChainIntegrityResult`. One definition,
  so `entries_checked`/`chain_valid`/`first_break_id`/`first_break_at`/`checked_at`
  cannot drift between the walk and the wire. This is the arrangement
  `plane.ProvisionResult` already has, one endpoint over. The cost is honest and
  recorded: a JSON-tagged type lives in the HTTP package, which is a mild layering
  inversion, contained by keeping the interface, the type, and the handler in one new
  file and the ledger's dependency on the plane explicit rather than hidden behind
  `internal/tenant`.
- **`Verify` is a new file, not an addition to `ledger.go`.** That file is 289 lines
  and the walk plus its result type is ~110, so the brief's "add to ledger.go" was
  impossible under the 300-line cap this project holds every file to. Same reason
  `chain.go` and `secrets.go` exist. `verify_test.go` hit the same ceiling at 322 lines
  and lost its two boundary cases to `verify_input_test.go`.
- **The verified tenant uuid is republished, not looked up.** The chain is keyed by
  `tenant_id uuid`, and the token already carries that value as its subject — but
  internal/plane cannot read internal/tenant's context keys, so `plane.WithTenantID`
  joins `WithTenantSlug`/`WithAgentID`/`WithTeamID` and `withClaims` publishes it. The
  alternative was deleting a query's worth of work by resolving slug→uuid in the
  adapter (`SELECT id FROM synapse_global.tenants WHERE slug = $1`) and touching no
  other package; two extra round trips and a second source of truth for "which tenant
  is this" were not worth avoiding one line.
- **`hmac.Equal`, not `!=`.** Both sides are 64-character hex digests, so the lengths
  always match and the comparison is constant time. A verifier has no reason to offer a
  timing channel about how far into a digest two values agree.
- **The walk starts at genesis, which is one check beyond the brief.** Seeding `prev`
  with `genesisHash()` holds the first surviving row to the rule the write path applied
  to it. It costs nothing and it is the *only* detector of a truncated chain: delete the
  first entries and every remaining row's signature is still valid over its own fields,
  so a signature-only walk reports a shortened ledger as valid. `TestVerifyDetectsTruncatedGenesis`
  is that case, and it asserts the surviving signature still verifies, so the test fails
  if the genesis link is removed and something else starts catching it.
- **`EntriesChecked` counts the row that broke the chain.** A break at the fifth of ten
  reports 5, and the walk stops there: once one row is unaccounted for, every later link
  is unverifiable with it, and "how many were fine after the break" is not a question
  this endpoint answers. Documented on the field, so the number is not read as "5
  entries verified".
- **A broken chain is a 200.** The check succeeded; `chain_valid: false` is the finding.
  An error status would collapse "your ledger no longer verifies" into "the check could
  not run", which is exactly the distinction an audit endpoint exists to draw. A check
  that could not run — no verifier wired, database unreachable, no stored secret — is
  500 with the one error body, and the underlying error is logged server-side only.
- **The tamper tests rewrite the ledger through the application connection, and assert
  that it can.** This deployment has no `postgres` role: compose sets
  `POSTGRES_USER: synapse`, and `SELECT rolname, rolsuper FROM pg_roles` returns exactly
  `synapse|t` and `ledger_writer|f`, so the application user *is* the superuser and the
  table's owner — the same fact Phase 15 recorded when it ran its permission probes
  inside `SET LOCAL ROLE ledger_writer`. `tamper` therefore asserts `rolsuper` (or
  `has_table_privilege(..., 'UPDATE')`) before its statement, so the probe states its
  precondition instead of passing vacuously on a database where nothing could be
  rewritten, and so the tests make the threat model explicit: what they simulate is
  precisely what the append-only grants cannot stop.
- **The three ids are cast to `text` in the verification query.** `uuid::text` is the
  canonical lowercase rendering, which is the exact string the signature covers, and
  reading a uuid back as text is what `tenant.Store.CreateTenant` already does
  (`RETURNING id::text`). One convention for one value; the values are identical to what
  the brief's un-cast `SELECT` would have produced.
- **The walk streams; it does not collect the chain.** Answering "where is the first
  break" does not need one tenant's whole ledger in memory, and a verifier that loaded it
  would be the slowest way to answer the cheapest question about a chain. `rows.Next()`
  keeps exactly one predecessor hash.
- **The route is tenant-scoped, not admin-guarded.** The chain verified is the caller's
  own, chosen from the verified token's `tenant_id`, so this is a tenant auditing its own
  records rather than an operator reading someone else's. There is no path, query, body,
  or header parameter that names a tenant, which is what keeps the endpoint from becoming
  a way to read another tenant's row ids and timestamps.

Verification (real output, this phase):

The compose stack from Phase 4 was already up for this phase (`deploy-db-1`,
pgvector/pgvector:pg16, healthy, published on `127.0.0.1:5432`), which is the only
precondition the integration tests need.

```text
$ gofmt -l internal/ledger internal/plane internal/tenant cmd/plane
$ go vet ./...
$ go vet -tags integration ./internal/ledger
VET OK

$ go test ./internal/ledger/... -run TestVerify -v -tags integration -count=1
=== RUN   TestVerifyEmptyChain
--- PASS: TestVerifyEmptyChain (0.09s)
=== RUN   TestVerifyRejectsUnusableInput
=== RUN   TestVerifyRejectsUnusableInput/no_ledger_at_all
=== RUN   TestVerifyRejectsUnusableInput/no_database_pool
=== RUN   TestVerifyRejectsUnusableInput/no_signing_secret
=== RUN   TestVerifyRejectsUnusableInput/tenant_id_is_not_a_uuid
--- PASS: TestVerifyRejectsUnusableInput (0.04s)
    --- PASS: TestVerifyRejectsUnusableInput/no_ledger_at_all (0.00s)
    --- PASS: TestVerifyRejectsUnusableInput/no_database_pool (0.00s)
    --- PASS: TestVerifyRejectsUnusableInput/no_signing_secret (0.00s)
    --- PASS: TestVerifyRejectsUnusableInput/tenant_id_is_not_a_uuid (0.00s)
=== RUN   TestVerify
--- PASS: TestVerify (0.17s)
=== RUN   TestVerifyDetectsTamperedTrace
--- PASS: TestVerifyDetectsTamperedTrace (0.19s)
=== RUN   TestVerifyDetectsRewrittenSignature
--- PASS: TestVerifyDetectsRewrittenSignature (0.17s)
=== RUN   TestVerifyDetectsRemovedRow
--- PASS: TestVerifyDetectsRemovedRow (0.17s)
=== RUN   TestVerifyDetectsTruncatedGenesis
--- PASS: TestVerifyDetectsTruncatedGenesis (0.17s)
=== RUN   TestVerifyRejectsAnotherSecret
--- PASS: TestVerifyRejectsAnotherSecret (0.27s)
PASS
ok  	synapse/internal/ledger	1.259s
```

The phase's headline case is in there: ten appends, `entry[4].trace_json` rewritten
with SQL, and `FirstBreakID` is `entry[4].ID` with `EntriesChecked == 5`. Phase 16's
tests still pass beside it, which is the check that verification did not quietly
change the write path:

```text
$ go test ./internal/ledger/... -tags integration -count=1
ok  	synapse/internal/ledger	1.630s

$ go test ./internal/plane/... -run TestVerifyLedger -v -count=1
=== RUN   TestVerifyLedgerRequiresAVerifiedTenantToken
--- PASS: TestVerifyLedgerRequiresAVerifiedTenantToken (0.00s)
    --- PASS: TestVerifyLedgerRequiresAVerifiedTenantToken/a_string_that_is_not_a_token_at_all (0.00s)
    --- PASS: TestVerifyLedgerRequiresAVerifiedTenantToken/token_without_the_scheme (0.00s)
    --- PASS: TestVerifyLedgerRequiresAVerifiedTenantToken/a_token_signed_with_another_secret (0.00s)
    --- PASS: TestVerifyLedgerRequiresAVerifiedTenantToken/no_header (0.00s)
    --- PASS: TestVerifyLedgerRequiresAVerifiedTenantToken/empty_bearer (0.00s)
=== RUN   TestVerifyLedgerAnswersWithTheChainVerdict
--- PASS: TestVerifyLedgerAnswersWithTheChainVerdict (0.00s)
    --- PASS: TestVerifyLedgerAnswersWithTheChainVerdict/the_whole_chain_verifies (0.00s)
    --- PASS: TestVerifyLedgerAnswersWithTheChainVerdict/a_rewritten_entry_is_reported,_not_an_error (0.00s)
=== RUN   TestVerifyLedgerReportsAFailedCheckAsInternal
--- PASS: TestVerifyLedgerReportsAFailedCheckAsInternal (0.00s)
=== RUN   TestVerifyLedgerFailsClosedWithoutAVerifier
--- PASS: TestVerifyLedgerFailsClosedWithoutAVerifier (0.00s)
=== RUN   TestVerifyLedgerRefusesAnUnverifiedRequest
--- PASS: TestVerifyLedgerRefusesAnUnverifiedRequest (0.00s)
PASS
ok  	synapse/internal/plane	0.008s

$ go test ./...
ok  	synapse/internal/api	(cached)
ok  	synapse/internal/budget	(cached)
ok  	synapse/internal/classifier	(cached)
ok  	synapse/internal/compiler	(cached)
ok  	synapse/internal/config	(cached)
ok  	synapse/internal/conflict	(cached)
ok  	synapse/internal/dedup	(cached)
ok  	synapse/internal/embedder	(cached)
ok  	synapse/internal/integration	(cached)
ok  	synapse/internal/plane	0.060s
ok  	synapse/internal/proxy	(cached)
ok  	synapse/internal/retrieval	(cached)
ok  	synapse/internal/scorer	(cached)
ok  	synapse/internal/store	2.021s
ok  	synapse/internal/supersession	(cached)
ok  	synapse/internal/sync	(cached)
ok  	synapse/internal/tenant	0.774s
ok  	synapse/internal/trace	(cached)
```

The rows the previous phases left behind in the development database were the
strongest check that a per-tenant walk stays per-tenant: `TestVerify` walked its own
fresh tenant only, and Phase 15's unsigned probe rows and Phase 16's four
deliberately forked chains were invisible to it, exactly as Phase 16 finding 6
predicted.

### Findings this phase surfaced (not fixed here — this phase is the read path)

1. **In a running deployment, `GET /v2/ledger/verify` cannot verify anything yet.**
   `tenant.GenerateAndStoreSecret` is still called by tests and by nothing else, and
   no code path constructs a `Ledger` outside a test, so every real tenant answers
   `500 {"error":"internal"}` (through `tenant.ErrSecretNotFound`) and every real
   ledger is empty. Phase 16's finding 1 now has a consumer that makes its cost
   visible: the endpoint is built, wired, and truthful, and there is nothing behind
   it. Minting a secret at provisioning — returning it once, the way the API key
   already is — is the next phase's first item.
2. **An empty chain is valid, so deleting a tenant's whole ledger is undetectable
   from inside the system.** `Verify` reports `chain_valid: true, entries_checked: 0`
   for a tenant whose rows are gone, and it has to: "nothing was ever appended" and
   "everything was removed" are indistinguishable from the rows alone. Anyone with
   superuser rights has exactly that power. The fix is an external anchor — publishing
   or storing the chain head hash somewhere the same credentials cannot rewrite — and
   until there is one, this property has to be stated rather than implied. It is the
   single largest hole in what this phase delivers.
3. **Rotation is now visibly indistinguishable from tampering.**
   `GenerateAndStoreSecret` refuses a second secret (`ErrSecretExists`) because the
   table has no key id, and Phase 16 called that an honest limitation. It now has a
   face: replace a tenant's secret and `Verify` reports a break at `entries[0]` — the
   same answer as an attacker who rewrote the first entry. Key versioning (a key id in
   the row, or in the signed message) is what makes rotation safe, and it is cheaper
   to add before real chains exist than after.
4. **Verification is unbounded work per call.** The walk reads one tenant's entire
   chain and recomputes an HMAC per row, with no limit and no cursor. That is the right
   shape for "answer the question exactly", but a tenant with a long history makes this
   endpoint the most expensive thing it can ask for, repeatedly, and the only thing
   bounding it is the request timeout the plane does not set. A row-count cap, a `since`
   cursor, or a per-tenant verification budget belongs with metering
   (`internal/metering`, still unbuilt).
5. **The verdict says "broken here", not "broken how".** `chain_valid: false` plus a
   first-break id cannot distinguish a rewritten row from a removed one — the walk
   refuses to interpret past the first failure, deliberately, since nothing after a
   break is trustworthy. An operator investigating will want per-row detail (which of
   the two checks failed, and the neighbouring hashes); that is a second, explicitly
   diagnostic endpoint rather than a wider verdict.
6. **The tamper tests leave permanent damage, like Phase 15's and 16's probes.**
   `TestVerifyDetectsRemovedRow` and `TestVerifyDetectsTruncatedGenesis` delete rows
   the append-only rule means nobody can put back, so each run adds test tenants with
   real holes in their chains. They are distinguishable by their `ledger-…` slugs and
   by having no in-product history, but the development database accumulates them, and
   PROGRESS.md is where that is recorded rather than discovered.
7. **CI still runs none of the walk.** Every test that touches Postgres in
   `internal/ledger` carries `//go:build integration`, so `go test ./...` — what
   `ci.yml` runs — compiles the package and executes nothing in it. This phase put what
   it could on the CI side of that line: the whole HTTP half of the feature (route,
   wire shape, fail-closed answers, tenant selection) is in
   `internal/plane/ledger_test.go` with no build tag, so the endpoint's contract *is*
   covered where CI looks, even though the chain maths is not.
8. **`ledger.ChainIntegrityResult` is now a plane type, and that is a real coupling.**
   A future CLI (`synapse ledger verify`) or an MCP tool can still use the name — the
   alias keeps it — but the struct it gets is declared in an HTTP package, so the audit
   domain's own result type now depends on a package whose subject is HTTP. The cycle
   that forced it is real and documented, and the alternative (two structurally
   identical structs, one per package) was worse: it is exactly the drift this project
   keeps refusing. If a third consumer ever appears, the move is a small
   `internal/ledgerverify` package both can import, not a second declaration.
9. **`first_break_at` is emitted as the zero time on a valid chain.** `omitempty` does
   not work on `time.Time` (a struct is never "empty" to `encoding/json`), so the field
   is always present and `0001-01-01T00:00:00Z` is the encoding of "no value". A client
   that parsed the timestamp without reading `chain_valid` first would see a date two
   thousand years in the past. Documented on the field and in the JSON contract test,
   which asserts the exact body.
10. **The verification route is the first plane endpoint whose 200 body carries
    timestamps.** `syncResponse` and `searchResponse` are counts and records;
    `ChainIntegrityResult` introduces RFC 3339 wire timestamps. Nothing else needs to
    change because of it, but the API's shape has moved, and the next endpoint that
    returns a time should follow this one rather than inventing a format.
11. **MCP does not have the endpoint's contract yet, and `.clinerules` will want it
    to.** The hard rule "MCP responses MUST include score breakdown (S/R/I/T) and
    trace_id so users can see WHY a memory was surfaced" is about memory search, not
    about the ledger — but the same rule implies an MCP ledger tool would have to
    report `chain_valid`, the first break, and the checked-at time, which is exactly
    this JSON. Reusing this shape rather than inventing a second one is the thing to
    do in the phase that adds MCP tools.

### Next phase

Mint a tenant secret when a tenant is provisioned, returning it once the way the API
key already is, and append a ledger entry from the plane for each compiled request —
otherwise this phase's endpoint has a permanent 500 and an empty table behind it
(finding 1). Key versioning (finding 3) belongs in that same work, because a verifier
now exists to make rotation safe to offer, and because a key id added after real chains
exist is a migration over data that cannot be rewritten. The external anchor for the
chain head (finding 2) is the item that closes the only hole this phase's design leaves
open. The S/R/I/T breakdown and `trace_id` on the plane's own memory-search surface —
the shape `.clinerules` already requires of MCP responses — is still queued from
Phase 14.


## Phase 18 — ledger wired into compilation (complete)

Scope was the write path's last missing piece: after every successful compilation
for an enterprise tenant, the finished trace is signed and appended to that
tenant's ledger chain, in a goroutine no request waits on. `internal/compiler` was
edited under the explicit exception the task granted ("do not touch any v1 internal
package except internal/compiler"); every other v1 package is untouched. Provisioning
now also mints each tenant's signing secret, without which no tenant could have
produced a row at all (Phase 17 finding 1).

Commit `feat: Phase 18 - ledger wired into compilation pipeline`

New files:

- `internal/compiler/ledger.go` — 162 lines: `EnterprisePlan`, `ledgerAppendTimeout`,
  the `LedgerSink` interface, `ledgerWiring`, the `atomic.Pointer` that holds it,
  `SetLedgerSink`, the gated `ledgerSink` reader, and `recordTrace` — the snapshot
  and the goroutine.
- `internal/compiler/ledger_test.go` — 250 lines: `recordingSink` (buffered channel,
  no sleeps), `blockingSink` (released only after `Compile` has returned),
  `installLedgerSink` (clears the process wiring via `t.Cleanup`), and six tests:
  the enterprise append, five non-enterprise plans that must stay silent, the
  no-sink standalone path, a nil sink under an enterprise plan, the
  does-not-wait-for-the-append proof, and a failing append that leaves the
  `CompileResult` alone.
- `cmd/synapse/ledger.go` — 239 lines: `enterpriseLedger` (the `compiler.LedgerSink`
  implementation), the `secretReader`/`entryAppender` seams that keep it testable
  without PostgreSQL, `ledgerRequestID` (the uuid mapping), `ledgerPlan`,
  `resolveLedgerPlan`, and `enableEnterpriseLedger` (the one boot-time wiring call).
- `cmd/synapse/ledger_test.go` — 266 lines: five test functions over fakes —
  the signed append, the two fail-closed stages, the uuid mapping's determinism and
  uniqueness, the claim reader, and the boot decision table.

Changed:

- `internal/compiler/compiler.go` — one call, `recordTrace(traceManifest)`, plus its
  comment, inserted after the trace is assembled and before `Compile` returns.
- `internal/tenant/token.go` — `ParseTokenClaims`, the unverified claim reader the
  edge needs and the plane must never use.
- `internal/tenant/token_test.go` — three tests: the round trip, the documented
  absence of a signature check, and the fail-closed shapes.
- `internal/tenant/provision.go` — `Provisioner` gained the pool and `NewProvisioner`
  a third parameter; `Provision` now mints and stores the tenant's signing secret.
- `internal/tenant/migrations_test.go` — the call site, a `t.Setenv(plane.EnvMasterKey, …)`
  so the path needs nothing exported, and the secret assertions.
- `internal/plane/config.go` — `Validate` now requires `master-key`, and the doc
  paragraph that said it was deliberately not required yet was rewritten to say why
  it is now.
- `internal/plane/config_test.go` — `validConfig` carries a master key; `TestValidate`
  gained the "missing master-key" case.
- `cmd/plane/main.go` — `NewProvisioner(cfg, tenant.NewStore(pool), pool)`.
- `cmd/synapse/main.go` — the wiring block: parse this node's credential, and when
  the plan is enterprise, open the ledger pool and install the sink; a warning if it
  cannot be wired, never a refusal to serve.
- `deploy/docker-compose.yml`, `synapse-plane.yaml.example` — a master key that is
  actually 64 hex characters. See decision 9: the previous dev placeholder was not
  hex at all.



### Decisions made in this phase

1. **The control plane does not compile, so there was one call site, not two.** The
   task asked to confirm this before wiring. `internal/plane.Routes()` registers
   `/health`, `POST /v2/tenants`, `POST /v2/sync/memories`, `GET /v2/memories/search`,
   and `GET /v2/ledger/verify` — there is no `/v1/compile` and no `compiler.Compile`
   call anywhere in `internal/plane` or `cmd/plane` (`grep -i compile` finds only
   `regexp.MustCompile`). Compilation happens in exactly two places, both on the edge
   node and both inside frozen v1 packages: `internal/proxy/proxy.go:616` for live
   proxied traffic and `internal/api/api.go:342` for `POST /v1/compile`. "Wire exactly
   one place" therefore resolved to the edge node's process, and the hook had to live
   in `internal/compiler` because neither call site could be edited.
2. **The sink is installed process-wide, and that is a deliberate exception to "no
   global state".** `compiler.Compile` is a free function with twelve positional
   parameters and no `Compiler` struct, and its two callers may not be touched this
   phase, so there is no constructor to inject into and no argument to pass. The
   wiring is therefore an `atomic.Pointer[ledgerWiring]` written once from
   `cmd/synapse/main.go` before the router serves and read once per compile. It costs
   a pointer load and a string comparison on the request path, and it is why a
   standalone node's behavior is unchanged rather than merely untested: with no sink
   installed, `recordTrace` returns before it marshals anything.
3. **The trace is marshalled synchronously and only the append is asynchronous.** This
   is not a detail: both callers mutate the very `*TraceManifest` `Compile` returns
   (`proxy.go:645-650`, `api.go:359-363` set `TokensUsed` and `ReductionPct`), so a
   goroutine that read the struct while they wrote it would be a data race, and
   `go test -race` would have caught it in any test that exercised an enterprise
   compile. Serializing before the goroutine takes the snapshot off the shared struct.
   The price is documented on `recordTrace` rather than hidden: the ledgered trace
   carries `tokens_used: 0` and `reduction_pct: 0`, because both are still unset at
   the only moment the trace is complete from the compiler's point of view (finding 1
   says what closing that would take).
4. **The request id is mapped, not passed through.** `ledger.Append` fails closed on a
   request id that is not a uuid, and v1 mints `req-<unixnano>` (`internal/api`) and
   `req-<unixnano>-<seq>` (`internal/proxy`). Rather than change a v1 id format from
   inside a frozen file, the ledger's `request_id` is derived with
   `uuid.NewSHA1(ledgerRequestNamespace, []byte(requestID))`. A uuid v5 is
   deterministic, so the mapping is traceable in the one direction that matters: the
   ledgered trace JSON still carries the original `request_id` verbatim, and a
   verifier recomputes the row's `request_id` from it. An id that is already a uuid is
   canonicalized instead of hashed, so a future v1 change to uuid ids needs no change
   here. Without this every append would have been rejected and this phase's
   verification would have shown `count = 0` beside a full error log.


5. **Which tenant a node belongs to comes from the node's own credential, read
   unverified — on purpose.** The edge holds the tenant JWT provisioning returned
   (`control-plane-api-key`) and nothing else that names a tenant: `internal/config`
   has no tenant-id key. `tenant.ParseTokenClaims` reads `tenant_id` and `plan` from
   it with `jwt.ParseUnverified`, which is safe here for a reason that does not
   generalize: the value is this node's own configuration, no request can supply it,
   and reading it grants nothing — the plane still verifies the same token on every
   sync and every candidate pull, and the ledger row it enables is a write this node
   makes under that credential's authority. A credential that parses but names no
   tenant is an error, not an empty identity, because appending to an unnamed tenant
   is the fail-open shape this path exists to avoid.
6. **Provisioning mints and stores the signing secret; it does not return it.**
   Phase 17 finding 1 said nothing in a running deployment ever called
   `GenerateAndStoreSecret`, so every tenant's chain was unwritable and
   `GET /v2/ledger/verify` answered 500. That is now closed at the only point where a
   tenant id exists. The secret is deliberately not handed back to the tenant the way
   the API key is: the ledger fetches and unwraps it per append, so nothing in the
   product needs the plaintext, and adding it to the provisioning response would have
   changed the wire shape for no requirement this phase had. Phase 17's note suggested
   returning it; that is deferred, not forgotten, and recorded as the open half. The
   mint is not atomic with the tenant row — `CreateTenant` commits first, because the
   uuid it returns is the additional authenticated data the secret is sealed with, so
   the id genuinely cannot exist any earlier — and finding 3 is the live tenant that
   window produced.
7. **`master-key` is now fatal at boot.** Phase 1's config comment said `MasterKey`
   was "parsed and redacted but deliberately not enforced yet, because Phase 1
   registers no authenticated route; it becomes required in the phase that introduces
   auth and tenant key wrapping." This is that phase. A plane without it would accept
   a provisioning request, commit the tenant row, and then fail to store the secret: a
   half-provisioned tenant whose audit chain can never be written or verified.
   `Validate` checks only that the key is present; the 64-hex-character shape stays
   `internal/tenant`'s rule (`masterKeyFromEnv`), next to the code that uses it,
   because two copies of that rule are two rules to keep in step.
8. **Wiring the ledger at the edge is never fatal.** Every failure — no credential, a
   credential that names no tenant, an enterprise tenant with no `database-dsn`, a
   pool that will not open — returns an error `main` logs as a warning, leaving no sink
   installed. An edge node whose ledger database is unreachable still compiles: putting
   the audit sink ahead of the product it audits would be the wrong trade, and the
   append itself is already fire-and-forget for the same reason.
9. **The compose stack's master key was not hex, and provisioning could not have
   worked with it.** `deploy/docker-compose.yml` shipped
   `SYNAPSE_MASTER_KEY: change-me-master-key-32-chars-min`, and `masterKeyFromEnv`
   hex-decodes the value and requires exactly 32 bytes. With this phase's minting in
   place that placeholder fails every provisioning request with `must be hex-encoded
   (64 characters for 32 bytes)` — which is exactly what the first live attempt
   produced. Both the compose file and `synapse-plane.yaml.example` now carry a
   shape-correct, clearly-labelled dev value. Worth recording: the replacement written
   first was 63 characters, not 64, and the new boot-time requirement plus the
   plane's own error message is what caught it.

### Verification (real output, this phase)

Unit and integration-free suites:

```text
$ go build ./...                      # clean
$ go vet ./...                        # VET OK
$ go test -count=1 ./...              # EXIT=0, no failures
$ go test -race -count=1 ./internal/compiler
ok  synapse/internal/compiler  2.077s

  --- PASS: TestCompileAppendsTraceForEnterprisePlan
  --- PASS: TestCompileSkipsLedgerForNonEnterprisePlans (5 subtests: oss, team, "", Enterprise, enterprise-plus)
  --- PASS: TestCompileWithoutLedgerSink
  --- PASS: TestSetLedgerSinkIgnoresNilSinkUnderEnterprisePlan
  --- PASS: TestCompileDoesNotWaitForLedgerAppend
  --- PASS: TestCompileSurvivesFailedLedgerAppend
     2026/09/21 13:02:23 ERROR ledger: append failed error="ledger: append needs a signing secret"

$ go test -count=1 ./cmd/synapse
ok  synapse/cmd/synapse  0.007s
  --- PASS: TestAppendTraceSignsForThisNodesTenant
  --- PASS: TestAppendTraceFailsClosed (3 subtests)
  --- PASS: TestLedgerRequestIDIsDeterministicAndUuidShaped
  --- PASS: TestResolveLedgerPlanReadsThisNodesOwnCredential (3 subtests)
  --- PASS: TestEnableEnterpriseLedgerRefusesIncompleteConfiguration (4 subtests)
```

`TestCompileDoesNotWaitForLedgerAppend` is the SLA proof rather than a timing
measurement: the sink blocks until the test releases it, and it is released only
after `Compile`'s result has been received, so a `Compile` that waited for the
append would hang the test instead of failing it.

The live run, against the compose stack (`docker compose up -d --build`, plane
`v2.0.0` on 127.0.0.1:9090, PostgreSQL on 127.0.0.1:5432) and a real edge node
(`/tmp/synapse-edge`, built from this tree, listening on 127.0.0.1:8099, ONNX
embedder, SQLite memory store, `control-plane-url` set):

```text
$ curl -sS -X POST http://127.0.0.1:9090/v2/tenants -H "Authorization: change-me-admin-token" \
       -H 'Content-Type: application/json' -d '{"slug":"enterprise-ledger18","plan":"enterprise"}'
HTTP 201
{"tenant_id":"b63b1d9d-898b-49ab-906e-64ed3edfa427","jwt":"eyJ…","api_key":"…"}

$ grep 'Audit ledger' /tmp/edge.log
1:12PM INFO synapse: Audit ledger enabled: every compiled trace is appended for this enterprise tenant

rows before any request: 0
request 1: http 200 in 0.273831s
  ledger rows after request 1: 1
request 2: http 200 in 0.052375s
  ledger rows after request 2: 2
request 3: http 200 in 0.053549s
  ledger rows after request 3: 3

$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    "SELECT COUNT(*) FROM synapse_global.ledger WHERE tenant_id='b63b1d9d-898b-49ab-906e-64ed3edfa427';"
 count
-------
     3
(1 row)

$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    "SELECT left(id::text,8) AS id, request_id, left(prev_hash,12) AS prev_hash, \
            left(hash_value,12) AS hash_value, created_at \
       FROM synapse_global.ledger WHERE tenant_id='b63b1d9d-898b-49ab-906e-64ed3edfa427' ORDER BY created_at;"
    id    |              request_id              |  prev_hash   |  hash_value  |          created_at
----------+--------------------------------------+--------------+--------------+-------------------------------
 9bec637b | 0912b375-cbac-5058-8b2d-6d3cf774d527 | aeebad4a796f | f5e10eca7123 | 2026-09-21 13:13:16.687888+00
 50e6b497 | 32c9a29d-5619-56de-b433-c52a7056567a | f5e10eca7123 | fe7d8ed21996 | 2026-09-21 13:13:17.901891+00
 bfad27cc | b48d3cb1-de43-585c-9ed4-865821381609 | fe7d8ed21996 | 60adf3bbbac6 | 2026-09-21 13:13:19.112474+00
(3 rows)
```

Three properties are visible in that table: the ids are uuid **v5** values derived
from the edge's ids, each row's `prev_hash` is the previous row's `hash_value` (one
chain, no fork), and the trace inside each row still carries the id the edge actually
minted:

```text
$ docker exec deploy-db-1 psql -U synapse -d synapse -t -A -c \
    "SELECT request_id || ' <- ' || (trace_json::json->>'request_id') FROM synapse_global.ledger WHERE …;"
0912b375-cbac-5058-8b2d-6d3cf774d527 <- req-1789996396680271140
32c9a29d-5619-56de-b433-c52a7056567a <- req-1789996397898105349
b48d3cb1-de43-585c-9ed4-865821381609 <- req-1789996399109191440
```

The tenant's own audit endpoint reads the same rows and agrees:

```text
$ curl -sS http://127.0.0.1:9090/v2/ledger/verify -H "Authorization: Bearer <enterprise jwt>"
{"entries_checked":3,"chain_valid":true,"first_break_at":"0001-01-01T00:00:00Z","checked_at":"2026-09-21T13:13:33.449193366Z"}
```

Latency. An A/B on the same host, same binary, same database-dsn, eight warm
compilations each — the only difference being the plan in the node's own credential,
which is the only thing that decides whether a sink is installed:

```text
enterprise node (ledger ON ):  44ms 35ms 56ms 35ms 34ms 35ms 38ms 56ms
team node       (ledger OFF):  44ms 43ms 35ms 43ms 54ms 45ms 58ms 35ms
median ledger ON : 36ms
median ledger OFF: 44ms
min/max ON : 34/56ms

$ grep 'API compile completed' /tmp/edge.log | tail -3
1:13PM INFO synapse: API compile completed total_duration_ms=265 total_tokens=0
1:13PM INFO synapse: API compile completed total_duration_ms=51 total_tokens=0
1:13PM INFO synapse: API compile completed total_duration_ms=51 total_tokens=0
```

The 265ms first request is the ONNX model's first inference, not the ledger: it
appears on the ledger-off node too (`0.249869s`), and every request after it is
34-56ms on both nodes — inside the <100ms the task asked for, and inside the 50ms
band the project tracks. After the A/B the enterprise tenant's chain held 11 rows
(3 + 8) and the team tenant's held none:

```text
              tenant_id               | rows
--------------------------------------+------
 b63b1d9d-898b-49ab-906e-64ed3edfa427 |   11
(1 row)
```

The gate, live: a `team` tenant was provisioned, given the *same* `database-dsn` and
a credential node of its own on 127.0.0.1:8098, and compiled once.

```text
$ grep -c 'Audit ledger enabled' /tmp/edge-team.log
0
$ curl -sS -o /dev/null -w '%{http_code} %{time_total}\n' -X POST http://127.0.0.1:8098/v1/compile …
200 0.249869
$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    "SELECT count(*) FROM synapse_global.ledger WHERE tenant_id='6b6e6afc-ad90-4460-a33a-da21d5ff1642';"
 count
-------
     0
(1 row)
```

The negative path was seen live too, when the edge was first started without
`SYNAPSE_MASTER_KEY` exported. Compilations kept succeeding and the failure stayed
where the design puts it:

```text
1:12PM INFO synapse: API compile completed total_duration_ms=33 total_tokens=0
1:12PM INFO synapse: API request method=POST path=/v1/compile status=200 duration_ms=34 …
1:12PM ERRO synapse: ledger: append failed error="ledger: fetch tenant secret: tenant: SYNAPSE_MASTER_KEY is required to wrap tenant secrets"
```

No trace payload, no request id, no secret, no ciphertext, and no key value in that
line — and no row written.


### Findings this phase surfaced (not fixed here — this phase wires the write path)

1. **The ledgered trace is not quite the trace the caller received.** `tokens_used`
   and `reduction_pct` are 0 in every row, because both are finalised by the call
   sites after `Compile` returns and the snapshot has to be taken before that (decision
   3). Everything else in the manifest — the ids, the scores, the counts, the intent,
   the memories and their provenance — is exact. Closing this means appending after the
   callers' fixups, which means editing `internal/proxy/proxy.go` and
   `internal/api/api.go`; that was Option C in this phase's plan and was declined to
   keep the v1 freeze. It is a two-file, handful-of-lines change whenever the project
   decides an audit entry's token count matters more than the freeze.
2. **The edge node now holds `SYNAPSE_MASTER_KEY`.** It has to: `tenant.GetSecret`
   unwraps the tenant's signing secret with it, and this phase chose in-process
   appending at the edge (Option A) over a plane-owned append route (Option B). For a
   single-tenant edge node that is one tenant's key material on one host, which is the
   deployment this is sized for — but it is a real widening of who can unwrap a signing
   secret, and Option B remains the design that does not require it. Recorded so the
   choice is visible rather than implied.
3. **There is now a half-provisioned tenant in the development database, and no way to
   repair it.** The first live provisioning attempt ran before the compose master key
   was fixed: the tenant row committed, the secret mint failed, and the request
   answered 500. `enterprise-demo` therefore has `has_secret = f`, and re-provisioning
   under that slug is refused (`ErrTenantExists`), so the tenant can never write or
   verify an audit chain. This is the non-atomic window decision 6 describes, caught
   live by accident. A repair path — reissue the secret for an existing tenant, the
   operation `ErrSecretExists` deliberately refuses — belongs in the phase that also
   does key versioning, because reissuing a secret after rows exist is exactly the
   situation Phase 17 finding 3 says must not be attempted without a key id.
4. **Key versioning is still absent.** Phase 17 finding 3 is unchanged by this phase:
   the ledger has no key id, `GenerateAndStoreSecret` refuses a second secret, and
   replacing a tenant's secret would make `Verify` report a break at `entries[0]` — the
   same answer as tampering. This phase made that limitation load-bearing rather than
   theoretical, because there are now real chains behind it.

5. **Every compilation means one row, including the ones a human is just poking at.**
   Both frozen call sites reach `Compile`, so `POST /v1/compile`, every proxied
   `/v1/messages`, `/v1/chat/completions`, and `/api/chat` turn, and
   `POST /api/playground/compile` from the trace inspector all produce a ledger entry
   with a full trace in it. That is what "after every successful compilation" asked
   for, and it is worth stating plainly: a chatty agent produces one row per turn, and
   a user experimenting in the playground adds audit rows to their tenant's chain.
   Excluding the playground would mean a per-route decision at a call site, so it is a
   deliberate later question rather than an accident.
6. **A failed append is logged once and dropped; there is no queue and no retry.** The
   negative path verified live (see above) is the intended shape — a compilation is
   never failed by its audit write — but the consequence is that a database outage
   during a burst of compilations loses those rows permanently, and the chain will not
   show the hole: each row still chains from the row before it, so the ledger looks
   internally perfect. Phase 17 finding 2's external anchor is the only thing that
   could ever make that visible, and it is still unbuilt.
7. **An enterprise edge node now writes to two stores.** Memories stay in the local
   SQLite file (`cmd/synapse` still calls `store.NewStore(cfg.DBPath)`;
   `store.NewStoreFromConfig`, the Postgres memory path, has no callers), while the
   audit ledger goes to the plane's PostgreSQL through `database-dsn`. The ledger is
   the only part of the v2 data model an edge node reaches, which is worth knowing
   before anything else on that node assumes a tenant schema exists.
8. **CI still cannot catch a broken ledger wiring.** `internal/ledger` has no untagged
   tests by design (a fake `pgx.Tx` would mostly test the fake), so the only thing that
   exercises `ledger.NewLedger(pool).Append` end to end is the manual run above. What
   is covered where CI looks is everything on this side of the database: the gate, the
   snapshot, the goroutine, the uuid mapping, the claim reader, the fail-closed stages,
   and the boot decision table — all in `internal/compiler` and `cmd/synapse`, both of
   which run untagged.

### Next phase

The write path now has a consumer and a hole, in that order. The hole is the external
anchor for the chain head (Phase 17 finding 2, restated by finding 6 here): publish or
store each tenant's head hash somewhere the credentials that can write the ledger
cannot rewrite, or "the whole chain was deleted" and "nothing was ever appended"
remain the same answer. Key versioning (finding 4) is the second item, and it now has
a concrete reason beyond rotation: the repair path finding 3 describes — reissuing the
secret of a tenant that already has rows — cannot be offered safely until a key id
exists in the row or the signed message. Returning the signing secret to the tenant
once at provisioning (Phase 17's note, decision 6's deferral) is the third item, and it
is the one that lets a tenant verify its own chain without trusting the plane. The
`tokens_used: 0` fidelity gap (finding 1) is a small change gated on unfreezing two v1
files, so it should be decided explicitly rather than absorbed. The S/R/I/T breakdown
and `trace_id` on the plane's own memory-search surface is still queued from Phase 14,
and is also the shape `.clinerules` already requires of MCP responses.

## Phase 19 — compliance audit query endpoint (complete)

Phase 16 wrote the ledger, Phase 17 taught it to verify itself, Phase 18 gave it a
producer. This phase adds the first reader: `GET /v2/compliance/audit`, one paginated
page of a tenant's own signed entries — newest first, inside a caller-chosen window,
with each stored `trace_json` handed back as a JSON object rather than as a string —
gated on the caller's verified `compliance_tier` claim, and recorded in
`synapse_global.compliance_access_log` before the answer is written.

No v1 internal package was touched, and no file that this phase did not have to touch:
`internal/tenant/migrations.go` already created both tables and the `(tenant_id,
created_at)` index in Phases 5/15, so the schema work was a read, not a change.
`openapi.yaml` documents `/v1/*` and `/health` only, so it was left alone rather than
made asymmetric.

Commit `feat: Phase 19 - compliance audit query endpoint`

New files:

- `internal/plane/compliance.go` — 296 lines: `complianceAuditRoute`, the tier, upsell,
  page-size, and access-log-timeout constants, `handleComplianceAudit` (identity →
  dependency → tier → parameters → read → record → answer), `parseAuditTrace`,
  `requireAccessRecord`, and `recordAccess`. Split from the two files below at the
  300-line ceiling this project holds every file to, along the line that is real: this
  is the file that decides what happens to a request.
- `internal/plane/compliance_types.go` — 187 lines: `ComplianceAuditor` (the contract
  its implementation satisfies), `AuditFilter`, `AuditRow`, `AuditPage`,
  `AccessRecord`, the 200/403 wire shapes, and `WithComplianceTier` /
  `ComplianceTierFromCtx`.
- `internal/plane/compliance_params.go` — 136 lines: `auditParams`, `parseAuditParams`
  (the four accepted parameters, their validation, and the redaction string), and
  `ipHash`.
- `internal/ledger/compliance.go` — 199 lines: `auditPageQuery`, `auditTotalQuery`,
  `insertAccessLog`, `Auditor`, `NewAuditor`, `AuditPage`, `RecordAccess`, and the
  compile-time assertion that the contract `internal/plane` declares is still satisfied
  here, in the package that owns the SQL.
- `internal/plane/compliance_test.go` — 182 lines: `TestComplianceAudit`, the phase's
  definition of done against a real PostgreSQL (the file the brief named; the setup it
  shares is one file over).
- `internal/plane/compliance_setup_test.go` — 242 lines: the DSN/pool helpers, the two
  provisioned tenants, the five signed appends, the router built exactly as `cmd/plane`
  builds it, the request helper, and the access-log reader.
- `internal/plane/compliance_unit_test.go` — 274 lines and
  `internal/plane/compliance_gate_test.go` — 225 lines: the untagged suite, 13 test
  functions plus two tables over a fake auditor — the response contract, the window
  round trip, the redaction rule, the tier/identity/parameter gates, and the four ways
  this endpoint refuses to serve.

Changed:

- `internal/plane/handlers.go` — `Server.auditor`, the ninth `NewServer` parameter and
  its doc paragraph, and the route registration behind `requireJWT`.
- `internal/plane/sync.go` — `complianceTierKey` appended to the existing `ctxKey`
  block (the accessors themselves live in `compliance_types.go`, beside the wire shapes
  of their only reader, because `sync.go` is at the ceiling).
- `internal/plane/handlers_test.go`, `ledger_test.go`, `search_test.go`, `sync_test.go`
  — the seven `NewServer` call sites, each gaining the new argument.
- `internal/tenant/auth.go` — `withClaims` now publishes the tier:
  `plane.WithComplianceTier(ctx, c.ComplianceTier)`.
- `cmd/plane/main.go` — `complianceAuditor := ledger.NewAuditor(pool)`, passed as the
  new argument; the comment says why the implementation is the ledger package's own read
  path and not an adapter in `main`.

### Decisions made in this phase

1. **The SQL lives in `internal/ledger`, behind a `plane`-declared interface, not in
   `internal/plane`.** The brief's step 1 reads as though the handler queries the table
   itself, and that is the one thing this codebase's structure does not allow: every
   endpoint in `internal/plane` is testable without PostgreSQL because the package holds
   interfaces rather than a database handle (`Database` has exactly one method, `Ping`).
   The implementation also could not live in `cmd/plane`, where Phase 17's
   `ledgerVerifier` adapter does: this endpoint's integration test wires the *real*
   implementation, and a test binary cannot import `package main`. So the package that
   already owns `ledgerTable`, `canonicalID`, and the table's read path (`verify.go`)
   implements a contract the HTTP package declares — the arrangement `MemoryWriter`,
   `MemorySearcher`, and `LedgerVerifier` already use.
2. **`NewServer` gained a ninth parameter rather than a setter.** The alternative — a
   `WithComplianceAuditor` method — would have avoided touching seven test call sites and
   was rejected because constructor injection is this project's documented style, and a
   server whose dependencies can be replaced after construction is a different thing from
   the one every other endpoint was written against. The mechanical cost was seven `nil`s
   and one value.
3. **The tier gate reads the verified claim, and the middleware now publishes it.**
   `compliance_tier` was already in the JWT payload and already parsed, but nothing
   exposed it to `internal/plane` (which cannot import `internal/tenant`, where the claims
   live), so `plane.WithComplianceTier`/`ComplianceTierFromCtx` were added and
   `withClaims` now calls them. Reading `synapse_global.tenants` instead would have made
   the endpoint depend on a second source of truth that can disagree with the token the
   caller is holding; the freshness cost of the claim is finding 2 below.
4. **The tier is checked before the window is validated.** A `team` tenant sending
   `limit=all` gets 403, not 400: authorization precedes validation, and the ordering also
   keeps the route from being a probe for what a valid query looks like when the caller
   may not read anything.
5. **The access record is written before the answer, fail-closed.** If the INSERT fails,
   the caller gets `500 {"error":"internal"}` and the audit data is withheld; the failure
   is logged with the tenant id and the code, and the page never leaves the process. This
   was the one behavioural question the brief left open, and the alternative (answer, log
   the failed record server-side) was rejected because it makes "log every call" a best
   effort that no auditor can rely on and no test can assert. The unit suite pins exactly
   that case: a fake whose `RecordAccess` fails gets a 500 whose body carries no entry id.
6. **`query_params_redacted` is rebuilt from parsed values, never echoed.**
   `r.URL.RawQuery` would have been one line and is a content-smuggling channel: an
   unknown parameter would land verbatim in the tenant's own audit table. The recorded
   string is `url.Values` over at most four keys (`since`, `until`, `limit`, `offset`),
   rendered from the parsed values — times in UTC as RFC 3339 Nano, integers in decimal,
   the rest dropped. The integration test sends `?content=must-not-be-recorded` and then
   asserts that string appears in no access-log row.
7. **`total` is a second `count(*)` query, not a window function.** `COUNT(*) OVER ()`
   would be one round trip, and its value disappears exactly when the derived table is
   empty — the case a client most wants a count for. The cost is stated in the code: the
   count and the page are not in one transaction, so a row appended between them makes
   `total` stale by one, and holding a transaction open to prevent that would put this
   read in front of the tenant's own appends.
8. **Every call that reaches a verified tenant is recorded — 200, 400, 403, and 500
   alike.** A denied attempt is an audit fact (it is how a run of `limit=all` probes
   becomes visible), so the 403 and 400 paths record before they answer as well. The one
   exception is the 401: with no verified tenant the row would have no one to name.
9. **The response carries the parsed trace and both chain fields.**
   `prev_hash`/`hash_value` travel with each entry so an audit reader can check an entry
   against the chain's own evidence without a second call, and the parsed `trace` replaces
   the stored string so a client is not made to decode a JSON document inside a JSON
   document.
10. **The window is parsed with `time.Parse(time.RFC3339, …)` on purpose.** Go's parser
    accepts a fractional second even though the layout does not require one, so a caller
    can send an entry's own `created_at` (RFC 3339 Nano, microsecond-exact) as an inclusive
    bound and get exactly that entry and everything after it. The integration test's
    "exactly two entries" assertion rests on this: a boundary floored to whole seconds
    would land in the *previous* entry and the test would read three.
11. **The page is capped at 200, with a default of 50.** A page carries whole traces;
    `maxSearchTopK` exists on the search endpoint for the same reason. `offset` is
    validated only for being non-negative — deep paging is a performance question the
    index answers, not an authorization one — and an inverted window (`until` before
    `since`) is an empty page rather than a 400, because it is a valid question with an
    empty answer.

### Verification (real output, this phase)

Formatting, build, and the untagged suites (what CI runs):

```text
$ gofmt -l internal/plane internal/ledger internal/tenant cmd/plane
                                     # no output: every file this phase touched is formatted
$ go build ./...                     # clean
$ go vet ./internal/plane/... ./internal/ledger/... ./internal/tenant/... ./cmd/...   # clean
$ go vet -tags integration ./internal/plane/...                                       # clean too
$ go test -count=1 ./...             # 19 packages ok, 0 failures
```

The phase's definition of done, against the compose database
(`postgres://synapse:synapse@127.0.0.1:5432/synapse`), fresh rather than cached:

```text
$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test -count=1 ./internal/plane/... -run TestComplianceAudit -v -tags integration
--- PASS: TestComplianceAuditDeniesANonEnterpriseTier (0.00s)
    --- PASS: .../team_tier   .../no_tier   .../other_tier   .../capitalized
--- PASS: TestComplianceAuditRequiresAVerifiedTenant (0.00s)
--- PASS: TestComplianceAuditRejectsBadQueryParameters (0.00s)
    --- PASS: .../limit_is_not_a_number  .../offset_is_not_a_number  .../since_is_a_date_only
    --- PASS: .../limit_is_zero          .../limit_exceeds_the_cap   .../offset_is_negative
    --- PASS: .../since_is_not_a_time    .../until_is_not_a_time     .../limit_is_negative
--- PASS: TestComplianceAuditRefusesToAnswerWhenTheAccessRecordFails (0.00s)
--- PASS: TestComplianceAuditReportsAReadFailureAsInternal (0.00s)
--- PASS: TestComplianceAuditRefusesAnUnreadableTrace (0.00s)
--- PASS: TestComplianceAuditFailsClosedWithoutAnAuditor (0.00s)
--- PASS: TestComplianceAuditRefusesAnUnverifiedRequestWithoutAnAuditor (0.00s)
--- PASS: TestComplianceAudit (0.43s)
--- PASS: TestComplianceAuditReturnsStoredTracesAsObjects (0.00s)
--- PASS: TestComplianceAuditRendersAnEmptyHistoryAsAnArray (0.00s)
--- PASS: TestComplianceAuditPassesTheWindowThrough (0.00s)
PASS
ok  synapse/internal/plane  0.520s
```

`TestComplianceAudit` is the one that needs the database, and what it did rather than
merely did-not-fail: provisioned two tenants through `tenant.Provisioner` (so the
tokens are the ones production issues, `compliance_tier` included), appended five
HMAC-signed entries to the enterprise tenant's chain through
`ledger.NewLedger(pool).Append`, then read them back over HTTP with the real
`ledger.Auditor` and the real middleware. It asserted five rows newest-first with
matching ids, request ids, `prev_hash`, and `hash_value`; that each `trace` is a JSON
**object** (`entry["trace"].(map[string]any)`) carrying the appended
`detected_intent` and one memory, which is the brief's "parsed, not a string"
requirement stated as an observation rather than by construction; that the team
tenant's token gets `403 {"error":"compliance_tier_required","upgrade_url":
"https://synapse.ai/enterprise"}` with a body naming nothing from the other tenant;
that `since` set to the second-newest entry's own `created_at`, sent to the
microsecond, returns exactly two rows with `total: 2`; that `limit=2&offset=1` returns
two rows with `total: 5`; and that the three enterprise calls plus the refused one left
four rows in `synapse_global.compliance_access_log` with the right endpoint, the right
response codes, a 64-character `ip_hash` that is not the raw address, the window and
paging recorded, and `?content=must-not-be-recorded` nowhere. It also asserts that no
token, no signing secret, and no trace content reached the process log.

Regression, unchanged by this phase but now running beside it:

```text
$ go test -count=1 -tags integration ./internal/ledger/... ./internal/tenant/...
ok  synapse/internal/ledger  1.742s
ok  synapse/internal/tenant  0.799s
```

### Findings this phase surfaced (not fixed here — this phase is one read endpoint)

1. **`compliance_access_log` is not append-only, and the ledger's machinery does not
   protect it.** Phase 15 made the ledger's immutability a database fact: the writer
   role holds INSERT and nothing else, not even SELECT. This phase writes the access log
   as the pool's own identity instead — the identity that, in the compose deployment,
   owns the table and is a Postgres superuser — so the rows recording who read a
   tenant's audit history can be rewritten or deleted by the credentials that write
   them. That is weaker evidence than the chain those rows describe, and it is the one
   asymmetry this phase introduces. A `compliance_log_writer` role (INSERT only, no
   SELECT/UPDATE/DELETE, granted to the app role) is the obvious fix and is deliberately
   not smuggled into a read-endpoint phase.
2. **The tier is as fresh as the token.** The gate reads the verified claim, so a tenant
   whose tier is lowered keeps reading until a new token is issued — and
   `tenant.TokenTTL` is 365 days, so that can be a year. Reading
   `synapse_global.tenants.compliance_tier` instead would bound the staleness at one
   query and hand the endpoint a second source of truth that can disagree with the token
   the caller holds; a short-lived tier claim or an explicit revocation check is the
   design that fixes this without that cost, and it belongs to a phase that owns token
   lifetimes.
3. **`ip_hash` is a pseudonym, not anonymization.** `hex(sha256("host:port"))` with no
   salt: the IPv4 space is small enough to brute-force, and the same address always
   hashes to the same value (which is the property the column needs — "did one client
   read this twenty times"). A per-deployment salt would raise the brute-force cost and
   make the column unjoinable across deployments; if that trade is ever taken it must be
   taken before the first production row, because existing rows would stop matching new
   ones.
4. **There is no rate limit, and each call is three database round trips.** One
   `count(*)`, one page query, one INSERT — with up to 200 whole traces per page. Every
   other plane endpoint has the same gap, so this is a known absence rather than a new
   one, but a compliance endpoint is an attractive place to hammer precisely because it
   reads history. The access log does at least make such a run visible, code included,
   which is one of the reasons the table exists.
5. **The traces this endpoint hands back carry Phase 18's fidelity gap.** `tokens_used`
   and `reduction_pct` are `0` in every ledgered trace (Phase 18 decision 3), and this is
   the second surface where a user can see it — the first being a `GET /v2/ledger/verify`
   that reports a chain verifying while the traces inside it under-report. Closing it
   still means editing two frozen v1 call sites.
6. **A page read verifies nothing, and the two audit endpoints are not joined.**
   `AuditPage` returns rows as stored; nothing here recomputes a signature. A caller who
   wants to know whether the entries they are reading are intact still needs
   `GET /v2/ledger/verify`, and a client that showed only this page would be showing rows
   it has no reason to trust. Embedding a verification verdict per page is possible and
   was not done: it would multiply the cost of a read by the length of the chain and pull
   the walk's secret handling into the page endpoint.
7. **The read runs as the table owner, so nothing in the database stops the plane from
   writing the ledger itself.** `AuditPage`'s SELECT needs the owner's identity (the
   writer role holds no SELECT), which means the running process holds every privilege on
   `synapse_global.ledger`. The append-only guarantee binds the writer role, not the
   process that can assume it. This is Phase 16's caveat restated from the read side, and
   it remains the structural limit of the design: the ledger is tamper-evident, not
   tamper-proof, against the plane's own credentials.
8. **CI still cannot run the half of this phase that matters most for the SQL.** The
   integration file needs a real database, so it runs on a developer machine (output
   above) while CI runs the 13 untagged tests. That is a better position than Phase 18's
   — which had no untagged coverage of its own wiring at all — and it is not yet the
   whole claim: the query, the redaction string as stored, and the access-log writes are
   verified where a database exists and nowhere else.

### Next phase

Finding 1 is the smallest and the most valuable: the compliance access table is the first
audit artifact this project writes that nothing in the database protects, and a role that
holds INSERT on it and nothing more is the same shape Phase 15 already applied to the
ledger. After that, the items Phases 17 and 18 queued are still open and still larger:
the external anchor for each tenant's chain head (Phase 17 finding 2 — without it, "the
whole chain was deleted" and "nothing was ever appended" are still the same answer); key
versioning, which is also what makes the secret-reissue repair path safe to offer; and
returning the signing secret to the tenant once at provisioning, the only way a tenant
verifies its own chain without trusting the plane. Finding 2's tier freshness belongs to
whichever phase picks up token lifetimes, and the S/R/I/T breakdown plus `trace_id` on
the plane's memory-search surface is still queued from Phase 14 — the same fields this
endpoint's `trace` object already carries verbatim out of the stored manifest, and the
shape `.clinerules` requires of MCP responses.


## Phase 20 — compliance report (JSON) and PDF report (complete)

Phase 19 read the ledger a page at a time. This phase summarises it: `GET
/v2/compliance/report` answers with a structured compliance report — the period it covers,
the volume, whose memories were used, how contradictions and supersessions were handled,
whether the chain is intact, and the Article 50 statement — as JSON, or as the PDF rendering
of the same document. Same enterprise-tier gate as Phase 19, same
`compliance_access_log` record before the answer, and the same tenant rule: the chain is the
verified token's own, and no request value can name another one.

No v1 internal package was touched. Two v2 packages gained files (`internal/plane`,
`internal/ledger`) and one config key was added; `cmd/plane` needed no change at all, because
the dependency the report needs — the auditor — was already wired for Phase 19.
`internal/tenant/migrations.go` was read again rather than changed: `usage_events` and the
`(tenant_id, created_at)` index already existed.

Commit `feat: Phase 20 - compliance PDF report`

New files:

- `internal/plane/compliance_report_types.go` — 221 lines: `complianceReportRoute`,
  `Article50Statement` (verbatim), `ReportWindow`, `ReportFacts`, `UsageTotals`,
  `ComplianceReport` and its seven section types, and the 503 body. The struct the brief asks
  for could not go in `compliance.go`: that file was already 296 lines, so the contract lives
  in its own file in the same package.
- `internal/plane/compliance_report.go` — 285 lines: `handleComplianceReport` (identity →
  dependency → tier → parameters → renderer → read → build → chain verdict → record →
  answer), `respondComplianceReportPDF`, `reportFailure`, `reportFilename`,
  `reportTemplatePath`.
- `internal/plane/compliance_report_build.go` — 232 lines: `buildComplianceReport` and the
  accumulator behind it. Every number the report carries is computed here, from values that
  were handed in.
- `internal/plane/compliance_report_pdf.go` — 255 lines: the template render, the renderer
  lookup, each tool family's argument shape, the temp working directory, and `cappedBuffer`.
- `internal/plane/compliance_access.go` — 74 lines: `requireAccessRecord` and `recordAccess`,
  moved out of `compliance.go` now that two endpoints write the same record and the endpoint
  is a parameter. Two reasons, one of them the ceiling.
- `internal/ledger/report.go` — 128 lines: `Auditor.ReportFacts`, the window query, and the
  metering-totals query.
- `ui/plane/compliance-report.html` — 428 lines: the print-ready A4 document, Go template
  syntax, no external assets.
- Tests: `compliance_report_unit_test.go` (196), `compliance_report_gate_test.go` (291),
  `compliance_report_build_test.go` (225), `compliance_report_pdf_test.go` (413),
  `compliance_report_setup_test.go` (213, integration), `compliance_report_test.go` (228,
  integration). 23 untagged test functions and 2 build-tagged ones.

Modified: `internal/plane/compliance.go` (the access helpers moved out; the audit handler now
passes its own route to them), `compliance_params.go` (`parseWindow` extracted and shared,
`parseReportParams` added, `formatJSON`/`formatPDF`), `compliance_types.go`
(`ComplianceAuditor` gained `ReportFacts`; `AccessRecord.Endpoint` documents both routes),
`handlers.go` (the route), `config.go` (`report-template`), `internal/ledger/doc.go`,
`internal/plane/compliance_unit_test.go` (the shared fake auditor gained the report's
fields), `synapse-plane.yaml.example`, `PROGRESS.md`.

### Decisions

1. **The report is built from the signed ledger, and the two metering-facing numbers prefer
   `usage_events` when the window has rows.** This was the phase's opening question rather
   than an implementation detail: nothing writes `usage_events` yet (metering is unbuilt), and
   every ledgered trace carries `reduction_pct: 0` (Phase 18 finding 1), so *both* candidate
   sources under-report today. The chosen rule — compilations and mean reduction from the
   metering table when it has rows for the window, one per signed ledger entry and the mean of
   the recorded traces when it does not — means the report shows honest ledger numbers now and
   real ones the moment metering lands, with no second code path to add later. The integration
   test asserts both sources and, crucially, that they produce *different* numbers (2 metering
   rows and a 15% mean against 5 ledger entries and 30%), so which source answered is
   observable rather than implied.
2. **The arithmetic is a pure function in `internal/plane`; the SQL is in `internal/ledger`.**
   The seam is `ReportFacts`: every ledger row the window holds, as stored, plus the metering
   totals. The store does not interpret traces, and the HTTP package does not hold a database
   handle. The reason is testability: `buildComplianceReport` is a function of its arguments,
   so the percentages, the de-duplication, and the cross-agent counting are covered by
   untagged tests that CI runs — 23 of this phase's 25 test functions need no database,
   including every PDF case (the renderer is faked through `PATH`, which is the same mechanism
   production uses).
3. **`chain_integrity.valid` is this request's own walk, over the whole chain, and
   `entries_in_period` is the window's count.** The verdict comes from Phase 17's
   `Ledger.Verify` through the already-wired `LedgerVerifier`, so the report does not
   reimplement signature checking and no new code path touches the tenant's signing secret.
   The two fields are deliberately different scopes: a report that certified only the rows it
   happened to read would certify nothing, and the struct says so.
4. **Every count that names a memory is a set, and the two that name events are sums.**
   `memories_superseded` and `contradictions_detected` count distinct memory ids — a memory
   superseded a thousand times is one memory, not a thousand — and the older half of a
   contradiction pair (`superseded_candidate`) is the same event seen from the other row, so it
   is not counted again. `total_memories_used` and `cross_agent_retrievals` are event counts,
   because "how many memories reached a context" and "how often another agent's memory showed
   up" are what those names promise. `memory_type_breakdown`'s denominator is the count of
   *used* memories by type, so the shares sum to 100 by construction rather than approximately.
5. **The Article 50 statement is one constant, in `internal/plane`, and both formats render it
   from that value.** The handler assigns it; the JSON field and the PDF's bordered box cannot
   disagree, because there is only one string. It is stored verbatim, with the brief's line
   wrapping joined by single spaces and no rewording (`organisation`, `2024/1689`, the
   disclaimer sentence as written). In the PDF the apostrophe in `organisation's` is `&#39;` —
   html/template escaping the same text, which is what makes the document safe to render.
6. **No caller-controlled text can reach the renderer.** The renderer is executed directly (no
   shell); its arguments are compile-time constants plus paths this process minted inside a
   directory `os.MkdirTemp` created; the report's content travels only inside the pre-rendered
   HTML file, where html/template has already escaped it. `format` is validated to one of two
   literals *before* any of that, and `since`/`until` are re-rendered from parsed `time.Time`
   values before they are recorded or used to name the attachment. The tests assert this from
   both ends: the fake renderer's argv is read back and checked for the query values it must
   not contain, and `format=pdf%3Brm%20-rf%20/`, `format=pdf%20--no-sandbox`, and
   `format=pdf%0A--no-sandbox` are all 400s.
7. **A missing renderer is 503 with the fix in the body; a renderer that fails is a logged
   500.** `findPDFTool` resolves `wkhtmltopdf` first and then the Chromium family (`chromium`,
   `chromium-browser`, `google-chrome`, `google-chrome-stable`) through `exec.LookPath`, and
   the tool lookup happens *before* the window is read: an unfulfillable request should also be
   a cheap one. The 503 body is the brief's, verbatim.
8. **No window means the whole ledger.** `since`/`until` keep Phase 19's semantics exactly —
   one shared `parseWindow`, the same RFC 3339 parsing with fractional-second fidelity, the
   same "absent means no predicate", the same inverted window meaning an empty answer rather
   than a 400. A report that silently defaulted to the last 30 days would answer a different
   question than the one printed on its own header.
9. **`report-template` is configuration with a documented default and no fatal validation.**
   The plane's import graph cannot embed `ui/` (it is not inside `internal/plane`), so the path
   is a config key: `ui/plane/compliance-report.html` by default, relative to the plane's
   working directory. A plane that never serves a PDF boots without the file; a request that
   needs it and cannot find it is a logged 500, with the path in the log and never in the
   response.
10. **`ComplianceAuditor` gained the report's read rather than the plane gaining a constructor
    parameter.** One new method (`ReportFacts`) on an interface that already meant "read a
    tenant's audit history and record that it happened" kept `NewServer`'s signature — and
    therefore 11 call sites across `cmd/plane` and seven test files — unchanged, and
    `ledger.NewAuditor` already satisfies it.


### Verification (real output, this phase)

Formatting, build, vet, and the untagged suites (what CI runs):

```text
$ gofmt -l internal/plane internal/ledger cmd/plane
                                       # no output: every file this phase touched is formatted
$ go build ./...                       # clean
$ go vet ./...                         # clean
$ go vet -tags integration ./internal/plane/... ./internal/ledger/...   # clean too
$ go test -count=1 ./...
ok  synapse/cmd/synapse      0.021s
ok  synapse/internal/api     0.878s
ok  synapse/internal/budget  0.210s
ok  synapse/internal/classifier  0.008s
ok  synapse/internal/compiler    0.761s
ok  synapse/internal/config      0.006s
ok  synapse/internal/conflict    0.006s
ok  synapse/internal/dedup       0.005s
ok  synapse/internal/embedder    2.583s
ok  synapse/internal/integration 1.938s
ok  synapse/internal/plane       0.200s
ok  synapse/internal/proxy       0.418s
ok  synapse/internal/retrieval   0.007s
ok  synapse/internal/scorer      0.008s
ok  synapse/internal/store       4.607s
ok  synapse/internal/supersession 0.006s
ok  synapse/internal/sync        4.409s
ok  synapse/internal/tenant      0.791s
ok  synapse/internal/trace       0.164s
                                       # 19 packages, exit 0
```

The phase's own tests, against the compose database
(`postgres://synapse:synapse@127.0.0.1:5432/synapse`), fresh rather than cached:

```text
$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test -count=1 ./internal/plane/... -run TestComplianceReport -v -tags integration
--- PASS: TestComplianceReportDeniesANonEnterpriseTier (0.00s)
    --- PASS: .../team_tier   .../no_tier   .../other_tier   .../capitalized
--- PASS: TestComplianceReportRequiresAVerifiedTenant (0.00s)
--- PASS: TestComplianceReportRejectsBadParameters (0.00s)
    --- PASS: .../since_is_not_a_time  .../since_is_a_date_only  .../until_is_not_a_time
    --- PASS: .../format_is_xml  .../format_is_capitalized  .../format_carries_a_flag
    --- PASS: .../format_carries_a_path  .../format_carries_a_shell_metachar
    --- PASS: .../format_carries_a_newline
--- PASS: TestComplianceReportFallsBackToJSONWhenTheQueryIsNotParseable (0.00s)
--- PASS: TestComplianceReportPassesTheWindowThrough (0.00s)
--- PASS: TestComplianceReportFailsClosedWithoutItsDependencies (0.00s)
    --- PASS: .../no_auditor   .../no_verifier
--- PASS: TestComplianceReportReportsFailuresAsInternal (0.00s)
    --- PASS: .../read_failure  .../chain_walk_failure  .../unreadable_trace
--- PASS: TestComplianceReportRefusesToAnswerWhenTheAccessRecordFails (0.00s)
--- PASS: TestComplianceReportPDFRendersThroughTheInstalledRenderer (0.02s)
--- PASS: TestComplianceReportPDFRendersThroughChromium (0.02s)
--- PASS: TestComplianceReportPDFNamesABoundedWindow (0.02s)
--- PASS: TestComplianceReportPDFAnswers503WithoutARenderer (0.00s)
--- PASS: TestComplianceReportPDFRefusesAMissingTemplate (0.00s)
--- PASS: TestComplianceReportTemplateSaysWhatTheReportSays (0.00s)
--- PASS: TestComplianceReportTemplateSignalsABrokenChain (0.00s)
--- PASS: TestComplianceReportTemplateRendersAnEmptyPeriod (0.00s)
--- PASS: TestComplianceReport (0.30s)
--- PASS: TestComplianceReportPrefersMeteringAndRefusesATeamTenant (4.30s)
--- PASS: TestComplianceReportAnswersEverySectionWithTheArticle50Statement (0.00s)
PASS
ok  synapse/internal/plane  4.690s
```

The two tagged tests are the ones that need the database, and what they did rather than
merely did-not-fail: `TestComplianceReport` provisioned an enterprise tenant through
`tenant.Provisioner`, appended five HMAC-signed entries through `ledger.NewLedger(pool).Append`
whose traces carry two used memories each (one of them another agent's), a superseded memory,
and both halves of a contradiction, then read the report back over HTTP with the real
`ledger.Auditor`, the real `Ledger.Verify` adapter, and the real middleware. It asserted the
whole document against the fixtures: `total_compilations: 5` and `avg_reduction_pct: 30` from the
ledger fallback, `total_memories_used: 9`, `memory_type_breakdown {fact: 57.14, decision:
42.86}` (four facts and three decisions — the superseded memory and the older conflict half are
not in the denominator), `cross_agent_retrievals: 2`, `unique_contributing_agents: 2`,
`contradictions_detected: 1`, `memories_superseded: 1`, `chain_integrity {valid: true,
entries_in_period: 5}`, and the Article 50 statement equal to `plane.Article50Statement`. It then
asserted that `since` set to the newest entry's own `created_at` (to the microsecond) narrows the
report to one entry and one memory type while the chain verdict still covers the whole chain. The
second tagged test inserted two `usage_events` rows (10% and 20%) and asserted the report then
says `total_compilations: 2, avg_reduction_pct: 15.00` with `entries_in_period: 5` — the two
sources disagreeing, which is the point of the sourcing decision — plus the team tenant's 403,
the PDF render, and the access-log rows for all of it (2 for the enterprise tenant, 1 for the
refused one, with the endpoint, the codes, the recorded `format`, and a 64-character `ip_hash`).

The integration test's PDF branch used a real renderer: the machine has no `wkhtmltopdf` but does
have `google-chrome 153.0.8010.52`, and the test asserts `Content-Type: application/pdf` and a
body starting `%PDF-` rather than skipping.

Regression, unchanged by this phase but now running beside it — and here is where this
phase's verification turned up something that is *not* about this phase:

```text
$ go test -count=1 -tags integration ./internal/plane/...      # three consecutive runs
ok  synapse/internal/plane  6.853s
ok  synapse/internal/plane  5.814s
ok  synapse/internal/plane  6.052s

$ go test -count=1 -tags integration ./internal/ledger/...
ok  synapse/internal/ledger  1.863s        # run 2 of 3
FAIL synapse/internal/ledger 1.525s        # run 1 of 3
    --- FAIL: TestVerifyDetectsRewrittenSignature (0.02s)
        Error: tenant: migrate ledger_revoke_public: ERROR: tuple concurrently updated (SQLSTATE XX000)
$ go test -count=1 -tags integration ./internal/tenant/...
ok  synapse/internal/tenant  1.279s        # run 1, 3 of 3
FAIL synapse/internal/tenant 0.904s        # run 2 of 3
    --- FAIL: TestProvisionerStoresTheHashAndIssuesAVerifiableToken
        Error: tenant: migrate ledger_revoke_public: ERROR: tuple concurrently updated (SQLSTATE XX000)
```

Both suites are intermittently red, in roughly one run in three, and both failures are the same
statement: `tenant.RunMigrations` re-issuing the role/ACL DDL (`REVOKE ALL ON ... FROM PUBLIC`,
`GRANT ledger_writer TO CURRENT_USER`) while another session touches the same catalog tuple.
Every integration test in both packages calls `RunMigrations` before it starts, and Go runs
package test binaries in parallel by default, so two suites sharing one database can migrate at
the same moment.

It is **pre-existing, not caused by this phase**, and that was checked rather than assumed: a
clean `git worktree` at the Phase 19 commit (`6b405ad`, this phase's parent) shows the same
failure on the same class of test when the same loop is run against it:

```text
$ git worktree add /tmp/scc-base HEAD && cd /tmp/scc-base
$ go test -count=1 -tags integration ./internal/ledger/...       # 4 consecutive runs
FAIL synapse/internal/ledger 2.002s   --- FAIL: TestVerifyRejectsUnusableInput
        Error:    tenant: migrate ledger_revoke_public: ERROR: tuple concurrently updated (SQLSTATE XX000)
ok   synapse/internal/ledger 1.845s
ok   synapse/internal/ledger 1.907s
ok   synapse/internal/ledger 2.050s
```

The worktree was removed afterwards. Phase 19's PROGRESS entry pasted one green combined run;
this phase's runs show the combined form is not reliably green on this machine. The phase's own
evidence above is therefore per-package (`./internal/plane/...` alone, three times in a row) and
the untagged `go test ./...`, both of which were green every time. The race is written up as
finding 1 below.


The live run, for the two formats the definition of done asks to be pasted. The plane was started
from the repository root with an environment-only config (`SYNAPSE_DB_DSN`, `SYNAPSE_JWT_SECRET`,
`SYNAPSE_ADMIN_TOKEN`, `SYNAPSE_MASTER_KEY` exported; no YAML file), against the compose database,
with the template at its default path:

```text
2:28PM INFO plane: Control plane config loaded listen_addr=127.0.0.1:9091 database_dsn=set
      jwt_secret=set admin_token=set master_key=set log_level=info ledger_retention_days=365
      report_template=ui/plane/compliance-report.html
2:28PM INFO plane: migrations complete schema=synapse_global
2:28PM INFO plane: Synapse Control Plane v2.0.0 listening addr=127.0.0.1:9091
```

The enterprise-tier token needed one step outside HTTP, because `POST /v2/tenants` hard-codes the
tier to `team` (finding 2): a throwaway `main` package calling the *production* issuer
(`tenant.Provisioner`) and the ledger's real write path, appending five fixture entries. It lived
in `.tmp-mint/`, printed only ids and tokens, and was deleted before the commit —
`git status --porcelain` at the end of this section is the proof.

**JSON report** (`format=json`, the response itself):

```text
$ curl -sS 'http://127.0.0.1:9091/v2/compliance/report?format=json' -H "Authorization: Bearer $ENTERPRISE_JWT" | python3 -m json.tool
{
    "header": {
        "tenant_id": "2dad74fe-de20-4eda-8b7a-e484b06d7a0f",
        "period": { "since": null, "until": null },
        "generated_at": "2026-09-21T14:28:45.15524909Z",
        "synapse_version": "2.0.0"
    },
    "summary": { "total_compilations": 5, "total_memories_used": 9, "avg_reduction_pct": 30 },
    "memory_type_breakdown": { "decision": 42.86, "fact": 57.14 },
    "global_brain": { "cross_agent_retrievals": 2, "unique_contributing_agents": 2 },
    "conflict_resolution": { "contradictions_detected": 1 },
    "supersession": { "memories_superseded": 1 },
    "chain_integrity": {
        "valid": true,
        "last_verified_at": "2026-09-21T14:28:45.158292894Z",
        "entries_in_period": 5
    },
    "article_50_statement": "This report is generated by Synapse Context Compiler, which provides cryptographically signed, tamper-evident audit trails of all memory compilation decisions made by AI agents in this organisation. Each compilation event is logged with its full decision trace, memory provenance, scoring rationale, and chain integrity verification. DISCLAIMER: This report provides technical traceability infrastructure. It does not constitute legal advice or guarantee regulatory compliance with Regulation (EU) 2024/1689 or any other instrument. Consult qualified legal counsel regarding your organisation's specific obligations under the EU AI Act."
}
```



**PDF report.** The definition of done asks for the `Content-Type` header from `curl -I`; `curl -I`
sends `HEAD`, and only `GET` is routed, so it answers `405 Method Not Allowed` with `Allow: GET` —
asserting the route rather than reporting it, and finding 7 says why a `HEAD` route was not added
purely for this. The headers below are therefore from a `GET`, which runs the same handler:

```text
$ curl -sS -I 'http://127.0.0.1:9091/v2/compliance/report?format=pdf' -H "Authorization: Bearer $ENTERPRISE_JWT"
HTTP/1.1 405 Method Not Allowed
Allow: GET

$ curl -sS -D /tmp/phase20-pdf-headers.txt -o /tmp/phase20-report.pdf \
    'http://127.0.0.1:9091/v2/compliance/report?format=pdf&since=2026-09-01T00:00:00Z' \
    -H "Authorization: Bearer $ENTERPRISE_JWT"
$ cat /tmp/phase20-pdf-headers.txt
HTTP/1.1 200 OK
Content-Disposition: attachment; filename="compliance-report-2026-09-01_now.pdf"
Content-Type: application/pdf
Date: Mon, 21 Sep 2026 14:28:54 GMT
Transfer-Encoding: chunked

$ ls -l /tmp/phase20-report.pdf
-rw-rw-r-- 1 ranscky ranscky 53306 Sep 21 14:28 /tmp/phase20-report.pdf
$ file /tmp/phase20-report.pdf
/tmp/phase20-report.pdf: PDF document, version 1.4, 2 page(s)
```

The document is the report, not a placeholder — `pdftotext -layout` on those bytes:

```text
$ pdftotext -layout /tmp/phase20-report.pdf - | sed -n '1,12p;40,60p'
generated 21 Sep 2026 14:28 UTC
synapse v2.0.0

Synapse Context Compiler
COMPLIANCE REPORT — ARTICLE 50 TRACEABILITY

TENANT                                        PERIOD COVERED
2dad74fe-de20-4eda-8b7a-e484b06d7a0f          01 Sep 2026 00:00 UTC → now

SUMMARY
5                  9                  30.00%
Compilations       Memories used      Average context reduction
...
   CHAIN VERIFIED
   Entries in this period                                     5
   Last verified                          21 Sep 2026 14:28:50 UTC
   Every entry in this tenant's audit ledger was re-checked against its HMAC signature and its
   predecessor's hash at the time this report was generated.

    ARTICLE 50 STATEMENT
    This report is generated by Synapse Context Compiler, which provides cryptographically signed,
    tamper-evident audit trails ... Consult qualified legal counsel regarding your
    organisation's specific obligations under the EU AI Act.
```

The tenant id, the `30.00%`, the entry count and the window are all this tenant's own values from
the same `ComplianceReport` the JSON body carried, and the statement sits in its bordered box after
the findings.


**Refusals and the renderer-less host:**

```text
$ curl -sS -w ' HTTP %{http_code}\n' 'http://127.0.0.1:9091/v2/compliance/report?format=json' -H "Authorization: Bearer $TEAM_JWT"
{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"} HTTP 403

$ curl -sS -w ' HTTP %{http_code}\n' 'http://127.0.0.1:9091/v2/compliance/report?format=pdf' -H "Authorization: Bearer $TEAM_JWT"
{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"} HTTP 403

# A second plane started with PATH pointing at an empty directory, i.e. a host with no renderer:
$ curl -sS -i 'http://127.0.0.1:9092/v2/compliance/report?format=pdf' -H "Authorization: Bearer $ENTERPRISE_JWT"
HTTP/1.1 503 Service Unavailable
Content-Type: application/json
Content-Length: 86

{"error":"pdf_tool_unavailable","message":"install wkhtmltopdf to enable PDF reports"}
$ curl -sS -o /dev/null -w 'HTTP %{http_code} %{content_type}\n' 'http://127.0.0.1:9092/v2/compliance/report?format=json' -H "Authorization: Bearer $ENTERPRISE_JWT"
HTTP 200 application/json
```

**Every call above was recorded**, read straight from the table rather than from any endpoint:

```text
$ docker exec deploy-db-1 psql -U synapse -d synapse -c \
    "SELECT endpoint, query_params_redacted, response_code, left(ip_hash,12) AS ip_hash_prefix, created_at \
       FROM synapse_global.compliance_access_log WHERE tenant_id IN ('$ENTERPRISE_TENANT','$TEAM_TENANT') ORDER BY created_at, id;"
       endpoint        |           query_params_redacted           | response_code | ip_hash_prefix |          created_at
-----------------------+-------------------------------------------+---------------+----------------+-------------------------------
 /v2/compliance/report | format=json                               |           200 | 7db19d8c23b9   | 2026-09-21 14:28:45.161594+00
 /v2/compliance/report | format=pdf&since=2026-09-01T00%3A00%3A00Z |           200 | 5f27b71e0f38   | 2026-09-21 14:28:54.407591+00
 /v2/compliance/report | format=json                               |           403 | b44d0ad8f66f   | 2026-09-21 14:29:02.294897+00
 /v2/compliance/report | format=pdf                                |           403 | c7e8fe942d8f   | 2026-09-21 14:29:02.336042+00
 /v2/compliance/report | format=pdf                                |           503 | 1aaec9d9abb1   | 2026-09-21 14:29:09.384416+00
 /v2/compliance/report | format=json                               |           200 | 0880a023414f   | 2026-09-21 14:29:09.435163+00
(6 rows)
```

Six calls, six rows: one endpoint name, the window and the format exactly as they were parsed, a
64-character `ip_hash` in every case (never the address), and the 403s and the 503 present as the
fail-closed rule requires — an attempt is an audit fact.

```text
$ git status --porcelain        # after the throwaway helper was removed, before the commit
 M bin/synapse                  # pre-existing local build artifact, NOT part of this commit
 M internal/ledger/doc.go
 M internal/plane/compliance.go
 M internal/plane/compliance_params.go
 M internal/plane/compliance_types.go
 M internal/plane/compliance_unit_test.go
 M internal/plane/config.go
 M internal/plane/handlers.go
 M synapse-plane.yaml.example
 M PROGRESS.md
?? internal/ledger/report.go
?? internal/plane/compliance_access.go
?? internal/plane/compliance_report.go
?? internal/plane/compliance_report_build.go
?? internal/plane/compliance_report_build_test.go
?? internal/plane/compliance_report_gate_test.go
?? internal/plane/compliance_report_pdf.go
?? internal/plane/compliance_report_pdf_test.go
?? internal/plane/compliance_report_setup_test.go
?? internal/plane/compliance_report_test.go
?? internal/plane/compliance_report_types.go
?? internal/plane/compliance_report_unit_test.go
?? ui/plane/
```

No `.tmp-mint/`, no stray binary, and no v1 internal package in the list.

### Findings this phase surfaced (not fixed here — this phase is one read endpoint)

1. **`tenant.RunMigrations` is not safe to run concurrently, and the integration suites prove it
   intermittently.** The verification section's failing runs are all this: `tenant: migrate
   ledger_revoke_public: ERROR: tuple concurrently updated (SQLSTATE XX000)`, from the `REVOKE ALL
   ON ... FROM PUBLIC` / `GRANT ledger_writer TO CURRENT_USER` statements that `migrations`
   re-issues on every call. It is reproduced on the unmodified Phase 19 tree, so it is not this
   phase's, but it makes "paste a green combined integration run" unreliable for whoever writes the
   next phase's PROGRESS entry. The file's own comment already says a second plane booting
   concurrently is unsupported; `SELECT pg_advisory_xact_lock(...)` as the first statement of
   `RunMigrations` would make that limitation true in the database rather than in a comment, and it
   is a one-line fix in a file this phase deliberately did not touch.
2. **The plane image cannot render a PDF, and does not ship the template.** `deploy/Dockerfile.plane`
   copies exactly one binary into `alpine:3.19`: no `ui/`, and no Chromium or wkhtmltopdf. In the
   compose deployment `?format=pdf` therefore answers the documented 503 (proof that the fail-closed
   path works, and useless to a compliance officer), and a deployment that installed a renderer but
   not the template would get a 500. The ways forward, in increasing cost: `COPY` the `ui/` tree and
   serve the HTML for client-side printing; add `chromium` (or `wkhtmltopdf`) to the image and accept
   the size, which is the only option that makes the endpoint self-contained; or split the renderer
   into a sidecar. This phase did none of them, and the finding is here so the choice is explicit.
3. **No HTTP path mints an enterprise-tier token.** `POST /v2/tenants` writes
   `ComplianceTier: defaultComplianceTier` — the literal `"team"` — so the tier both compliance
   endpoints gate on is unreachable from outside: a tenant is enterprise only if a caller used
   `tenant.Provisioner` directly (as the tests do) or a row was edited. That is Phase 2's decision
   restated from the reporting side, and it is why this phase's live evidence needed a throwaway
   helper. A validated `compliance_tier` field on the provisioning request (admin-only, since it is
   a billing/compliance fact rather than a tenant preference) closes it.
4. **Nothing writes `usage_events`, and `avg_reduction_pct` still reads 0 in any deployment.** The
   sourcing decision makes the report metering-ready, but until metering exists the number comes
   from ledgered traces whose `reduction_pct` is 0 (Phase 18 finding 1). So a compliance officer
   reading today's report sees a real compilation count, a real memory-type mix, real conflict and
   supersession counts, a real chain verdict — and `0.00%` context reduction, with no field in the
   report to explain it. The fixes are metering (write the table) or Phase 18's fidelity gap (two
   frozen v1 call sites); a note field on the summary was not added, because the brief's struct does
   not have one.
5. **A report costs a whole-window read plus a whole-chain walk, with no cap.** The audit
   endpoint's page is limited to 200 rows; this one is limited by nothing, deliberately, because a
   report covers the period it names. The consequence is that a tenant with a hundred thousand
   entries pays a hundred thousand re-signed HMACs per report — and the walk is the *chain's*
   length, not the window's, since the verdict covers everything. A cached verdict with a short TTL,
   or a per-tenant rollup in the metering phase's shape, is the fix.
6. **`usage_events` has no index on `(tenant_id, created_at)`.** The report's totals query uses
   exactly that predicate, and today it is a sequential scan over a table nothing writes. It is
   cheap now and will not be once metering lands, which makes it a migration worth adding in the
   phase that starts writing rows rather than a phase that reads an empty table.
7. **`curl -I` cannot show this endpoint's headers.** Only `GET` is routed (chi's `Get`), so `HEAD`
   is a 405 — correct, and a small deviation from the definition of done's wording, which asked for
   a `curl -I`. Adding a `HEAD` route would run the whole report and the whole PDF render to produce
   headers nobody reads, unless a header-only path were written; that is a decision for a phase that
   wants it, not a side effect of this one.
8. **The chain verdict inside a report is not itself signed.** `chain_integrity.valid` is a claim
   this process makes at a moment in time, and the document carrying it is not a ledger row, so
   nobody can later prove which report a tenant was shown. Phase 19's finding 6 said the same about
   a page read; a report is a stronger case for it, because a PDF is the artifact that gets filed.
   Appending the report's own digest as a ledger entry — one row per report, with the window in the
   payload — is the shape that would close it, and it is a write, so it belongs with a phase that
   owns the ledger's growth.
9. **`contradictions_detected` and `memories_superseded` are only as complete as the traces they
   are computed from.** Both are read from `trace_json`, and a compilation whose trace was appended
   before the contradicting memory was written will not mention it; nothing back-fills a trace,
   deliberately, because a trace is a record of what the compiler saw rather than a mutable view of
   the store. The store's own `memories` table is the fuller record (`conflict_status`,
   `superseded_by`) and lives in the tenant's schema, which the compliance path deliberately does
   not reach into for a cross-period report. An implementation that wants the store's view has to
   decide whose numbers a compliance report should carry, which is a question about the report
   rather than about the query.


### Next phase

Two items are now the obvious ones, and both come from findings above rather than from a roadmap.
The smallest is finding 1 — an advisory lock at the top of `tenant.RunMigrations` — because it is
one statement in the file that already owns the append-only role, and it turns an intermittent test
failure into a database guarantee. The largest is the entry the last several phases have all
queued: the external anchor for each tenant's chain head (Phase 17 finding 2, restated by Phase 18
finding 6 and Phase 20 finding 8), without which "the whole chain was deleted" and "nothing was
ever appended" remain the same answer, and without which no report can prove which report a tenant
was shown. Key versioning and returning the signing secret to the tenant at provisioning are the
other two queued ledger items.

Metering is the phase that would make this report's summary section honest rather than merely
explicit (findings 4 and 6): it writes the table the report already prefers, and it is where
`avg_reduction_pct` stops being 0. The compliance-tier provisioning field (finding 3) is a small
addition that belongs with whichever phase touches `POST /v2/tenants` next. And the S/R/I/T
breakdown plus `trace_id` on the plane's memory-search surface is still queued from Phase 14, still
the shape `.clinerules` requires of MCP responses, and now the third surface (after the audit page
and this report) where those fields would be read.

## Phase 21 — the compliance tier gate on every compliance surface (complete)

Phases 19 and 20 each built a compliance endpoint and each put the same gate in front of
it. Phase 17's `GET /v2/ledger/verify` — the chain verdict, which is the third thing a
compliance officer asks the same ledger — had no gate at all: a `team` or `business` JWT
could walk its own chain, read its own entry ids and break timestamps, and get a verdict
that Phase 19 and Phase 20 would have refused to read the same rows for. This phase closes
that, and closes it by making the gate one thing instead of three:

- `GET /v2/compliance/chain-integrity` is the chain verdict's canonical path. Phase 17's
  `/v2/ledger/verify` is still routed, as a deprecated alias of the *same handler*.
- `requireComplianceTier` (`internal/plane/compliance_tier.go`) is the one place the tier
  is compared, the one place the `403` body is written, and the one place a refusal is
  recorded. All three surfaces call it, so they cannot drift apart.
- Every refusal answers exactly
  `{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}`,
  from enterprise, business, and team tokens alike.

No v1 internal package was touched. `internal/ledger` and `internal/tenant` were read and
*not* changed: `compliance_access_log.endpoint` is plain `text` (Phase 19's migration), so
the third surface name needed no schema work, and `openapi.yaml` documents `/v1/*` and
`/health` only, which is Phase 19's reason for leaving it alone again rather than making
it asymmetric. `internal/plane` was the only package that changed.

Commit `feat: Phase 21 - 403 enforcement on all compliance endpoints`

New files:

- `internal/plane/compliance_tier.go` — 81 lines: `requireComplianceTier`, plus the two
  arguments that are worth writing down — why it is a handler helper and not chi
  middleware (the refusal's access record is written *with* the answer's status code, and
  middleware runs before the handler that knows the tenant and the parsed window), and why
  the gate lives in the handler rather than on a route (two paths reach one handler, so a
  route-attached gate would leave whichever spelling was registered without it open).
- `internal/plane/compliance_gate_test.go` — 233 lines: `TestComplianceGate`, the 3×3
  matrix (three surfaces × enterprise/business/team = the nine cases the definition of
  done names), and `TestLedgerVerifyAliasIsGatedByComplianceTier` for the alias, whose
  name deliberately does *not* match `-run TestComplianceGate` so the definition-of-done
  command keeps reporting exactly nine.

Changed:

- `internal/plane/ledger.go` — 136 → 181 lines: `complianceChainIntegrityRoute` added,
  `ledgerVerifyRoute` kept and documented as the deprecated alias, the gate inserted after
  the identity and dependency checks, and the file, type, interface, and handler docs
  rewritten to name both paths.
- `internal/plane/handlers.go` — 219 → 227 lines: both paths registered to
  `s.handleVerifyLedger`, and the `Routes` comment rewritten for three compliance surfaces.
- `internal/plane/compliance.go` — 245 → 249 lines: the inline gate block (ten lines)
  replaced by one `requireComplianceTier` call; the tier and upsell constants' docs widened
  from one endpoint to the surfaces they now gate.
- `internal/plane/compliance_report.go` — 285 → 283 lines: the same replacement, and the
  tier-gate bullet in the file header now points at the shared implementation.
- `internal/plane/compliance_types.go` — 201 → 207 lines: `AccessRecord.Endpoint` names all
  three surfaces and says the record carries the canonical name rather than the spelling
  the caller used; `ComplianceAuditor` is "the compliance surfaces'" dependency now.
- `internal/plane/ledger_test.go` — 222 → 238 lines: the three cases that reach the
  verifier present an enterprise token, and the dependency-before-tier order is pinned in
  a comment on `TestVerifyLedgerFailsClosedWithoutAVerifier`. The two 401 cases are
  untouched, because the middleware still refuses them before any gate runs.

Renamed:

- `internal/plane/compliance_gate_test.go` → `compliance_audit_gate_test.go` (225 lines,
  contents unchanged, `git mv`). That file is the audit endpoint's refusal suite; the
  name the brief asked for now holds the matrix that covers all three surfaces, and the
  rename makes it symmetric with `compliance_report_gate_test.go`, which it already sat
  beside.

### The breaking change, stated plainly

A `team` or `business` token presented to `GET /v2/ledger/verify` got `200` with a chain
verdict from Phase 17 through Phase 20, and gets `403` now. That was verified rather than
assumed before the change: on `bca0303`, `TestVerifyLedgerAnswersWithTheChainVerdict`
passed while presenting `tenantToken` (plan `team`, tier `team`) to that route, and it is
updated in this phase to present an enterprise token instead. The path itself was kept —
precisely so the break is the tier requirement and not a `404` for every deployed client
holding the old URL — and the requirement is the point of the phase: the same chain rows
that the audit page refuses to page for a `team` tenant were readable as a verdict.

### Decisions

1. **Alias, not a rename.** There are no callers of `/v2/ledger/verify` left in this
   repository (`grep '/v2/ledger'` finds the route, its tests, and prose comments), so a
   rename would have been mechanically safe *here* — but the route shipped in Phase 17 to
   deployments this repository cannot see. Keeping it costs one `router.Get` line and one
   constant, and it turns a client-breaking change into a tier requirement.
2. **The gate is in the handler, not on the route.** Two paths reach one handler, so a
   gate attached to a route would have to be attached twice and would be one edit away
   from being attached once. It also preserves what Phases 19 and 20 documented: the
   refusal is *recorded before it is answered*, which a pre-handler middleware could not
   do — it has no verified window to record, because the handler that parses one has not
   run yet.
3. **A helper, not middleware, for the audit and report surfaces too.** Collapsing the two
   existing inline blocks into the same helper is what makes "all three surfaces refuse
   identically" a property rather than a coincidence; the 403 body and the reason string
   now exist in exactly one place in the codebase.
4. **The refusal is recorded, and the record names the surface rather than the spelling.**
   A refusal reached through the alias writes `complianceChainIntegrityRoute` into
   `compliance_access_log.endpoint`, because the record answers "which compliance surface
   was asked for", and because a route constant is not caller input.
5. **Identity, then dependency, then tier.** The gate sits *after* the nil-verifier check,
   which is the order the audit and report handlers already document. So a plane started
   without a chain verifier answers `500` to an enterprise caller rather than `403` to
   everyone, and `TestVerifyLedgerFailsClosedWithoutAVerifier` pins that with an
   enterprise token.
6. **The matrix moves plan and tier together; the claim-versus-plan distinction is pinned
   elsewhere.** A real business tenant has plan `business` *and* tier `business`, so the
   nine cases read that way. That a gate written against `plan` would be wrong is asserted
   by the alias test's `plan=enterprise, tier=team` case, and by Phases 19/20's existing
   `no tier`, `hipaa`, and `Enterprise` cases on the audit and report surfaces.

### Test output

The definition of done, run against the working tree (`-count=1` only disables the test
cache, so what is pasted is what ran):

```text
$ go test ./internal/plane/... -run TestComplianceGate -v -count=1
=== RUN   TestComplianceGate
=== RUN   TestComplianceGate/compliance_audit/enterprise
=== RUN   TestComplianceGate/compliance_audit/business
=== RUN   TestComplianceGate/compliance_audit/team
=== RUN   TestComplianceGate/compliance_chain_integrity/enterprise
=== RUN   TestComplianceGate/compliance_chain_integrity/business
=== RUN   TestComplianceGate/compliance_chain_integrity/team
=== RUN   TestComplianceGate/compliance_report/enterprise
=== RUN   TestComplianceGate/compliance_report/business
=== RUN   TestComplianceGate/compliance_report/team
--- PASS: TestComplianceGate (0.00s)
    --- PASS: TestComplianceGate/compliance_audit/enterprise (0.00s)
    --- PASS: TestComplianceGate/compliance_audit/business (0.00s)
    --- PASS: TestComplianceGate/compliance_audit/team (0.00s)
    --- PASS: TestComplianceGate/compliance_chain_integrity/enterprise (0.00s)
    --- PASS: TestComplianceGate/compliance_chain_integrity/business (0.00s)
    --- PASS: TestComplianceGate/compliance_chain_integrity/team (0.00s)
    --- PASS: TestComplianceGate/compliance_report/enterprise (0.00s)
    --- PASS: TestComplianceGate/compliance_report/business (0.00s)
    --- PASS: TestComplianceGate/compliance_report/team (0.00s)
PASS
ok  	synapse/internal/plane	0.008s
```

Nine of nine. The alias's own case, deliberately outside that filter:

```text
$ go test ./internal/plane/... -run TestLedgerVerifyAlias -v -count=1
=== RUN   TestLedgerVerifyAliasIsGatedByComplianceTier
=== RUN   TestLedgerVerifyAliasIsGatedByComplianceTier/enterprise_tier
=== RUN   TestLedgerVerifyAliasIsGatedByComplianceTier/enterprise_plan,_team_tier
--- PASS: TestLedgerVerifyAliasIsGatedByComplianceTier (0.00s)
    --- PASS: TestLedgerVerifyAliasIsGatedByComplianceTier/enterprise_tier (0.00s)
    --- PASS: TestLedgerVerifyAliasIsGatedByComplianceTier/enterprise_plan,_team_tier (0.00s)
PASS
ok  	synapse/internal/plane	0.006s
```

Regression, run with this phase's tree:

```text
$ go test ./internal/plane/... -count=1
ok  	synapse/internal/plane	0.115s

$ go test ./... -count=1
?   	synapse/cmd/benchmark	[no test files]
?   	synapse/cmd/counttokens	[no test files]
?   	synapse/cmd/mergesessions	[no test files]
?   	synapse/cmd/plane	[no test files]
ok  	synapse/cmd/synapse	0.011s
ok  	synapse/internal/api	1.040s
ok  	synapse/internal/budget	0.278s
ok  	synapse/internal/classifier	0.008s
ok  	synapse/internal/compiler	0.758s
ok  	synapse/internal/config	0.004s
ok  	synapse/internal/conflict	0.007s
ok  	synapse/internal/dedup	0.006s
ok  	synapse/internal/embedder	3.949s
ok  	synapse/internal/integration	1.964s
?   	synapse/internal/ledger	[no test files]
ok  	synapse/internal/plane	0.505s
ok  	synapse/internal/proxy	0.569s
ok  	synapse/internal/retrieval	0.018s
ok  	synapse/internal/scorer	0.012s
?   	synapse/internal/session	[no test files]
ok  	synapse/internal/store	6.314s
ok  	synapse/internal/supersession	0.010s
ok  	synapse/internal/sync	6.472s
ok  	synapse/internal/tenant	0.775s
ok  	synapse/internal/trace	0.200s

$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/plane/... -tags integration -count=1
ok  	synapse/internal/plane	17.194s
```

The integration run (91 PASS lines, 0 FAIL) is the one that matters most here: it wires the
real `ledger.Auditor`, the real chain verifier, and a real PostgreSQL, and it includes
Phase 19's and Phase 20's assertions about `compliance_access_log` itself — the row a
refused read must leave, carrying its endpoint, response code, window, and hashed address —
plus `TestComplianceReportPrefersMeteringAndRefusesATeamTenant`, which asserts the report's
`403` against a real team token. All of them pass unchanged, which is the evidence that
collapsing the two inline gates into `requireComplianceTier` was behaviour-preserving.
`gofmt -l internal/plane` is empty, and the longest file this phase touched is
`compliance_report.go` at 283 lines.

### Findings this phase surfaced (not fixed here — this phase is a gate)

1. **The chain verdict's *success* path is still not access-logged, while its refusals now
   are.** A refused chain-integrity call writes a `403` row when an auditor is wired; a
   successful one writes nothing, because Phase 17's handler had no access record and this
   phase did not add one. That asymmetry is deliberate — a gate is not a feature, and
   adding a record to the success path means deciding what an "endpoint read" means for a
   route with no query parameters and no body — but it is a real gap: "tenant X read its
   chain verdict at T" is not recoverable from the access log today, and it is the one
   compliance surface where that is true.
2. **A failed access-log write turns a chain-integrity `403` into a `500`.** This follows
   from recording the refusal at all, and it is the same coupling the audit and report
   endpoints have always had: refuse rather than answer unrecorded. The weaker alternative
   — log the failure and refuse anyway with `403` — was rejected because it makes "every
   refusal is recorded" a best effort. What it costs is now explicit: a chain-integrity
   refusal is the answer whose *shape* depends on the access-log database.
3. **Tier staleness, inherited and now three surfaces wide.** The gate reads the signed
   `compliance_tier` claim, never the registry row (Phase 19's decision, kept), so a tenant
   downgraded from enterprise keeps reading the audit page, the chain verdict, and the
   report until its token is replaced. Phase 19 stated this for one surface; it is now the
   same statement for all three, and there is still no token-revocation path.
4. **`GET /v2/ledger/verify` is now a compliance surface that is not named like one.** The
   alias keeps deployed clients working, but it also means the compliance surface set is
   larger than the `/v2/compliance/*` prefix suggests — a reader auditing "which routes
   require enterprise" cannot answer it by reading route paths alone. `handlers.Routes`'
   comment and `compliance_tier.go` both say so, and a future phase that dares a
   deprecation window could remove the alias and make the prefix the whole answer.
5. **The three surfaces refuse with one body and one link.** `403`
   `{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}` is
   now identical everywhere by construction, which is what the brief asked for. It also
   means the body cannot tell a client which surface it was refused on — the access log
   can, the response cannot. That is the price of uniformity and it is stated rather than
   discovered later.

### Next phase

Nothing about the gate is left queued; what is queued is the same list the last three
phases ended with, plus one item this phase made sharper. The largest is still the external
anchor for each tenant's chain head (Phase 17 finding 2, Phase 18 finding 6, Phase 20
finding 8), without which "the whole chain was deleted" and "nothing was ever appended"
remain the same answer. The smallest is `tenant.RunMigrations`' advisory lock (Phase 20
finding 1). Metering is the phase that would make Phase 20's summary honest rather than
merely explicit. The compliance-tier provisioning field belongs with whichever phase
touches `POST /v2/tenants` next. And item 1 above is new and small enough to travel with
any of them: record the chain verdict's successful reads, so the one compliance surface
whose successes are invisible stops being the exception.

## Phase 22 — MCP server boots (complete)

Phase 22 is the first phase of the MCP surface, and it deliberately ships no tools: the server,
two transports, one placeholder tool (`synapse_ping`), and the plumbing that starts it. What it
proves is that a request reaches this process and an answer comes back, over both transports,
without an MCP-aware client in between.

Commit `feat: Phase 22 - MCP server boots`

The dependency is `github.com/mark3labs/mcp-go v1.1.0`, and it moves this project's Go floor.
`go.mod`'s `go 1.22.5` is now `go 1.25.5`: every published mcp-go version requires at least Go
1.23 (v0.10.0 through v0.48.0 declare `go 1.23`) and v0.49.0 onward declares `go 1.25.5`, so
there was no version to pin that avoided the jump. The floor is declared rather than left
implicit (`go get go@1.25.5`) and the six other places that pinned 1.22 moved with it —
`ci.yml` (all three jobs), `release.yml`, `deploy/Dockerfile.plane`, `README.md`, `setup.sh`,
and `.clinerules`' own stack line, since a future session reading "Go 1.22+" would be reading
something false.

New files:

- `internal/mcp/server.go` — 224 lines: `Server` (store seam + config + the mcp-go server),
  `NewServer`, `registerTools`, and `Serve(ctx, transport, port)` with its two transports.
  `Store` names `store.Backend`, the contract `internal/store/factory.go` already declares and
  that nothing consumed until now: `*store.Store` — what `main` holds — satisfies it as it
  stands, so the wiring needed no adapter, and no v1 file changed to make it fit. The brief's
  literal `store store.Store` could not compile: every `Store` method has a pointer receiver at
  `internal/store/store.go:113-517`, and `*Store` is not assignable to `Store`.
- `internal/mcp/server_test.go` — 262 lines: five tests, all through mcp-go's own client
  (in-process and Streamable HTTP), plus a compile-time assertion that `*store.Store` satisfies
  this package's seam.
- `.cursor/mcp.json` — the editor template the brief names, written verbatim:
  `{"mcpServers":{"synapse":{"command":"synapse","args":["--mcp"]}}}`, mode `0600` per the
  v1 file-permission rule (git records only the exec bit, so this is a local-mode statement).

Changed:

- `cmd/synapse/main.go` — 641 → 713 lines: `--mcp` and `--mcp-port`, the flag merge, the boot
  block placed after every other initialisation, and a shutdown wait for the MCP transports.
- `internal/config/config.go` — 339 → 364: `MCPEnabled`/`MCPPort` with defaults `false`/`0`,
  and one `Validate` rule (`mcp-port` must be 0-65535, zero meaning stdio).
- `internal/config/config_test.go` — 204 → 259: four validation cases and
  `TestDefaultConfigMCPKnobs`.
- `synapse.yaml.example` — `mcp-enabled: false` and `mcp-port: 0`, with the reasoning for the
  default in the comment above them.
- `go.mod`/`go.sum`, `ci.yml`, `release.yml`, `deploy/Dockerfile.plane`, `README.md`,
  `setup.sh`, `.clinerules` — the Go 1.25.5 floor, as above.
- `bin/synapse` was rebuilt for the manual verification below and is **not** staged: it was
  already dirty in the working tree before this phase (`M bin/synapse`, last committed in
  `371186d`), and Phase 21's commit touched source plus PROGRESS.md only. It grew from 17.0 MB
  to 24.8 MB; CI's 50 MB gate still passes, and the growth is mcp-go's code (jsonschema
  validation, uritemplate, cast) rather than anything this phase wrote.

### The two transports, and why stdio is what `--mcp` alone gets

`Serve` accepts exactly `"stdio"` and `"tcp"`; anything else is an error rather than a fallback,
so a typo cannot silently pick a transport.

- **stdio** — `NewStdioServer(...).Listen(ctx, os.Stdin, os.Stdout)`, not mcp-go's convenience
  `ServeStdio`: `ServeStdio` installs its own signal handler *and* its own context, so it could
  never stop for the context `main` cancels. `Listen` takes both, reads through a goroutine that
  selects on `ctx.Done()` (`server/stdio.go:480-498`), and returns the context's error when
  cancelled — which `serveStdioWith` treats as a clean stop, alongside EOF.
- **tcp** — Streamable HTTP (`NewStreamableHTTPServer`, endpoint `/mcp`), the current spec
  transport rather than the deprecated SSE one, bound through `listenAddr`, which hard-codes
  `127.0.0.1` and rejects any port outside 1-65535. There is no configuration that widens the
  bind: this server has no authentication of its own yet, so a routable interface would be a
  memory-readable-by-anyone surface.

`--mcp-port`'s own default (8765) is the port TCP *would* use, not a request for TCP. Writing it
into `cfg.MCPPort` unconditionally would make the brief's own rule (`cfg.MCPPort > 0` selects
TCP) unreachable for stdio, and every editor integration would start an HTTP listener nobody
asked for. So the flag is merged only when it was explicitly passed (`flag.Visit`), and
`mcp-enabled: true` in the config file works on its own. Three runs pin that behaviour: the flag
alone (stdio), the flag plus an explicit port (tcp), and the config file with no flags at all
(both).

### Verification — real output

Build, vet, formatting:

```text
$ go build ./cmd/synapse && go build -o bin/synapse ./cmd/synapse
(no output)

$ go vet ./internal/mcp/...
(no output)

$ gofmt -l internal/mcp
(no output)
```

`gofmt -l internal/config cmd/synapse` still lists `config.go`, `config_test.go` and `main.go`,
and did before this phase: the same three files are already unformatted in HEAD (trailing
whitespace on blank lines in v1 code, e.g. `main.go:255`). Checked rather than assumed — no line
this phase added appears in `gofmt -d` for any of them, so nothing here is smuggled in behind a
reformat that was deliberately not done.

The new tests, through mcp-go's own clients:

```text
$ go test ./internal/mcp/... -v -count=1
=== RUN   TestPingToolIsRegisteredAndAnswersPing
--- PASS: TestPingToolIsRegisteredAndAnswersPing (0.00s)
=== RUN   TestServeStdioListsPingTool
--- PASS: TestServeStdioListsPingTool (0.00s)
=== RUN   TestServeStdioStopsWhenContextIsCancelled
--- PASS: TestServeStdioStopsWhenContextIsCancelled (0.10s)
=== RUN   TestServeRejectsUnknownTransport
--- PASS: TestServeRejectsUnknownTransport (0.00s)
=== RUN   TestServeTCPBindsLoopbackOnlyAndAnswers
2026/09/22 09:44:48 INFO MCP server listening transport=tcp addr=127.0.0.1:35073 path=/mcp
--- PASS: TestServeTCPBindsLoopbackOnlyAndAnswers (0.03s)
PASS
ok  	synapse/internal/mcp	0.152s

$ go test ./internal/config/... -v -count=1
--- PASS: TestConfigValidation/MCP_port_above_the_TCP_range_rejected (0.00s)
--- PASS: TestConfigValidation/Negative_MCP_port_rejected (0.00s)
--- PASS: TestConfigValidation/Zero_MCP_port_is_permissive (0.00s)
--- PASS: TestConfigValidation/MCP_enabled_on_the_default_TCP_port_validates (0.00s)
--- PASS: TestDefaultConfigMCPKnobs (0.00s)
PASS
ok  	synapse/internal/config	0.008s
```

Regression, run with this phase's tree (the Go directive is 1.25.5 and the toolchain is 1.26.2):

```text
$ go test ./... -count=1
?   	synapse/cmd/benchmark	[no test files]
?   	synapse/cmd/counttokens	[no test files]
?   	synapse/cmd/mergesessions	[no test files]
?   	synapse/cmd/plane	[no test files]
ok  	synapse/cmd/synapse	0.025s
ok  	synapse/internal/api	0.766s
ok  	synapse/internal/budget	0.279s
ok  	synapse/internal/classifier	0.010s
ok  	synapse/internal/compiler	1.014s
ok  	synapse/internal/config	0.015s
ok  	synapse/internal/conflict	0.016s
ok  	synapse/internal/dedup	0.007s
ok  	synapse/internal/embedder	2.873s
ok  	synapse/internal/integration	2.684s
?   	synapse/internal/ledger	[no test files]
ok  	synapse/internal/mcp	0.160s
ok  	synapse/internal/plane	0.465s
ok  	synapse/internal/proxy	0.466s
ok  	synapse/internal/retrieval	0.018s
ok  	synapse/internal/scorer	0.008s
?   	synapse/internal/session	[no test files]
ok  	synapse/internal/store	6.384s
ok  	synapse/internal/supersession	0.020s
ok  	synapse/internal/sync	6.441s
ok  	synapse/internal/tenant	0.824s
ok  	synapse/internal/trace	0.305s
```






### The brief's probe, and the handshake behind it

The probe the brief specifies, verbatim except for `timeout` (the process keeps serving the proxy
after its stdin ends, so it has to be killed) and `--port 8098` (this machine already runs a
proxy on 8080; without the override the second instance would exit on that bind failure before
answering, which is finding 2):

```text
$ printf '%s\n' '{"jsonrpc":"2.0","method":"tools/list","id":1}' | timeout 5 ./bin/synapse --mcp --config synapse.yaml --port 8098 2>/dev/null
{"jsonrpc":"2.0","id":1,"result":{"tools":[{"annotations":{"readOnlyHint":false,"destructiveHint":true,"idempotentHint":false,"openWorldHint":true},"description":"Liveness probe: answers {\"pong\":true} when the Synapse MCP server is reachable.","inputSchema":{"properties":{},"required":[],"type":"object"},"name":"synapse_ping"}]}}
--- exit code: 124
```

`synapse_ping` is listed. This answers without a preceding `initialize` because mcp-go v1.1.0 gates
exactly one method on session initialization (`setLevel`, `server/server.go:1414`) — worth knowing
before concluding from a hand-typed probe that the handshake is optional in general. The full
handshake, including a tool call, on the same transport:

```text
$ printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
    '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"synapse_ping","arguments":{}}}' \
  | timeout 5 ./bin/synapse --mcp --config synapse.yaml --port 8098 2>/dev/null
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"synapse","version":"0.1.0"}}}
{"jsonrpc":"2.0","id":2,"result":{"tools":[{"annotations":{...},"description":"Liveness probe: answers {\"pong\":true} when the Synapse MCP server is reachable.","inputSchema":{"properties":{},"required":[],"type":"object"},"name":"synapse_ping"}]}}
{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"{\"pong\":true}"}],"structuredContent":{"pong":true}}}
```

The TCP transport, from `--mcp --mcp-port 8765` to a SIGTERM:

```text
$ ss -ltnp | grep 8765
LISTEN 0 4096 127.0.0.1:8765 0.0.0.0:* users:(("synapse",pid=627867,fd=8))

$ curl -sS -D - -o body -X POST http://127.0.0.1:8765/mcp \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl-probe","version":"0"}}}'
HTTP/1.1 200 OK
Content-Type: application/json
Mcp-Session-Id: mcp-session-e720fbc4-9e78-4bc1-a4cc-7f17261f6f89
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"synapse","version":"0.1.0"}}}

$ curl -sS -o /dev/null -w 'http %{http_code}\n' -X POST .../mcp -H "Mcp-Session-Id: $SID" \
    -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
http 202

$ curl -sS -X POST .../mcp -H "Mcp-Session-Id: $SID" \
    -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"synapse_ping","arguments":{}}}'
{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"{\"pong\":true}"}],"structuredContent":{"pong":true}}}

$ kill -TERM $PID; wait $PID
exit status: 0
9:45AM INFO synapse: MCP server started transport=tcp port=8765
9:45AM INFO synapse: MCP server listening transport=tcp addr=127.0.0.1:8765 path=/mcp
9:45AM INFO synapse: Shutting down server...
9:45AM INFO synapse: MCP server stopped
9:45AM INFO synapse: Server stopped

$ ss -ltn | grep 8765 || echo 'port 8765 released'
port 8765 released
```

The listener is `127.0.0.1:8765`: no `0.0.0.0` line appears for it in `ss`, and
`TestServeTCPBindsLoopbackOnlyAndAnswers` additionally proves the port is **refused** on this
host's routable address, which is the assertion a `ss` reading alone cannot make.

The startup path itself, with logs where they belong — stderr, so stdout stays a clean JSON-RPC
stream:

```text
$ timeout 3 ./bin/synapse --mcp --config synapse.yaml --port 8096 >/tmp/mcp_stdio.out 2>/tmp/mcp_stdio.err
exit status: 124
--- stdout ---
(empty)
--- stderr ---
9:45AM INFO synapse: Store initialized db_path=/home/ranscky/.local/share/synapse/synapse.db
9:45AM INFO synapse: ONNX embedder initialized with real inference model=models/all-MiniLM-L6-v2/model.onnx vocab=models/all-MiniLM-L6-v2/vocab.txt
9:45AM INFO synapse: Trace inspector available at http://127.0.0.1:8096/ui
9:45AM INFO synapse: Synapse security: proxy bound to 127.0.0.1:8096, upstream 127.0.0.1:11434, trace persistence false, header redaction active, injection sanitization active
9:45AM INFO synapse: MCP server started transport=stdio port=0
9:45AM INFO synapse: Starting Synapse proxy address=127.0.0.1:8096 upstream=http://127.0.0.1:11434
9:45AM INFO synapse: MCP server listening transport=stdio
9:45AM INFO synapse: Shutting down server...
9:45AM INFO synapse: MCP server stopped
9:45AM INFO synapse: Server stopped
```

And the config-file half of the switch, with no flags at all (the yaml tags `mcp-enabled` and
`mcp-port` are therefore proven to be read, not merely documented):

```text
$ timeout 3 ./bin/synapse --config /tmp/mcp_yaml_test.yaml --port 8095   # mcp-enabled: true, mcp-port: 8766
9:46AM INFO synapse: MCP server started transport=tcp port=8766
9:46AM INFO synapse: MCP server listening transport=tcp addr=127.0.0.1:8766 path=/mcp
9:46AM INFO synapse: MCP server stopped

$ timeout 3 ./bin/synapse --config /tmp/mcp_yaml_stdio.yaml --port 8094 # mcp-enabled: true, mcp-port: 0
9:46AM INFO synapse: MCP server started transport=stdio port=0
9:46AM INFO synapse: MCP server listening transport=stdio
9:46AM INFO synapse: MCP server stopped
```

### Findings this phase surfaced

1. **One compiled-in v1 dependency moved, and it could not be pinned back.** `go.mod` now needs
   `github.com/dlclark/regexp2 v1.11.0` where it had v1.10.0: `santhosh-tekuri/jsonschema/v6`
   (an mcp-go dependency) requires v1.11.0 while `pkoukk/tiktoken-go v0.1.8` requires v1.10.0, so
   minimal version selection picks the higher one and no `replace` could honestly lower it.
   regexp2 is the regex engine behind tiktoken's BPE splitter, so this is the one place where
   "nothing about v1 changed" needed evidence rather than assertion: `internal/budget` and
   `internal/compiler` (whose assertions are token counts) pass unchanged, as does the whole
   suite above. The same `go mod tidy` also re-pruned two indirect entries (`kr/text`,
   `rogpeppe/go-internal`) that are now reachable only through mcp-go's own graph; nothing in the
   tree imports either.
2. **`--mcp` does not replace the proxy, so a second instance dies on the bind clash.** The brief
   is explicit that the MCP server starts after every other initialisation, and that is what the
   code does — but `main.go`'s existing `ListenAndServe` failure path is `os.Exit(1)`, so an
   editor-spawned `synapse --mcp` (exactly what `.cursor/mcp.json` asks for) exits immediately if
   a proxy already owns `127.0.0.1:8080`, taking the MCP server with it. The manual verification
   above works around it with `--port 8098`/`--port 8097`. Deciding between "skip the HTTP
   listener in stdio MCP mode" and "document a distinct listen-addr" is a real design choice and
   belongs to the phase that makes MCP useful, not this one.
3. **MCP boot still requires the rest of a valid config**, `upstream-url` included, because
   `Validate` runs before anything MCP-related. `.cursor/mcp.json` is therefore a template that
   only works where `./synapse.yaml` (or the OS-standard config) already exists. Same phase as
   finding 2.
4. **The process outlives its MCP client.** An editor that closes the pipe gets `Listen` to
   return, the goroutine closes `mcpDone`, and `main` goes back to waiting for a signal — the
   proxy is still serving, which is the point, but it does mean the editor has to SIGTERM the
   child to reap it. Shutdown itself is clean and ordered: transports stop on context cancel, the
   exit path waits for them (two seconds), and the TCP port is released before the process is.
5. **The `.clinerules` MCP rules are still unexercised, on purpose.** No tool here returns a
   memory, so there is no score breakdown to carry and nothing to sanitize: `synapse_ping`
   answers with a constant. The next phase's tools owe both (S/R/I/T + `trace_id` on everything
   surfaced; the REST write path's sanitization pipeline for anything accepted). One shape
   consequence is already visible: a real recall tool needs an embedding, so `NewServer` will grow
   an embedder argument — the store seam is in place, the embedder is not.
6. **`internal/config` was touched, which the v1 freeze nominally forbids.** Two additive fields,
   defaults unchanged (`false`/`0`), one `Validate` rule, and four test cases pinning that a config
   which never mentions MCP validates exactly as before. The brief asked for the fields by name;
   the alternative — reading `mcp-enabled` outside the config package — would have meant a second
   YAML parser for one boolean.
7. **Go 1.25.5 is now a hard floor for anyone building this tree**, not just for CI: a developer on
   1.22-1.24 needs `GOTOOLCHAIN=auto` (the default) and network access, or the build stops with
   "requires go >= 1.25.5". CI and the Docker image were moved to 1.25 rather than left to
   auto-switch, which is the difference between a deterministic build and a silent download.
8. **The loopback bind is not authentication, and should not be read as any.** Any local process
   can call whatever this server exposes; only the port range (1-65535) and the host are
   constrained. That is acceptable for a liveness probe and is why the first memory-returning tool
   should arrive with the MCP surface's own tenant check, not just a bind address.

### Next phase

The largest item is the one this phase sets up: the first real MCP tools (recall and list over the
local store), which owe the score breakdown, the trace id, and the shared sanitization pipeline,
and which will widen `NewServer` with an embedder. Findings 2, 3 and 4 are the MCP-shaped half of
the queue; finding 8 is a gate on the tool that returns memory content rather than a follow-up.
Nothing carried over from Phases 17-21 has moved: the external anchor for each tenant's chain head
(Phase 17 finding 2, Phase 18 finding 6, Phase 20 finding 8) is still the one whose absence makes
"the chain was deleted" and "nothing was ever appended" the same answer; `tenant.RunMigrations`'
advisory lock (Phase 20 finding 1) is still the smallest; metering is still the phase that would
make the compliance report's summary honest; the compliance-tier provisioning field still belongs
with whichever phase touches `POST /v2/tenants` next; and recording `GET /v2/compliance/chain-integrity`'s
*successful* reads (Phase 21 finding 1) is still small enough to travel with any of them. And
finding 1 above is new and worth carrying: from this phase on, `go.mod` is only as green as a
toolchain at or above 1.25.5.


## Phase 23 — synapse_compile MCP tool (complete)

Phase 22 proved a request could reach a handler; this phase gives the handler something to do. The
MCP surface now advertises exactly one tool, `synapse_compile`, and it does not implement compiling:
it hands the conversation it was given to the same function `POST /v1/compile` calls, so an editor's
compile and an HTTP compile are one compilation with one store, one embedder, one control-plane
candidate source, and one trace.

Commit `feat: Phase 23 - synapse_compile MCP tool`

New files:

- `internal/mcp/compile.go` — 273 lines: the `Compiler` seam, the tool definition and its schema,
  the handler, the error envelope, and the response types. Separate from `server.go` for a reason
  the file rules make non-negotiable: `server.go` was 224 lines and the tool plus its types is
  another ~180, so adding it there would have crossed the 300-line ceiling this project holds
  itself to. `registerTools` stays in `server.go` as the table of what the server offers, and now
  delegates to `s.registerCompileTool()`.
- `internal/mcp/compile_test.go` — 447 lines: seven tests, the DoD one driving a real store and a
  real `*api.APIServer` through mcp-go's in-process client.
- `/home/ranscky/synapse-mcp-test/` — the Cline fixture, outside this tree: `synapse.yaml`
  (absolute paths, scratch database, proxy on 8081), `.cursor/mcp.json` (absolute binary path,
  `--mcp`, `--config`), and a `README.md` recording the manual probe. All three are mode 0600.

Changed:

- `internal/mcp/server.go` — 224 → 207 lines. `synapse_ping`, `pingResult`, `handlePing` and
  `pingToolName` are deleted, `NewServer` takes a third argument (`Compiler`), and `Server` carries
  the seam. The package doc comment was rewritten: it described the package as "boot path only, no
  tool reads memory yet", which stopped being true in this phase.
- `internal/mcp/server_test.go` — 262 → 225 lines. The ping test is replaced by
  `TestCompileToolIsRegisteredAndDescribesTheSieve` (in `compile_test.go`); the stdio test is
  renamed `TestServeStdioListsCompileTool`; the TCP test asserts `synapse_compile` is advertised.
  `newTestServer` now injects a stub pipeline, so a handshake test never needs an embedder.
- `internal/api/api.go` — 736 → 763 lines: `CompileContext` (the exported seam) and
  `ValidateSessionID`/`ValidateMessageContent` renamed out of their unexported spellings.
- `internal/api/api_test.go` — 6 call sites updated for that rename, nothing else. Listed here rather
  than folded into "mechanical" because it is a v1 test file and a reader deserves to see it named.
- `cmd/synapse/main.go` — the MCP boot block passes `apiServer` as the pipeline (+7/-1 lines).
- `bin/synapse` was rebuilt for the verification below and is **not** staged: it was already dirty
  in the working tree before this phase (`M bin/synapse`, last committed in `371186d`), and Phase 22
  made the same call. 24.8 MB → 24.9 MB, well inside CI's 50 MB gate.


### The shared pipeline, and the one exported method internal/api gained

The brief's two rules point in opposite directions — "do not touch any v1 internal package except
`internal/mcp`" and "call the same function that POST /v1/compile calls in `internal/api`" — because
the function in question, `(*APIServer).runCompilePipeline`, is unexported and reachable only from
inside `internal/api`. Three shapes were on the table and the decision was made explicitly rather
than by drift: (a) export a wrapper around the existing private method, (b) move the pipeline body
into `internal/compiler` and have both front ends call it, (c) have the MCP tool HTTP-POST to the
local REST endpoint. (b) was rejected as the larger change to two frozen packages for the same
result, and (c) as a self-call that adds a hop and a dependency on this process's own listener.
(a) is what shipped:

```go
func (a *APIServer) CompileContext(ctx context.Context, sessionID string, messages []Message, tokenBudgetOverride int) (*compiler.CompileResult, error) {
	return a.runCompilePipeline(ctx, sessionID, messages, tokenBudgetOverride, true)
}
```

Two lines of behavior, no logic moved, no call site in `handleCompile` or `handlePlaygroundCompile`
changed, and `persist=true` is deliberately hard-coded into it: "the same thing /v1/compile does" is
the contract, and the playground's `persist=false` variant is not what an editor's compile should
be. The validators were renamed out of `validateSessionID`/`validateMessageContent` (6 call sites in
`api.go`, 6 references in `api_test.go`) rather than duplicated in `internal/mcp`, because two copies
of an input rule are two rules the day one of them is edited.

`internal/mcp` therefore imports `internal/api` and declares the dependency the other way round, as
an interface it owns:

```go
type Compiler interface {
	CompileContext(ctx context.Context, sessionID string, messages []api.Message, tokenBudgetOverride int) (*compiler.CompileResult, error)
}
```

`*api.APIServer` satisfies it structurally, so nothing in `internal/api` knows MCP exists — and a
test can inject a stub, which is how the five non-DoD tests stay fast.


### `invalid_params` is a tool error here, and that is a protocol fact, not a shortcut

The brief asks for "MCP error with type `invalid_params`". From inside a handler that cannot be a
JSON-RPC `-32602`: mcp-go v1.1.0 maps *any* error a handler returns to `mcp.INTERNAL_ERROR`
(`-32603`) at `server/server.go:2130-2136`, and the code constants are not a handler's to choose. The
protocol's own route for a tool-reported failure is a `CallToolResult` with `IsError` set, so that is
what this tool returns, carrying a machine-readable type:

```json
{"error":{"type":"invalid_params","message":"session_id is required"}}
```

`compile_failed` is the second type, for the cases that are not the caller's fault: a pipeline that
ran and failed, and a server built with no pipeline at all. The pipeline's own error goes to the log
and **not** to the caller — an error from that layer can name a database path or an upstream host,
and this is the one place where "same treatment as REST" is also the safer treatment: the REST
handlers log the cause and answer "Internal server error", and so does this.

### What the response carries beyond the four fields the brief names

The brief specifies `compiled_messages`, `tokens_used`, `reduction_pct`, `detected_intent`. The
response also carries `trace_id` and a `memories` array of the four factor scores per memory,
because `.clinerules` makes those mandatory for any MCP tool that surfaces a memory: "MCP responses
MUST include score breakdown (S/R/I/T) and trace_id so users can see WHY a memory was surfaced
(differentiation from OpenMemory MCP)". A compile surfaces memories, so the requirement applies, and
the manual run below shows what that buys: the compiled context, and next to it the scoring that put
one memory in and left two out.

What it deliberately does **not** carry is `content_preview`. The memories that were compiled are
already in `compiled_messages`; the ones that were not are not this caller's business, so an
excluded memory is reported by score only. The tool also has no header input at all, and
`session_id` is required, which is why `extractSessionID`'s Authorization-derived session id is
unreachable from this path: there is no header to hash.

### synapse_ping is gone

The brief says "replace synapse_ping with synapse_compile", and that is what happened: the tool, its
result type, its handler, and its constant are deleted, and the four test references now name the
compile tool. The consequence is worth stating plainly: `tools/list` advertises one tool, and there
is no longer a zero-dependency liveness probe inside this package. What still covers that role is
`initialize` plus `tools/list` — neither of which touches a store, an embedder, or an ONNX session,
since `NewServer` accepts a nil store and a stub pipeline — and `TestServeStdioListsCompileTool`
drives exactly that through buffers.


### Verification — real output

Formatting, vet, build:

```text
$ gofmt -l internal/mcp
(no output)

$ go vet ./...
(no output)

$ go build ./... && go build -o bin/synapse ./cmd/synapse
(no output)
```

`gofmt -l internal/api` still lists `api.go`, `api_test.go`, `header_sanitize.go`,
`header_sanitize_test.go`, `integration_test.go`, `ratelimit.go`, and `gofmt -l cmd/synapse` still
lists `main.go` — all of them were already unformatted at HEAD (the same trailing-whitespace v1
habit Phase 22 documented for `internal/config` and `main.go`). Checked rather than assumed: no line
this phase added appears in `gofmt -d` for any of them, so nothing here is smuggled in behind a
reformat that was deliberately not done.

The DoD command, verbatim:

```text
$ go test ./internal/mcp/... -run TestCompile -v -count=1
=== RUN   TestCompileToolIsRegisteredAndDescribesTheSieve
--- PASS: TestCompileToolIsRegisteredAndDescribesTheSieve (0.00s)
=== RUN   TestCompileToolCompilesSessionThroughSharedPipeline
2026/09/22 10:07:35 INFO Store initialized db_path=/tmp/TestCompileToolCompilesSessionThroughSharedPipeline3685994256/001/mcp-compile.db
    compile_test.go:191: synapse_compile response: {"compiled_messages":[{"content":"[Memory: context] the order handler panics when the payload is empty\n\nthere is a stack trace in the order handler, it crashes on an empty payload","role":"user"}],"tokens_used":10,"reduction_pct":60,"detected_intent":"debug","trace_id":"req-1790071656098986218","memories":[{"id":"mem-0","memory_type":"context","score_semantic":1,"score_recency":0.9999999464442252,"score_importance":0.5,"score_task_alignment":0.48333333333333334,"score_total":0.7466666613110893,"included":true},{"id":"mem-1","memory_type":"context","score_semantic":0,"score_recency":0.97153188912246,"score_importance":0.5,"score_task_alignment":0.48333333333333334,"score_total":0.3438198555789127,"included":false},{"id":"mem-2","memory_type":"context","score_semantic":0,"score_recency":0.9438742621317734,"score_importance":0.5,"score_task_alignment":0.48333333333333334,"score_total":0.341054092879844,"included":false}]}
--- PASS: TestCompileToolCompilesSessionThroughSharedPipeline (1.05s)
=== RUN   TestCompilePassesArgumentsThroughUnchanged
--- PASS: TestCompilePassesArgumentsThroughUnchanged (0.00s)
=== RUN   TestCompileRejectsInvalidParams
=== RUN   TestCompileRejectsInvalidParams/missing_session_id
=== RUN   TestCompileRejectsInvalidParams/empty_session_id
=== RUN   TestCompileRejectsInvalidParams/illegal_session_id
=== RUN   TestCompileRejectsInvalidParams/missing_messages
=== RUN   TestCompileRejectsInvalidParams/empty_messages
=== RUN   TestCompileRejectsInvalidParams/message_is_not_an_object
=== RUN   TestCompileRejectsInvalidParams/null_byte_in_content
=== RUN   TestCompileRejectsInvalidParams/negative_token_budget
--- PASS: TestCompileRejectsInvalidParams (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/missing_session_id (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/empty_session_id (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/illegal_session_id (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/missing_messages (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/empty_messages (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/message_is_not_an_object (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/null_byte_in_content (0.00s)
    --- PASS: TestCompileRejectsInvalidParams/negative_token_budget (0.00s)
=== RUN   TestCompileReportsPipelineFailureAsToolError
2026/09/22 10:07:36 ERROR MCP compile failed error="failed to search memories: /var/lib/synapse/secret.db is locked"
--- PASS: TestCompileReportsPipelineFailureAsToolError (0.00s)
=== RUN   TestCompileWithoutPipelineFailsSafelyNotPanics
--- PASS: TestCompileWithoutPipelineFailsSafelyNotPanics (0.00s)
PASS
ok  	synapse/internal/mcp	1.078s
```


That response is the whole phase in one line, and it is worth reading closely. `tokens_used` 10
against a budget of 12 with `reduction_pct` 60 means the budget really did trim the pool rather than
being reported after the fact; `detected_intent` "debug" is the classifier's verdict on a message
about a stack trace; `mem-0` scores semantic 1.0 because the test stores it with the query's own
embedding, and its `score_total` 0.7467 is exactly `0.4·1 + 0.3·0.5 + 0.2·0.4833 + 0.1·1.0` under the
configured weights; and the two excluded memories are reported by score with no content. The
`ERROR MCP compile failed` line is the other half of that: the stub's error text, which contains a
database path, appears in the log and in the log only — the assertion that follows it is
`require.NotContains(t, text, "secret.db")`.

The whole package and the frozen package it now depends on:

```text
$ go test ./internal/mcp/... ./internal/api/... -count=1
ok  	synapse/internal/mcp	1.200s
ok  	synapse/internal/api	0.880s
```

`internal/api`'s suite passing unchanged is the evidence for the seam being additive: the rename and
`CompileContext` altered no behavior, and the v1 tests that cover both are the ones that say so.

The real binary, over stdio, against the fixture config an editor would use — `initialize`,
`tools/list`, then a five-message `tools/call`, with everything on stderr and only JSON-RPC on
stdout:

```text
$ cd /tmp/synapse-verify
$ timeout -s INT 20 /home/ranscky/Dev/synapse/bin/synapse --mcp --config ./synapse.yaml < requests.jsonl
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"synapse","version":"0.1.0"}}}
{"jsonrpc":"2.0","id":2,"result":{"tools":[{"annotations":{"readOnlyHint":false,"destructiveHint":true,"idempotentHint":false,"openWorldHint":true},"description":"Compile conversation history into token-budgeted, task-aware context using the 4-Factor Sieve (Semantic 0.4, Importance 0.3, Task Alignment 0.2, Recency 0.1 with 24h half-life decay).","inputSchema":{"properties":{"messages":{...},"session_id":{...},"token_budget":{...}},"required":["messages","session_id"],"type":"object"},"name":"synapse_compile"}]}}
{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"{\"compiled_messages\":[{\"content\":\"[Memory: error] Now there is an error: the synapse_compile tool returns invalid_params for a session that exists, here is the stack trace from the MCP server.\\n\\nNow there is an error: the synapse_compile tool returns invalid_params for a session that exists, here is the stack trace from the MCP server.\",\"role\":\"user\"}],\"tokens_used\":30,\"reduction_pct\":0,\"detected_intent\":\"debug\",\"trace_id\":\"req-1790071576544894642\",\"memories\":[{\"id\":\"req-1790071552527909122\",\"memory_type\":\"error\",\"score_semantic\":1,\"score_recency\":0.9998073425217096,\"score_importance\":0.9,\"score_task_alignment\":0.6666666666666667,\"score_total\":0.9033140675855043,\"included\":true}]}"}],"structuredContent":{...}}
```

That output is the second run against the same scratch session: the first run compiled with no
memories in the session yet (`tokens_used` 0, `memories` empty, `detected_intent` "debug"), and
because the tool persists exactly as `/v1/compile` does, its last user message became the memory the
second run surfaced — through the real ONNX embedder, hence semantic 1.0 and importance 0.9 for a
memory the store typed "error". `reduction_pct` is 0 there because the candidate pool is that one
memory and all of it fit, which is the same arithmetic `/v1/compile` does. Nothing but JSON-RPC
reached stdout; the two warnings the process emitted (`Failed to load UI file`, `Failed to load
session UI file`) are on stderr, which is where every logger in this process writes by design.

For the editor half of the brief, `/home/ranscky/synapse-mcp-test/` holds a `.cursor/mcp.json` and
the `synapse.yaml` it points at, and the probe above was run from that directory against that config,
so the file an editor will read is a file that was exercised. What this session cannot do is restart
Cline: reading its MCP panel and pasting the tool's output is the operator's step, and it is the one
item on the brief's manual-verification list that stays open here.


### Findings

1. **The MCP surface still has no authentication, and it now writes.** Phase 22's finding 8 said the
   loopback bind should not be read as authentication and that the first memory-returning tool should
   arrive with a tenant check. This phase added the first tool that both *returns* session memories
   and *writes* one (the last user message, exactly as `/v1/compile` does). The mitigating facts are
   real but limited: the transport is stdio or 127.0.0.1, the tool accepts no header and requires an
   explicit `session_id`, and this process is the same binary whose standalone REST API has no auth
   either — so the exposure added is a local one, not a new network one. It is still the largest open
   item on this surface, and it is a gate on the first tool that goes looking for memories the caller
   did not name.
2. **An MCP handler cannot emit JSON-RPC `-32602`.** mcp-go v1.1.0 hard-maps any handler error to
   `INTERNAL_ERROR`, so "invalid params" from a tool is necessarily an `IsError` result, optionally
   with a typed body as here. The alternative — declaring the schema and enabling
   `WithInputSchemaValidation` — produces SEP-1303 tool execution errors with the validator's own
   wording, which is a different message shape again. Recorded because a reviewer will ask why this
   tool does not use the numeric code, and because the next tool has to make the same choice.
3. **A compile with no memories is indistinguishable from a compile that failed to retrieve.** The
   first probe run answered `compiled_messages` containing only the caller's own last message,
   `tokens_used` 0, `memories` `[]` — correct, and exactly what `/v1/compile` returns for a fresh
   session. A model reading that may reasonably conclude nothing was found *and* that retrieval
   broke. A future phase could add something like `memories_considered` or a `state` field; it is not
   in this phase's response shape, so it was not invented here.
4. **The tool is not rate-limited.** The REST path has a per-IP `RateLimiter`; this path has none,
   and each call costs one embedding plus one store write. A local process looping on `tools/call` is
   not bounded by anything in this package.
5. **Testing the real pipeline needed a deliberately non-degenerate stub embedder.** `basisEmbedder`
   returns a unit vector rather than zeros, and the DoD test stores one memory with the query's own
   vector. Zero vectors would have been "fine" — `store.CosineSimilarity` returns 0 when either norm
   is 0 — which is precisely the problem: a broken scoring pass would have looked like a working one
   that happened to score everything 0. The semantic 1.0 in the test output is what rules that out.
6. **`Server.store` is still a seam no tool uses.** The compile tool reaches storage through the
   pipeline, not through the field, so `NewServer`'s first argument is unused by this phase's tool
   set. A recall tool would use it; if the surface never grows one, the field should be deleted
   rather than left as decoration.

### Next phase

The largest item is the one Phase 22 queued: the recall/list tools, which owe the score breakdown,
the trace id and the shared sanitization pipeline just as this one does — and which, unlike this one,
are gated by finding 1 above, because a tool that returns memories the caller did not name is a
tool that needs to know whose memories they are. Finding 3 and finding 4 are small and could travel
with it. Nothing carried over from Phases 17-22 has moved: the external anchor for each tenant's
chain head (Phase 17 finding 2, Phase 18 finding 6, Phase 20 finding 8) is still what makes "the
chain was deleted" and "nothing was ever appended" the same answer; `tenant.RunMigrations`' advisory
lock (Phase 20 finding 1) is still the smallest; metering is still the phase that would make the
compliance report's summary honest; the compliance-tier provisioning field still belongs with
whichever phase touches `POST /v2/tenants` next; and recording `GET /v2/compliance/chain-integrity`'s
successful reads (Phase 21 finding 1) is still small enough to ride along with any of them. Phase
22's Go 1.25.5 floor (its finding 7) now applies to any toolchain that builds this tree, MCP-aware or
not.


## Phase 24 — synapse_search_memories MCP tool (complete)

Phase 23 gave the MCP surface a compiler; this phase gives it the read half. `synapse_search_memories`
answers with the Global Brain's memories ranked by the same 4-Factor model the compiler scores with —
retrieval for the candidates, `internal/scorer` for the ranking, the classifier for the query's intent
— and every result carries `score_s`/`score_r`/`score_i`/`score_t`/`score_total` plus the provenance
that explains a demoted total. It never writes, and it does not compile.

Commit `feat: Phase 24 - synapse_search with 4-Factor score breakdown`

New files:

- `internal/mcp/search.go` — 274 lines: the `Embedder` seam, the tool definition and its schema, the
  handler, the argument validation, the visibility→scope mapping, and the trace-id generator. It is a
  separate file from `server.go` for the reason Phase 23 recorded: each tool's definition and handler
  live in their own file so `registerTools` stays a table of what the server offers.
- `internal/mcp/search_result.go` — 101 lines: the response shape and the mapping from
  `scorer.ScoredMemory` into it. Split out of `search.go` on the same seam Phase 23 split
  `compile.go` from `server.go` — by line count, not by taste: registration + handler + validation
  is already 274 lines, and the payload types are another ~100.
- `internal/mcp/search_test.go` — 243 lines: the DoD test plus `top_k`, registration/schema, and the
  helpers the other two test files share (`newSearchStore`, `decodeSearchResult`, `scoreKeys`).
- `internal/mcp/search_scope_test.go` — 234 lines: the `recordingStore` double, the
  visibility→reader-scope table, the pool-width rule, invalid-argument rejection, and the two
  failure paths.
- `internal/mcp/search_plane_test.go` — 93 lines: the control-plane-first policy and its local
  fallback.

Changed:

- `internal/mcp/server.go` — 207 → 246 lines. `Server` gains `embedder` and `plane`, `NewServer`
  takes a fourth argument (`Embedder`), `registerTools` gains one line, and `SetPlaneCandidates` is
  added with the same name and meaning it has on `*api.APIServer` and `*proxy.Proxy`. The package
  doc comment now describes both tools and names the three `.clinerules` the code makes true.
- `internal/mcp/compile.go` — +5 lines: the `errorTypeSearchFailed` constant, kept with the other two
  tool-error types rather than beside its only user, because a reader looking for "what can a tool
  report" should find all of them in one place.
- `internal/mcp/server_test.go`, `internal/mcp/compile_test.go` — the constructor's new argument at
  six call sites (a `basisEmbedder` where an embedder is not the subject). Mechanical, listed here
  because a test file's shape is part of the record.
- `cmd/synapse/main.go` — +7/-1: the MCP server is built with `embedderInstance` (the same model
  instance the proxy and the compile pipeline hold) and is handed the same `*sync.Syncer` the other
  two servers get when a control plane is configured.
- `bin/synapse` — **not** staged. It was already dirty before this phase (`M bin/synapse`, last
  written 10:05, before this session) and Phase 22 and 23 made the same call. Nothing here rebuilt
  it: `go build ./...` discards binaries when the pattern matches several packages.


### Same embedder, same weights, same intent — so a score means one thing

The DoD is the score breakdown, and a breakdown is only worth reading if it is the same arithmetic the
rest of the system uses. Three things were therefore borrowed rather than reimplemented:

1. **The embedder.** `cmd/synapse` passes the instance the proxy already holds, so a query is embedded
   by the model that indexed the memories it is compared against. Two models would make cosine
   similarity a number about nothing.
2. **The weights.** `scorer.GetWeights(cfg.Weight…)` with the same four config keys
   `runCompilePipeline` reads — not another hard-coded 0.4/0.3/0.2/0.1.
3. **The intent.** `classifier.Classify(query)`, the same call the pipeline makes on a conversation's
   last user turn, so Task Alignment is computed from this search's own text.

The chain is `retrieval.Candidates` → `scorer.Score` and stops there: no write (the compile tool's
step 3b), no dedup, no token budget. A search reports; the compiler decides what fits.

The candidate pool is `max(cfg.RetrievalCandidateK, top_k)`, not `top_k`. The store returns its
nearest `poolK` by similarity; the 4-Factor total is what picks the answer. Asking for exactly `top_k`
would let similarity pre-empt the scoring this tool exists to explain, which is the same mistake the
`GetRecent(20)` → `Search()` consolidation fixed in Phase 6.



### `visibility` is a reader scope, and `session_id` is optional on top of it

The brief's schema was `query`, `top_k`, `visibility` (default `"org"`). `visibility` is not a search
input anywhere in this codebase — it is a per-memory column (`internal/store/pgvisibility.go`) — and
the brief's schema has no session at all, while the standalone backend can only search
`WHERE session_id = ?`. The decision was put to the user rather than guessed, and the answer is what
shipped:

- **`visibility` selects the reader scope.** `org` names no agent and no team (the Postgres predicate
  reduces to `visibility = 'org'` — the Global Brain's shared record), `team` adds this node's
  configured team id, `private` adds this agent id. Agent and team always come from this node's
  config and never from the request, so no argument can widen a read; a blank team id or a blank
  session matches no row, which is the fail-closed direction.
- **`session_id` is an optional fourth argument that always narrows and never widens.** On Postgres it
  is inert outside the private branch (the predicate binds it, but an unnamed agent cannot match that
  branch), so `org` stays org-only across every session — which is what a shared plane means. On a
  standalone node the session is the *only* filter there is, so naming one is how a local node is
  searched at all.
- **The alternative was rejected on evidence, not on taste.** The first implementation built `org` as
  a scope with no identity at all, and the DoD test failed with `[]`: a session-less search against
  SQLite's `WHERE session_id = ?` matches nothing, so the tool would have returned an empty list for
  every local caller — including the Cline user this phase is verified from.

### Every result carries all five score fields, zero or not

`searchMemoryScore` has no `omitempty` on any field, and the DoD test asserts the keys rather than
decoding struct fields, because `score_s: 0` and no `score_s` key decode identically into a struct and
only the former is a breakdown. The test also asserts the two non-matching memories score exactly
`0.0` on the semantic factor, so a regression that dropped zero-valued fields fails on the semantics
rather than on a technicality.

Two normalizations are borrowed from `trace.TraceMemory` so one memory reads the same way here and in
the trace of the compile that surfaced it: a blank `conflict_status` is `"none"`, and a blank
`visibility` is the column's own default, `"org"`. `cross_agent` uses the trace's rule verbatim
(`AgentID != "" && AgentID != localAgentID`): a blank agent is unattributed, never someone else's.
`created_at` is emitted in UTC RFC3339Nano so Postgres and SQLite render the same instant identically.

The response is `{"trace_id": ..., "memories": [...]}`. `trace_id` is per `.clinerules`' "score
breakdown (S/R/I/T) and trace_id" requirement, and it is a random 8-byte id that appears in this
process's log line for the call — a search records no trace manifest, so the id's job is to tie one
answer to the one log entry naming its parameters and counts. The log carries counts and identifiers
only: a query is memory content the moment it is embedded, and this process does not log memory
content.

### Errors: `invalid_params` for the caller, `search_failed` for the deployment

Same shape as Phase 23's two types, for the same protocol reason (a handler cannot choose `-32602`;
mcp-go maps every handler error to `-32603`, so a tool reports failure with `IsError` and a typed
body). `invalid_params` covers the seven inputs the tests reject — missing/blank/null-byte query,
negative `top_k`, `top_k` above the same 500 ceiling plane's search endpoint enforces, unknown
`visibility`, illegal `session_id` — and the query is validated with `api.ValidateMessageContent`, the
same rule the REST front end applies, which is the `.clinerules` sanitization-pipeline requirement
expressed as a shared validator rather than a second copy. `search_failed` covers the rest: no store or
no embedder configured (typed error, never a panic — the Phase 23 lesson), and a retrieval that
failed, whose cause goes to the log because a store error can name a database path.

### The plane seam was added, and it is not scope creep

The brief's handler says "call `retrieval.Candidates()`", and the first draft passed `nil` for the
plane source. Reading `cmd/synapse` shows why that would have been wrong: this binary always opens the
SQLite store (`store.NewStore(cfg.DBPath)`), so a node with `control-plane-url` set is an *edge* node
whose org-wide memories live on the plane and reach the compile path only through
`syncer.PullCandidates`. A search reading only the local store would have surfaced a different half of
the brain from the compile it is explaining, while its own description promised "the Global Brain".
Ten lines close it: a `plane` field, a `SetPlaneCandidates` with the same spelling as its two
siblings on `*api.APIServer` and `*proxy.Proxy`, `s.plane` in the `Candidates` call, and the wiring in
`main` beside theirs. `search_plane_test.go` pins both halves — an answered plane *is* the candidate
set (the local store is asserted not to have been searched), and a failing one falls back to it with no
error to the caller, which is the policy `retrieval.Candidates` already documents.

### Annotation hints are part of the tool's contract now

`tools/list` has always advertised both tools with mcp-go's default annotations — `readOnlyHint:false`,
`destructiveHint:true` — which was invisible while the only tool wrote a memory and is wrong for one
that does not: a client that gates on hints would be told the read path is a write path. The search
tool therefore declares `readOnlyHint: true`, `destructiveHint: false`, `openWorldHint: false`, and
the registration test asserts all three, so the claim is checked rather than commented. `idempotentHint`
is deliberately left at its default: the ranking depends on the clock (recency decay) and on the store
it reads, so "calling it twice gives the same answer" is not a promise this tool can make, even though
it has no side effects.

### Verification (real output, this phase)

```text
$ gofmt -l internal/mcp/*.go                     # empty
$ go vet ./internal/mcp ./cmd/synapse
VET_OK
$ go build ./...
BUILD_OK

$ go test ./internal/mcp/... -run TestSearch -v
=== RUN   TestSearchMemoriesRanksNearestMemoryFirstWithItsScoreBreakdown
--- PASS: TestSearchMemoriesRanksNearestMemoryFirstWithItsScoreBreakdown (1.13s)
=== RUN   TestSearchMemoriesHonorsTopK
--- PASS: TestSearchMemoriesHonorsTopK (0.54s)
=== RUN   TestSearchMemoriesReadsAtTheVisibilityScopeItWasGiven
    --- PASS: TestSearchMemoriesReadsAtTheVisibilityScopeItWasGiven/org_with_no_session_searches_the_cross-session_bucket
    --- PASS: TestSearchMemoriesReadsAtTheVisibilityScopeItWasGiven/team_names_this_node's_team_and_still_no_agent
    --- PASS: TestSearchMemoriesReadsAtTheVisibilityScopeItWasGiven/private_without_a_session_reaches_no_private_memory,_which_is_fail-closed
--- PASS: TestSearchMemoriesReadsAtTheVisibilityScopeItWasGiven (0.00s)
--- PASS: TestSearchMemoriesWidensThePoolForALargeTopK (0.00s)
--- PASS: TestSearchMemoriesConsultsThePlaneBeforeTheLocalStore (0.00s)
--- PASS: TestSearchMemoriesFallsBackToTheLocalStoreWhenThePlaneFails (0.00s)
--- PASS: TestSearchMemoriesRejectsInvalidParams (0.00s)      # 7 subtests
--- PASS: TestSearchMemoriesReportsAStoreFailureAsToolError (0.00s)
--- PASS: TestSearchMemoriesWithoutItsDependenciesFailsSafelyNotPanics (0.00s)   # 3 subtests
--- PASS: TestSearchMemoriesToolIsRegisteredAndAdvertisesItsSchema (0.00s)
PASS
ok  	synapse/internal/mcp	1.712s

$ go test ./internal/... ./cmd/...
ok  	synapse/internal/api	(cached)
ok  	synapse/internal/budget	(cached)
ok  	synapse/internal/classifier	(cached)
ok  	synapse/internal/compiler	(cached)
ok  	synapse/internal/config	(cached)
ok  	synapse/internal/conflict	(cached)
ok  	synapse/internal/dedup	(cached)
ok  	synapse/internal/embedder	(cached)
ok  	synapse/internal/integration	(cached)
ok  	synapse/internal/mcp	(cached)
ok  	synapse/internal/plane	(cached)
ok  	synapse/internal/proxy	(cached)
ok  	synapse/internal/retrieval	(cached)
ok  	synapse/internal/scorer	(cached)
ok  	synapse/internal/store	(cached)
ok  	synapse/internal/supersession	(cached)
ok  	synapse/internal/sync	(cached)
ok  	synapse/internal/tenant	(cached)
ok  	synapse/internal/trace	(cached)
ok  	synapse/cmd/synapse	(cached)
```

The unit tests are not the only evidence this phase has, because the interesting question — does a real
model's embedding rank a real memory, with a breakdown a caller can read — needed the real embedder. A
throwaway config (`/tmp/phase24-live.yaml`, mode 0600, absolute model path, `db-path` in `/tmp`) and
mcp-go's own stdio transport, exactly the shape Cline spawns:

```text
$ printf '%s\n' \
    '{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"synapse_compile","arguments":{"session_id":"sess-phase24-live","messages":[{"role":"user","content":"the retry budget for the order handler is three attempts"}]}}}' \
  | timeout 60 /tmp/synapse-phase24 --mcp --config /tmp/phase24-live.yaml
{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"compiled_messages\":[...\"[Memory: context] the retry budget for the order handler is three attempts\"...],\"tokens_used\":10,...,\"memories\":[{\"id\":\"req-...\",\"score_semantic\":0.9999999999999998,...

$ printf '%s\n' '{"jsonrpc":"2.0","method":"tools/call","id":9,"params":{"name":"synapse_search_memories","arguments":{"query":"how many retry attempts does the order handler make","session_id":"sess-phase24-live","top_k":1}}}' \
  | timeout 90 /tmp/synapse-phase24 --mcp --config /tmp/phase24-live.yaml
{"trace_id":"search-c5b5dc3bf2c75d96","memories":[{"id":"req-1790073710807144074","content":"the retry budget for the order handler is three attempts","memory_type":"context","agent_id":"","cross_agent":false,"conflict_status":"none","created_at":"2026-09-22T10:41:50.807149756Z","session_id":"sess-phase24-live","visibility":"org","score_s":0.8322861649922297,"score_r":0.9983529959094402,"score_i":0.5,"score_t":0.5333333333333333,"score_total":0.6894164322545026}]}

$ ... tools/list ...
annotations: {"readOnlyHint": true, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}
required: ['query']
properties: ['query', 'session_id', 'top_k', 'visibility']
```

`score_s` 0.832 is a real MiniLM cosine similarity between a paraphrase and the memory, not a 1.0
artefact; `score_r` 0.998 is a memory four seconds old; `score_i` 0.5 and `score_t` 0.533 come from the
type lookup and the classifier's `code` intent, and they are the same numbers the compile that surfaced
that memory reported.



### Findings (recorded, not fixed)

1. **A standalone node cannot answer a session-less search.** The SQLite backend's `Search` is
   `WHERE session_id = ?`, so `{"query": "..."}` alone returns `{"memories": []}` on a node with no
   control plane — correct for the backend, and exactly the kind of empty answer a caller reads as
   "broken". The `session_id` description tells the caller what to do, and a future phase that wants a
   true cross-session read on a standalone node has to change `internal/store`'s Search, which is a v1
   package this phase was told not to touch.
2. **The response cannot say it was truncated.** With the default `top_k` of 10 and a pool of
   `retrieval-candidate-k` (50), 40 scored candidates can be discarded with nothing in the answer to
   say so — the same shape Phase 23's finding 3 recorded for compiles. A `candidates_considered` field
   would fix it; it is not in this phase's response shape, so it was not invented here.
3. **Superseded memories are not filtered.** The Postgres backend excludes them in SQL; the local one
   does not, and this tool does not either. Retrieval hands back what the store answers, and the
   scorer's conflict penalty is the only signal. Filtering here would make MCP's answer differ from the
   REST answer to the same query, which is the drift this package exists to avoid.
4. **`cross_agent` is always false on a standalone node**, because SQLite stores no agent id and a
   blank one is unattributed rather than someone else's (the rule `trace.TraceMemory` already
   documents). It is a field that only says something on a plane-backed node.
5. **No rate limit.** Each call costs one embedding; the REST path has a per-IP limiter, this one has
   nothing, and a local process looping on `tools/call` is unbounded — Phase 23's finding 4, unchanged.
6. **mcp-go's stdio server answers concurrent requests concurrently.** The manual probe's first run
   sent `synapse_compile` and `synapse_search_memories` in one piped session and the search answer
   (`id: 3`) came back before the compile's (`id: 2`), so the search saw an empty store. Cline (and any
   real client) awaits each response, so this is a fixture hazard rather than a product bug — but the
   probe had to be split into two processes to test a write-then-read sequence, and that is worth
   knowing before someone writes a shell-based end-to-end script.
7. **`synapse_compile` still advertises `destructiveHint: true`.** That is correct (it writes the last
   user message back) and is mcp-go's default, so nothing changed there; now that the surface has one
   read tool and one write tool, the annotation difference is something a client can finally act on.

### Next phase

The MCP surface now has one write tool and one read tool, which is the shape the `.clinerules`
differentiator describes. The obvious next items: a listing/recall tool for a known session (which
would owe the same breakdown, the same shared validators, and the same reader scope), the
`candidates_considered` field from finding 2, and the rate limit from finding 5. Finding 1 belongs to
whoever next touches `internal/store`'s standalone `Search`, and finding 3 to whoever decides whether
MCP may answer differently from REST. Nothing carried over from Phases 17-23 has moved: the external
anchor for each tenant's chain head (Phase 17 finding 2, Phase 18 finding 6, Phase 20 finding 8) is
still what makes "the chain was deleted" and "nothing was ever appended" the same answer;
`tenant.RunMigrations`' advisory lock (Phase 20 finding 1) is still the smallest; metering is still the
phase that would make the compliance report's summary honest; the compliance-tier provisioning field
still belongs with whichever phase touches `POST /v2/tenants` next; recording
`GET /v2/compliance/chain-integrity`'s successful reads (Phase 21 finding 1) is still small enough to
ride along; and Phase 23's finding 3 (a compile with no memories is indistinguishable from a compile
that failed to retrieve) is unchanged, now joined by its search-side twin, finding 2 above.

---

## Phase 25 — synapse_write_memory MCP tool (complete)

Phase 23 gave the MCP surface a compiler and Phase 24 gave it the read half; this phase gives it the
write. `synapse_write_memory` stores one memory through the same sanitization pipeline every other
write path in this process runs, embeds it with the same model, and answers with four facts and
nothing else: the uuid it was stored under, whether the pipeline had to rewrite the content, whether
it contradicts a memory the node already holds, and the id of the memory it contradicts. It never
returns the content it was given — sanitized or otherwise — and it never logs it.

Commit `feat: Phase 25 - synapse_write_memory MCP tool`

New files:

- `internal/mcp/write.go` — 253 lines: the tool definition and its schema, `writeArgs`, the handler,
  `parseWriteArgs`/`validMemoryType`, and the `defaultWriteSessionID` constant.
- `internal/mcp/write_conflict.go` — 67 lines: `writeConflictCandidatePool` and `conflictingMemoryID` —
  the one part of this tool that is about the memories the node already holds rather than about the
  write, with its own rationale for living in this layer instead of in the store.
- `internal/mcp/write_result.go` — 29 lines: `writeToolResult`, split out of `write.go` on the same
  seam that split `search_result.go` from `search.go` — the line count, not the taste: registration,
  handler, validation and detection are already 250 lines, and the payload is another 30.
- `internal/mcp/write_test.go` — 376 lines: the DoD round trip, the DoD injection case, the conflict
  case the brief does not ask for, the rejection table, the registration/schema test, and the
  `writeStore` double they share.

Changed:

- `internal/mcp/server.go` — 246 → 263 lines: `registerTools` gains one line, and the package doc gains
  the Phase 25 paragraph plus a revision of its `.clinerules` paragraph. The write tool is the first
  tool here that answers with neither a score breakdown nor a trace id, and the doc says why — it
  surfaces nothing, so it is explained by its id and its conflict verdict — rather than leaving a
  reader to infer that the rule was forgotten.
- `internal/mcp/compile.go` — 278 → 285 lines: the `errorTypeWriteFailed` constant, kept beside its two
  siblings for the reason Phase 24 recorded.
- `bin/synapse` — **not** staged, and not rebuilt by this phase. It was already dirty at the first
  `git status` of this session (`M bin/synapse`) and Phases 22-24 made the same call.

### The conflict verdict forced a real decision, because `store.Write` reports nothing

The brief says "call `store.Write(ctx, entry)`" and "return `conflict_detected` bool", and those two are
only compatible if something tells this layer what the store thought. Nothing does:

- `store.Backend.Write` returns `error` and nothing else (`internal/store/factory.go:31`), so no
  verdict can come back with the write;
- on the Postgres backend the verdict exists, but it is produced *inside* `Write` by a
  `ConflictDetector` that only `cmd/plane` installs (`SetConflictDetector`), and it is written into the
  row's `conflict_status`/`conflict_with_id` columns rather than returned;
- the SQLite backend has no conflict columns at all, so on a standalone node there is no verdict stored
  anywhere to read back — and a read-back would need a by-id read the `Backend` contract does not have;
- `internal/store` is a v1 package this phase was told not to touch, so widening `Write` was not an
  option.

`write_conflict.go` therefore runs detection in this layer: `conflict.NewContradictionDetector` over the
candidates `store.Search` returns, reporting what it found. Three properties are what make that a reuse
rather than a second implementation:

1. **The same detector the plane installs.** `internal/conflict` is a v2 package (import-only, which is
   allowed), its `ContradictionDetector` is stateless, and `cmd/plane/main.go:149` passes the same
   constructor with `conflict.DefaultJaccardThreshold`.
2. **The same threshold from the same config key.** `cfg.ConflictJaccardThreshold`, whose default 0.4
   the config file documents as having to stay in step with `conflict.DefaultJaccardThreshold`; a zero
   value resolves to that constant — the "unset means default" convention this project uses for every
   numeric knob — which is what makes a hand-built `Config` in a test behave like a configured node.
3. **The same best-effort policy.** A candidate read that fails is logged and treated as "no
   contradiction", exactly as `pgconflict.go` documents for its own detection: a write must not gain a
   new failure mode from a feature whose whole purpose is to annotate a row that is otherwise perfectly
   storable.

The candidate set is the store's own `Search` at *this node's* identity (`cfg.AgentID`/`cfg.TeamID`) and
the session the memory is being written into — the same backend the row lands in, under the same
visibility predicate, which is the pair this detector exists for, and never anything the caller can
name. The control plane is deliberately **not** consulted, unlike the search tool: a write's detection is
best effort by policy, a plane pull would cost a second embedding, and it would return candidates that
are not stored where this row is. `TestWriteMemoryReportsAConflict` pins both halves — the verdict
itself, using `internal/conflict`'s own documented fixture pair (`"We decided to use Postgres"` against
`"We decided to use MySQL"`, Jaccard 0.667, inside the 0.4 gate), and the exact reader scope the
candidate read was made at.

### The response cannot echo the content, and the tests check that rather than trusting it

`.clinerules` for this phase says the sanitized content must never come back, and there are three
distinct ways content could leak into this response: a `content` field, the error path quoting what
failed to embed, and the log line. All three are closed:

- `writeToolResult` has no content field at all, so there is nothing for a future edit to populate by
  accident;
- the embed failure path logs the cause and hands the caller a fixed sentence (the Phase 23/24 pattern:
  an embedding error can name a model path, and that does not belong in a model's context);
- the log line carries the memory id, the type, the visibility and the two booleans — no content, and
  no session either, because a session is how private memories are addressed.

The two DoD tests assert it from the outside: each takes the tool's own JSON text and
`require.NotContains` the content it just sent. The injection test goes further and checks the **flag
against storage** rather than against itself — it reads the row back with `GetRecent` and requires
`"[SANITIZED]"`, because `sanitized: true` is a claim about what the store now holds, and a test that
only checked the claim would pass for a tool that reported the flag and stored the original.

That is also why the tool calls `store.Sanitize` itself instead of only relying on `Write` to do it:
`Write` sanitizes silently, and a caller cannot ask a silent function what it did. `store.Sanitize` is
the exported, package-level form of exactly the same pipeline (`internal/store/syncqueue.go`, which
Phase 8 added for the same reason on the plane's write path) — one pattern list, one truncation rule,
one place to change them, and the second pass inside `Write` is idempotent: neither `"[SANITIZED]"` nor
an already-capped prefix re-triggers anything.

### Defaults: three small judgment calls, all named here rather than buried

1. **`default-visibility` is finally read.** The brief says the default is `"org"`, and this node's
   config key says the same thing about itself (`config.go:82`: "DefaultVisibility is applied to
   memories written with no visibility of their own") while nothing had ever read it. An omitted
   `visibility` therefore resolves to `cfg.DefaultVisibility`, falling back to `store.VisibilityOrg`
   when a hand-built `Config` leaves it blank. On every default-configured node that is `"org"`, exactly
   as the brief specifies; on a node whose operator set `default-visibility: private`, the write tool
   honours it, which is what the key was written for.
2. **`session_id` is an optional argument defaulting to `"default-session"`.** The brief's schema named
   no session, and a schema with no session is unimplementable on this project's backends: the
   standalone store's reads are `WHERE session_id = ?` (`store.go:250`), so a memory written outside
   every session is a memory no search can reach — the write would succeed and the DoD's round trip
   would fail. The argument mirrors the search tool's optional `session_id`, is validated by the same
   `api.ValidateSessionID`, and its default is the same `"default-session"` literal that
   `api.extractSessionID` and `proxy` already fall back to.
3. **The annotation hints are explicit, including the ones that are `false`.** `readOnlyHint: false`
   (it writes), `destructiveHint: false` (every call inserts a new row under a freshly generated uuid;
   no call updates or deletes an existing one — the SQLite backend only replaces on an id collision,
   which a new uuid cannot cause, and the Postgres backend inserts `ON CONFLICT (id) DO NOTHING`), and
   `idempotentHint: false` (two identical calls store two memories under two ids). mcp-go's defaults
   describe every tool as destructive, so the destructive claim is the one that had to be made
   explicitly; the registration test asserts all three, because a client gates on them.

### Verification (real output, this phase)

```text
$ gofmt -l internal/mcp/*.go                     # empty
$ go vet ./internal/mcp ./cmd/synapse
VET_OK
$ go build ./...
BUILD_OK

$ go test ./internal/mcp/... -run TestWriteMemory -v
=== RUN   TestWriteMemoryStoresItAndSearchFindsIt
2026/09/22 10:58:14 INFO Store initialized db_path=/tmp/TestWriteMemoryStoresItAndSearchFindsIt2874516267/001/mcp-write.db
2026/09/22 10:58:14 INFO MCP memory write memory_id=663b90ea-776f-4522-bb72-29da88a6a4bb memory_type=decision visibility=org sanitized=false conflict_detected=false
    write_test.go:128: synapse_write_memory response: {"id":"663b90ea-776f-4522-bb72-29da88a6a4bb","conflict_detected":false,"conflict_with_id":"","sanitized":false}
2026/09/22 10:58:14 INFO MCP memory search trace_id=search-681e69ad42ebd392 visibility=org top_k=10 candidates=1 memories=1
--- PASS: TestWriteMemoryStoresItAndSearchFindsIt (0.35s)
=== RUN   TestWriteMemorySanitizesInjectionPattern
2026/09/22 10:58:14 INFO Store initialized db_path=/tmp/TestWriteMemorySanitizesInjectionPattern3612561400/001/mcp-write.db
2026/09/22 10:58:14 WARN Prompt injection detected and neutralized pattern="ignore all"
2026/09/22 10:58:14 INFO MCP memory write memory_id=39ce700b-035b-4dc9-a194-9268e7fda2a3 memory_type=context visibility=org sanitized=true conflict_detected=false
    write_test.go:184: synapse_write_memory response (injection): {"id":"39ce700b-035b-4dc9-a194-9268e7fda2a3","conflict_detected":false,"conflict_with_id":"","sanitized":true}
--- PASS: TestWriteMemorySanitizesInjectionPattern (0.29s)
=== RUN   TestWriteMemoryReportsAConflict
2026/09/22 10:58:14 INFO MCP memory write memory_id=831830bf-ef4c-41a1-8802-827095f632b5 memory_type=decision visibility=org sanitized=false conflict_detected=true
    write_test.go:234: synapse_write_memory response (conflict): {"id":"831830bf-ef4c-41a1-8802-827095f632b5","conflict_detected":true,"conflict_with_id":"existing-memory","sanitized":false}
--- PASS: TestWriteMemoryReportsAConflict (0.00s)
=== RUN   TestWriteMemoryRejectsInvalidArguments
=== RUN   TestWriteMemoryRejectsInvalidArguments/an_unknown_memory_type_is_not_stored
=== RUN   TestWriteMemoryRejectsInvalidArguments/a_missing_memory_type_is_not_stored
=== RUN   TestWriteMemoryRejectsInvalidArguments/blank_content_is_not_stored
=== RUN   TestWriteMemoryRejectsInvalidArguments/content_with_a_null_byte_is_not_stored
=== RUN   TestWriteMemoryRejectsInvalidArguments/an_unknown_visibility_is_not_stored
=== RUN   TestWriteMemoryRejectsInvalidArguments/an_illegal_session_id_is_not_stored
--- PASS: TestWriteMemoryRejectsInvalidArguments (0.01s)
    --- PASS: TestWriteMemoryRejectsInvalidArguments/an_unknown_memory_type_is_not_stored (0.00s)
    --- PASS: TestWriteMemoryRejectsInvalidArguments/a_missing_memory_type_is_not_stored (0.00s)
    --- PASS: TestWriteMemoryRejectsInvalidArguments/blank_content_is_not_stored (0.00s)
    --- PASS: TestWriteMemoryRejectsInvalidArguments/content_with_a_null_byte_is_not_stored (0.00s)
    --- PASS: TestWriteMemoryRejectsInvalidArguments/an_unknown_visibility_is_not_stored (0.00s)
    --- PASS: TestWriteMemoryRejectsInvalidArguments/an_illegal_session_id_is_not_stored (0.00s)
=== RUN   TestWriteMemoryToolIsRegisteredAndAdvertisesItsSchema
--- PASS: TestWriteMemoryToolIsRegisteredAndAdvertisesItsSchema (0.01s)
PASS
ok  	synapse/internal/mcp	0.678s

$ go test ./internal/mcp/...
ok  	synapse/internal/mcp	3.547s

$ go test ./...
ok  	synapse/cmd/synapse	0.024s
ok  	synapse/internal/api	(cached)
ok  	synapse/internal/classifier	(cached)
ok  	synapse/internal/compiler	(cached)
ok  	synapse/internal/conflict	(cached)
ok  	synapse/internal/dedup	(cached)
ok  	synapse/internal/embedder	(cached)
ok  	synapse/internal/integration	(cached)
ok  	synapse/internal/mcp	3.160s
ok  	synapse/internal/plane	(cached)
ok  	synapse/internal/proxy	(cached)
ok  	synapse/internal/retrieval	(cached)
ok  	synapse/internal/scorer	(cached)
ok  	synapse/internal/store	(cached)
ok  	synapse/internal/supersession	(cached)
ok  	synapse/internal/sync	(cached)
ok  	synapse/internal/tenant	(cached)
ok  	synapse/internal/trace	(cached)
EXIT=0
```

### Findings, limitations, and what this phase did not do

1. **On a standalone node, detection can only see one session.** The local backend's `Search` is
   `WHERE session_id = ?` (`store.go:250`), so the cross-agent, cross-session contradiction
   `internal/conflict` exists for — "we decided to use Postgres" on one node, "we decided to use MySQL"
   on another — is undetectable on a node with no control plane. Fixing it means changing
   `internal/store`'s `Search` or adding a cross-session candidate read, both v1 (out of scope for this
   phase). Same shape as Phase 24's finding 1, and the same owner.
2. **The reported verdict and the stored row's `conflict_status` are two different things on a
   plane-backed node.** This tool labels nothing, because it cannot: the SQLite table has no conflict
   columns and `PGStore` owns its own. If the node's `PGStore` happens to have a detector installed,
   `Write` marks its verdict on the rows while this tool reports the one it computed. They agree in
   algorithm and threshold but not in candidate set — `PGStore` compares against the 20 most recent
   org-scoped memories, this compares against the 20 most similar ones the node may read — so the two
   can disagree on the margin. A future phase that wants literally one verdict should return it from
   `Write`, which is a v1 signature change and therefore not this phase's to make.
3. **A memory written here is never queued for sync.** `SyncStatus` is left blank so each backend applies
   its own default (`local_only` locally, `synced` on the plane), which is exactly what the proxy's and
   the compile path's writes do — nothing on the edge ever sets `sync_pending`. So a write performed
   through this tool on an edge node does not reach the control plane. Whoever owns the edge's push loop
   should decide whether it should; it is a one-line change at the write site and a policy question
   rather than a technical one.
4. **The candidate read and the insert are not atomic.** Two concurrent writes with contradictory content
   can both compare against a store that does not yet hold the other, and neither is flagged. This is
   the store's own shape too (`detectConflict` runs before the insert there as well), and the failure
   mode is a missing label rather than a lost or corrupted memory.
5. **Twenty candidates, chosen by similarity.** `writeConflictCandidatePool` mirrors
   `store.conflictCandidateLimit`, so a contradiction with a memory outside the 20 nearest is missed — by
   design and by precedent rather than by accident. Note the two pools are *ordered* differently
   (similarity here, recency there), which is what finding 2 turns on.
6. **No rate limit.** Unchanged from Phase 24's finding 5: every call is one embedding pass plus one
   candidate search, and a local process looping on `tools/call` is unbounded.
7. **A `team`-scoped write on a node with no team id is not rejected here.** `PGStore.visibilityForWrite`
   narrows it to `private` and logs; the SQLite backend ignores visibility entirely. This tool validates
   the three scope names and passes the value through, deliberately leaving the normalization to the
   store, which documents itself as the one place a write's scope is decided — so a caller asking for
   team scope on a standalone node gets neither an error nor the scope it named. If that is the wrong
   call, the fix belongs in `write.go`'s validation rather than in three places.
8. **The DoD asks for the tests in `server_test.go`, and they live in `write_test.go`.** `server_test.go`
   is the transport file (227 lines: the stdio and TCP seams), and the five tests here would have carried
   it past the 300-line cap. Every tool since Phase 23 has its own test file (`compile_test.go`,
   `search_test.go`) and this one keeps that shape. The DoD's command, `-run TestWriteMemory`, is
   unaffected — all five run under it.

### Next phase

The MCP surface now has all three verbs — compile, read, and write — and the differentiator the
`.clinerules` describe holds across them: the two tools that surface a memory show its S/R/I/T
breakdown and a trace id, and the one that stores a memory answers with the id and the conflict verdict
that are the only two facts a write can be explained by. The obvious next items: a listing/recall tool
for a known session (which would owe the same breakdown and the same reader scope), the
`candidates_considered` field Phase 24 finding 2 asked for, the rate limit from Phase 24 finding 5, and
finding 3 above — whether a write performed on an edge node should reach the plane — which is small and
belongs to whoever owns the push loop. Finding 1 belongs to whoever next touches `internal/store`'s
standalone `Search`, and finding 2 to whoever decides whether `Write` should return its verdict.

Nothing carried over from Phases 17-24 has moved: the external anchor for each tenant's chain head
(Phase 17 finding 2, Phase 18 finding 6, Phase 20 finding 8) is still what makes "the chain was
deleted" and "nothing was ever appended" the same answer; `tenant.RunMigrations`' advisory lock
(Phase 20 finding 1) is still the smallest; metering is still the phase that would make the compliance
report's summary honest; the compliance-tier provisioning field still belongs with whichever phase
touches `POST /v2/tenants` next; recording `GET /v2/compliance/chain-integrity`'s successful reads
(Phase 21 finding 1) is still small enough to ride along; and Phase 23's finding 3 (a compile with no
memories is indistinguishable from a compile that failed to retrieve) is unchanged, now with its
search-side twin (Phase 24 finding 2) and its write-side counterpart above (finding 5).

## Phase 26 — Usage metering (complete)

Every successful compilation now leaves one row in `synapse_global.usage_events`, written in a goroutine no
request waits on, and every compilation through the edge node's live path logs one structured line that
says what the sieve saved: `raw`, `compiled`, `reduction_pct`, `savings_usd`. The row is what the
compliance report's two metering-facing fields are read from — `TotalCompilations` and
`AvgReductionPct` — so for the first time in this project those numbers are written by the thing they
describe rather than inferred from signed traces whose own `tokens_used` and `reduction_pct` are still
zero (Phase 18 finding 1, Phase 20 finding 4).

Commit `feat: Phase 26 - usage metering`

New files:

- `internal/metering/meter.go` — 201 lines: `UsageEvent` (the brief's eight fields), `Meter`, `NewMeter`,
  `Record`, `canonicalID`, `nullableText`, the one INSERT this package owns, and the table name taken from
  `internal/tenant` so it is written down once, exactly as `internal/ledger` takes `.ledger` and
  `.compliance_access_log`.
- `internal/metering/meter_test.go` — 233 lines: the definition of done (ten `Record` calls with ten
  distinct tenant ids, then ten rows), the one-tenant variant with a `raw_tokens` sum, and the shared
  helpers (`meterPool`, `newEvent`, `countForTenants`, `settle`).
- `internal/metering/meter_row_test.go` — 154 lines: what a single row's columns have to hold, the index
  the migration adds, and the malformed-tenant-id guard with the negative control that follows it.
- `internal/metering/meter_guard_test.go` — 102 lines, no build tag: nil meter and nil pool inertness,
  "Record returns before the write completes" against a non-routable address, and the two small mappings
  (`canonicalID`, `nullableText`).
- `internal/compiler/usage.go` — 159 lines: `UsageEvent`, the `UsageSink` interface, the `atomic.Pointer`
  wiring, `SetUsageSink`, `RecordUsage`, and `SavingsUSD`, which is the one definition of the rate.
- `internal/compiler/usage_test.go` — 165 lines: the seam's contract, including which side owns the
  asynchrony — the assertion is that `RecordUsage` has already handed the event over when it returns, not
  that it returns early.
- `cmd/synapse/meter.go` — 116 lines: `meterSink` (the adapter that fills in this node's tenant) and
  `enableMetering`, which mirrors `enableEnterpriseLedger`'s structure and fails the same soft way.

Changed:

- `internal/proxy/proxy.go` — 872 → 901 lines: two statements after `reductionPct` is computed — the
  `compiler.RecordUsage` call and the `compiled` log line. This is the canonical completion point for live
  traffic, and the only place in that file this phase touched.
- `internal/api/api.go` — 762 → 783 lines: the same `RecordUsage` call in `runCompilePipeline`, guarded by
  the `persist` flag the pipeline already had, which covers `POST /v1/compile` and MCP
  `synapse_compile` (both ride `CompileContext`) and excludes the playground.
- `internal/config/config.go` — 364 → 374 lines: `UpstreamModel` / `upstream-model`, the operator's label
  for what the upstream serves, read only by the usage event.
- `internal/tenant/migrations.go` — 206 → 218 lines: one idempotent migration, the
  `usage_events_tenant_created_idx` index on `(tenant_id, created_at)` that PROGRESS.md's Phase 20
  finding 6 said belonged to whichever phase started writing rows.
- `cmd/synapse/main.go` — 728 → 747 lines: the metering block beside Phase 18's ledger block, inside the
  same `control-plane-url` gate.
- `synapse.yaml.example` — 134 → 139 lines: `upstream-model` documented next to `upstream-url`, and the
  same three lines added to `synapse init`'s template in `cmd/synapse/main.go`.
- `bin/synapse` — **not** staged, and left dirty by the demo build, as in Phases 22-25.

### The numbers forced the two call sites, not `Compile`

The brief said to wire metering "the same place Phase 18 wired the ledger", and the honest reading of that
turned out to be the wiring, not the line. Phase 18 installed its sink from `cmd/synapse` but *called* it
from inside `compiler.Compile`, which is where `internal/compiler/ledger.go` records — in its own words —
that the ledgered trace "carries tokens_used 0 and reduction_pct 0, because both are still unset at the
moment the trace is complete from this package's point of view", and that "editing the two v1 call sites
to append after their own fixups is what would make those fields truthful, and that is a deliberate
later-phase decision."

Those two fields are this phase's entire payload. `raw_tokens` is the retrieved candidate pool's token
count and `reduction_pct` is the difference between that pool and what the sieve emitted, and both of them
exist only after `Compile` returns: `internal/proxy` and `internal/api` compute them from
`budget.Fill`'s two return values and then write them back into the trace. A sink called from inside
`Compile` could therefore only ever have written `raw_tokens 0 / compiled_tokens 0 / reduction_pct 0` —
three stored falsehoods, once per compilation, in the table a compliance report reads. So the seam is
Phase 18's (`SetUsageSink`, an interface declared in `internal/compiler`, installed once before the router
serves), and the *call* is at the two places that own the numbers. This phase is the later-phase decision
`ledger.go` predicted, and it leaves that file's own fidelity gap exactly where it was, now with a
precedent next door.

### What this phase added to v1 files, and why that was the only honest option

`.clinerules` says a v1 internal may not be touched "unless a v2 bug explicitly requires it". Four v1 files
changed here, and each change is one statement or one field:

- `internal/proxy/proxy.go` (live traffic) and `internal/api/api.go` (`/v1/compile` and MCP): one
  `compiler.RecordUsage(...)` call each. Without them there are no truthful numbers to record — see above —
  so the alternative was not a smaller change to v1 but a different, dishonest feature.
- `internal/tenant/migrations.go`: the `(tenant_id, created_at)` index the report's totals query needs.
  Phase 20 finding 6 recorded that this belonged to the phase that starts writing rows, and this is it.
- `internal/config/config.go`: `upstream-model`, because the brief names `cfg.UpstreamModel` and no such
  field existed. It is the operator's label for what the upstream serves — nothing about proxying reads
  it, it never reaches the upstream, and it is never taken from a request — and it exists so a usage row
  can say which model a saving was measured against. Blank records `NULL`, not the empty string.

Everything else is additive: `internal/compiler` gained one new file and one file each in
`internal/metering` and `cmd/synapse`. No v2 package is imported by the scoring pipeline, no behaviour
changed for a node that does not meter, and the log line is the one edit that every node now emits.




### The decisions the brief did not make

**Metering is not plan-gated, and the ledger still is.** `enableMetering` asks for the same two things
`enableEnterpriseLedger` asks for — a credential that names a tenant, and a database — and deliberately
does not ask about the plan. `usage_events` is where the compliance report's compilation count and mean
reduction come from for every tenant, so an `oss` or `team` tenant that was not metered would read zero
compilations for work it actually did. The ledger's enterprise gate is right for the ledger, which is a
signed chain an enterprise SKU includes; it is wrong here, and the difference is written down in both the
function and `internal/compiler/usage.go` rather than left to look like an oversight.

**The playground is not metered; MCP is.** `runCompilePipeline` already had the switch that means "real
traffic": the `persist` flag. It is true for `POST /v1/compile` and for the MCP surface's
`synapse_compile` — both of which `CompileContext` drives — and false for `/api/playground/compile`,
which is this operator's own UI. Metering it would bill a tenant for its own dashboard, so the call sits
behind `persist` and the reason is in the comment there.

**A node with no credential names no tenant, so it does not meter.** The tenant id comes from the node's
own control-plane credential through `resolveLedgerPlan`, the function Phase 18 already wrote, so "this
node's tenant" has one definition rather than two that could disagree. TenantID is left empty by both v1
call sites and filled in by the adapter: nothing a request carries can reach a billing column.

**`session_id` is stored and never logged or returned.** The column exists because "which session spent
this" is a real billing question; `Record`'s only log line carries the error and nothing else, which is
why `internal/metering`'s failure path cannot leak a session handle even if a database error quotes one.
The integration test asserts the value is in the row, so a later "we never return it, so why store it"
cleanup cannot quietly remove it.

**Failures are dropped, not retried.** `Record` never returns an error and never retries: a retry would
have to buffer usage data this process has no store for, or block a caller, and the next compilation
writes its own row anyway — the same reasoning the ledger sink documents for a failed append. The
consequence is honest and recorded below as finding 4.

**The write is asynchronous; the seam is not.** `RecordUsage` calls the sink and returns when the sink
returns; the goroutine, the 3-second ceiling, and the absent error live in `metering.Meter.Record`.
Putting the goroutine in the seam would have cost one unaccounted goroutine per compilation and would have
handed the sink a copy of a struct both callers keep writing to.

### Evidence

The definition of done's command, verbatim (a fresh run, not a cached one):

```text
$ go test ./internal/metering/... -count=1 -v -tags integration
=== RUN   TestRecordWithNoPoolIsInert
--- PASS: TestRecordWithNoPoolIsInert (0.00s)
=== RUN   TestRecordReturnsBeforeTheWriteCompletes
2026/09/23 09:20:13 WARN metering: record failed err="context deadline exceeded"
--- PASS: TestRecordReturnsBeforeTheWriteCompletes (3.20s)
=== RUN   TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse
=== RUN   TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_
=== RUN   TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_not-a-uuid
=== RUN   TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_1234
=== RUN   TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_6f9c1e5a-3b7d-4c21-9a5e-8d0f4b2c7e3
--- PASS: TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse (0.00s)
    --- PASS: TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_ (0.00s)
    --- PASS: TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_not-a-uuid (0.00s)
    --- PASS: TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_1234 (0.00s)
    --- PASS: TestCanonicalIDAcceptsAUUIDAndRefusesAnythingElse/refuses_6f9c1e5a-3b7d-4c21-9a5e-8d0f4b2c7e3 (0.00s)
=== RUN   TestNullableTextMapsNothingToNull
--- PASS: TestNullableTextMapsNothingToNull (0.00s)
=== RUN   TestRecordStoresEveryColumn
--- PASS: TestRecordStoresEveryColumn (0.22s)
=== RUN   TestUsageEventsTableHasTheReportIndex
--- PASS: TestUsageEventsTableHasTheReportIndex (0.08s)
=== RUN   TestRecordDropsAMalformedTenantID
2026/09/23 09:20:14 WARN metering: record failed err="metering: tenant id must be a uuid"
--- PASS: TestRecordDropsAMalformedTenantID (0.14s)
=== RUN   TestRecordWritesOneRowPerEvent
--- PASS: TestRecordWritesOneRowPerEvent (0.34s)
=== RUN   TestRecordWritesEveryEventForOneTenant
--- PASS: TestRecordWritesEveryEventForOneTenant (0.17s)
PASS
ok  	synapse/internal/metering	4.181s
```

Two lines in that output are this phase's own claims being demonstrated rather than asserted: the
`context deadline exceeded` warning belongs to the asynchrony test — the write it never waited for fails
three seconds later, under its own ceiling, and the warning names the error and nothing else — and the
`tenant id must be a uuid` warning is the guard that keeps one bad credential from becoming a failed
INSERT per compilation for as long as the node runs.

The ten-events count is scoped to the ten tenant ids the test mints
(`WHERE tenant_id = ANY($1::uuid[])`), so no other run's rows can satisfy it, and the same test asserts
each tenant has exactly one row — "ten rows" cannot be ten rows for one tenant. The one-tenant variant
additionally sums `raw_tokens`, which is what would catch a column overwritten in place; a count alone
would not.

One real compilation through the proxy, against Ollama's `gemma4:31b-cloud`, on a live edge node whose boot
log also says the sink is installed:

```text
9:16AM INFO synapse: Usage metering enabled: every compiled request records a usage event
9:16AM INFO synapse: compiled raw=7477 compiled=2927 reduction_pct=60.9% savings_usd=$0.0682
```

and the rows that compilation and its neighbours produced, read back from the database (the tenant is the
one `POST /v2/tenants` had just provisioned, `352b78e2-ded0-40ae-9033-2ba34059ddc4`):

```text
 rows | min_raw | max_raw | avg_reduction_pct |      model       |     agent
------+---------+---------+-------------------+------------------+----------------
    3 |       0 |    7477 |              20.3 | gemma4:31b-cloud | phase26-agent

          created_at           | raw_tokens | compiled_tokens |   reduction_pct   |      model
-------------------------------+------------+-----------------+-------------------+------------------
 2026-09-23 09:16:48.368398+00 |          0 |               0 |                 0 | gemma4:31b-cloud
 2026-09-23 09:16:46.201872+00 |          0 |               0 |                 0 | gemma4:31b-cloud
 2026-09-23 09:16:42.932411+00 |       7477 |            2927 | 60.85328340243413 | gemma4:31b-cloud
```

The two `raw_tokens 0` rows are not a bug in the write path: those requests were made before a stable
`Authorization` header was sent, so each had a session of its own and nothing to retrieve (finding 10).
The `session_id` column is deliberately not selected above — it is in the rows, and it is in no log line
and no response — and `psql`'s own output is the only place these numbers appear outside the database.

The same database shows the index the migration added, `usage_events_tenant_created_idx`, alongside the
primary key, and the plane's boot in that run logged `migrations complete schema=synapse_global` — so the
new statement applies cleanly on a real boot and is a no-op on the ones after it.

Also run and green: `go build ./...`, `go vet ./...`, every untagged package (`go test ./... -count=1`, 21
packages), and the integration-tagged suites of `internal/ledger`, `internal/tenant`, `internal/plane` and
`internal/store` — the four packages whose database the new migration statement touches:

```text
$ go test ./internal/ledger/... ./internal/tenant/... ./internal/plane/... ./internal/store/... -count=1 -tags integration
ok  	synapse/internal/ledger	15.710s
ok  	synapse/internal/tenant	0.946s
ok  	synapse/internal/plane	17.144s
ok  	synapse/internal/store	7.711s
```

### Findings

1. **`avg_reduction_pct` is a real number now, for the windows that metering covers.** Phase 20 finding 4
   recorded that the report read `0.00%` because nothing wrote `usage_events` and the ledgered traces it
   fell back to carry `reduction_pct 0`. A metering node's window now answers from rows that hold the
   number the edge node computed: the demo's three rows average `20.3`, and `TotalCompilations` prefers the
   same source. It is a real number and not yet a complete one — a window that predates this phase still
   falls back to traces with zeroes, and the report has no field that says which of the two it used, which
   is the other half of Phase 20 finding 4 and is still open.
2. **Phase 18's trace fidelity gap is untouched and now has a template.** The ledger still signs
   `tokens_used 0` and `reduction_pct 0`, because its append still happens inside `Compile`. The fix is two
   lines at the same two call sites this phase edited, in the same shape as `compiler.RecordUsage` — which
   means the two artifacts a compliance officer can compare (a signed trace and a metering row for the same
   compilation) currently disagree by construction, and the fix is now cheap enough to be a phase of its
   own.
3. **A node with a DSN and a credential but no `control-plane-url` meters nothing.** `cmd/synapse` only
   reaches `enableMetering` inside the `control-plane-url` gate, because that is where Phase 18 put its
   block and because `database-dsn` is documented as meaningful only alongside a control plane. The write
   path itself does not care — `enableMetering` opens its own pool from the DSN — so widening the gate is a
   one-line decision for whoever wants a standalone node to meter.
4. **A metering outage loses events silently.** There is no spool, no retry, and no reconciliation: a
   database that is down at compile time produces a warning and no row, so `usage_events` under-counts
   rather than over-counts. Nothing bills from this table yet, which is what makes the trade acceptable
   today; before anything invoices from it, the missing piece is either a durable local spool or a
   plane-side rollup from the sync stream, which already carries every compiled trace for an enterprise
   tenant.
5. **`Meter` holds a concrete pool, so the "never blocks" guard is a timing test.** The brief specifies
   `pool *pgxpool.Pool`, which leaves no seam to inject a slow executor; the untagged guard therefore
   asserts against a non-routable address (TEST-NET-1) that `Record` returns in under a second while its
   write is still outstanding, with the three-second ceiling documented as the reason the bound is loose.
   An `Executor` interface would make the property structural, and is worth adding when a second
   implementation exists — not before, since a one-implementation interface is only indirection.
6. **An enterprise node now holds two pools to one database.** Phase 18's ledger wiring and this phase's
   metering wiring each open their own `store.OpenPGPool` and each own its lifetime. Two or three
   connections are not a problem; it is the kind of duplication that quietly becomes four pools as more v2
   sinks are added, and one shared edge pool is the small refactor that prevents it.
7. **Nothing reads `agent_id`, `session_id`, or `model` yet.** The report reads only `count(*)` and
   `avg(reduction_pct)`, which is why the new index is on `(tenant_id, created_at)` alone: that is the only
   predicate anything runs. The three other columns are the substrate for the billing and
   session-attribution work that comes after, and they were far cheaper to record now than to backfill.
8. **Observed during the demo, and pre-existing rather than caused here: `cmd/synapse` never calls
   `store.NewStoreFromConfig`.** With `control-plane-url`, a credential, `agent-id`, `team-id`, and
   `database-dsn` all set, the node logged
   `Store initialized db_path=/home/ranscky/.local/share/synapse/synapse.db` — the SQLite store — while
   metering wrote to Postgres. `NewStoreFromConfig`, the function whose documented job is "the local SQLite
   store when no control plane is configured, or a tenant-scoped Postgres store when one is", has no caller
   anywhere in `cmd` or `internal`. Metering does not depend on which store is in use, so this phase could
   proceed without answering it, but a node that believes it is storing memories in its tenant's Postgres
   schema while writing them to a local file is a wiring bug worth fixing before metering feeds anything
   that matters. It is the first thing this phase's demo found by accident, and it belongs to whoever owns
   `cmd/synapse`'s store construction.
9. **A failed provisioning leaves a tenant row behind, so a retry answers 409 with a tenant that has no
   secret.** The demo's first `POST /v2/tenants` failed with HTTP 500 (`SYNAPSE_MASTER_KEY` was missing from
   the plane's environment even though the config file carried `master-key`, because `internal/tenant`
   reads that key from the environment only), and the retry with the same slug answered 409: the registry
   row had already been written, while the wrapped secret and the minted token never were. That is Phase
   2/16's provisioning order showing up as an operational shape — the endpoint is not atomic across those
   three writes, and an operator who hits it can neither reuse the slug nor see the half-provisioned tenant
   from outside. A compensating delete, or writing the registry row last, is the fix; the demo's own
   recovery was a second slug.
10. **A compile with no candidates still looks exactly like a compile whose session is new.** The demo
    needed a stable `Authorization` header before any memory was retrieved: without one, `deriveSessionID`
    falls back to a per-conversation fingerprint, so a dozen distinct messages are a dozen sessions and
    `original_candidates=0` on every request — the same log line an empty store produces (Phase 23 finding
    3). The metering rows for those requests faithfully record `raw_tokens 0`, which is correct and useless
    for telling the two cases apart; a `candidates_considered`-style field (Phase 24 finding 2) would fix
    both.

Nothing carried over from Phases 17-25 has moved except the one item this phase was named for. The
external anchor for each tenant's chain head (Phase 17 finding 2, Phase 18 finding 6, Phase 20 finding 8)
is still what makes "the chain was deleted" and "nothing was ever appended" the same answer;
`tenant.RunMigrations`' advisory lock (Phase 20 finding 1) is still the smallest — and still unfixed after
a phase that ran migrations against a live database; the compliance-tier provisioning field still belongs
with whichever phase next touches `POST /v2/tenants`, which is also where finding 9 above lands; recording
`GET /v2/compliance/chain-integrity`'s successful reads (Phase 21 finding 1) is still small enough to ride
along; and the two fidelity gaps that now sit next to each other — the ledgered trace's zeroed tokens
(finding 2 above) and a compile that cannot say why it retrieved nothing (finding 10) — are the pair a
reporting phase would close first, because this phase made the numbers they feed real.

## Phase 27 — Stripe webhook handler (complete)

`POST /v2/billing/webhook` is live on the control plane, and it is the first route in this project whose
authentication is not a token. Three Stripe events drive one column — `synapse_global.tenants.status` —
between `active`, `grace_period`, and `suspended`: `invoice.payment_succeeded` clears the grace period and
sets `active`, `invoice.payment_failed` sets `grace_period` with `grace_period_started_at = now()`, and
`customer.subscription.deleted` sets `suspended`. The `Stripe-Signature` header is validated against the
signing secret before anything else happens, and a delivery that fails it is answered
`400 {"error":"invalid_signature"}` and reaches no SQL at all — which the tests assert structurally, with a
recording double, rather than by observing that a row happened not to change.

Commit `feat: Phase 27 - Stripe webhook handler`

New files:

- `internal/billing/stripe.go` — 295 lines: `WebhookHandler` (the brief's `cfg, pool` signature) over
  `newHandler` (the same handler over the narrow `rowQuerier` interface), the three status statements,
  `customerOf`, `readBody`, `applyStatus`, the `StatusActive` / `StatusGracePeriod` / `StatusSuspended`
  vocabulary, and `MaxWebhookBodyBytes`.
- `internal/billing/doc.go` — 38 lines: the package comment alone. It moved out of stripe.go when that file
  crossed the 300-line ceiling, which is the arrangement `internal/ledger/doc.go` already makes.
- `internal/billing/respond.go` — 53 lines: `{"received":true}`, `{"error":"reason"}`, and the two writers the
  package shares.
- `internal/billing/stripe_test.go` — 260 lines: the four tests the phase is defined by, plus the payload and
  signing helpers they share. Every accepted case is signed through `webhook.GenerateTestSignedPayload` and
  validated by the handler's real `ConstructEventWithOptions` call, so nothing about the signature is stubbed.
- `internal/billing/stripe_db_test.go` — 127 lines: the pool, `tenant.RunMigrations`, the seeded tenant row,
  and `billingStatus` — the read-back that is the assertion in every database-backed case.
- `internal/billing/stripe_unit_test.go` — 269 lines: `recordingDB` / `stubRow` and the five tests that run
  with no database at all, which is what keeps the security property's evidence present in CI.
- `internal/plane/billing.go` — 44 lines: `billingWebhookRoute` and `handleBillingWebhook`, split out of
  handlers.go the way sync.go and search.go are.
- `internal/plane/billing_route_test.go` — 69 lines: the route answers with no `Authorization` header and
  delegates, and a plane built without a handler answers 503 rather than 404.
- `internal/plane/config_redact.go` — 70 lines: `RedactedFields`, `secretState`, and `UnsafePermissions`,
  moved out of config.go when the new field would have pushed it past 300.

Changed:

- `internal/plane/config.go` — 296 → 268 lines: `StripeWebhookSecret` / `stripe-webhook-secret`,
  `EnvStripeWebhookSecret` (`STRIPE_WEBHOOK_SECRET`, the one variable that is not `SYNAPSE_`-prefixed, because
  it is Stripe's own name), the `applyEnvOverrides` line, and the comment that counted four secret keys.
- `internal/plane/handlers.go` — 227 → 254 lines: the tenth `NewServer` parameter and its `Server` field, the
  route line, and the `Routes` docs that now name two open routes instead of one.
- `internal/tenant/migrations.go` — 218 → 248 lines: `tenants_billing_columns` (the brief's three
  `ADD COLUMN IF NOT EXISTS`) and `tenants_stripe_customer_idx`.
- `cmd/plane/main.go` — 251 → 263 lines: `billing.WebhookHandler(cfg, pool)` built beside the ledger verifier
  and the compliance auditor, and passed into `NewServer`.
- Thirteen test call sites in `internal/plane/*_test.go` — one trailing `nil` each — plus
  `internal/plane/config_test.go` (+52 lines: the new key in `sampleConfig`, the env-precedence test, the
  fail-closed-Validate test, and `EnvStripeWebhookSecret` added to `clearSecretEnv` so a developer's exported
  variable cannot change an outcome) and `internal/plane/redact_test.go` (+5 lines).
- `synapse-plane.yaml.example` — 64 → 87 lines: the new key documented with its variable name, its
  fail-closed behaviour, and the fact that its value never reaches a log.
- `deploy/docker-compose.yml` — the plane service passes `STRIPE_WEBHOOK_SECRET` through (blank allowed), and
  the header's "only the four secret keys are overridable" became "only the secret keys".
- `go.mod` / `go.sum` — `github.com/stripe/stripe-go/v76 v76.25.0`, direct after `go mod tidy`.
- `bin/synapse` — **not** staged, and left dirty by the demo build, as in Phases 22-26.

### The brief's `stripe.ConstructEvent()` does not exist in v76, and its replacement needed one option changed

`stripe-go` moved webhook handling into its own package: v76 has no `webhook.go` at the module root, and the
function is `webhook.ConstructEvent(payload []byte, header, secret string) (stripe.Event, error)`
(`webhook/client.go:68`). Same HMAC-SHA256 over `"<unix-ts>.<body>"`, same 300-second tolerance, same error
semantics — one package over. The route therefore imports `stripe-go/v76/webhook` as well as the root package,
and the brief's intent (validation is mandatory, and it is the only auth) is exactly what shipped.

What did change is an option the brief's verbatim call would have kept. Plain `ConstructEvent` validates more
than the signature: it fails when `event.APIVersion != stripe.APIVersion` (`webhook/client.go:206`), and v76
pins that constant at `2023-10-16`. A Stripe account whose webhook endpoint is pinned to any other version —
which today is most of them, since v76 is from April 2024 — would have had **every legitimately signed
delivery rejected with 400**, and no amount of correct configuration on the Synapse side would have fixed it.
So the handler calls `ConstructEventWithOptions(..., ConstructEventOptions{IgnoreAPIVersionMismatch: true})`:
the signature and the timestamp tolerance are still enforced in full, and only the schema-version assertion is
relaxed, which is safe because the one field this package reads (`data.object.customer`) is stable across
those versions. The tests document the choice by *omitting* `api_version` from every payload they sign.

The other library decision: the customer id comes from `json.Unmarshal(event.Data.Raw, &struct{ Customer
*stripe.Customer })`, not from `event.GetObjectValue("customer")`. `Data.Raw` is exactly `data.object`, and
`stripe.Customer` has its own `UnmarshalJSON` that accepts both shapes Stripe can send — a bare id string on an
unexpanded object, a whole object on an expanded one (`customer.go`, `ParseID`). The map-based accessor would
have stringified the expanded shape into a `map[...]` that is not an id.

### internal/plane cannot import internal/billing, so the route is registered here and wired there

`internal/billing` reads the schema name from `internal/tenant` (the arrangement `internal/ledger` and
`internal/metering` already have: the schema is written down once, in the package that owns the migration), and
`internal/tenant` imports `internal/plane` for `PlaneConfig`. So `plane -> billing` would be
`plane -> billing -> tenant -> plane`. The route is still registered in `handlers.go`, and the handler is the
tenth `NewServer` parameter, constructed in `cmd/plane` — the same shape and the same reasoning the `auth`
field already carries one line above it. `plane.Server` holds a `Database{Ping}` rather than a pool, so the
alternative (importing billing and calling `WebhookHandler(cfg, pool)` from the route) was never available
anyway: this package has never had a pool to hand it. Verified with `go list`: `internal/billing` imports
`internal/plane` and `internal/tenant`, and neither imports `internal/billing`.

### The route is open, and that is not the same as unauthenticated

`POST /v2/billing/webhook` has no `requireJWT` and no `requireAdmin`, which makes it the second open route in
the plane after `GET /health`. That is deliberate and it is the only shape that can work: Stripe is not a
tenant, holds no Synapse credential, and a middleware that demanded one would 401 every real delivery. Its
credential is the `Stripe-Signature` header, checked inside the handler against a secret only Stripe and the
plane hold. The tests pin the property from both sides — `internal/plane` proves the route answers with no
`Authorization` header and hands the delivery to the injected handler, and `internal/billing` proves that the
same route with a wrong or missing signature changes nothing.

The status code the unconfigured plane gives is worth its own line: 503, not 400. A plane whose
`stripe-webhook-secret` is empty can validate nothing, so it trusts nothing — but 400 would tell Stripe the
delivery was bad, and Stripe would stop retrying something that was never the sender's fault. A 503 is
retryable, and Stripe retries a non-2xx for about three days, which is the window an operator has to configure
the secret without the events being lost. It is the `AdminToken` precedent (a missing credential fails closed
at the route that needs it rather than refusing to boot) with one refinement the brief did not name.

### `status` is written and nothing reads it yet

This is the phase's honest boundary, recorded rather than papered over: a tenant marked `suspended` today keeps
working. `internal/billing` has no request-serving path to hang enforcement on, and the middleware that would
refuse a suspended tenant's token belongs with `internal/tenant/auth.go` — a later phase, and a small one. What
exists now is the fact and the timestamp it started, in the registry, which is what a report or an enforcement
gate needs to read.

### Evidence

The definition of done's command, with the compose database named explicitly so the four database-backed cases
actually run (a fresh `-count=1` run, not a cached one):

```text
$ SYNAPSE_TEST_DB_DSN='postgres://synapse:synapse@127.0.0.1:5432/synapse?sslmode=disable' \
    go test ./internal/billing/... -count=1 -v
=== RUN   TestWebhookPaymentFailedSetsGracePeriod
--- PASS: TestWebhookPaymentFailedSetsGracePeriod (0.07s)
=== RUN   TestWebhookSubscriptionDeletedSuspendsTenant
--- PASS: TestWebhookSubscriptionDeletedSuspendsTenant (0.06s)
=== RUN   TestWebhookRejectsInvalidSignature
=== RUN   TestWebhookRejectsInvalidSignature/signed_with_another_endpoint's_secret
=== RUN   TestWebhookRejectsInvalidSignature/body_replaced_after_signing
=== RUN   TestWebhookRejectsInvalidSignature/no_signature_header
--- PASS: TestWebhookRejectsInvalidSignature (0.04s)
    --- PASS: TestWebhookRejectsInvalidSignature/signed_with_another_endpoint's_secret (0.00s)
    --- PASS: TestWebhookRejectsInvalidSignature/body_replaced_after_signing (0.00s)
    --- PASS: TestWebhookRejectsInvalidSignature/no_signature_header (0.00s)
=== RUN   TestWebhookPaymentSucceededClearsGracePeriod
--- PASS: TestWebhookPaymentSucceededClearsGracePeriod (0.05s)
=== RUN   TestWebhookIssuesNoSQLForARejectedDelivery
=== RUN   TestWebhookIssuesNoSQLForARejectedDelivery/signed_with_another_endpoint's_secret
=== RUN   TestWebhookIssuesNoSQLForARejectedDelivery/body_replaced_after_signing
=== RUN   TestWebhookIssuesNoSQLForARejectedDelivery/no_signature_header
=== RUN   TestWebhookIssuesNoSQLForARejectedDelivery/body_over_the_documented_ceiling
--- PASS: TestWebhookIssuesNoSQLForARejectedDelivery (0.00s)
    --- PASS: TestWebhookIssuesNoSQLForARejectedDelivery/signed_with_another_endpoint's_secret (0.00s)
    --- PASS: TestWebhookIssuesNoSQLForARejectedDelivery/body_replaced_after_signing (0.00s)
    --- PASS: TestWebhookIssuesNoSQLForARejectedDelivery/no_signature_header (0.00s)
    --- PASS: TestWebhookIssuesNoSQLForARejectedDelivery/body_over_the_documented_ceiling (0.00s)
=== RUN   TestWebhookWithoutASecretRefusesEveryDelivery
--- PASS: TestWebhookWithoutASecretRefusesEveryDelivery (0.00s)
=== RUN   TestWebhookDispatchesEachEventTypeToItsOwnStatement
=== RUN   TestWebhookDispatchesEachEventTypeToItsOwnStatement/invoice.payment_succeeded
=== RUN   TestWebhookDispatchesEachEventTypeToItsOwnStatement/invoice.payment_failed
=== RUN   TestWebhookDispatchesEachEventTypeToItsOwnStatement/customer.subscription.deleted
--- PASS: TestWebhookDispatchesEachEventTypeToItsOwnStatement (0.00s)
    --- PASS: TestWebhookDispatchesEachEventTypeToItsOwnStatement/invoice.payment_succeeded (0.00s)
    --- PASS: TestWebhookDispatchesEachEventTypeToItsOwnStatement/invoice.payment_failed (0.00s)
    --- PASS: TestWebhookDispatchesEachEventTypeToItsOwnStatement/customer.subscription.deleted (0.00s)
=== RUN   TestWebhookAcknowledgesWhatItCannotActOn
=== RUN   TestWebhookAcknowledgesWhatItCannotActOn/an_event_type_this_phase_does_not_handle
=== RUN   TestWebhookAcknowledgesWhatItCannotActOn/a_signed_event_whose_object_names_no_customer
--- PASS: TestWebhookAcknowledgesWhatItCannotActOn (0.00s)
    --- PASS: TestWebhookAcknowledgesWhatItCannotActOn/an_event_type_this_phase_does_not_handle (0.00s)
    --- PASS: TestWebhookAcknowledgesWhatItCannotActOn/a_signed_event_whose_object_names_no_customer (0.00s)
=== RUN   TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently
=== RUN   TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently/no_tenant_claims_the_customer
=== RUN   TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently/the_database_refuses_the_write
=== RUN   TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently/a_plane_with_no_database_at_all
--- PASS: TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently (0.00s)
    --- PASS: TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently/no_tenant_claims_the_customer (0.00s)
    --- PASS: TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently/the_database_refuses_the_write (0.00s)
    --- PASS: TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently/a_plane_with_no_database_at_all (0.00s)
PASS
ok  	synapse/internal/billing	0.240s
```

The same command with the variable unset — the CI shape, `go test ./...` on three operating systems with no
database — is not a silent pass: the four database-backed cases report `--- SKIP` and the five double-based ones
still run, so what CI cannot check is visible as unchecked rather than hidden behind a green line:

```text
$ go test ./internal/billing/... -v
--- SKIP: TestWebhookPaymentFailedSetsGracePeriod (0.00s)
--- SKIP: TestWebhookSubscriptionDeletedSuspendsTenant (0.00s)
--- SKIP: TestWebhookRejectsInvalidSignature (0.00s)
--- SKIP: TestWebhookPaymentSucceededClearsGracePeriod (0.00s)
--- PASS: TestWebhookIssuesNoSQLForARejectedDelivery (0.00s)
--- PASS: TestWebhookWithoutASecretRefusesEveryDelivery (0.00s)
--- PASS: TestWebhookDispatchesEachEventTypeToItsOwnStatement (0.01s)
--- PASS: TestWebhookAcknowledgesWhatItCannotActOn (0.00s)
--- PASS: TestWebhookAnswersAnUnmatchedCustomerAndADatabaseFailureDifferently (0.00s)
PASS
ok  	synapse/internal/billing	0.019s
```

The whole suite and the two packages this phase touched most:

```text
$ go test ./...
(no failures: every package reports ok or no test files)
$ go vet ./internal/plane/ ./internal/billing/ ./cmd/plane/
(no output)
$ grep stripe go.mod
	github.com/stripe/stripe-go/v76 v76.25.0
```

**The live demo**, because a passing test can still be a test that agrees with the code rather than with Stripe.
A real plane was booted on `127.0.0.1:9199` against the compose database, one tenant row was seeded with
`stripe_customer_id = 'cus_phase27_demo'`, and each delivery was signed with `openssl` — an independent
implementation of Stripe's scheme, `HMAC-SHA256(secret, "<unix-ts>.<body>")` as `t=...,v1=...`, not with
stripe-go's own helper — and posted with `curl`:

```text
state before:        suspended | 2026-09-23 09:45:01.571846+00
  invoice.payment_succeeded -> HTTP 200 {"received":true}
after succeeded:     active | NULL
  invoice.payment_failed -> HTTP 200 {"received":true}
after failed:        grace_period | 2026-09-23 09:45:31.144804+00
  customer.subscription.deleted -> HTTP 200 {"received":true}
after deleted:       suspended | 2026-09-23 09:45:31.144804+00
  invoice.payment_failed -> HTTP 400 {"error":"invalid_signature"}   (signed with whsec_attacker_secret)
after forged sig:    suspended | 2026-09-23 09:45:31.144804+00
  invoice.payment_failed -> HTTP 200 {"received":true}               (customer cus_phase27_unknown)
```

...with the plane's own log for that run, secret and signature absent from every line, and the brief's required
`payment_failed` line carrying the tenant id:

```text
9:45AM INFO plane: Synapse Control Plane v2.0.0 listening addr=127.0.0.1:9199
2026/09/23 09:45:30 INFO payment_succeeded tenant_id=1032dbd5-4091-4f80-b0ae-fd7a9ca1bba8
2026/09/23 09:45:31 WARN payment_failed tenant_id=1032dbd5-4091-4f80-b0ae-fd7a9ca1bba8
2026/09/23 09:45:31 INFO subscription_deleted tenant_id=1032dbd5-4091-4f80-b0ae-fd7a9ca1bba8
2026/09/23 09:45:31 WARN stripe webhook rejected: signature validation failed
2026/09/23 09:45:31 WARN stripe webhook matched no tenant event=payment_failed stripe_customer_id=cus_phase27_unknown
```

The plane's startup line for the same boot reported `stripe_webhook_secret=set` and nothing more, which is the
redaction doing its job on the one line that prints the whole config. Four things that transcript shows and the
tests cannot: the route is reachable on a freshly booted plane; recovery clears the timestamp (`active | NULL`)
while suspension preserves it, which is the difference between the two statements; a forged signature is refused
without touching the row it named; and an unknown customer is acknowledged with a warning instead of a retry
storm.

### Findings

1. **`status` is recorded and enforced by nothing.** A `suspended` tenant's token still authenticates on every
   route it did yesterday. The gate is small — `requireJWT` already reads the registry's `compliance_tier`, so
   the status is a second column in a query that already runs — but it is a later phase's decision, and it will
   want a status-to-policy mapping rather than an `if suspended`. Worth doing before anything bills: a webhook
   that suspends a tenant nobody suspends is a legal posture without a technical one.
2. **The plane is loopback-only, so Stripe cannot reach this route as shipped.** `PlaneConfig.Validate` refuses
   a non-loopback `listen-addr`, which is the project's hard rule and stays; the consequence is that a real
   deployment needs a reverse proxy or tunnel in front of it. That proxy is also where two more requirements
   land that nothing enforces yet: TLS termination, and a request-body limit at least as large as
   `MaxWebhookBodyBytes` (65536), or the ceiling here becomes the proxy's 413 instead of this handler's 400.
3. **Every Stripe endpoint configured for other event types gets a 200 and an INFO line.** That is the right
   answer to the retry contract (a non-2xx would make Stripe redeliver for three days and then drop the event),
   but it means an operator who wants only these three events should subscribe to only these three, or the log
   carries a line per delivery of every invoice draft. Nothing deduplicates by `event.id` either: Stripe can
   redeliver the same event after a timeout, and the second delivery only re-stamps `grace_period_started_at`.
4. **Nothing links a Stripe customer to a tenant yet.** The webhook matches on `stripe_customer_id`, and no code
   writes that column — `POST /v2/tenants` does not accept it and there is no checkout flow to set it. The live
   demo had to seed it with `psql`. Every delivery for a real account therefore logs `stripe webhook matched no
   tenant` until the provisioning or checkout phase writes the link, which is the same shape of gap Phase 26
   finding 9 recorded for `POST /v2/tenants`: a half-wired flow whose missing half is visible only from outside.
5. **The four database-backed tests skip in CI.** That is `internal/store`'s documented convention and the
   reason CI is green on three operating systems with no Postgres, and this phase added the double-based layer
   underneath (five tests that always run) so that the security property itself is not part of what skips. What
   stays CI-invisible is anything about the SQL: a wrong status literal or a broken `RETURNING id::text` would be
   caught only by a run with `SYNAPSE_TEST_DB_DSN` set. `internal/metering` took the other branch — an
   `integration` tag with a compose-DSN fallback, so its tests are never silently skipped and never run in CI
   either. The two conventions now coexist; neither is wrong, but a reader has to know which package chose which.
6. **`config.go` was four lines under the ceiling when this phase started.** It is v2 and therefore fair game,
   and the split that kept it legal (`config_redact.go`) is honest — loading configuration and reporting on it
   are different jobs — but it is worth naming that the next key added to `PlaneConfig` will have to look at that
   file's length first. The struct declaration itself cannot be split, which is what makes this a recurring tax.
7. **The signature header is logged nowhere, including on the failure paths.** The rejection line names neither
   the event id nor the reason stripe-go gave, on the argument that its error text is not a contract about what
   it quotes; an operator debugging a rejected delivery has Stripe's own delivery log for the header and can
   compare secrets by hand. That trade is deliberate and belongs in the record, because the alternative —
   logging the error text, as many integrations do — is one library release away from putting a header in a
   shared log.

### Next phase

The smallest and earliest of what this phase leaves behind: enforcement of `tenants.status` in `requireJWT`
(finding 1), the customer-id link that makes the webhook match anything (finding 4), and, if duplicate
deliveries become observable, deduplication by `event.id` (finding 3). None of the three is large; the first two
are prerequisites for a self-serve billing flow, and the reverse proxy and TLS from finding 2 are what make any
of it reachable from Stripe at all.


## Phase 28 — tenant status enforcement (402 for suspended) (complete)

`RequireActiveStatus(pool)` is live in front of every tenant surface, and `synapse_global.tenants.status` —
the column Phase 27's webhook writes — is now enforced rather than merely recorded. A `suspended` tenant is
answered `402 {"error":"payment_required","upgrade_url":"https://synapse.ai/pricing"}` before any handler
runs; a `grace_period` tenant is served normally and told how much of its window is left in
`X-Synapse-Grace-Period: {days}days`; `active` — and any value this phase has never heard of — is served with
nothing added. Three routes a billing status has no bearing on stay outside the gate: `GET /health`,
`POST /v2/tenants` (the admin provisioning surface), and `POST /v2/billing/webhook`, the last one because a 402
there would be self-defeating — the delivery that lifts a suspension is exactly the one a status-aware gate
would refuse.

Commit `feat: Phase 28 - tenant status enforcement (402 for suspended)`

New files:

- `internal/plane/status.go` — 202 lines: `RequireActiveStatus`, the 402 body, the `TenantStatus*` vocabulary,
  the grace-period arithmetic, and the comments that explain the two spellings the import cycle forces.
- `internal/plane/status_test.go` — 256 lines: the four tests this phase is defined by, plus two that pin the
  exempt routes and the unknown-status branch.
- `internal/plane/status_setup_test.go` — 190 lines: the pool, the provisioned tenants, `setTenantStatus`, the
  router, and the request helper — split out at the 300-line ceiling, the arrangement
  `compliance_setup_test.go` already makes.

Changed:

- `internal/plane/handlers.go` — 254 → 292 lines: the eleventh `NewServer` parameter plus its `Server` field,
  and the tenant surfaces moved into one `router.Group` whose only middleware is
  `s.requireJWT, RequireActiveStatus(s.statusPool)`.
- `internal/billing/stripe.go` — the three `Status*` constants became aliases of `plane.TenantStatus*` (three
  lines and a comment), so the package that writes a status and the package that enforces it cannot drift over
  a spelling. Phase 27's own tests and callers are unchanged by construction.
- `cmd/plane/main.go` — the pool passed as the new parameter, beside `billing.WebhookHandler(cfg, pool)`.
- Thirteen test call sites in `internal/plane/*_test.go` — one trailing `nil` each, the arrangement Phase 27's
  tenth parameter already established. The two compliance integration setups pass the **real** pool instead, so
  Phases 19-21's tests now run through the gate exactly as a deployment does — which is how this phase finds out
  that `active` really is transparent.
- `bin/synapse` — **not** staged, and left dirty by an earlier demo build, as in Phases 22-27.

### The gate has to be inside `requireJWT`, so the tenant routes became a group

The middleware reads its tenant from `plane.TenantIDFromCtx`, which only `internal/tenant`'s JWT middleware
fills — and chi runs `With(a, b)`/`Use(a, b)` in order, `a` outermost. The gate therefore has to be the second
middleware, never the first, and the route table says so in one place:

```go
router.Group(func(r chi.Router) {
	r.Use(s.requireJWT, RequireActiveStatus(s.statusPool))

	r.Post(syncRoute, s.handleSyncMemories)
	r.Get(searchRoute, s.handleSearchMemories)
	r.Get(complianceAuditRoute, s.handleComplianceAudit)
	r.Get(complianceChainIntegrityRoute, s.handleVerifyLedger)
	r.Get(complianceReportRoute, s.handleComplianceReport)
	r.Get(ledgerVerifyRoute, s.handleVerifyLedger)
})
```

A `Group` rather than six `With(s.requireJWT, gate)` spellings because the claim being made is about *all*
tenant surfaces: an endpoint added inside this group inherits the gate without its author having to remember
it. The three exempt routes are registered outside the group for the same reason — the exemption list is the
route table, and there is no path string anywhere in the middleware for a future route to match by accident.
The order is not left to trust either: with the gate outermost, a suspended tenant's request would carry no
verified tenant id, and the first test below would see 401 instead of 402.

### A nil pool installs no gate, and the pool arrives as a constructor parameter

`NewServer` grew an eleventh parameter (`statusPool *pgxpool.Pool`) rather than reaching for the
`Database{Ping}` handle it already holds: the gate needs `QueryRow`, the health probe needs `Ping`, and the one
thing they share is that `cmd/plane` holds the pool both are built from. This is the same shape as Phase 27's
tenth parameter, and it cost the same thirteen one-token test edits.

A nil pool means the gate is not installed at all (`RequireActiveStatus` returns `next`), which is a
pass-through and therefore a fail-open — so it is worth being precise about why that cannot happen in
production. `cmd/plane` fatals before `ListenAndServe` when `SYNAPSE_DB_DSN` is empty, and pings and migrates
the database before it serves anything, so the only constructors that can pass nil are the plane's own unit
tests, which inject fake provisioners and fake searchers and would have no status to read anyway. The
alternative design — mounting the gate in `cmd/plane` around `srv.Routes()` and matching the three exempt paths
by string — was rejected precisely because it hides the exemption list in the one place the route table is not,
and because it would leave the gate untestable through the real router.

### The status vocabulary moved into `internal/plane`, and `internal/billing` aliases it

Phase 27 exported `StatusActive` / `StatusGracePeriod` / `StatusSuspended` with a comment saying whoever
enforces them should not be spelling those strings a second time. Enforcing them from here was impossible as
written: `internal/billing` reads the schema name from `internal/tenant`, which imports `internal/plane` for
`PlaneConfig`, so `plane -> billing -> tenant -> plane`. The strings therefore live in `status.go` as
`TenantStatusActive` / `TenantStatusGracePeriod` / `TenantStatusSuspended`, and `internal/billing`'s exported
names became aliases of them (that package already imports `plane`, one direction only). Phase 27's comment is
now true rather than aspirational, and nothing in Phase 27's tests moved. The same cycle forces `synapse_global`
to be written as a literal in `status.go`, with a comment naming `tenant.SchemaName` as its single owner.

### Failure paths: a status that cannot be read is never a status that passes

Three refusals sit beside the two decisions, and each is a deliberate choice:

- No verified tenant id — a request that never passed through `requireJWT` — answers `401 unauthorized`,
  matching what every handler in the package already does for the same impossible case rather than treating an
  empty id as a tenant that owns nothing.
- `pgx.ErrNoRows` — a signature-valid token naming a tenant the registry no longer holds — answers `401`, not
  `200`. The row is the source of the status, so a missing row has no status, and the fail-closed reading is
  the one `requireJWT` takes for a token with no `tenant_id`.
- Any other query error answers `500 {"error":"internal"}`. This is the rule that must not bend: a database
  that cannot answer must not let a suspended tenant through, and the pgx error — which can quote the
  connection target — is never reflected to the client. The read is bounded by a two-second context, so a hung
  lookup becomes a fast 500 rather than a hung request.

The grace header has two edge cases, both documented in `remainingGraceDays`: a `NULL` start time on a
`grace_period` row reports the full seven days (the only way that state exists is a row written before the
column did, and "0 days left" would cut access off on the strength of a missing value), and a window that has
run out reports `0days` rather than a negative count. The window itself is `gracePeriodDays = 7` in `plane`
rather than in `billing`, because `billing` only stamps the start time; the number is a property of the
enforcement decision.

### Evidence

The definition of done, verbatim, against the compose database:

```text
$ go test ./internal/plane/... -run TestTenantStatus -v -tags integration
=== RUN   TestTenantStatusSuspendedTenantIsRefusedWith402
--- PASS: TestTenantStatusSuspendedTenantIsRefusedWith402 (0.16s)
=== RUN   TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader
=== RUN   TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader/just_entered_the_grace_period
=== RUN   TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader/two_days_into_it
--- PASS: TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader (0.30s)
    --- PASS: TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader/just_entered_the_grace_period (0.14s)
    --- PASS: TestTenantStatusGracePeriodTenantIsServedWithTheWarningHeader/two_days_into_it (0.16s)
=== RUN   TestTenantStatusActiveTenantIsServedWithoutTheWarning
--- PASS: TestTenantStatusActiveTenantIsServedWithoutTheWarning (0.17s)
=== RUN   TestTenantStatusSuspendedTenantCanStillReadHealth
--- PASS: TestTenantStatusSuspendedTenantCanStillReadHealth (0.15s)
=== RUN   TestTenantStatusRoutesWithoutATenantStatusStayOpen
--- PASS: TestTenantStatusRoutesWithoutATenantStatusStayOpen (0.36s)
=== RUN   TestTenantStatusUnknownStatusIsServed
--- PASS: TestTenantStatusUnknownStatusIsServed (0.18s)
PASS
ok  	synapse/internal/plane	1.322s
```

The regressions the change could have caused, each re-run after it: the untagged plane suite
(`ok synapse/internal/plane 0.157s`), Phase 27's billing suite against the same database
(`SYNAPSE_TEST_DB_DSN=… go test ./internal/billing/... -count=1` → `ok synapse/internal/billing 1.579s`, which
is the alias change and the vocabulary staying put), Phases 19-21's two compliance integration tests through
the now-gated router (`-run 'TestComplianceAudit|TestComplianceReport' -tags integration` →
`ok synapse/internal/plane 15.365s`), and `go vet -tags integration ./...` across the module, which is what
proves all sixteen `NewServer` call sites compile at the new arity.

**The live demo**, because the tests exercise the middleware through a router this repo builds and a
deployment builds its own. A real plane was booted on `127.0.0.1:9198` against the compose database (env-only
config, so no file on disk), a tenant was provisioned through `POST /v2/tenants` — its JWT minted by the real
issuer, the same token a customer would hold — and its status was moved with `psql` while the plane kept
serving:

```text
tenant phase28demo10498 (03db74d7-d1bf-417e-97f9-94d5b0bac675)
  db: active | NULL
  active (default)       -> HTTP 200  header=none       {"memories":[]}
  db: suspended | NULL
  suspended              -> HTTP 402  header=none       {"error":"payment_required","upgrade_url":"https://synapse.ai/pricing"}
  db: grace_period | 2026-09-21 10:00:00.514293+00
  grace_period (2d ago)  -> HTTP 200  header=5days      {"memories":[]}
  grace_period (fresh)   -> HTTP 200  header=7days      {"memories":[]}
  health (suspended tok) -> HTTP 200  {"status":"ok","version":"2.0.0","db":"connected"}
  stripe webhook         -> HTTP 503  {"error":"billing_unavailable"}
```

Six things that transcript shows and a unit test cannot: a freshly booted plane reads the column it was
written to read; the 402 body is byte-for-byte the published shape; suspension really is immediate, on the next
request rather than on a restart; the header counts a real interval down (a stamp two days old reads `5days`;
the two-day case is 5 and not 6, so the arithmetic floors rather than rounds); a suspended tenant's `/health`
still answers, because an orchestrator must not read a suspension as an outage; and the webhook's own route is
not behind the gate (`503 billing_unavailable` is this plane having no
`STRIPE_WEBHOOK_SECRET`, which is the point — anything but 402). The plane's startup line for the same boot
reported `database_dsn=set jwt_secret=set admin_token=set master_key=set stripe_webhook_secret=unset` and
nothing else, which is the redaction doing its job on the one line that prints the whole config.

### Findings

1. **The gate is enforced, but nothing in production can trip it yet.** Phase 27's finding 4 still stands: no
   code writes `stripe_customer_id`, so no real Stripe delivery matches a tenant and no real tenant is ever
   suspended. This phase closed the enforcement half of that gap — the half a customer would experience — and
   the linking half (checkout, or a provisioning field) is what makes the whole path reachable. It is the
   smallest next thing and the only one that changes behaviour for a real account rather than for a test.
2. **The middleware cannot log, by construction.** Its signature is `pool` and nothing else, so the plane's
   structured logger never reaches it: a status read that fails is a silent 500 server-side while the client
   sees `{"error":"internal"}`, where every handler in this package logs its own failures. Threading the logger
   through would cost the one-argument shape the brief specified, so it was left alone — but an operator with a
   500-only symptom has nothing to grep for, which is a real operational gap rather than a style note.
3. **Suspension is per tenant, not per credential.** There is no way to disable one leaked agent key without
   disabling the whole tenant: `status` lives on the tenant row, and the JWT's `agent_id` never reaches the
   gate. A key-revocation path is a different mechanism (a `revoked_at` on the key, or a jti denylist) and
   nothing in this phase precludes it, but "suspend" today has one granularity and it is the billing one.
4. **Only the control plane is gated; the MCP server is not.** Phases 22-25 put `synapse_compile`,
   `synapse_search_memories`, and `synapse_write_memory` in the edge binary, working against a local
   sqlite-vec store, and nothing in that path asks the plane for a status. A suspended tenant's edge node
   therefore keeps compiling and searching locally, and its writes queue for the next successful sync. That is
   defensible — an edge is supposed to survive the network, and the plane refuses the sync that would persist
   anything — but it should be a decision on the record: suspension today means "the control plane refuses",
   not "the product stops working".
5. **The status is read on every request, uncached, with a two-second bound.** Correctness first: the read is
   one indexed primary-key lookup, so the cost is a round trip rather than a scan, and "suspended" takes effect
   on the next request instead of on the next restart. A cache is where a revocation delay would first appear,
   which is why there is not one yet — worth naming because per-request status reads are exactly the kind of
   thing that gets cached later for the wrong reason.
6. **The grace window is split across two packages.** `internal/billing` writes `grace_period_started_at`;
   `internal/plane` decides that the stamp buys seven days. Both halves are honest — one stamps the fact, the
   other owns the policy — but nothing but a comment connects them, and the test pins `7days` against this
   package's own constant, so a change of intent in either file would be a release-time discovery. A
   `grace-period-days` config key is the follow-up, and it is small.
7. **The 402's `upgrade_url` is a constant baked into a release.** The pricing page moving, or a per-plan
   checkout URL appearing, means a new binary. That is acceptable while billing is half-wired (finding 1) and
   user-facing the moment it is not: the body is the one response in this package a human reads.
8. **The tests provision tenants and never delete them**, which is internal/billing's convention too. Each run
   adds six or seven rows to `synapse_global.tenants` and a schema each; harmless for correctness — every test
   is scoped to its own slug — and eventually noisy for a database shared with a development plane. The
   registry is, in effect, accumulating a log of every test run since Phase 4.

### Next phase

The customer-id link is first, because it is what turns this phase's enforcement from a tested property into
one a real account can experience (finding 1) — and Phase 27's finding 4 named it too, so two phases now point
at the same gap. After it, in rough order of value: a status-to-policy mapping rather than the `if status ==
suspended` this phase ships, so a fourth status (a paused plan, a read-only state) is a table entry rather than
a code change; per-key revocation, which is the granularity suspension cannot express (finding 3); a decision
about whether an edge node should learn its tenant's status and what it should do about it (finding 4); and the
grace window as configuration (finding 6). Nothing in this phase blocks any of them, and the route group means
a new tenant surface inherits the gate without being asked.





## Phase 29 — multi-agent integration test (complete)

### What this phase is

One scenario, two agents, one shared brain, and no mocks between them. `internal/integration/multiagent_test.go`
starts two real edge nodes -- each with its own SQLite store, its own HTTP surface, its own MCP server on a
loopback port, and its own background sync client -- against one real control plane (`internal/plane`'s own
router) listening on `127.0.0.1:9090` over the Phase 4 compose database. agent_a records a decision, agent_b
compiles a fresh session and sees it as *another agent's* memory, agent_b contradicts it, and the contradiction
is followed all the way out: onto the plane's own rows, into the next Memory Trace with a demoted score, into
the tenant's HMAC chain, and out of the two compliance endpoints an enterprise tenant reads.

The edge assembly is cmd/synapse's, constructor for constructor -- `store.NewStore` + `sync.NewSyncer` +
`RunBackground` + `api.NewAPIServer` + `proxy.NewProxy` + `mcp.NewServer`, with `SetPlaneCandidates` on all
three -- and the plane's is cmd/plane's, including the contradiction detector installed on the tenant
`MemoryWriter`, which is the component that makes a cross-agent contradiction detectable at all. The only mock
is the model upstream (`multiagent_upstream_test.go`: an OpenAI-shaped echo server that records every header it
received); the only substitute is the embedder, because an ONNX session cannot be loaded per test, so it is a
deterministic 384-axis unit-vector embedder that pgvector stores, indexes, and compares exactly as it compares
real vectors.

### The one production change this phase needed

**Nothing in the write path ever set `sync_status = 'sync_pending'`.** `store.Write` normalizes a blank status to
`local_only`, every writer leaves it blank, and `PendingSync` matches only `sync_pending` -- so an edge node's
background flusher was draining a permanently empty queue and *no edge node has ever pushed a memory to a
control plane*. Phases 8, 9, 10, and 25 each recorded that as a finding (Phase 25's finding 3 ends: "a one-line
change at the write site and a policy question rather than a technical one"); this phase's step (a) is the
assertion that cannot pass until it is fixed.

The fix is `internal/sync/pending.go`: `sync.PendingWriter`, a `store.Backend` decorator that stamps a blank
`SyncStatus` as `sync_pending` on the way in and delegates `Search`, `GetRecent`, and `MarkSuperseded`
unchanged. `cmd/synapse/main.go` installs it on the two paths that take an interface -- the **proxy** and the
**MCP server** -- while the flusher keeps the concrete `*store.Store` it needs for `PendingSync`/`MarkSynced`.
`internal/api` keeps the concrete store, because `NewAPIServer` takes one (v1, frozen), which is why a memory
`/v1/compile` writes for itself is still `local_only`: recorded as a finding below rather than papered over. No
v1 internal was touched.

### Definition of done, verbatim

```text
$ go test ./internal/integration/... -run TestMultiAgent -v -tags integration
=== RUN   TestMultiAgent
    multiagent_test.go:70: STEP 0: provisioned slug=ma-660dbf1c1a3 tenant_id=3d7e345a-fa2f-4c74-ad0b-10c90a705bd4 jwt=407 chars api_key=64 chars (never printed)
    multiagent_test.go:76: STEP 0: control plane answering at http://127.0.0.1:9090 over the compose database
    multiagent_test.go:94: STEP 0: agent_a and agent_b are up, each with its own store and flusher
    multiagent_test.go:108: STEP a: five proxied turns through edge_a, then the plane's own table
    multiagent_test.go:119: STEP a: proxied turn 1/5 through agent_a
    multiagent_test.go:119: STEP a: proxied turn 2/5 through agent_a
    multiagent_test.go:119: STEP a: proxied turn 3/5 through agent_a
    multiagent_test.go:119: STEP a: proxied turn 4/5 through agent_a
    multiagent_test.go:119: STEP a: proxied turn 5/5 through agent_a
    multiagent_test.go:122: STEP a: agent_a has 10 memories queued for the plane
    multiagent_test.go:134: STEP a: the plane holds 7 memories from agent_a
    multiagent_test.go:143: STEP b: edge_a writes the decision through synapse_write_memory (decision, org)
    multiagent_test.go:150: STEP b: stored memory 20ab8f79-dd6a-4e19-9bfd-ec96e63bfafd (sanitized=false conflict_detected=false)
    multiagent_test.go:165: STEP b: the plane row is agent_id=agent_a visibility=org conflict_status=none
    multiagent_test.go:174: STEP c: edge_b compiles a fresh session and reads the Memory Trace
    multiagent_test.go:189: STEP c: trace req-1790159097954744304 entry id=20ab8f79-dd6a-4e19-9bfd-ec96e63bfafd agent_id=agent_a cross_agent=true conflict_status=none total=0.5000 included=true
    multiagent_test.go:205: STEP d: edge_b writes the contradicting MySQL decision
    multiagent_test.go:214: STEP d: stored memory f81e67d9-17f5-4363-bfd4-11a2789bf25c (this node's own conflict_detected=false); the cross-agent verdict is the plane's
    multiagent_test.go:240: STEP d: the plane recorded it -- mysql f81e67d9-17f5-4363-bfd4-11a2789bf25c status=conflict with=20ab8f79-dd6a-4e19-9bfd-ec96e63bfafd; postgres 20ab8f79-dd6a-4e19-9bfd-ec96e63bfafd status=superseded_candidate with=f81e67d9-17f5-4363-bfd4-11a2789bf25c
    multiagent_test.go:257: STEP e: a second fresh session through edge_b, after the conflict
    multiagent_test.go:274: STEP e: superseded candidate 20ab8f79-dd6a-4e19-9bfd-ec96e63bfafd conflict=superseded_candidate with=f81e67d9-17f5-4363-bfd4-11a2789bf25c total=0.2500 (S=0.000 R=1.000 I=1.000 T=0.500) included=true
    multiagent_test.go:277: STEP e: contradictory memory f81e67d9-17f5-4363-bfd4-11a2789bf25c conflict=conflict with=20ab8f79-dd6a-4e19-9bfd-ec96e63bfafd total=0.5000 (S=0.000 R=1.000 I=1.000 T=0.500) included=true
    multiagent_test.go:281: STEP f: header names and values the model upstream actually received
    multiagent_test.go:282: STEP f: 5 upstream requests, header names on the first: [Accept-Encoding Content-Length Content-Type User-Agent X-Forwarded-For] -- no Authorization, no x-api-key, no credential
    multiagent_test.go:284: STEP g: synapse_global.ledger, counted for this tenant
    multiagent_test.go:285: STEP g: 7 compilations performed, 7 ledger entries
    multiagent_test.go:287: STEP h: chain_valid=true entries_checked=7 first_break_id=""
    multiagent_test.go:287: STEP i: audit returned 7 entries (total=7 limit=50 offset=0); newest trace=req-1790159098984063979 memories=12
    multiagent_test.go:289: PASS: two agents, one shared brain -- agent_a's decision reached agent_b's trace (cross_agent), agent_b's contradiction was recorded by the plane, and the enterprise tenant's chain holds 7 signed entries
--- PASS: TestMultiAgent (4.12s)
PASS
ok  	synapse/internal/integration	4.151s
```

The line numbers in that transcript are the finished file's, and the identifiers change every run (each run
provisions its own tenant, by design). The command was run four times back to back while the files above were
being finished; the output pasted is the last of them.

### The nine assertion groups, and where each one is pinned

| Group | What it asserts | Where the fact lives |
| --- | --- | --- |
| a | five proxied turns through agent_a's node reach the plane | `SELECT count(*) FROM tenant_<slug>.memories WHERE agent_id='agent_a'` — the tenant's own table, read directly |
| b | agent_a's decision is stored and pushed with `agent_id=agent_a`, `visibility=org`, `conflict_status=none` | `synapse_write_memory`'s reply (uuid, `sanitized`, `conflict_detected`) plus that row |
| c | a fresh session compiled on agent_b's node contains agent_a's memory with `cross_agent=true` | the `Memory Trace` in `POST /v1/compile`'s response — the full manifest, so the provenance fields are visible |
| d | the contradiction is detected: MySQL row `conflict` naming the Postgres row, Postgres row `superseded_candidate` naming MySQL | the plane's two rows, both directions, because the verdict is the plane's (see finding 2) |
| e | both versions survive in the next trace, the contradicted one carries the marker, and the marker shows as a lower `score_total` | the trace's per-memory `conflict_status`, `conflict_with_id`, and four factor scores |
| f | no `Authorization`, no `x-api-key`, and no Synapse credential reached the model upstream or any edge log line | every header the mock upstream received (names and values), plus the captured `slog` output of both nodes |
| g | the enterprise tenant's ledger holds at least one entry per compilation | `SELECT count(*) FROM synapse_global.ledger WHERE tenant_id=…`, polled (the append is asynchronous) |
| h | `GET /v2/compliance/chain-integrity` answers `chain_valid=true` | the walk, which re-derives every HMAC from the tenant's stored secret |
| i | `GET /v2/compliance/audit` returns the entries, each carrying the trace it signed | the endpoint's `total` and its page, with the newest entry parsed back into a `TraceManifest` |

Every assertion is `require`, so the first failure aborts the run — the brief's "fail fast", and the only reading
of "use testify/assert" that is compatible with it. Each group logs a `STEP <letter>` line first, and the log
lines carry counts, ids, statuses, and scores but never the JWT, the API key, or the master key (only their
lengths, once).

### Files

**New:** `internal/sync/pending.go` (the decorator) and `internal/sync/pending_test.go` (its cases: blank becomes
pending, a stated status is preserved, a nil backend is an error, and delegation for the other three methods);
plus, under `internal/integration/`, `multiagent_test.go` (the scenario), `multiagent_setup_test.go` (database,
tenant, plane router), `multiagent_edge_test.go` (the two nodes and the MCP client),
`multiagent_tools_test.go` (the client verbs), `multiagent_probe_test.go` (the waits and direct reads),
`multiagent_ledger_test.go` (the two adapters that mirror package main), `multiagent_upstream_test.go` (the
recording echo server), and `multiagent_steps_test.go` (steps f–i as functions).

**Edited:** `cmd/synapse/main.go` (`writeBackend`: the decorator on the proxy and MCP paths when
`control-plane-url` is set) and `PROGRESS.md`.

### Findings, limitations, and what this phase did not do

1. **The fix this phase needed is a product policy that was made here.** `sync.PendingWriter` decides that a
   memory written on a node *with a control plane configured* is promised to that control plane. That is the only
   reading that makes an edge node's sync mean anything, but it is a decision that had been left open for
   twenty-one phases and is now a line of code rather than a question. A `sync-enabled` key, or a per-write flag,
   is open and small.
2. **A node's own `conflict_detected` cannot see another node's memory.** `synapse_write_memory` compares the
   memory it is about to store against the candidates *its own store* can read (`write_conflict.go`); the memory
   agent_a wrote is on the plane, so agent_b's reply is `false`, and this test asserts that rather than the
   brief's "conflict_detected in the response". The conflict *is* detected — by the plane, on the push, which is
   where cross-agent candidates can meet — and step (d) pins both marked rows. Making the edge's reply capable of
   the cross-agent verdict means letting that tool ask the plane for candidates (Phase 25's finding 2, still
   open).
3. **The brief's direction for the superseded candidate is inverted, and the test follows the code.** It expects
   the *second* memory (MySQL) to be the `superseded_candidate` and to score lower. `pgconflict.go` marks the
   older row as the candidate and the newer one as `conflict`, and the scorer penalizes only the candidate, so
   the Postgres memory — written first, by agent_a — is the one demoted (0.2500 against 0.5000 in the transcript).
   The test asserts that, with the discrepancy written down at the step and here. Asserting the brief's direction
   would mean changing `internal/store` or `internal/conflict`, both v1 internals.
4. **`POST /v2/tenants` can only mint a `team` compliance tier.** The provisioning handler hardcodes
   `defaultComplianceTier` (`plane/tenants.go`), so a tenant created through the public API — however its plan is
   set — gets 403 from all three compliance surfaces. This test therefore provisions through
   `tenant.NewProvisioner` with `ComplianceTier: "enterprise"`, which is what internal/plane's own compliance
   tests do, and the gap is real: "enterprise" compliance is currently unreachable for a customer who signed up
   through the API.
5. **`control-plane-api-key` must be the tenant JWT, as the config comment says.** The brief passes `<api_key>`
   for the edges; the plane verifies a signed token on `/v2/sync/memories` and `/v2/memories/search`, and the API
   key is only a bcrypt row, so the edges present `result.JWT`. The API key is still provisioned, and it is one of
   the two secrets step (f) proves never leaves the machine.
6. **A memory `/v1/compile` writes for itself is still `local_only`.** `api.NewAPIServer` takes the concrete
   `*store.Store` (a v1 signature), so the decorator cannot be installed there; the proxy's and the MCP server's
   writes do sync. Consequence in a deployment: a node whose only traffic is the compile playground never pushes
   anything. Fixing it is an interface change in a frozen v1 package.
7. **The plane binds a fixed port, so two concurrent runs of this package collide.** `127.0.0.1:9090` is what the
   brief names and what a deployment uses; `SYNAPSE_TEST_PLANE_ADDR` moves it. The failure is a clear message
   rather than a mysterious hang, and it is what a parallel `go test` invocation of the same package produces.
8. **`store.NewStoreFromConfig` — the tenant-Postgres edge backend — still has no callers.** It returns
   `store.Backend` and `api.NewAPIServer` wants a `*store.Store`, so the factory's Postgres branch cannot be
   wired into the binary as it stands. Nothing here depends on it and nothing about it changed; it is named
   because a reader of `factory.go` would reasonably conclude an edge can be pointed at a tenant schema.
9. **The test provisions a tenant per run and never deletes it** — internal/plane's and internal/billing's
   convention, now for a fifth package. Each run adds a row to `synapse_global.tenants`, a schema, and seven
   ledger rows.

### Also verified after the change

`go vet -tags integration ./...` clean; the untagged suite (`go test ./... -count=1`) all `ok`, including
`internal/sync` (the decorator's own package) and `internal/proxy`; the tagged suites that share this database —
`internal/plane` (the compliance, ledger, and status gates), `internal/store` (conflict detection and isolation),
`internal/ledger`, and `internal/metering` — all `ok`; and the DoD command four times in a row. The two
flakes found while finishing the test are worth naming because both were real ordering facts rather than test
noise: a flusher that has pushed one batch has not pushed the rest (so step (a) waits for the fifth memory, not
the first), and the plane labels the older row of a contradiction in a second statement after inserting the newer
one (so step (d) waits for both halves of the verdict).

### Next phase

Finding 4 is the next product-shaped gap: the provisioning endpoint cannot mint the tier that three endpoints it
advertises are gated on, which makes Phase 21's gate untestable through the public API and unreachable for a real
customer. After it, in rough order of value: letting the write tool ask the plane for candidates (finding 2),
which is what would make a cross-agent conflict visible in the reply a caller reads; giving the sync decorator a
config key rather than an unconditional decision (finding 1); and the interface change that would let the compile
path queue its own writes (finding 6). Nothing in this phase blocks any of them.




## Phase 30 — benchmark: established, cold-start, Global Brain (complete)

### What this phase is

The benchmark measured one scenario and printed one number with one warning. This phase makes it measure three,
and — the part that matters — makes it print the framing each number needs, because two of the three are easy to
misread. Measured, real output, on this machine:

| scenario | Raw | Compiled | Reduction |
|---|---|---|---|
| v1 established (`session_merged.json`, target ≥40%) | 5569 | 2999 | **46.1%** |
| v1 cold-start (`session_code.json`, empty store, expected ~15%) | 3263 | 2757 | **15.5%** |
| v2 Global Brain, `distilled` pool (target ≥55%) | 5569 | 381 | **93.2%** (+47.0% vs established) |
| v2 Global Brain, `full` pool | 5569 | 2999 | **46.1%** (+0.0%) |

### The file split

`cmd/benchmark/main.go` was 297 lines — three under the 300-line ceiling — so three scenarios could not be added
to it. It is now four files: `main.go` (flags, scenario orchestration, the two v1 summary blocks), `session.go`
(fixtures, token counting, the pure helpers: chunking, write-back selection, the reduction formula), `pipeline.go`
(the classify → score → dedup → budget chain, extracted from the old single-scenario body so the three scenarios
cannot drift apart), and `globalbrain.go` (scenario 3 and its framing). Two test files add sixteen unit tests over
the pure parts: chunk coverage/ordering/clamping, write-back selection and its fallback, the reduction formula, and
every way the Global Brain flags can be wrong — including the assertion that a rejected credential never appears in
the error text.

### The budget bound, which is the finding this phase is really about

A compile can never print more than the token budget: `budget.Fill` packs into `TokenBudget − systemTokens` and the
compiled figure is that plus the pinned prompt, so with the default budget of 3000 the ceiling is 3000 tokens.
Reduction is therefore `1 − M/raw` with `M ≤ 3000`, and for the 5569-token established fixture the best number
arithmetically available is **46.1%**. Scenario 3's ≥55% target is reachable only when the candidate pool is
*smaller* than the budget. With `--global-brain=full` (26 pushed memories, top-k 50) the compile saturates the
budget and lands on exactly the v1 established figure, an uplift of `+0.0%`, and the mandated warning — which is
why the default is `--global-brain=distilled` (one memory per session: the last-user-message write-back
`synapse_compile` itself performs), and why the framing line printed with a full-pool run says *"the pool is larger
than the budget, so the compile fills the budget — reduction is budget-bound, not sync-bound"* instead of leaving
an operator to conclude that sync is broken.

### What the Global Brain number is not

`Raw` is `agent_b`'s own session and `Compiled` is the context it builds from the shared brain, so the two sides
are not the same text: 93.2% is cross-agent recall measured with scenarios 1 and 2's arithmetic, not compression of
one conversation. Reading `+47.0%` as "the Global Brain compresses 47 points better than v1" would be wrong, and
the output says so on purpose — that sentence is printed by the tool, not only written here.

### Verified against a real plane

The plane was built from `cmd/plane`, started on `127.0.0.1:9090` against the compose database with
`SYNAPSE_DB_DSN`/`SYNAPSE_JWT_SECRET`/`SYNAPSE_ADMIN_TOKEN`/`SYNAPSE_MASTER_KEY` in the environment, and two
tenants were provisioned through `POST /v2/tenants` (one per push policy, so neither pool was polluted by the
other). The DoD command then ran with `--plane http://127.0.0.1:9090 --api-key <jwt>`. What that run proves end to
end, rather than by argument: five batches accepted by `POST /v2/sync/memories`; the memories readable by a
*different* agent id through `GET /v2/memories/search`; the compile scoring rows that live in the tenant's
PostgreSQL schema rather than in the benchmark process's memory; and the whole thing failing loudly, with no
credential in the message, when the plane is not there (`Scenario 3 failed: ... dial tcp 127.0.0.1:59599: connect:
connection refused`).

Scenarios 1 and 2 print byte-identical numbers to the pre-refactor run (46.1% and 15.5%), which is what makes the
extraction into `pipeline.go` a refactor rather than a rewrite. `gofmt -l cmd/benchmark` clean, `go vet` clean,
`go build ./...` clean, and the untagged suite (`go test ./... -count=1`) all `ok`, with `cmd/benchmark` itself now
carrying tests where it previously had `[no test files]`.

### Findings

1. **The README's headline says 46.4%; the tool prints 46.1%** (Raw 5569, Compiled 2999). The published number is
   the owner's call and was deliberately not changed here, but the reproduced number is now printed by the tool
   with its own arithmetic rather than inferred from a README, and the two differ by 0.3 points.
2. **Scenario 3's ≥55% target is unreachable for a full-fidelity pool** (the budget bound above). The warning the
   brief asks for therefore fires by construction in that mode; `distilled` is the default precisely so the
   headline scenario and the target are about the same thing, and `full` is kept as the honest stress test.
3. **Scenario 2's 15.5% is what tripped the old 40% warning**, which is why the cold start needs its own framing
   rather than a shared one: the 40% WARNING now belongs to scenario 1 alone and names it.
4. **Scenarios 1 and 2 still build their candidates from the fixture's own messages**, not from the store: the
   `:memory:` store is opened and closed unused, as it was in v1. That stand-in is unchanged, but the framing line
   now says it out loud instead of implying a store was searched.
5. **`--api-key` must be the tenant JWT.** Provisioning's `api_key` is a bcrypt row, not a credential the sync and
   search routes accept — the same finding Phase 29 recorded, repeated here because the benchmark's flag name is
   the older, ambiguous one.
6. **Pushing only works because the benchmark calls `sync.Syncer.Push` itself.** The compile path's own write
   (`api.NewAPIServer`, a frozen v1 signature) still leaves a memory `local_only` — Phase 29's finding 6, untouched
   here and still true.
7. **Every scenario-3 run leaves a tenant row, a schema, and its memories behind** — the Phase 29 finding 9
   pattern, now with a second entry point. Two runs against one tenant also mix pools (the distilled write-backs
   and the full memories have different ids), which is why the full-pool number above was measured against a fresh
   tenant.

### Next phase

The most valuable follow-up is a way to scope a benchmark run to its own tenant and reset it, so the numbers stay
reproducible and the development plane stops accumulating benchmark rows (finding 7). After it: making scenarios 1
and 2 write their candidates into the store and search it, which removes the stand-in in finding 4 and would make
the cold start real rather than simulated; a third push policy between distilled and full — the memories the
session's own compile selected, bounded by the budget — which is the pool shape a real agent's write-back produces;
and reconciling the README's quoted number with what the tool prints (finding 1).


---

## Phase 31 — README v2 and COMPLIANCE.md (complete)

### What this phase is

Two documents, and the release's account of itself. `README.md` grew by 187 lines into the v2 story: the
Global Brain setup walkthrough (start the plane, point a node at it, provision a tenant), the MCP setup
section including the OpenMemory MCP comparison that names the score breakdown as the difference, the signed
ledger and the three compliance surfaces, the Homebrew quickstart with its `brew trust` step, and a Known
limitations section that says out loud what is not finished. `COMPLIANCE.md` is new — 241 lines, written for
the compliance officer rather than the developer: what the ledger records, what chain integrity does and does
not prove, the one-turn-late supersession property, retention recommendations, and the Article 50 statement
verbatim.

### Note on this entry

Written in Phase 32. The Phase 31 build session closed without a PROGRESS.md entry, which this project's phase
discipline asks for ("Update PROGRESS.md after each phase before closing"). Nothing here is recalled from that
session: it is what commit `ad95c0d` and the two files themselves show, recorded so the log has no hole
between Phase 30 and Phase 32.

### Next phase

Phase 32: the final QA checklist before the v2.0.0 tag.


---

## Phase 32 — final QA checklist (complete)

### What this phase is

The checklist that gates `v2.0.0`: twenty-three items covering both platforms' builds and suites, the compose
stack's start-up cost, cross-tenant isolation, the offline fallback, multi-agent provenance, conflict
detection and its demotion, the ledger's chain integrity at a hundred entries and under tampering, the
ledger's append-only grants at the database, the compliance PDF, the compliance tier gate, the MCP tools'
result shapes, the header-redaction guarantee, both benchmark targets, the suspended-tenant and webhook
paths, and the four documentation disclosures. Every item was run and its real output recorded; the two that
needed code are the first two findings below.

### The one code defect the checklist found

**CI was red, and had been since Phase 20.** The last two runs on `main` failed, and the six commits after
them — Phases 26 through 31 — had never been pushed, so CI had never compiled them at all. The failing job
was `test (windows-latest)`, and the cause was not the plane: four tests in
`internal/plane/compliance_report_pdf_test.go` fake an HTML-to-PDF renderer by writing an extensionless
`#!/bin/sh` script into a temp directory and pointing `PATH` at it. Windows' `exec.LookPath` requires a
`PATHEXT` extension, so the stub was never found and the endpoint answered `503 pdf_tool_unavailable` — for a
reason that was a fact about the fixture rather than about the endpoint. The four renderer-dependent tests now
call `skipRendererStubOnWindows`, whose skip message names the mechanism; the 503 case and the three template
cases — everything on that endpoint that needs no fake executable — still run on Windows. The render path
itself stays covered by the ubuntu and macos legs, which are the deployments this endpoint targets.

`go test ./...` on Linux amd64 and the Windows cross-compile of the affected package both pass locally, and
the ubuntu/macos/windows matrix was re-run on the push that carries this phase.

### The checklist, item by item

Every number below is from a run on this machine (Linux amd64, Go 1.26.2) unless the row says CI.

| # | Item | Result |
|---|---|---|
| 1 | `go build ./...` clean, both platforms | clean locally; Windows cross-vet clean; CI re-run on the phase push |
| 2 | `go test ./...` clean, both platforms | `exit 0`, every package `ok` locally; CI re-run on the phase push |
| 3 | `docker compose up` → `/health` 200 in < 2 min | **28 s**, `{"status":"ok","version":"2.0.0","db":"connected"}` |
| 4 | Cross-tenant isolation | `TestCrossTenantIsolation` PASS — 5 subtests, 10 random queries all empty |
| 5 | Offline fallback | compile **200** with the plane stopped, `WARN ... falling back to local search plane_unavailable=true`; `sync_pending=2` → `synced=6` after restart |
| 6 | Multi-agent `cross_agent=true` | `TestMultiAgent` PASS — `STEP c: entry id=89e1d199… agent_id=agent_a cross_agent=true` |
| 7 | Conflict → superseded candidate at 0.5× | `TestMultiAgent` STEP e: candidate `total=0.2500` vs conflicting `total=0.5000`; new reversed-order test: `0.900000 × 0.5 = 0.450000` |
| 8 | 100-entry chain verifies | `{"entries_checked":100,"chain_valid":true}` |
| 9 | Tamper entry 47 | `{"entries_checked":47,"chain_valid":false,"first_break_id":"0a93107c-…"}` — entry 47's own id |
| 10 | `UPDATE` on the ledger refused | `UPDATE refused: SQLSTATE 42501: permission denied for table ledger` |
| 11 | Compliance PDF downloads | `HTTP 200`, `Content-Type: application/pdf`, **58 216 bytes** (empty period) and **54 452 bytes** (populated), `file`: *PDF document, version 1.4, 2 page(s)*, text extracted with `pdftotext` |
| 12 | `/v2/compliance/*` 403 for non-enterprise | `TestComplianceGate` PASS — 9/9 (three surfaces × enterprise/business/team) |
| 13 | MCP search returns the breakdown | over a real stdio MCP session: `score_s`, `score_r`, `score_i`, `score_t`, `score_total`, `trace_id` |
| 14 | MCP compile returns `compiled_messages` | over the same session: `synapse_compile` → `compiled_messages`, `tokens_used=292`, `reduction_pct=36.66`, `trace_id`. A real Cline session cannot be driven from a script; the README's MCP setup section is the reproduction |
| 15 | No `Authorization`/`x-api-key` in log output | 0 matches of either name in a header-value position; the 14 matches in `-v` output are all subtest names (`TestSanitizeHeaders/Remove_Authorization_header`) |
| 16 | v1 established ≥ 40% | `Raw: 5569 \| Compiled: 2999 \| Reduction: 46.1%` |
| 17 | Global Brain ≥ 55% | `Raw: 5569 \| Compiled: 381 \| Reduction: 93.2%`, uplift +47.0% |
| 18 | Suspended tenant 402 | `TestTenantStatusSuspendedTenantIsRefusedWith402` PASS |
| 19 | Invalid Stripe signature 400 | `TestWebhookRejectsInvalidSignature` PASS — 3 subtests, 0 skips |
| 20 | SQLite WAL cleanup documented | present, README §Resetting the store |
| 21 | Stale-Homebrew-binary warning | added, README §Development (and a Known limitations bullet) |
| 22 | Apple Silicon disclosure | present |
| 23 | Intel macOS disclosure | present |

### Findings

1. **The conflict item's wording is order-dependent, not wrong** — and the checklist got Option 1: no
   production change. It reads "Postgres vs MySQL memory → MySQL is superseded_candidate, scored 0.5x", but
   the shipped rule is directional: the memory persisted *first* becomes the candidate and the memory written
   *second* is `conflict`. With Postgres written first, Postgres is the candidate — which is what
   `TestMultiAgent` and `TestConflictMarksBothMemories` both assert, and which one live trace shows as
   `0.2500` against the challenger's `0.5000`. Reversing the product's rule to satisfy the checklist literally
   would demote the *current* decision and rank the stale one above it, which is the wrong answer for a
   context compiler. The checklist is satisfied instead by `conflict_order_test.go`, which writes MySQL first
   and asserts MySQL is the candidate at exactly `0.5 ×` the other's score — the same rule, the other order.
2. **The same breakdown has two wire names.** `synapse_search_memories` returns `score_s`/`score_r`/`score_i`/
   `score_t` (matching the checklist); `synapse_compile` returns `score_semantic`/`score_recency`/
   `score_importance`/`score_task_alignment`. Both carry `score_total` and are documented, but an editor
   rendering both tools sees two shapes for one concept. Also, the tool is
   `synapse_search_memories`, not `synapse_search`.
3. **The checklist's Global Brain command cannot work as written.** `--plane` requires `--api-key` (the tenant
   JWT); without it `globalBrainOptionsFromFlags` fails and the process exits. The measured run was
   `--plane http://127.0.0.1:9090 --api-key <jwt>`.
4. **The compose plane image ships no HTML-to-PDF renderer**, so `GET /v2/compliance/report?format=pdf`
   answers `503 pdf_tool_unavailable` out of the box on the stack the README tells self-hosters to start. The
   README now says so explicitly, names the five programs it looks for, and points at the two workarounds;
   bundling `chromium` in `deploy/Dockerfile.plane` is the alternative and is left as a product decision.
   Item 11's render was produced by the plane binary on a host with `google-chrome`
   (`renderer=google-chrome bytes=58216` in the plane's own log).
5. **`internal/store.NewStoreFromConfig` is dead code.** `cmd/synapse` calls `store.NewStore(cfg.DBPath)`
   unconditionally, so a node with `control-plane-url` set still stores its memories locally in SQLite — which
   is exactly the behaviour the checklist's "compilation continues from local sqlite-vec" assumes, and the
   opposite of what `synapse.yaml.example` states ("Set it and the process stores memories in the named
   tenant's Postgres schema instead of the local SQLite file") and of `factory.go`'s own doc comment. One of
   the two has to move; the example file is the one that is wrong today.
6. **An edge node whose tenant is `enterprise` needs `SYNAPSE_MASTER_KEY`** or every compile logs
   `ledger: append failed ... SYNAPSE_MASTER_KEY is required to wrap tenant secrets`. The request still
   succeeds — a failed ledger append never fails a compile, by design — but the tenant's chain silently gets a
   hole, which is the one thing an audit ledger exists to prevent. Now documented in the README's Global
   Brain setup; the compose stack already passes the key to the plane.
7. **Item 11 cannot be verified through the public API alone.** `POST /v2/tenants` hard-codes the `team`
   compliance tier (`defaultComplianceTier`), and the gate reads the *token claim*, so no tenant created over
   HTTP can reach a compliance surface — updating the registry row is not enough. The live curl therefore used
   a dev-signed token carrying `compliance_tier: enterprise`. This is Phase 21's and Phase 30's finding again,
   now with a cost attached: a release-blocking checklist item needed out-of-band token minting to be checked.
8. **Phase 31 closed without a PROGRESS.md entry**, against the project's own phase discipline. Written
   retroactively from commit `ad95c0d` and the two artifacts, and marked as such.
9. **`bin/synapse` is a tracked 16 MB binary** and was dirty in the working tree from a rebuild to 25 MB; it
   was restored rather than committed. `/bin/plane` is in `.gitignore` and `bin/synapse` is not, so the
   repository carries one build artifact and ignores its sibling — the tracked one should probably follow it
   out of the tree.
10. **The README's MCP setup spawns `"command":"synapse"`**, which on this machine resolves to the Homebrew
    binary (`/home/linuxbrew/.linuxbrew/bin/synapse`) rather than a dev build — the stale-binary gotcha
    finding 5 records, landing on the one config file a developer is most likely to edit. An absolute path in
    the example would remove the trap.

### Next phase

The highest-value follow-up is the one three phases have now run into: a supported way to provision an
enterprise compliance tier, because until it exists the compliance surfaces cannot be exercised through the
public API at all — not by a customer, and not by a checklist. After it, in rough order: wiring
`NewStoreFromConfig` (or deleting it and correcting `synapse.yaml.example`, finding 5); deciding whether the
plane image bundles a PDF renderer (finding 4); the two wire names for one score breakdown (finding 2); and
the tracked `bin/synapse` artifact (finding 9).

