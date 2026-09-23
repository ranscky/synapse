// Scenario 3: the Global Brain.
//
// Two agents, one shared brain, over real HTTP. agent_a works through five
// sequential sessions and pushes what it learned to a running control plane
// (POST /v2/sync/memories); agent_b then compiles a session of its own against
// the memories the plane answers with (GET /v2/memories/search). Nothing here is
// faked: the candidates agent_b scores are rows in a tenant's PostgreSQL schema
// that agent_a's push put there, retrieved through the same two calls cmd/synapse
// wires into a real edge node.
//
// This file also owns the scenario's framing, because the number it prints
// depends entirely on what agent_a pushed -- and the honest thing to do with a
// benchmark number is to say which pool produced it.

package main

import (
	"context"
	"fmt"
	"time"

	"synapse/internal/config"
	"synapse/internal/store"
	"synapse/internal/sync"
)

const (
	// globalBrainDistilled is the default push policy: one memory per session,
	// the same last-user-message write-back synapse_compile performs, so the
	// brain receives what the product itself keeps rather than a transcript.
	globalBrainDistilled = "distilled"

	// globalBrainFull pushes every memory of every session, which is what a
	// proxying edge node queues per turn. It is the higher-fidelity pool.
	globalBrainFull = "full"

	// globalBrainTarget is the reduction this scenario is expected to clear.
	globalBrainTarget = 55.0

	// sessionAFormat names agent_a's sessions; the index makes each one a
	// distinct session the plane stores under its own session_id.
	sessionAFormat = "benchmark-agent-a-session-%d"

	// sessionB is agent_b's session id in every candidate pull.
	sessionB = "benchmark-agent-b-session"
)

// globalBrainOptions is the validated --plane configuration.
//
// APIKey is a secret: it is presented to the plane and never printed, logged, or
// included in an error -- the same rule the plane's own binaries hold to.
type globalBrainOptions struct {
	URL      string
	APIKey   string
	AgentA   string
	AgentB   string
	Mode     string
	Sessions int
}

// enabled reports whether scenario 3 should run at all.
func (o globalBrainOptions) enabled() bool { return o.URL != "" }

// globalBrainOptionsFromFlags validates the Global Brain flags.
//
// Without --plane the scenario is skipped and nothing below matters, so the only
// error that can fire with a blank URL is a credential given with no plane to
// present it to -- a typo worth failing on rather than silently ignoring. A blank
// --api-key with --plane set is a usage error rather than an endless 401.
func globalBrainOptionsFromFlags(planeURL, apiKey, agentA, agentB, mode string, sessions int) (globalBrainOptions, error) {
	if planeURL == "" {
		if apiKey != "" {
			return globalBrainOptions{}, fmt.Errorf("benchmark: --api-key requires --plane")
		}
		return globalBrainOptions{}, nil
	}

	if apiKey == "" {
		return globalBrainOptions{}, fmt.Errorf("benchmark: --api-key is required with --plane (the tenant credential provisioning returned)")
	}

	switch mode {
	case "", globalBrainDistilled:
		mode = globalBrainDistilled
	case globalBrainFull:
	default:
		return globalBrainOptions{}, fmt.Errorf("benchmark: --global-brain must be %q or %q", globalBrainDistilled, globalBrainFull)
	}

	if agentA == "" || agentB == "" {
		return globalBrainOptions{}, fmt.Errorf("benchmark: --agent-a and --agent-b must both name an agent")
	}
	if agentA == agentB {
		return globalBrainOptions{}, fmt.Errorf("benchmark: --agent-a and --agent-b must differ: one agent reading another's memory is the scenario")
	}
	if sessions < 1 {
		return globalBrainOptions{}, fmt.Errorf("benchmark: --global-sessions must be at least 1")
	}

	return globalBrainOptions{
		URL:      planeURL,
		APIKey:   apiKey,
		AgentA:   agentA,
		AgentB:   agentB,
		Mode:     mode,
		Sessions: sessions,
	}, nil
}

