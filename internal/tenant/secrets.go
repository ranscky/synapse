package tenant

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"synapse/internal/plane"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// secretEntropyBytes is the size of a tenant's ledger signing secret: 32
	// bytes, which is also SHA-256's output size and therefore the point above
	// which a longer HMAC key buys nothing.
	secretEntropyBytes = 32

	// masterKeyBytes is the size SYNAPSE_MASTER_KEY must hex-decode to. 32 bytes
	// is AES-256's key size and the only length accepted.
	masterKeyBytes = 32
)

// ErrSecretNotFound is returned by GetSecret for a tenant that has no stored
// secret. It is a sentinel rather than a bare error because "this tenant was
// never given a signing key" is an answer a caller has to be able to tell apart
// from "the database is unreachable".
var ErrSecretNotFound = errors.New("tenant: no stored secret for tenant")

// ErrSecretExists is returned by GenerateAndStoreSecret when the tenant already
// has one.
//
// Overwriting is refused rather than upserted on purpose: every ledger entry is
// signed with the secret that existed when it was written, and the ledger table
// has no key-version column, so replacing the row would leave one secret for the
// whole chain and no way to verify the entries signed with the old one -- the
// opposite of what an audit ledger is for. Rotation needs key versioning first;
// until then a second generation is an error instead of silent damage.
var ErrSecretExists = errors.New("tenant: tenant already has a stored secret")

// GenerateAndStoreSecret mints a tenant's ledger signing secret, stores it
// encrypted, and returns the raw bytes so the caller can hand them to the tenant
// once.
//
// The 32 bytes come from crypto/rand. AES-256-GCM wraps them under
// SYNAPSE_MASTER_KEY, which is read from the environment on every call and never
// cached -- no copy of the master key, the raw secret, or the ciphertext lives in
// this package between calls. The raw secret is returned and nowhere else: it is
// never persisted, never logged, and (unlike an API key, which only needs to be
// verified) it cannot be recovered from the database without the master key,
// because that is the whole point of the wrap.
//
// The tenant id is authenticated as additional data, so a ciphertext moved into
// another tenant's row fails to decrypt instead of quietly becoming that
// tenant's signing key.
func GenerateAndStoreSecret(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]byte, error) {
	if pool == nil {
		return nil, fmt.Errorf("tenant: generating a tenant secret needs a database pool")
	}

	id, err := canonicalTenantID(tenantID)
	if err != nil {
		return nil, err
	}

	key, err := masterKeyFromEnv()
	if err != nil {
		return nil, err
	}

	raw := make([]byte, secretEntropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("tenant: read random bytes for tenant secret: %w", err)
	}

	stored, err := sealSecret(key, id, raw)
	if err != nil {
		return nil, err
	}

	// DO NOTHING rather than DO UPDATE: see ErrSecretExists. The conflict is
	// answered by the database, not by a preceding SELECT, so two concurrent
	// generations for one tenant cannot both succeed and leave the loser's
	// secret nowhere.
	tag, err := pool.Exec(ctx,
		`INSERT INTO `+SchemaName+`.tenant_secrets (tenant_id, secret_encrypted)
		 VALUES ($1, $2)
		 ON CONFLICT (tenant_id) DO NOTHING`,
		id, stored,
	)
	if err != nil {
		return nil, fmt.Errorf("tenant: store tenant secret: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return nil, ErrSecretExists
	}

	return raw, nil
}

// GetSecret returns the tenant's raw signing secret, decrypted.
//
// Nothing is cached: the ciphertext is fetched and opened on every call, so a
// rotation or a revoke takes effect on the next read instead of on the next
// restart, and no long-lived copy of a signing key exists in this process.
//
// The error for a missing row is ErrSecretNotFound; a failure to decrypt is not
// (it means the row exists and the master key does not match it, which is a
// different problem). Neither error contains the secret, the ciphertext, or the
// master key.
func GetSecret(ctx context.Context, pool *pgxpool.Pool, tenantID string) ([]byte, error) {
	if pool == nil {
		return nil, fmt.Errorf("tenant: reading a tenant secret needs a database pool")
	}

	id, err := canonicalTenantID(tenantID)
	if err != nil {
		return nil, err
	}

	key, err := masterKeyFromEnv()
	if err != nil {
		return nil, err
	}

	var stored string
	err = pool.QueryRow(ctx,
		`SELECT secret_encrypted FROM `+SchemaName+`.tenant_secrets WHERE tenant_id = $1`,
		id,
	).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrSecretNotFound
	case err != nil:
		return nil, fmt.Errorf("tenant: read tenant secret: %w", err)
	}

	return openSecret(key, id, stored)
}

