package tenant

import (
	"context"
	"errors"
	"fmt"

	"synapse/internal/plane"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultComplianceTier is the tier a tenant gets when the caller does not name
// one. It matches the DDL default on synapse_global.tenants.compliance_tier, so
// the token claim and the stored row cannot disagree.
const defaultComplianceTier = "team"

// Provisioner turns a validated provisioning request into stored tenant
// material: a registry row, a bcrypt API-key hash, the ledger signing secret,
// and a signed JWT.
//
// It implements plane.TenantProvisioner. The plane declares that interface
// because internal/tenant already depends on internal/plane (see auth.go), so
// the reverse import would be a cycle -- the consumer owns the contract.
//
// It holds the database pool as well as the registry store (Phase 18) because
// minting the tenant's signing secret is a second write that the Store's own
// transaction cannot carry: the tenant's uuid is minted by the database inside
// CreateTenant, and the secret is sealed with that uuid as additional
// authenticated data, so the secret can only be wrapped once the id exists.
type Provisioner struct {
	cfg   *plane.PlaneConfig
	store *Store
	pool  *pgxpool.Pool
}

// NewProvisioner returns a Provisioner that signs with cfg's JWT secret, writes
// through store, and mints the tenant's ledger signing secret through pool.
func NewProvisioner(cfg *plane.PlaneConfig, store *Store, pool *pgxpool.Pool) *Provisioner {
	return &Provisioner{cfg: cfg, store: store, pool: pool}
}

// Provision creates the tenant and returns the material the caller may show
// once: the tenant id, a JWT, and the plaintext API key.
//
// The API key is generated here and only its bcrypt hash is stored, so the
// plaintext exists in memory for the length of this call and in the HTTP
// response body -- nowhere else. Nothing in this path logs: neither the key, the
// hash, the JWT, nor the signing secret.
//
// The tenant's ledger signing secret is minted and stored here too (Phase 18),
// which is what closes Phase 17's first finding: before this, nothing in a
// running deployment ever called GenerateAndStoreSecret, so every tenant's audit
// chain was unwritable (Append needs a secret) and unverifiable
// (GET /v2/ledger/verify answered 500 through ErrSecretNotFound). The secret is
// *stored*, not returned: the ledger fetches and unwraps it per append, and the
// tenant does not need it to use the endpoint. Handing it to the tenant the way
// the API key is handed over -- so the tenant can verify its own chain without
// trusting the plane -- is a deliberate later item rather than an oversight; it
// changes the provisioning response shape, which this phase had no reason to do.
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
// layer. If token signing or secret minting fails after the row is committed the
// tenant exists without a usable token, or without a signing secret -- reissuing
// is a later phase's problem, and pretending the tenant was not created would be
// worse.
func (p *Provisioner) Provision(ctx context.Context, req plane.ProvisionRequest) (plane.ProvisionResult, error) {
	if p == nil || p.store == nil {
		return plane.ProvisionResult{}, fmt.Errorf("tenant: provisioner has no store")
	}
	if p.pool == nil {
		return plane.ProvisionResult{}, fmt.Errorf("tenant: provisioner has no database pool")
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

	// The tenant's signing secret, minted once and never returned. Its error is
	// returned rather than swallowed: a tenant without a secret cannot write an
	// audit entry or verify one, and a caller that believes it provisioned a
	// complete tenant when it did not would be worse off than one told the
	// truth. The row is already committed at this point; the caller sees an
	// error, and re-provisioning under the same slug is refused -- the same
	// shape IssueToken's failure has below, for the same reason.
	if _, err := GenerateAndStoreSecret(ctx, p.pool, tenantID); err != nil {
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
