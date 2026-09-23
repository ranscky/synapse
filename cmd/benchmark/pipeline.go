// The compile pipeline every scenario is measured with: classify -> score ->
// deduplicate -> fill the token budget.
//
// It is deliberately the same four steps in the same order the v1 compile path
// runs, over candidate pools that differ per scenario:
//
//	scenarios 1 and 2  the fixture's own messages, embedded with the real model
//	                   (the stand-in for a session whose memories are already stored)
//	scenario 3         the candidates the control plane answers with (the Global Brain)
//
// Extracted from the command's original single-scenario body so the three
// scenarios cannot drift into three slightly different pipelines.

package main

import (
	"context"
	"fmt"
	"time"

	"synapse/internal/budget"
	"synapse/internal/classifier"
	"synapse/internal/config"
	"synapse/internal/dedup"
	"synapse/internal/scorer"
	"synapse/internal/store"
)

// textEmbedder is the one method this command needs from internal/embedder.
//
// Declared here rather than taken as embedder.Embedder so a test can inject a
// deterministic embedder and measure the pipeline's arithmetic without an ONNX
// session -- the same reason internal/retrieval declares its own.
type textEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// pipeline holds what every scenario shares: one configuration, and one
// embedder (loading the ONNX model once per run rather than once per scenario).
type pipeline struct {
	cfg      *config.Config
	embedder textEmbedder
}

// sessionIntent is what a compile scores against: the classified intent, and the
// query embedding that drove it.
//
// It is a value passed around rather than recomputed because scenario 3 needs
// the query embedding before the compile runs: the same vector is what it sends
// to the control plane to pull candidates, so the memories retrieved and the
// memories scored are compared against exactly one query.
type sessionIntent struct {
	Text       string
	Intent     classifier.Intent
	Confidence float64
	Embedding  []float32
}

// compileResult is one compile's numbers, and everything printed about it.
type compileResult struct {
	// RawTokens is the whole conversation as it would be sent without Synapse.
	RawTokens int
	// CompiledTokens is what the compile actually sends: selected memories plus
	// the pinned system prompt.
	CompiledTokens int
	// SystemTokens is the pinned system prompt, counted separately because it
	// never competes for the budget.
	SystemTokens int
	// Budget is the configured token ceiling.
	Budget int
	// Candidates, AfterDedup, and Selected are the three pool sizes the compile
	// moved through.
	Candidates int
	AfterDedup int
	Selected   []scorer.ScoredMemory
	// Deduped is the pool the budget walked, selected and excluded alike.
	Deduped []scorer.ScoredMemory
	// Intent and Confidence are the classification the scoring used.
	Intent     string
	Confidence float64
}

// Reduction is the reduction this compile achieved, as a percentage.
func (r compileResult) Reduction() float64 {
	return reductionPct(r.RawTokens, r.CompiledTokens)
}

// newEmptyStore opens the fresh in-memory store a fixture scenario runs against.
//
// It is honest about what it is: an empty database. The v1 pipeline scores a
// fixture's own messages directly, so this store holds nothing either way -- and
// that is precisely the cold-start condition scenario 2 measures: with no prior
// memories to draw from, there is nothing to remove.
func newEmptyStore() (*store.Store, error) {
	st, err := store.NewStore(":memory:")
	if err != nil {
		return nil, fmt.Errorf("failed to create store: %w", err)
	}
	return st, nil
}

// buildMemories turns messages into a scorer input pool, with real per-message
// embeddings.
//
// idPrefix namespaces the memory ids: scenarios 1 and 2 pass "" and keep the
// original "mem-N" ids, while agent_a's Global Brain sessions pass their session
// id so two sessions can never collide on one id (the plane derives its row uuid
// from this string, which also makes pushing the same session twice a no-op
// rather than a duplicate).
func (p *pipeline) buildMemories(ctx context.Context, messages []Message, sessionID, idPrefix string) ([]store.MemoryEntry, error) {
	memories := make([]store.MemoryEntry, 0, len(messages))
	for i, msg := range messages {
		embedding, err := p.embedder.Embed(ctx, msg.Content)
		if err != nil {
			return nil, fmt.Errorf("failed to embed message %d: %w", i, err)
		}

		memories = append(memories, store.MemoryEntry{
			ID:         fmt.Sprintf("%smem-%d", idPrefix, i),
			SessionID:  sessionID,
			Content:    msg.Content,
			MemoryType: classifyMemType(msg.Content),
			Embedding:  embedding,
			Timestamp:  time.Now().Add(-time.Duration(i) * time.Hour), // Older memories get lower recency scores
		})
	}

	return memories, nil
}

