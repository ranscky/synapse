//go:build integration

// Phase 29's mock model server: the upstream an edge node proxies its LLM traffic
// to, and the record of what arrived there.
//
// It is a mock because no test can call a real model provider, and it is the only
// mock in this phase's scenario. Everything on the Synapse side of it -- both edge
// nodes, the control plane, the database -- is real, so what this file lets the
// test assert is a property of the real path: what an edge node sends to the model
// API, headers included.
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// multiagentUpstreamRequest is one request the mock model server received: its
// path, every header it arrived with, and its body.
type multiagentUpstreamRequest struct {
	path    string
	headers http.Header
	body    string
}

// multiagentUpstream is the mock LLM every edge node is configured with.
//
// It records what it received rather than what it answered, because that is what
// the header assertion is about: an edge node holds a control-plane credential,
// and the one thing it must never do with it is spend it on the model API. The
// log lines it keeps name header *names* only -- a log that could hold a token
// value could not be evidence about tokens.
type multiagentUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []multiagentUpstreamRequest
	logs     []string
}

// multiagentStartUpstream starts the mock model server.
func multiagentStartUpstream(t *testing.T) *multiagentUpstream {
	t.Helper()

	u := &multiagentUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", u.handle)

	u.server = httptest.NewServer(mux)
	t.Cleanup(u.server.Close)

	return u
}

// URL returns the address the edge nodes are configured with.
func (u *multiagentUpstream) URL() string { return u.server.URL }

// handle answers OpenAI-shaped and echoes the request it was given.
//
// Both halves matter: the proxy captures the assistant's reply out of
// choices[0].message.content, so a server that answered a bare echo would produce
// no assistant memory at all, and echoed_request carries the request verbatim so
// the test can see what actually crossed the wire.
func (u *multiagentUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	u.mu.Lock()
	u.requests = append(u.requests, multiagentUpstreamRequest{path: r.URL.Path, headers: r.Header.Clone(), body: string(body)})
	u.logs = append(u.logs, fmt.Sprintf("upstream received %s %s headers=[%s]",
		r.Method, r.URL.Path, strings.Join(multiagentHeaderNames(r.Header), ",")))
	u.mu.Unlock()

	reply := "Echoing the last user turn: " + multiagentLastUserMessage(body)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":             "multiagent-echo",
		"object":         "chat.completion",
		"created":        time.Now().Unix(),
		"echoed_request": string(body),
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": reply},
			"finish_reason": "stop",
		}},
	})
}

// snapshot returns the requests and log lines received so far, copied out under
// the lock so an assertion never races a live handler.
func (u *multiagentUpstream) snapshot() ([]multiagentUpstreamRequest, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]multiagentUpstreamRequest(nil), u.requests...), append([]string(nil), u.logs...)
}

// multiagentHeaderNames returns the header names of h, sorted, with no values.
func multiagentHeaderNames(h http.Header) []string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// multiagentLastUserMessage pulls the last user turn out of an OpenAI-shaped
// request body, so the mock's reply is about something. A body it cannot read is
// answered with an empty quote rather than an error: the upstream is not the thing
// under test.
func multiagentLastUserMessage(body []byte) string {
	var decoded struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ""
	}

	last := ""
	for _, message := range decoded.Messages {
		if message.Role == "user" {
			last = message.Content
		}
	}

	return last
}
