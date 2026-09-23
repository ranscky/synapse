// The edge node's half of v2 usage metering: the one place that can see both
// the compiler's sink interface and internal/metering.
//
// It lives in package main for exactly the reasons cmd/synapse/ledger.go does.
// internal/compiler must not import internal/metering (that would pull Postgres
// into a package the scoring pipeline depends on), and internal/metering cannot
// import internal/compiler, so neither package can hold this wiring: main is
// the only place that sees both.
//
// What this file adds to the sketch a caller might expect -- "construct a Meter
// and call Record" -- is one decision and one translation. The decision is
// which tenant a compilation belongs to, and it is read from the credential
// this node already holds, never from a request: the same resolveLedgerPlan
// Phase 18 uses, so there is one definition of "this node's tenant" rather than
// two that could disagree. The translation is the event's shape: the compiler's
// UsageEvent and metering.UsageEvent are identical field for field, so the
// conversion below is a compile-time contract -- a field added to one and not
// the other stops the build here rather than silently dropping out of a row.
package main

import (
	"context"
	"fmt"

	"synapse/internal/compiler"
	"synapse/internal/config"
	"synapse/internal/metering"
	"synapse/internal/store"
)

// meterSink implements compiler.UsageSink for one tenant: fill in the tenant
// this node belongs to, then hand the event to the meter.
//
// It holds no logger, and the compiler's interface gives it nowhere to put one.
// A usage event carries a session id and a tenant id, and the only logging that
// happens on this path is inside metering.Meter.Record, which logs the error
// alone.
type meterSink struct {
	meter    *metering.Meter
	tenantID string
}

// meterSinkIsAUsageSink is the compile-time assertion that this type still
// satisfies the contract internal/compiler declares. It is a variable rather
// than a comment because a signature that drifted would then fail the build
// here, in the package that owns the translation, instead of at the call site
// in cmd/synapse's main.
var meterSinkIsAUsageSink compiler.UsageSink = meterSink{}

// Record implements compiler.UsageSink.
//
// The tenant id overwrites whatever the call site left in the event -- both v1
// call sites leave it empty, deliberately, because neither of them can know it
// and a value a request could influence must never reach a billing table. The
// conversion is legal because the two structs are identical field for field;
// see this file's header comment.
func (s meterSink) Record(event compiler.UsageEvent) {
	event.TenantID = s.tenantID

	s.meter.Record(metering.UsageEvent(event))
}

// enableMetering turns this process's usage metering on when it should be, and
// reports what it did: a cleanup for the pool it opened, and whether a sink is
// installed.
//
// It is called once, before the router serves. Like enableEnterpriseLedger, it
// returns every failure rather than exiting, and no partial wiring is possible:
// a missing credential, a credential that names no tenant, a missing
// database-dsn, or a pool that will not open all end with no sink installed,
// which is the standalone node's shape. An edge node whose database is
// unreachable must still compile -- refusing to serve because a metering sink
// is down would put the invoice ahead of the product the invoice describes.
//
// What it deliberately does not do, and the difference from the ledger's gate,
// is ask about the plan. Metering is not a feature a plan buys: usage_events is
// where the compliance report's compilation count and average context
// reduction come from (see internal/ledger/report.go), so an oss or team tenant
// that was not metered would have a report reading zero compilations for work
// it actually did. The ledger's own gate is right for the ledger -- an
// enterprise SKU includes the signed chain -- and wrong here.
//
// The pool is opened through store.OpenPGPool, the project's one way to open a
// Postgres pool, which is also what the ledger's wiring does. Two pools to one
// database when both sinks are on is the accepted cost of keeping each sink's
// lifetime its own; sharing one would mean threading a pool through both
// wirings for a connection or two.
func enableMetering(cfg config.Config) (func(), bool, error) {
	if cfg.ControlPlaneAPIKey == "" {
		return nil, false, fmt.Errorf("metering: control-plane-api-key is required to name the tenant whose compilations would be metered")
	}

	plan, err := resolveLedgerPlan(cfg.ControlPlaneAPIKey)
	if err != nil {
		return nil, false, err
	}

	if cfg.DatabaseDSN == "" {
		return nil, false, fmt.Errorf("metering: usage rows go to Postgres, so this node needs database-dsn (set it in the config file or %s)", config.EnvDatabaseDSN)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ledgerPoolTimeout)
	defer cancel()

	pool, err := store.OpenPGPool(ctx, cfg.DatabaseDSN)
	if err != nil {
		return nil, false, err
	}

	// Installed before serving, read by every compilation afterwards: Record
	// returns immediately and the INSERT runs in its own goroutine, so no
	// compilation waits for the database this pool points at.
	compiler.SetUsageSink(meterSink{meter: metering.NewMeter(pool), tenantID: plan.tenantID})

	return pool.Close, true, nil
}
