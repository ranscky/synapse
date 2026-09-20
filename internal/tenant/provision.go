package tenant

import (
	"context"
	"errors"
	"fmt"

	"synapse/internal/plane"
)

// defaultComplianceTier is the tier a tenant gets when the caller does not name
// one. It matches the DDL default on synapse_global.tenants.compliance_tier, so
// the token claim and the stored row cannot disagree.
const defaultComplianceTier = "team"

// Provisioner turns a validated provisioning request into stored tenant
// material: a registry row, a bcrypt API-key hash, and a signed JWT.
//
// It implements plane.TenantProvisioner. The plane declares that interface
// because internal/tenant already depends on internal/plane (see auth.go), so
// the reverse import would be a cycle -- the consumer owns the contract.
type Provisioner struct {
	cfg   *plane.PlaneConfig
	store *Store
}

// NewProvisioner returns a Provisioner that signs with cfg's JWT secret and
// writes through store.
func NewProvisioner(cfg *plane.PlaneConfig, store *Store) *Provisioner {
	return &Provisioner{cfg: cfg, store: store}
}

// Provision creates the tenant and returns the material the caller may show
// once: the tenant id, a JWT, and the plaintext API key.
//
// The API key is generated here and only its bcrypt hash is stored, so the
// plaintext exists in memory for the length of this call and in the HTTP
// response body -- nowhere else. Nothing in this path logs: neither the key, the
// hash, the JWT, nor the signing secret.
//
// When the request names an agent (and optionally a team), both become claims in
// the returned token. That is what gives the plane a verified identity to
// evaluate memory visibility against, rather than a request body's word for who
// is asking; the claims are signed here because this is the only place that
// holds the signing key. A request that names no agent mints a tenant-level
// token, whose reader sees org-scoped memories only.
//
// A duplicate slug surfaces as plane.ErrTenantExists; the translation happens
// here so the tenant package's sentinel never has to be imported by the HTTP
// layer. If token signing fails after the row is committed the tenant exists
// without a usable token -- reissuing is a later phase's problem, and pretending
// the tenant was not created would be worse.
func (p *Provisioner) Provision(ctx context.Context, req plane.ProvisionRequest) (plane.ProvisionResult, error) {
	if p == nil || p.store == nil {
		return plane.ProvisionResult{}, fmt.Errorf("tenant: provisioner has no store")
	}

	apiKey, err := GenerateAPIKey()
	if err != nil {
		return plane.ProvisionResult{}, err
	}

	keyHash, err := HashAPIKey(apiKey)
	if err != nil {
		return plane.ProvisionResult{}, err
	}

	tier := req.ComplianceTier
	if tier == "" {
		tier = defaultComplianceTier
	}

	tenantID, err := p.store.CreateTenant(ctx, CreateTenantParams{
		Slug:           req.Slug,
		Plan:           req.Plan,
		ComplianceTier: tier,
		KeyHash:        keyHash,
	})
	if err != nil {
		if errors.Is(err, ErrTenantExists) {
			return plane.ProvisionResult{}, plane.ErrTenantExists
		}
		return plane.ProvisionResult{}, err
	}

	token, err := IssueToken(p.cfg, TokenIdentity{
		TenantID: tenantID,
		Slug:     req.Slug,
		Plan:     req.Plan,
		Tier:     tier,
		AgentID:  req.AgentID,
		TeamID:   req.TeamID,
	})
	if err != nil {
		return plane.ProvisionResult{}, err
	}

	return plane.ProvisionResult{TenantID: tenantID, JWT: token, APIKey: apiKey}, nil
}
