package plane

import (
	"context"
	"errors"
)

// ErrTenantExists is returned by a TenantProvisioner when the requested slug is
// already taken. It lives here, on the consumer side, because internal/plane
// cannot import internal/tenant (that package already imports this one for
// PlaneConfig, so the reverse import would be a cycle). internal/tenant
// translates its own duplicate-slug sentinel into this value.
var ErrTenantExists = errors.New("plane: tenant slug already exists")

// ProvisionRequest is one tenant provisioning request, already validated by the
// HTTP layer: the slug has been matched against the allowed pattern and Plan is
// never blank.
type ProvisionRequest struct {
	// Slug is the tenant's unique, URL-safe identifier.
	Slug string
	// Plan is the tenant's billing plan.
	Plan string
	// ComplianceTier is the tenant's compliance tier.
	ComplianceTier string
}

// ProvisionResult is the material a freshly provisioned tenant needs.
//
// APIKey is the only time the plaintext key leaves the tenant layer: it is
// returned to the caller and never stored in plaintext or logged.
type ProvisionResult struct {
	// TenantID is the new tenant's uuid, as stored.
	TenantID string
	// JWT is a signed tenant token the tenant can present to the plane.
	JWT string
	// APIKey is the tenant's plaintext API key, shown exactly once.
	APIKey string
}

// TenantProvisioner is everything POST /v2/tenants needs from the tenant layer.
// The handler depends on this interface rather than on a database handle, so the
// provisioning endpoint is testable without PostgreSQL, and swapping the
// implementation never touches HTTP code.
type TenantProvisioner interface {
	// Provision creates the tenant described by req, or returns
	// ErrTenantExists when the slug is taken.
	Provision(ctx context.Context, req ProvisionRequest) (ProvisionResult, error)
}
