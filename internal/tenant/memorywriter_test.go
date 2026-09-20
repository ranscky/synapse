package tenant

import (
	"context"
	"testing"

	"synapse/internal/store"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemoryUUIDIsDeterministicAndLeavesRealUUIDsAlone covers the translation
// between the two id spaces: the local write path names memories "req-<nanos>"
// while the tenant table's column is a uuid.
func TestMemoryUUIDIsDeterministicAndLeavesRealUUIDsAlone(t *testing.T) {
	localID := "req-1730000000000000000-1"

	mapped := memoryUUID(localID)
	_, err := uuid.Parse(mapped)
	require.NoError(t, err, "the mapped id must be a uuid, or the insert cannot succeed")

	assert.Equal(t, mapped, memoryUUID(localID),
		"the same local id must always map to the same uuid, or a retried push would write a second copy")
	assert.NotEqual(t, mapped, memoryUUID("req-1730000000000000000-2"))

	existing := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	assert.Equal(t, existing, memoryUUID(existing), "an id that is already a uuid is used as it is")
}

// TestEmbeddingWithinColumnWidthDropsOnlyWrongWidthVectors covers the one
// predictable way a pushed memory could fail to insert: the tenant column is
// vector(384) and a shorter or longer vector is rejected by PostgreSQL.
func TestEmbeddingWithinColumnWidthDropsOnlyWrongWidthVectors(t *testing.T) {
	assert.Nil(t, embeddingWithinColumnWidth("req-1", nil), "a memory without an embedding stays without one")

	right := make([]float32, store.EmbeddingDimensions)
	assert.Equal(t, right, embeddingWithinColumnWidth("req-1", right))

	assert.Nil(t, embeddingWithinColumnWidth("req-1", make([]float32, 8)),
		"an embedding the column cannot hold is dropped, because failing the insert would fail the whole batch")
	assert.Nil(t, embeddingWithinColumnWidth("req-1", make([]float32, store.EmbeddingDimensions+1)))
}

// TestMemoryWriterWithoutAPoolFailsLoudly documents the constructor contract: a
// writer with no database is an error on use, not a nil dereference.
func TestMemoryWriterWithoutAPoolFailsLoudly(t *testing.T) {
	ctx := context.Background()
	entries := []store.MemoryEntry{{ID: "req-1", Content: "a memory"}}

	_, _, err := NewMemoryWriter(nil).WriteBatch(ctx, "my-team", entries)
	require.Error(t, err)

	var zero *MemoryWriter
	_, _, err = zero.WriteBatch(ctx, "my-team", entries)
	require.Error(t, err)

	// An empty batch never needs a database at all.
	written, sanitized, err := zero.WriteBatch(ctx, "my-team", nil)
	require.NoError(t, err)
	assert.Zero(t, written)
	assert.Zero(t, sanitized)
}