// classifyIntent classifies a conversation's intent and embeds the same text.
//
// The text is the last two scorable messages, system prompt excluded, because
// that is the conversational signal -- a system prompt is pinned context, not
// something the user is currently asking for. The query embedding is real, from
// the same model that embedded the candidates, so semantic similarity is scored
// against something representative of the task rather than an arbitrary vector.
func (p *pipeline) classifyIntent(ctx context.Context, messages []Message) (sessionIntent, error) {
	_, scorable := splitScorable(messages)

	intentText := ""
	if len(scorable) > 0 {
		intentText = scorable[len(scorable)-1].Content
		if len(scorable) > 1 {
			intentText = scorable[len(scorable)-2].Content + " " + intentText
		}
	}

	classification := classifier.Classify(intentText)

	queryEmbedding, err := p.embedder.Embed(ctx, intentText)
	if err != nil {
		return sessionIntent{}, fmt.Errorf("failed to embed query: %w", err)
	}

	return sessionIntent{
		Text:       intentText,
		Intent:     classification.Intent,
		Confidence: classification.Confidence,
		Embedding:  queryEmbedding,
	}, nil
}

// compile scores, deduplicates, and budgets a caller-supplied candidate pool.
func (p *pipeline) compile(ctx context.Context, messages []Message, candidates []store.MemoryEntry) (compileResult, error) {
	intent, err := p.classifyIntent(ctx, messages)
	if err != nil {
		return compileResult{}, err
	}
	return p.compileWithIntent(ctx, messages, candidates, intent)
}

// compileWithIntent is compile for a caller that already classified the
// conversation -- scenario 3, whose candidate pull has to happen before this
// runs and must use the same query vector.
func (p *pipeline) compileWithIntent(ctx context.Context, messages []Message, candidates []store.MemoryEntry, intent sessionIntent) (compileResult, error) {
	systemTokens, _ := splitScorable(messages)

	weights := scorer.Weights{
		SemanticSimilarity: p.cfg.WeightSemanticSimilarity,
		Recency:            p.cfg.WeightRecency,
		Importance:         p.cfg.WeightImportance,
		TaskAlignment:      p.cfg.WeightTaskAlignment,
	}

	scorerInstance := scorer.NewScorer(weights, intent.Intent, intent.Confidence, time.Now())

	scoredMemories := scorerInstance.Score(ctx, intent.Embedding, candidates)

	dedupInstance := dedup.Deduplicate(scoredMemories, p.cfg.DeduplicationThreshold)

	// Reserve budget for the pinned system prompt before filling the rest of
	// the budget with scored conversational history.
	remainingBudget := p.cfg.TokenBudget - systemTokens
	if remainingBudget < 0 {
		remainingBudget = 0
	}

	selectedMemories, tokensUsed := budget.Fill(dedupInstance, remainingBudget)

	return compileResult{
		RawTokens:      countTokens(messages),
		CompiledTokens: tokensUsed + systemTokens,
		SystemTokens:   systemTokens,
		Budget:         p.cfg.TokenBudget,
		Candidates:     len(candidates),
		AfterDedup:     len(dedupInstance),
		Selected:       selectedMemories,
		Deduped:        dedupInstance,
		Intent:         string(intent.Intent),
		Confidence:     intent.Confidence,
	}, nil
}

// runFixtureScenario is scenarios 1 and 2: one fixture compiled against its own
// messages, over a fresh empty store. That is the original benchmark's behavior,
// unchanged.
func (p *pipeline) runFixtureScenario(ctx context.Context, messages []Message) (compileResult, error) {
	st, err := newEmptyStore()
	if err != nil {
		return compileResult{}, err
	}
	defer st.Close()

	_, scorable := splitScorable(messages)

	candidates, err := p.buildMemories(ctx, scorable, "benchmark-session", "")
	if err != nil {
		return compileResult{}, err
	}

	return p.compile(ctx, messages, candidates)
}

// printDetail writes the per-scenario working: what was selected, what lost, and
// the pool sizes behind the number.
//
// maxExcluded bounds the excluded list so a Global Brain pool of fifty
// candidates cannot bury the summary; scenarios 1 and 2 pass -1 for the original
// unbounded output.
func (r compileResult) printDetail(maxExcluded int) {
	for _, sm := range r.Selected {
		fmt.Printf("  selected: %s — %.40s...\n", sm.ID, sm.Content)
	}

	selectedIDs := make(map[string]bool, len(r.Selected))
	for _, sm := range r.Selected {
		selectedIDs[sm.ID] = true
	}
	printed := 0
	for _, sm := range r.Deduped {
		if selectedIDs[sm.ID] {
			continue
		}
		if maxExcluded >= 0 && printed >= maxExcluded {
			break
		}
		fmt.Printf("  excluded: %s (T=%.3f S=%.3f total=%.3f) — %.40s...\n",
			sm.ID, sm.ScoreT, sm.ScoreS, sm.Total, sm.Content)
		printed++
	}

	fmt.Printf("System prompt tokens (pinned): %d\n", r.SystemTokens)
	fmt.Printf("Candidates retrieved: %d\n", r.Candidates)
	fmt.Printf("After deduplication: %d\n", r.AfterDedup)
	fmt.Printf("Final selected: %d\n", len(r.Selected))
	fmt.Printf("Tokens used: %d\n", r.CompiledTokens)
	fmt.Printf("Token budget: %d\n", r.Budget)
	fmt.Printf("Detected intent: %s (confidence: %.2f)\n", r.Intent, r.Confidence)
}
