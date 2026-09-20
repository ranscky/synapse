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


