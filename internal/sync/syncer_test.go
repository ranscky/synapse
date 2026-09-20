package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"synapse/internal/config"
	"synapse/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testPlaneKey = "plane-key-that-must-never-be-logged"
	testAgentID  = "edge-agent-1"
	testSession  = "session-under-test"
)

// pushRecord is one request the stub plane received.
type pushRecord struct {
	method        string
	path          string
	authorization string
	contentType   string
	body          pushRequest
	raw           string
}

// newPlaneStub returns a control-plane stub that answers everything with status
// and delivers each received request over a channel.
//
// A channel rather than a mutex-guarded slice: the handler runs in the server's
// goroutine, and a channel send before the response is written gives the test a
// happens-before edge on the recorded value instead of a data race. It also
// makes "exactly one request" a property the test can assert directly (nothing
// else is in the channel).
func newPlaneStub(t *testing.T, status int) (*httptest.Server, <-chan pushRecord) {
	t.Helper()

	received := make(chan pushRecord, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}

		var body pushRequest
		_ = json.Unmarshal(raw, &body)

		received <- pushRecord{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			body:          body,
			raw:           string(raw),
		}

		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"written":0,"sanitized":0}`))
	}))
	t.Cleanup(server.Close)

	return server, received
}

// nextPush returns the next recorded request, failing the test rather than
// hanging if the syncer never made one.
func nextPush(t *testing.T, received <-chan pushRecord) pushRecord {
	t.Helper()

	select {
	case record := <-received:
		return record
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the syncer never pushed")
		return pushRecord{}
	}
}

// newTestSyncer returns a Syncer pointed at url with a batch size of 20.
func newTestSyncer(url string) *Syncer {
	return NewSyncer(config.Config{
		ControlPlaneURL:     url,
		ControlPlaneAPIKey:  testPlaneKey,
		AgentID:             testAgentID,
		SyncBatchSize:       20,
		SyncIntervalSeconds: 30,
	})
}

// newTestStore returns a real SQLite store in a temp directory: the flusher's
// contract with the store is SQL, so it is exercised against SQLite, not a
// double. Nothing here needs PostgreSQL.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.NewStore(filepath.Join(t.TempDir(), "sync.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	return st
}

// pendingEntry is a memory the local write path has queued for the plane.
func pendingEntry(id string, written time.Time) store.MemoryEntry {
	return store.MemoryEntry{
		ID:         id,
		SessionID:  testSession,
		Content:    "memory " + id,
		MemoryType: "fact",
		Importance: 0.5,
		Timestamp:  written,
		SyncStatus: store.SyncStatusSyncPending,
	}
}

// seedPending writes count pending memories, oldest first by one second, and
// returns their ids in that order.
func seedPending(t *testing.T, st *store.Store, count int) []string {
	t.Helper()

	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)
	ids := make([]string, 0, count)

	for i := 0; i < count; i++ {
		id := fmt.Sprintf("mem-%02d", i)
		require.NoError(t, st.Write(ctx, pendingEntry(id, base.Add(time.Duration(i)*time.Second))))
		ids = append(ids, id)
	}

	return ids
}

// idsOf maps entries onto their ids, in order.
func idsOf(entries []store.MemoryEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}

	return ids
}

// indexByID indexes entries by id, so a test can assert on one row without
// depending on the order a read returned.
func indexByID(entries []store.MemoryEntry) map[string]store.MemoryEntry {
	index := make(map[string]store.MemoryEntry, len(entries))
	for _, entry := range entries {
		index[entry.ID] = entry
	}

	return index
}

func TestNewSyncerUsesConfiguredValuesAndGuardsZeroes(t *testing.T) {
	syncer := newTestSyncer("http://127.0.0.1:9090/")

	assert.Equal(t, 5*time.Second, syncer.httpClient.Timeout)
	assert.Equal(t, 30*time.Second, syncer.interval)
	assert.Equal(t, 20, syncer.batchSize)
	assert.Equal(t, syncBacklogWarn, syncer.backlogWarn)
	assert.Equal(t, syncBacklogMax, syncer.backlogMax)
	assert.Equal(t, "http://127.0.0.1:9090/v2/sync/memories", syncer.endpoint(),
		"a trailing slash on the configured URL must not double up")

	// A hand-built Config with no sync fields must not produce a ticker that
	// panics on a zero interval.
	bare := NewSyncer(config.Config{})
	assert.Equal(t, defaultSyncInterval, bare.interval)
	assert.Equal(t, defaultSyncBatchSize, bare.batchSize)
}

func TestPushSendsOneRequestWithEveryMemory(t *testing.T) {
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)

	base := time.Now().UTC().Truncate(time.Second)
	entries := make([]store.MemoryEntry, 0, 5)
	for i := 0; i < 5; i++ {
		entries = append(entries, pendingEntry(fmt.Sprintf("mem-%d", i), base.Add(time.Duration(i)*time.Second)))
	}

	require.NoError(t, syncer.Push(context.Background(), entries))

	record := nextPush(t, received)
	assert.Equal(t, http.MethodPost, record.method)
	assert.Equal(t, "/v2/sync/memories", record.path)
	assert.Equal(t, "Bearer "+testPlaneKey, record.authorization)
	assert.Equal(t, "application/json", record.contentType)
	assert.Equal(t, testAgentID, record.body.AgentID)
	assert.Equal(t, testSession, record.body.SessionID)
	require.Len(t, record.body.Memories, 5)
	assert.Equal(t, "mem-0", record.body.Memories[0].ID)
	assert.Equal(t, "mem-4", record.body.Memories[4].ID, "a batch keeps the order the store returned")
	assert.Empty(t, received, "one batch is exactly one request")
}

func TestPushMixedSessionsSendsAnEmptyEnvelopeSession(t *testing.T) {
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)

	first := pendingEntry("a", time.Now())
	second := pendingEntry("b", time.Now())
	second.SessionID = "another-session"

	require.NoError(t, syncer.Push(context.Background(), []store.MemoryEntry{first, second}))

	record := nextPush(t, received)
	assert.Empty(t, record.body.SessionID, "one envelope session cannot describe two sessions")
	require.Len(t, record.body.Memories, 2)
	assert.Equal(t, "another-session", record.body.Memories[1].SessionID, "each memory keeps its own session")
}

func TestPushRejectsNon2xxWithoutLeakingTheCredential(t *testing.T) {
	server, received := newPlaneStub(t, http.StatusInternalServerError)
	syncer := newTestSyncer(server.URL)

	err := syncer.Push(context.Background(), []store.MemoryEntry{pendingEntry("a", time.Now())})

	require.Error(t, err)
	assert.Equal(t, "sync: push failed status=500", err.Error())
	assert.NotContains(t, err.Error(), testPlaneKey)

	record := nextPush(t, received)
	assert.NotContains(t, record.raw, testPlaneKey, "the credential travels in the header and nowhere in the body")
}

func TestPushWrapsATransportError(t *testing.T) {
	server, _ := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)

	// Nothing is listening any more, so the push cannot leave the machine.
	server.Close()

	err := syncer.Push(context.Background(), []store.MemoryEntry{pendingEntry("a", time.Now())})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync: push request:")
	assert.NotContains(t, err.Error(), testPlaneKey)
}

func TestPushWithoutCredentialOrEntries(t *testing.T) {
	server, received := newPlaneStub(t, http.StatusOK)

	noKey := NewSyncer(config.Config{ControlPlaneURL: server.URL, AgentID: testAgentID})
	err := noKey.Push(context.Background(), []store.MemoryEntry{pendingEntry("a", time.Now())})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "control-plane-api-key")

	syncer := newTestSyncer(server.URL)
	require.NoError(t, syncer.Push(context.Background(), nil))

	assert.Empty(t, received, "neither call may reach the plane")
}

// captureLogs redirects the default logger into a buffer for the duration of a
// test, so a test can assert what was -- and was not -- written. None of these
// tests run in parallel, so swapping the default logger is safe here.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()

	var captured strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &captured
}

func TestFlushMarksEverythingThePlaneAccepted(t *testing.T) {
	logs := captureLogs(t)
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)
	st := newTestStore(t)
	ctx := context.Background()

	ids := seedPending(t, st, 5)
	require.NoError(t, st.Write(ctx, store.MemoryEntry{
		ID: "local", SessionID: testSession, Content: "never queued",
		MemoryType: "fact", Timestamp: time.Now(),
	}))

	syncer.flush(ctx, st)

	record := nextPush(t, received)
	assert.Equal(t, ids, idsOf(record.body.Memories), "only pending memories are pushed, oldest first")

	count, err := st.CountPendingSync(ctx)
	require.NoError(t, err)
	assert.Zero(t, count, "nothing is left waiting")

	memories, err := st.GetRecent(ctx, testSession, 10)
	require.NoError(t, err)

	synced := 0
	for _, memory := range memories {
		if memory.ID == "local" {
			assert.Equal(t, store.SyncStatusLocalOnly, memory.SyncStatus, "a memory that was never queued is untouched")
			continue
		}
		assert.Equal(t, store.SyncStatusSynced, memory.SyncStatus, "memory %s should read back as synced", memory.ID)
		synced++
	}
	assert.Equal(t, 5, synced, "every pushed memory reads back as synced")

	assert.NotContains(t, logs.String(), testPlaneKey)
}

func TestFlushLeavesMemoriesPendingWhenThePlaneRefuses(t *testing.T) {
	logs := captureLogs(t)
	server, received := newPlaneStub(t, http.StatusBadGateway)
	syncer := newTestSyncer(server.URL)
	st := newTestStore(t)
	ctx := context.Background()

	seedPending(t, st, 5)
	syncer.flush(ctx, st)

	nextPush(t, received)
	assert.Empty(t, received, "a refused batch is not retried inside the same interval")

	count, err := st.CountPendingSync(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5, count, "the rows stay pending so the next interval tries again")

	assert.Contains(t, logs.String(), "memories stay pending")
	assert.NotContains(t, logs.String(), testPlaneKey)
}

func TestFlushDrainsInBatchesOfTheConfiguredBatchSize(t *testing.T) {
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)
	syncer.batchSize = 2
	st := newTestStore(t)
	ctx := context.Background()

	ids := seedPending(t, st, 5)
	syncer.flush(ctx, st)

	assert.Equal(t, []string{ids[0], ids[1]}, idsOf(nextPush(t, received).body.Memories))
	assert.Equal(t, []string{ids[2], ids[3]}, idsOf(nextPush(t, received).body.Memories))
	assert.Equal(t, []string{ids[4]}, idsOf(nextPush(t, received).body.Memories), "the last batch is short, not padded")

	count, err := st.CountPendingSync(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestFlushAbandonsTheOldestWhenTheBacklogIsOverTheHardLimit(t *testing.T) {
	logs := captureLogs(t)
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)
	syncer.backlogWarn = 2
	syncer.backlogMax = 3
	st := newTestStore(t)
	ctx := context.Background()

	ids := seedPending(t, st, 5)
	syncer.flush(ctx, st)

	assert.Equal(t, []string{ids[2], ids[3], ids[4]}, idsOf(nextPush(t, received).body.Memories),
		"the newest memories inside the limit are still pushed")

	pending, err := st.PendingSync(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)

	memories, err := st.GetRecent(ctx, testSession, 10)
	require.NoError(t, err)
	byID := indexByID(memories)
	require.Len(t, byID, 5, "abandoning a memory must not delete it")
	assert.Equal(t, store.SyncStatusLocalOnly, byID[ids[0]].SyncStatus, "the overflow is abandoned, not deleted")
	assert.Equal(t, store.SyncStatusLocalOnly, byID[ids[1]].SyncStatus)
	assert.Equal(t, store.SyncStatusSynced, byID[ids[2]].SyncStatus)

	assert.Contains(t, logs.String(), "backlog over the hard limit")
	assert.NotContains(t, logs.String(), testPlaneKey)
}

func TestFlushWarnsOnALargeBacklogBelowTheHardLimit(t *testing.T) {
	logs := captureLogs(t)
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)
	syncer.backlogWarn = 2
	st := newTestStore(t)
	ctx := context.Background()

	seedPending(t, st, 5)
	syncer.flush(ctx, st)

	nextPush(t, received)
	assert.Contains(t, logs.String(), "sync backlog high")
	assert.Contains(t, logs.String(), "pending=5")
	assert.NotContains(t, logs.String(), "backlog over the hard limit")
	assert.NotContains(t, logs.String(), testPlaneKey)
}

// failingAckStore is a PendingStore whose acknowledgement always fails, so the
// "the plane has it but the local rows do not say so" path can be exercised
// without a database that can be made to fail on demand.
type failingAckStore struct {
	entries []store.MemoryEntry
}

func (f *failingAckStore) PendingSync(context.Context, int) ([]store.MemoryEntry, error) {
	return f.entries, nil
}

func (f *failingAckStore) MarkSynced(context.Context, []string) error {
	return fmt.Errorf("disk is full")
}

func (f *failingAckStore) CountPendingSync(context.Context) (int, error) {
	return len(f.entries), nil
}

func (f *failingAckStore) DropOldestPendingSync(context.Context, int) (int, error) { return 0, nil }

func TestFlushReportsAnAcknowledgementFailure(t *testing.T) {
	logs := captureLogs(t)
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)
	st := &failingAckStore{entries: []store.MemoryEntry{pendingEntry("a", time.Now())}}

	syncer.flush(context.Background(), st)

	nextPush(t, received)
	assert.Contains(t, logs.String(), "could not be marked synced")
	assert.NotContains(t, logs.String(), testPlaneKey)
}

func TestRunBackgroundFlushesUntilTheContextIsCancelled(t *testing.T) {
	server, received := newPlaneStub(t, http.StatusOK)
	syncer := newTestSyncer(server.URL)
	syncer.interval = 20 * time.Millisecond
	st := newTestStore(t)

	ids := seedPending(t, st, 3)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		syncer.RunBackground(ctx, st)
		close(stopped)
	}()

	// The flusher's first pass happens immediately, so this does not wait an
	// interval to observe the first push.
	assert.Equal(t, ids, idsOf(nextPush(t, received).body.Memories))

	require.Eventually(t, func() bool {
		count, err := st.CountPendingSync(context.Background())
		return err == nil && count == 0
	}, 5*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunBackground did not return after its context was cancelled")
	}

	// A missing store is a no-op, not a nil-pointer panic in a goroutine.
	syncer.RunBackground(context.Background(), nil)
}