// runGlobalBrain is scenario 3.
//
// establishedReduction is scenario 1's measured reduction, taken from the same
// run and the same fixture rather than remembered from a README: the uplift line
// is only honest if both halves were measured by this process.
func runGlobalBrain(ctx context.Context, p *pipeline, fixture *Session, establishedReduction float64, opts globalBrainOptions) error {
	// agent_a's sequential sessions, in order, covering the fixture exactly once
	// -- the same work an established agent would have done over a day.
	_, scorable := splitScorable(fixture.Messages)
	sessions := splitSequential(scorable, opts.Sessions)

	fmt.Printf("agent_a: %d sequential sessions -> %s\n", len(sessions), opts.URL)
	pushed, err := pushAgentSessions(ctx, p, opts, sessions)
	if err != nil {
		return err
	}
	fmt.Printf("agent_a: %d memories on the Global Brain at visibility=org\n", pushed)

	// One query vector for both halves: what agent_b asks the brain for and what
	// it then scores are the same question, embedded by the same model.
	intent, err := p.classifyIntent(ctx, fixture.Messages)
	if err != nil {
		return err
	}

	candidates, err := pullGlobalBrain(ctx, p, opts, intent)
	if err != nil {
		return err
	}
	fmt.Printf("agent_b: pulled %d candidates from the Global Brain\n", len(candidates))

	result, err := p.compileWithIntent(ctx, fixture.Messages, candidates, intent)
	if err != nil {
		return err
	}
	result.printDetail(3)

	printGlobalBrainSummary(result, len(candidates), establishedReduction, opts)

	return nil
}

// pushAgentSessions pushes each of agent_a's sessions as one batch.
//
// One batch per session rather than one batch of everything, because a failure
// should name the session that failed. Push's contract is all-or-nothing per
// batch, so a batch that fails leaves nothing half-accepted and the returned
// count is exactly what the plane holds.
func pushAgentSessions(ctx context.Context, p *pipeline, opts globalBrainOptions, sessions [][]Message) (int, error) {
	agentA := sync.NewSyncer(config.Config{
		ControlPlaneURL:    opts.URL,
		ControlPlaneAPIKey: opts.APIKey,
		AgentID:            opts.AgentA,
	})

	pushed := 0
	for i, chunk := range sessions {
		sessionID := fmt.Sprintf(sessionAFormat, i+1)

		entries, err := agentSessionMemories(ctx, p, chunk, sessionID, opts.Mode)
		if err != nil {
			return pushed, err
		}
		if len(entries) == 0 {
			continue
		}

		if err := agentA.Push(ctx, entries); err != nil {
			return pushed, fmt.Errorf("benchmark: %s pushing session %d: %w", opts.AgentA, i+1, err)
		}

		pushed += len(entries)
		fmt.Printf("  pushed %s session %d: %d memory(ies)\n", opts.AgentA, i+1, len(entries))
	}

	return pushed, nil
}

// agentSessionMemories builds what one of agent_a's sessions contributes to the
// brain, under the two push policies.
func agentSessionMemories(ctx context.Context, p *pipeline, chunk []Message, sessionID, mode string) ([]store.MemoryEntry, error) {
	if mode == globalBrainFull {
		entries, err := p.buildMemories(ctx, chunk, sessionID, sessionID+"-")
		if err != nil {
			return nil, err
		}
		return orgScoped(entries), nil
	}

	// distilled: one memory per session, the write-back synapse_compile itself
	// performs, so the pool is what the product would have kept.
	content := lastUserMessage(chunk)
	if content == "" {
		return nil, nil
	}

	embedding, err := p.embedder.Embed(ctx, content)
	if err != nil {
		return nil, fmt.Errorf("failed to embed the write-back memory of %s: %w", sessionID, err)
	}

	return orgScoped([]store.MemoryEntry{{
		ID:         sessionID + "-writeback",
		SessionID:  sessionID,
		Content:    content,
		MemoryType: classifyMemType(content),
		Embedding:  embedding,
		Timestamp:  time.Now(),
	}}), nil
}

