// The edge node's half of the v2 audit ledger: the one place that can see both
// the compiler's sink interface and internal/ledger.
//
// It lives in package main for the same reason cmd/plane/ledger.go does. The
// compiler must not import internal/ledger (that would pull Postgres and the
// tenant key store into a package the scoring pipeline depends on), and
// internal/ledger cannot import the compiler, so neither package can hold this
// wiring: main is the only place that sees both.
//
// What this file adds to the sketch a caller might expect -- "fetch the secret,
// call Append" -- is one translation and one decision. The translation is the
// request id: v1 mints ids like req-1758301000123456789, and ledger.Append
// refuses anything that is not a uuid, so the id has to be mapped (see
// ledgerRequestID). The decision is which tenant and which plan this node is,
// which is read from the credential the node already holds (see ledgerPlan).
package main

import (
	"context"
	"fmt"
	"time"

	"synapse/internal/compiler"
	"synapse/internal/config"
	"synapse/internal/ledger"
	"synapse/internal/store"
	"synapse/internal/tenant"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ledgerPoolTimeout bounds opening the ledger's Postgres pool at boot, so a
// database that is down delays startup by a bounded wait instead of hanging it.
const ledgerPoolTimeout = 15 * time.Second

// ledgerRequestNamespace is the fixed uuid namespace edge request ids are
// hashed under. It is a constant of this deployment, never a value from a
// request: changing it would change every future row's request_id, which is why
// it is written down here rather than derived from anything.
var ledgerRequestNamespace = uuid.MustParse("6f9c1e5a-3b7d-4c21-9a5e-8d0f4b2c7e31")

// secretReader is everything the sink needs from the tenant secret store. It is
// an interface so the sink's own logic -- which tenant, which id, what happens
// when the secret is missing -- is testable without PostgreSQL.
type secretReader interface {
	// GetSecret returns the tenant's raw signing secret.
	GetSecret(ctx context.Context, tenantID string) ([]byte, error)
}

// entryAppender is everything the sink needs from the ledger's write path. The
// signature is ledger.Append's, so *ledger.Ledger satisfies it as it stands and
// no adapter has to translate a row in and out.
type entryAppender interface {
	// Append signs traceJSON with secret and appends it to tenantID's chain.
	Append(ctx context.Context, tenantID, requestID, traceJSON string, secret []byte) (ledger.LedgerEntry, error)
}

// tenantSecretStore adapts tenant.GetSecret (a function) to secretReader, so the
// sink can depend on the behaviour rather than on a pool.
type tenantSecretStore struct {
	pool *pgxpool.Pool
}

// GetSecret implements secretReader. The secret is returned to the caller and
// held by nobody: it is fetched per append, handed to Append, and dropped when
// this call's frame goes away.
func (s tenantSecretStore) GetSecret(ctx context.Context, tenantID string) ([]byte, error) {
	return tenant.GetSecret(ctx, s.pool, tenantID)
}

// enterpriseLedger implements compiler.LedgerSink for one tenant: fetch that
// tenant's signing secret, then sign and append one trace.
//
// It holds no logger. A ledger write handles an HMAC key and a whole trace
// payload, and the structural reason neither can be logged is that this type has
// nowhere to log them -- the same choice internal/ledger makes for its own
// writer. Errors travel up to the compiler, which logs the error and nothing
// else.
type enterpriseLedger struct {
	reader   secretReader
	appender entryAppender
	tenantID string
}

// newEnterpriseLedger returns the sink that appends this node's compiled traces
// to tenantID's chain through pool.
func newEnterpriseLedger(pool *pgxpool.Pool, tenantID string) enterpriseLedger {
	return enterpriseLedger{
		reader:   tenantSecretStore{pool: pool},
		appender: ledger.NewLedger(pool),
		tenantID: tenantID,
	}
}

// AppendTrace implements compiler.LedgerSink: one trace in, one signed row out,
// or an error naming the stage that failed.
//
// The tenant is this node's own, fixed at construction from the node's verified
// credential; nothing in the call can change whose chain is written to, which is
// why requestID and traceJSON are the only values this method takes.
//
// The returned error never contains the secret, the ciphertext, or the master
// key: internal/tenant's own errors are already careful about that, and the
// wrapping here adds only stage names.
func (e enterpriseLedger) AppendTrace(ctx context.Context, requestID, traceJSON string) error {
	if e.reader == nil || e.appender == nil {
		return fmt.Errorf("ledger: enterprise sink has no database pool")
	}

	secret, err := e.reader.GetSecret(ctx, e.tenantID)
	if err != nil {
		return fmt.Errorf("ledger: fetch tenant secret: %w", err)
	}

	if _, err := e.appender.Append(ctx, e.tenantID, ledgerRequestID(requestID), traceJSON, secret); err != nil {
		return fmt.Errorf("ledger: append compiled trace: %w", err)
	}

	return nil
}

// ledgerRequestID maps an edge request id onto the uuid shape ledger.Append
// requires.
//
// v1 mints request ids as req-<unixnano> (internal/api) and
// req-<unixnano>-<seq> (internal/proxy), and ledger.Append refuses anything that
// is not a uuid -- fail-closed, because the id is part of the signed message.
// Changing v1's id format is out of this phase's scope (both call sites are
// frozen), so the ledger's request_id is derived instead:
//
//	uuid.NewSHA1(ledgerRequestNamespace, []byte(requestID))
//
// A uuid v5 is deterministic, so the row is traceable in both directions: given
// the request id the ledgered trace JSON still carries verbatim, a verifier
// recomputes the row's request_id, and given the row's request_id nobody can
// invert it back -- which is why the trace, not this column, is what an auditor
// reads. A request id that is already a uuid is canonicalized rather than
// hashed, so a future v1 change to uuid ids needs no change here.
func ledgerRequestID(requestID string) string {
	if parsed, err := uuid.Parse(requestID); err == nil {
		return parsed.String()
	}

	return uuid.NewSHA1(ledgerRequestNamespace, []byte(requestID)).String()
}

// ledgerPlan is what the edge node needs to know about the tenant whose
// compilations it writes to a ledger: which chain, and whether that tenant's plan
// asks for one at all.
type ledgerPlan struct {
	tenantID string
	plan     string
}

// resolveLedgerPlan reads this node's tenant and plan out of its own control
// plane credential.
//
// That credential is the tenant JWT provisioning returned (control-plane-api-key)
// and it is the only value in this process that names a tenant. It is read
// unverified on purpose and safely: it is this node's own configuration, never
// something a request can supply, and nothing is granted by reading it -- the
// plane still verifies the same token on every sync and every candidate pull, and
// the ledger row it enables is a write this node makes under that credential's
// own authority. A caller must never pass a header, a body, or a query value
// (see tenant.ParseTokenClaims).
//
// A credential that parses but names no tenant is an error rather than an empty
// identity: appending to an unnamed tenant is exactly the fail-open shape this
// path exists to avoid.
func resolveLedgerPlan(credential string) (ledgerPlan, error) {
	claims, err := tenant.ParseTokenClaims(credential)
	if err != nil {
		return ledgerPlan{}, err
	}

	if claims.TenantID == "" {
		return ledgerPlan{}, fmt.Errorf("ledger: control-plane credential names no tenant")
	}

	return ledgerPlan{tenantID: claims.TenantID, plan: claims.Plan}, nil
}

// enableEnterpriseLedger turns this process's audit ledger on when it should be,
// and reports what it did: a cleanup for the pool it opened, and whether a sink
// is installed.
//
// It is called once, before the router serves. Every failure here is returned
// rather than fatal, and no partial wiring is possible: a missing credential, a
// credential that names no tenant, a missing database-dsn, or a pool that will
// not open all end with no sink installed, which is the standalone node's shape.
// An edge node whose ledger database is unreachable must still compile -- the
// alternative, refusing to serve because an audit sink is down, would put the
// audit path ahead of the product it audits.
//
// A tenant whose plan is not enterprise gets (nil, false, nil) -- not an error,
// and deliberately not a log line in this function: the ledger is simply not part
// of what that tenant was sold, so there is nothing to warn about. The caller
// logs the enabled case, where there is something to see.
//
// The pool is opened through store.OpenPGPool, the project's one way to open a
// Postgres pool, which also creates the pgvector extension and registers the
// vector type on every connection. That is more than a ledger needs, and it is
// accepted rather than duplicated: a second pool setup is a second set of
// connection rules to keep in step with the plane's, and the extension is
// already enabled on any database this node can reach (the plane created it).
func enableEnterpriseLedger(cfg config.Config) (func(), bool, error) {
	if cfg.ControlPlaneAPIKey == "" {
		return nil, false, fmt.Errorf("ledger: control-plane-api-key is required to name the tenant whose compilations would be ledgered")
	}

	plan, err := resolveLedgerPlan(cfg.ControlPlaneAPIKey)
	if err != nil {
		return nil, false, err
	}

	if plan.plan != compiler.EnterprisePlan {
		return nil, false, nil
	}

	if cfg.DatabaseDSN == "" {
		return nil, false, fmt.Errorf("ledger: an enterprise tenant needs database-dsn (set it in the config file or %s) to write the audit ledger", config.EnvDatabaseDSN)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ledgerPoolTimeout)
	defer cancel()

	pool, err := store.OpenPGPool(ctx, cfg.DatabaseDSN)
	if err != nil {
		return nil, false, err
	}

	// Installed before serving, read by every compilation afterwards: the
	// compiler snapshots the trace and appends in its own goroutine, so no
	// compilation waits for the database this pool points at.
	compiler.SetLedgerSink(newEnterpriseLedger(pool, plan.tenantID), plan.plan)

	return pool.Close, true, nil
}
