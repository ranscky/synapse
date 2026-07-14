// Package session infers a stable session ID from repeated, growing
// messages[] arrays sent by stateless AI API clients. No client changes
// required: we fingerprint the conversation itself.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"synapse/internal/trace"
)

// Message is the minimal shape we need out of an incoming request body.
// Content is kept as raw JSON since it may be a string or a content-block
// array depending on provider.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type Session struct {
	ID         string
	Messages   []Message
	TaskIntent string // filled in later by internal/classifier, optional here
	CreatedAt  time.Time
	LastActive time.Time
	LastTrace  *trace.TraceManifest
}

type Manager struct {
	mu sync.Mutex
	// bucket: hash of first user message -> candidate session IDs.
	// Multiple sessions can share a first message (e.g. same system
	// prompt template), so this is a bucket, not a 1:1 map.
	buckets map[string][]string
	byID    map[string]*Session
	ttl     time.Duration
}

func NewManager(ttl time.Duration) *Manager {
	m := &Manager{
		buckets: make(map[string][]string),
		byID:    make(map[string]*Session),
		ttl:     ttl,
	}
	go m.reapLoop()
	return m
}

// Identify returns the session ID for this message array, creating a new
// session if no existing one matches. isNew reports whether it was created.
func (m *Manager) Identify(messages []Message) (id string, isNew bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(messages) == 0 {
		return m.newSession(messages), true
	}

	key := firstUserMessageHash(messages)
	now := time.Now()

	for _, candidateID := range m.buckets[key] {
		s, ok := m.byID[candidateID]
		if !ok {
			continue
		}
		if isPrefixOf(s.Messages, messages) {
			s.Messages = messages
			s.LastActive = now
			return s.ID, false
		}
	}

	return m.newSession(messages), true
}

// must be called with m.mu held
func (m *Manager) newSession(messages []Message) string {
	id := newSessionID()
	now := time.Now()
	m.byID[id] = &Session{
		ID:         id,
		Messages:   messages,
		CreatedAt:  now,
		LastActive: now,
	}
	if len(messages) > 0 {
		key := firstUserMessageHash(messages)
		m.buckets[key] = append(m.buckets[key], id)
	}
	return id
}

// List returns all live sessions, most-recently-active first.
// Used by internal/api for GET /api/sessions.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*Session, 0, len(m.byID))
	for _, s := range m.byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActive.After(out[j].LastActive)
	})
	return out
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byID[id]
	return s, ok
}

func (m *Manager) reapLoop() {
	t := time.NewTicker(m.ttl / 2)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		cutoff := time.Now().Add(-m.ttl)
		for id, s := range m.byID {
			if s.LastActive.Before(cutoff) {
				delete(m.byID, id)
				// leave bucket entries; harmless dangling refs,
				// filtered out by the `ok` check in Identify.
			}
		}
		m.mu.Unlock()
	}
}

func isPrefixOf(stored, incoming []Message) bool {
	if len(stored) == 0 || len(stored) >= len(incoming) {
		// stored must be a strict prefix (incoming has at least one new turn)
		if len(stored) == len(incoming) {
			return equalMessages(stored, incoming)
		}
		return false
	}
	for i := range stored {
		if stored[i].Role != incoming[i].Role {
			return false
		}
		if string(stored[i].Content) != string(incoming[i].Content) {
			return false
		}
	}
	return true
}

func equalMessages(a, b []Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || string(a[i].Content) != string(b[i].Content) {
			return false
		}
	}
	return true
}

func newSessionID() string {
	return uuid.NewString()
}

func firstUserMessageHash(messages []Message) string {
	for _, m := range messages {
		if m.Role == "user" {
			sum := sha256.Sum256(m.Content)
			return hex.EncodeToString(sum[:])
		}
	}
	sum := sha256.Sum256(messages[0].Content)
	return hex.EncodeToString(sum[:])
}

// SetTrace attaches the most recently compiled trace to a session, so the
// inspector can fetch it via GET /api/sessions/{id}/trace. In-memory only,
// consistent with "traces are in-memory only by default" -- this does not
// touch --persist-traces disk storage at all.
func (m *Manager) SetTrace(id string, tr *trace.TraceManifest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.byID[id]; ok {
		s.LastTrace = tr
	}
}

// SetIntent records the classified task intent (debug/plan/code/write/
// generic) for a session, so the inspector sidebar can show it without
// needing to re-fetch and inspect the full trace.
func (m *Manager) SetIntent(id string, intent string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.byID[id]; ok {
		s.TaskIntent = intent
	}
}