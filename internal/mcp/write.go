// The write half of the Global Brain: synapse_write_memory stores one memory the
// caller hands it, through the same sanitization pipeline every other write in
// this process goes through, and answers with the two things a caller cannot know
// on its own -- whether the content had to be rewritten, and whether the memory it
// just stored contradicts one that was already there.
//
// Split out of server.go on the same seam that put compile.go and search.go where
// they are: a tool's definition, handler, and argument validation live in their own
// file, so registerTools stays a table of what this server offers rather than a
// place where tool logic accumulates.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"synapse/internal/api"
	"synapse/internal/scorer"
	"synapse/internal/store"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const (
	// writeToolName is the tool an MCP client calls to store a memory.
	writeToolName = "synapse_write_memory"

	// writeToolDescription is the text tools/list advertises. It names the half of
	// this tool that a plain insert does not have: a caller learns whether what it
	// wrote disagrees with what is already stored, which is the one thing it cannot
	// see from its own side.
	writeToolDescription = "Write a memory directly to the Global Brain with automatic conflict detection against existing memories."

	// defaultWriteSessionID is the session a write lands in when the caller names
	// none: the same "default-session" fallback internal/api's extractSessionID and
	// internal/proxy's session extraction already use when nothing identifies a
	// session. It is a literal there and here because neither is exported, and a
	// write cannot land in no session on a backend whose reads are session-scoped.
	defaultWriteSessionID = "default-session"
)

// writeArgs is the tool's input, decoded from the MCP arguments object.
type writeArgs struct {
	Content    string `json:"content"`
	MemoryType string `json:"memory_type"`
	Visibility string `json:"visibility"`
	SessionID  string `json:"session_id"`
}

// registerWriteTool registers synapse_write_memory and its handler.
func (s *Server) registerWriteTool() {
	s.mcp.AddTool(
		mcpgo.NewTool(writeToolName,
			mcpgo.WithDescription(writeToolDescription),
			// This tool writes and does not read, but it is not destructive:
			// every call inserts a new row under a freshly generated uuid, and no
			// call updates or deletes a row that is already stored (the local
			// backend only replaces on an id collision, which a new uuid cannot
			// cause, and the Postgres backend inserts ON CONFLICT (id) DO
			// NOTHING). It is not idempotent either -- two identical calls store
			// two memories under two ids -- so both hints are set explicitly
			// rather than left to mcp-go's defaults, which describe every tool as
			// destructive.
			mcpgo.WithReadOnlyHintAnnotation(false),
			mcpgo.WithDestructiveHintAnnotation(false),
			mcpgo.WithIdempotentHintAnnotation(false),
			mcpgo.WithOpenWorldHintAnnotation(false),
			mcpgo.WithString("content",
				mcpgo.Required(),
				mcpgo.Description("The memory to store. Sanitized before storage: null bytes are stripped, prompt-injection patterns are neutralized, and content is capped at 2048 bytes."),
			),
			mcpgo.WithString("memory_type",
				mcpgo.Required(),
				mcpgo.Description("One of \"decision\", \"fact\", \"error\", \"preference\", or \"context\". This is what the ranking's importance and task-alignment factors are keyed by, so it is not free text."),
			),
			mcpgo.WithString("visibility",
				mcpgo.Description("Who may read the memory: \"org\" (default) every agent in the tenant, \"team\" this node's team, \"private\" this agent in the session it was written in."),
			),
			mcpgo.WithString("session_id",
				mcpgo.Description("Session the memory belongs to. Omit for \"default-session\". A standalone node's memories are session-scoped, so a later synapse_search_memories must name the same session to find this one."),
			),
		),
		s.handleWriteMemory,
	)
}