// masterKeyFromEnv returns the AES-256 key that wraps tenant secrets, hex-decoded
// from SYNAPSE_MASTER_KEY.
//
// It is read from the environment rather than a config file because the key that
// protects every tenant's signing secret must not sit next to the database
// credentials in a file on disk. Errors name the variable and never its value:
// this message can reach a log line, and the key it complains about is the one
// secret in the process that can unwrap every tenant's.
func masterKeyFromEnv() ([]byte, error) {
	encoded := os.Getenv(plane.EnvMasterKey)
	if encoded == "" {
		return nil, fmt.Errorf("tenant: %s is required to wrap tenant secrets", plane.EnvMasterKey)
	}

	key, err := hex.DecodeString(encoded)
	if err != nil {
		// The cause is dropped rather than wrapped: encoding/hex's error quotes
		// the offending character, and the offending character is part of a key.
		return nil, fmt.Errorf("tenant: %s must be hex-encoded (%d characters for %d bytes)", plane.EnvMasterKey, masterKeyBytes*2, masterKeyBytes)
	}

	if len(key) != masterKeyBytes {
		return nil, fmt.Errorf("tenant: %s must decode to %d bytes, got %d", plane.EnvMasterKey, masterKeyBytes, len(key))
	}

	return key, nil
}

// sealSecret encrypts raw for tenantID under key and returns the string stored in
// synapse_global.tenant_secrets.secret_encrypted: standard base64 of the nonce
// followed by the AES-256-GCM ciphertext.
//
// The 12-byte nonce is drawn from crypto/rand per call and travels with the
// ciphertext, which is what lets one master key wrap many tenants' secrets
// safely: a repeated nonce under one key is the failure GCM cannot survive, and
// there is no counter to reuse because nothing here holds state between calls.
func sealSecret(key []byte, tenantID string, raw []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("tenant: read random bytes for secret nonce: %w", err)
	}

	envelope := make([]byte, 0, len(nonce)+len(raw)+gcm.Overhead())
	envelope = append(envelope, nonce...)
	envelope = gcm.Seal(envelope, nonce, raw, []byte(tenantID))

	return base64.StdEncoding.EncodeToString(envelope), nil
}

// openSecret is the inverse of sealSecret. It fails, rather than returning
// something, when the tenant id differs from the one the ciphertext was sealed
// for or when the bytes have been altered -- GCM authenticates both, so a
// tampered or transplanted ciphertext surfaces as an error here instead of as a
// plausible-looking secret.
func openSecret(key []byte, tenantID, stored string) ([]byte, error) {
	envelope, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return nil, fmt.Errorf("tenant: stored tenant secret is not valid base64: %w", err)
	}

	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	if len(envelope) < gcm.NonceSize() {
		return nil, fmt.Errorf("tenant: stored tenant secret is too short to hold a nonce")
	}

	nonce, ciphertext := envelope[:gcm.NonceSize()], envelope[gcm.NonceSize():]

	raw, err := gcm.Open(nil, nonce, ciphertext, []byte(tenantID))
	if err != nil {
		return nil, fmt.Errorf("tenant: decrypt tenant secret: %w", err)
	}

	return raw, nil
}

// newGCM builds the AEAD every tenant secret goes through. key must be exactly
// masterKeyBytes long, which aes.NewCipher enforces.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("tenant: build tenant secret cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("tenant: build tenant secret cipher mode: %w", err)
	}

	return gcm, nil
}

// canonicalTenantID renders a tenant id the way Postgres stores and returns a
// uuid, so the value used as GCM additional data is identical on the way in and
// on the way out however the caller cased it.
//
// The input is not echoed: an id is not a secret, but an error message is a log
// line and there is no reason to put a caller-supplied identifier in one.
func canonicalTenantID(tenantID string) (string, error) {
	parsed, err := uuid.Parse(tenantID)
	if err != nil {
		return "", fmt.Errorf("tenant: tenant id must be a uuid")
	}

	return parsed.String(), nil
}
