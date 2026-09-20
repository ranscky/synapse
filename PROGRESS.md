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
