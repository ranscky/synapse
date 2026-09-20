// The control plane's side of the sync protocol: an edge node's pushed memories
// land in that tenant's own PostgreSQL schema through the same store and the
// same sanitization pipeline every other write goes through.
package tenant

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"synapse/internal/plane"
	"synapse/internal/store"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// synapseMemoryNamespace is the UUIDv5 namespace every derived memory id is
// hashed under. It is a fixed, random uuid belonging to this project: changing
// it would remap every already-synced memory onto a new row, so it must never
// change.
var synapseMemoryNamespace = uuid.MustParse("8f14e45f-ceea-467f-a4d3-9b1f6a5f4e21")

// MemoryWriter stores memories an edge node pushed into the tenant schemas held
// in PostgreSQL. It implements plane.MemoryWriter.
//
// One PGStore is opened per tenant slug and reused, because constructing one
// runs that tenant's DDL; a map is safe here because a slug can only reach this
// type through a verified JWT, so its keys are tenants the plane provisioned.
type MemoryWriter struct {
	pool   *pgxpool.Pool
	mu     sync.Mutex
	stores map[string]*store.PGStore
}

// NewMemoryWriter returns a writer that opens one PostgreSQL store per tenant.
func NewMemoryWriter(pool *pgxpool.Pool) *MemoryWriter {
	return &MemoryWriter{pool: pool, stores: make(map[string]*store.PGStore)}
}

// WriteBatch stores every entry of one pushed batch in tenantSlug's schema.
//
// It returns how many memories were written and how many had their content
// rewritten by store.Sanitize, which is the same pipeline the local backend
// applies -- one pattern list, one truncation rule, one place to change them.
//
// The batch is not wrapped in a transaction. Every write is idempotent on the
// memory id (the tenant table inserts with ON CONFLICT (id) DO NOTHING), so a
// batch that fails halfway and is retried by the edge commits exactly once: the
// memories written before the failure are skipped on the retry rather than
// duplicated. A caller therefore only has to treat an error as "not
// acknowledged", which is the contract the sync endpoint publishes.
func (w *MemoryWriter) WriteBatch(ctx context.Context, tenantSlug string, entries []store.MemoryEntry) (int, int, error) {
	// Nothing to store is nothing to do, even for a writer with no database:
	// an empty batch is a legitimate answer to give a live endpoint.
	if len(entries) == 0 {
		return 0, 0, nil
	}
	if w == nil || w.pool == nil {
		return 0, 0, fmt.Errorf("tenant: memory writer has no database pool")
	}

	st, err := w.storeFor(tenantSlug)
	if err != nil {
		return 0, 0, err
	}

	written, sanitized := 0, 0
	for _, entry := range entries {
		entry.ID = memoryUUID(entry.ID)
		if entry.SupersededBy != "" {
			entry.SupersededBy = memoryUUID(entry.SupersededBy)
		}
		entry.Embedding = embeddingWithinColumnWidth(entry.ID, entry.Embedding)

		clean := store.Sanitize(entry.Content)
		if clean != entry.Content {
			sanitized++
		}
		entry.Content = clean
		entry.SyncStatus = store.SyncStatusSynced

		if err := st.Write(ctx, entry); err != nil {
			return written, sanitized, err
		}
		written++
	}

	return written, sanitized, nil
}

// storeFor returns the tenant's store, opening it on first use.
func (w *MemoryWriter) storeFor(tenantSlug string) (*store.PGStore, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if st, ok := w.stores[tenantSlug]; ok {
		return st, nil
	}

	st, err := store.NewPGStore(w.pool, tenantSlug)
	if err != nil {
		return nil, err
	}

	w.stores[tenantSlug] = st

	return st, nil
}

// memoryUUID maps an edge node's local memory id onto the uuid the tenant table
// stores, and leaves an id that is already a uuid alone.
//
// This is not cosmetic: the local write path names memories "req-<nanos>" and
// the plane's column is uuid, so a pushed id has to be translated somewhere.
// Doing it here rather than on the edge keeps the edge's own ids untouched --
// local supersession still refers to them -- and makes the mapping a property of
// the schema that needs it.
//
// UUIDv5, not NewRandom: the same local id must always produce the same uuid,
// which is what makes a retried push and a re-pushed batch land on the same row
// instead of filling the tenant's table with copies.
func memoryUUID(localID string) string {
	if _, err := uuid.Parse(localID); err == nil {
		return localID
	}

	return uuid.NewSHA1(synapseMemoryNamespace, []byte(localID)).String()
}

// embeddingWithinColumnWidth returns an embedding the tenant column can hold,
// or nil for one it cannot.
//
// The column is vector(384), so a vector of any other width makes the insert
// fail -- and a failed insert here fails the whole batch, which the edge then
// retries forever. Dropping the embedding stores the memory without semantic
// reachability instead of losing the memory entirely, and says so in the log:
// the id and the width, never the content.
func embeddingWithinColumnWidth(memoryID string, embedding []float32) []float32 {
	if len(embedding) == 0 || len(embedding) == store.EmbeddingDimensions {
		return embedding
	}

	slog.Warn("Dropped a pushed embedding the tenant column cannot hold",
		"memory_id", memoryID, "dimensions", len(embedding), "want", store.EmbeddingDimensions)

	return nil
}

// MemoryWriter is the plane's write path for synced memories; this assertion is
// what makes a signature drift a build failure instead of a runtime surprise.
var _ plane.MemoryWriter = (*MemoryWriter)(nil)