// handleWriteMemory answers a synapse_write_memory call.
//
// The order is deliberate: validate the shape, sanitize with the store's own
// pipeline, embed, detect, store. Sanitizing first is what makes the stored row,
// the vector that points at it, and the text a contradiction was found in all
// describe the same memory -- the same reason PGStore.Write detects "on the text
// the row will actually hold" (pgstore.go).
func (s *Server) handleWriteMemory(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args, err := s.parseWriteArgs(req)
	if err != nil {
		return toolError(errorTypeInvalidParams, err.Error()), nil
	}
	if s.store == nil || s.embedder == nil {
		// Not the caller's mistake: this deployment was built without the two
		// things a write needs. Reported as a typed failure rather than a panic,
		// for the same reason a missing compile pipeline is -- a process that
		// starts must keep answering, including when one of its tools cannot.
		return toolError(errorTypeWriteFailed, "memory writes are not configured on this server"), nil
	}

	// One pattern list, in one place: store.Sanitize is the exported form of the
	// pipeline Store.Write itself runs, so this strips null bytes, neutralizes
	// prompt-injection patterns, and caps content at 2048 bytes on a UTF-8
	// boundary -- and running it here is what lets the answer say whether it had
	// to. The store sanitizes again when it stores, which is idempotent: neither
	// "[SANITIZED]" nor an already-capped prefix triggers anything the second
	// time.
	content := store.Sanitize(args.Content)
	sanitized := content != args.Content

	embedding, err := s.embedder.Embed(ctx, content)
	if err != nil {
		// Cause to the log (stderr, never the JSON-RPC stream), typed failure to
		// the caller: an embedding error can name a model path, and that does not
		// belong in a model's context.
		slog.Error("MCP memory write could not embed", "error", err)
		return toolError(errorTypeWriteFailed, "the memory could not be embedded; see the synapse MCP server log for the cause"), nil
	}

	entry := store.MemoryEntry{
		ID:         uuid.NewString(),
		SessionID:  args.SessionID,
		Content:    content,
		MemoryType: args.MemoryType,
		Timestamp:  time.Now().UTC(),
		Embedding:  embedding,
		// The writing agent and team are this node's own configured identity,
		// never anything the caller typed: an identity a caller can name is an
		// identity a caller can lie about, and on a plane-backed node agent_id is
		// part of what decides who may read the row afterwards.
		AgentID:    s.cfg.AgentID,
		TeamID:     s.cfg.TeamID,
		Visibility: args.Visibility,
	}

	// Detection runs before the insert, so the candidate set cannot contain the
	// memory being written and every candidate it returns is a memory this node
	// could already read.
	conflictWithID := s.conflictingMemoryID(ctx, entry, embedding)

	if err := s.store.Write(ctx, entry); err != nil {
		slog.Error("MCP memory write failed", "error", err, "memory_id", entry.ID)
		return toolError(errorTypeWriteFailed, "the memory could not be stored; see the synapse MCP server log for the cause"), nil
	}

	// The id, the type, and the two verdicts: a memory's content never reaches a
	// log line, and neither does the caller's session -- the memory id is what ties
	// this line to the row.
	slog.Info("MCP memory write",
		"memory_id", entry.ID,
		"memory_type", entry.MemoryType,
		"visibility", entry.Visibility,
		"sanitized", sanitized,
		"conflict_detected", conflictWithID != "",
	)

	payload, err := mcpgo.NewToolResultJSON(writeToolResult{
		ID:               entry.ID,
		ConflictDetected: conflictWithID != "",
		ConflictWithID:   conflictWithID,
		Sanitized:        sanitized,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: write result is not marshallable: %w", err)
	}

	return payload, nil
}

// parseWriteArgs decodes and validates the tool's arguments.
//
// The rules the REST front end applies to caller text are applied here too
// (api.ValidateMessageContent for the content, api.ValidateSessionID for the
// session) rather than reimplemented, because .clinerules requires the MCP surface
// to share the REST sanitization pipeline: content this tool stores must be content
// POST /v1/compile would also have accepted. The memory type is checked against
// internal/scorer's own constants -- the vocabulary the 4-Factor sieve's importance
// and task-alignment tables are keyed by -- so a type nothing can score is rejected
// here instead of being stored as a row whose two factors silently fall back to a
// generic value.
//
// An omitted visibility resolves to this node's configured default-visibility,
// which is what that key documents itself as (config.go: "applied to memories
// written with no visibility of their own") and which makes its own default, "org",
// the documented default scope. A hand-built Config with the field blank falls back
// to org rather than storing an empty scope string.
//
// An omitted session resolves to defaultWriteSessionID rather than to nothing: on a
// backend whose reads are session-scoped, a memory stored outside every session is
// a memory no search can reach.
func (s *Server) parseWriteArgs(req mcpgo.CallToolRequest) (writeArgs, error) {
	var args writeArgs
	if err := req.BindArguments(&args); err != nil {
		return args, fmt.Errorf("arguments must be an object with content, memory_type, and an optional visibility and session_id: %w", err)
	}

	if strings.TrimSpace(args.Content) == "" {
		return args, errors.New("content is required and must not be empty")
	}
	if err := api.ValidateMessageContent(args.Content); err != nil {
		return args, fmt.Errorf("content: %w", err)
	}

	if !validMemoryType(args.MemoryType) {
		return args, fmt.Errorf("memory_type must be %q, %q, %q, %q, or %q",
			scorer.Decision, scorer.Fact, scorer.Error, scorer.Preference, scorer.Context)
	}

	if args.Visibility == "" {
		args.Visibility = s.cfg.DefaultVisibility
	}
	if args.Visibility == "" {
		args.Visibility = store.VisibilityOrg
	}
	switch args.Visibility {
	case store.VisibilityOrg, store.VisibilityTeam, store.VisibilityPrivate:
	default:
		return args, fmt.Errorf("visibility must be %q, %q, or %q",
			store.VisibilityOrg, store.VisibilityTeam, store.VisibilityPrivate)
	}

	if args.SessionID == "" {
		args.SessionID = defaultWriteSessionID
	}
	if err := api.ValidateSessionID(args.SessionID); err != nil {
		return args, err
	}

	return args, nil
}

// validMemoryType reports whether t is one of the five memory types this project
// has. The list is internal/scorer's own constants rather than a second spelling of
// the five strings: those constants key the importance and task-alignment tables,
// so a type outside them is a memory whose two factors are defaults nothing chose.
func validMemoryType(t string) bool {
	switch scorer.MemoryType(t) {
	case scorer.Decision, scorer.Error, scorer.Fact, scorer.Preference, scorer.Context:
		return true
	}
	return false
}
