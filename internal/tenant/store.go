package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTenantExists is returned by Store.CreateTenant when the slug is already
// taken. It is the tenant package's own sentinel; the plane-facing equivalent is
// plane.ErrTenantExists, and Provisioner translates between them.
var ErrTenantExists = errors.New("tenant: slug already exists")

// uniqueViolation is PostgreSQL's SQLSTATE for a unique-constraint violation --
// what a duplicate slug looks like when the database, not a prior SELECT,
// decides. Relying on it is what makes provisioning race-free.
const uniqueViolation = "23505"

// CreateTenantParams is one provisioning write: the registry fields plus the
// already-hashed API key. The plaintext key never reaches this package, which is
// why the field is named KeyHash.
type CreateTenantParams struct {
	// Slug is the tenant's unique, URL-safe identifier.
	Slug string
	// Plan is the tenant's billing plan; never blank (the DDL default is 'oss').
	Plan string
	// ComplianceTier is the tenant's compliance tier; never blank.
	ComplianceTier string
	// KeyHash is the bcrypt hash of the tenant's API key.
	KeyHash string
}

// Store is the pgx-backed tenant registry. It talks to the synapse_global
// schema and logs nothing: a database error can quote the connection target, so
// callers decide what is safe to report.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store that reads and writes through pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateTenant inserts the tenant and its API-key hash in one transaction and
// returns the new tenant's id -- the database's uuid, not one invented here, so
// the default in the DDL stays the single source of truth.
//
// The tenant row and its key must land together: a tenant without a key could
// never authenticate, and a key without a tenant would be unreachable.
func (s *Store) CreateTenant(ctx context.Context, p CreateTenantParams) (string, error) {
	if s == nil || s.pool == nil {
		return "", fmt.Errorf("tenant: store has no database pool")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("tenant: begin tenant transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO `+SchemaName+`.tenants (slug, plan, compliance_tier) VALUES ($1, $2, $3) RETURNING id::text`,
		p.Slug, p.Plan, p.ComplianceTier,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrTenantExists
		}
		return "", fmt.Errorf("tenant: insert tenant: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO `+SchemaName+`.tenant_keys (tenant_id, key_hash) VALUES ($1, $2)`,
		id, p.KeyHash,
	); err != nil {
		return "", fmt.Errorf("tenant: insert tenant key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("tenant: commit tenant: %w", err)
	}

	return id, nil
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation, i.e. the slug is already in use.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError

	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