// orgScoped stamps visibility=org on a batch.
//
// A private memory is readable only by the agent that wrote it, in the session it
// wrote it in, so a cross-agent compile could never contain one -- which is the
// whole point of the scenario. The plane normalizes a blank visibility to org as
// well; naming it here keeps the intent legible on the wire.
func orgScoped(entries []store.MemoryEntry) []store.MemoryEntry {
	for i := range entries {
		entries[i].Visibility = store.VisibilityOrg
	}
	return entries
}

// pullGlobalBrain reads agent_b's candidate pool from the plane.
//
// The credential is the client's half of the protocol: the plane decides what
// this agent may read from the token's claims, never from the request body. A
// failure here fails the scenario rather than falling back to local search --
// the edge's production fallback is right for a live compile and wrong for a
// benchmark, because a number measured against the local store while claiming to
// measure the Global Brain would be worse than no number.
func pullGlobalBrain(ctx context.Context, p *pipeline, opts globalBrainOptions, intent sessionIntent) ([]store.MemoryEntry, error) {
	agentB := sync.NewSyncer(config.Config{
		ControlPlaneURL:    opts.URL,
		ControlPlaneAPIKey: opts.APIKey,
		AgentID:            opts.AgentB,
	})

	candidates, err := agentB.PullCandidates(ctx, intent.Embedding, sessionB, p.cfg.RetrievalCandidateK)
	if err != nil {
		return nil, fmt.Errorf("benchmark: %s pulling candidates from %s: %w", opts.AgentB, opts.URL, err)
	}

	return candidates, nil
}

// printGlobalBrainSummary prints the scenario's mandated lines and the framing
// that makes them readable.
//
// The framing is not decoration: a reduction is only as meaningful as the raw
// text it is measured against and the pool it was compiled from, and here both
// are choices (--global-brain and the fixture), so both are printed with the
// number rather than left in a README.
func printGlobalBrainSummary(result compileResult, poolSize int, establishedReduction float64, opts globalBrainOptions) {
	if opts.Mode == globalBrainFull {
		fmt.Printf("framing: mode=full — %s pushed every memory of its sessions (%d in the pool, budget %d).\n",
			opts.AgentA, poolSize, result.Budget)
		fmt.Println("framing: the pool is larger than the budget, so the compile fills the budget — reduction is budget-bound, not sync-bound.")
	} else {
		fmt.Printf("framing: mode=distilled — %s pushed one memory per session (%d in the pool, budget %d).\n",
			opts.AgentA, poolSize, result.Budget)
		fmt.Println("framing: that memory is the last-user-message write-back synapse_compile performs, so the pool is what the product itself keeps.")
	}
	fmt.Printf("framing: Raw is %s's own session; Compiled is what it sends after compiling against the Global Brain.\n", opts.AgentB)
	fmt.Println("framing: the two sides are not the same text — this measures cross-agent recall with scenarios 1 and 2's arithmetic.")

	reduction := result.Reduction()
	fmt.Printf("v2 Global Brain: Raw: %d | Compiled: %d | Reduction: %.1f%% (target ≥%.0f%%)\n",
		result.RawTokens, result.CompiledTokens, reduction, globalBrainTarget)
	fmt.Printf("Global Brain uplift: %+.1f%% vs v1 established\n", reduction-establishedReduction)

	if reduction < globalBrainTarget {
		fmt.Printf("WARNING: Global Brain target missed (%.1f%%) — check weights and sync\n", reduction)
	} else {
		fmt.Printf("OK: Global Brain reduction (%.1f%%) meets target (≥%.0f%%).\n", reduction, globalBrainTarget)
	}
}
