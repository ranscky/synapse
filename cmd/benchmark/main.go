// Command benchmark measures what Synapse's compile pipeline removes from a
// conversation, in three scenarios printed side by side with the framing each
// one deserves:
//
//	v1 established  an established multi-session conversation -- the headline number
//	v1 cold-start   a brand-new session with no prior memories -- small by design
//	v2 Global Brain two agents, one shared control plane -- needs --plane
//
// Scenarios 1 and 2 always run, against fixtures and a fresh in-memory store.
// Scenario 3 runs only when --plane is given with a tenant credential, because
// it talks to a real control plane over real HTTP; when it is not given, the run
// stays completely local.
//
// Every number is measured rather than asserted: a scenario below its target
// prints a warning naming what to check, and the framing lines say which
// candidate pool the number came from.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"synapse/internal/config"
	"synapse/internal/embedder"
)

const (
	// onnxModelPath is named so the hash-embedder fallback warning can point at
	// the file it did not find.
	onnxModelPath = "models/all-MiniLM-L6-v2/model.onnx"

	// v1EstablishedTarget is scenario 1's target: the number the README's
	// headline claim rests on.
	v1EstablishedTarget = 40.0

	// expectedColdStartReduction is what scenario 2 is expected to show. It is a
	// documented expectation rather than a target, because a session with no
	// prior memories has almost nothing to compress yet.
	expectedColdStartReduction = 15.0
)

func main() {
	// budgetOverride: -1 means "not set, use config default"
	budgetFlag := flag.Int("budget", -1, "Override token budget (defaults to config value)")
	planeFlag := flag.String("plane", "", "Control plane URL, e.g. http://127.0.0.1:9090. Runs the Global Brain scenario when set.")
	apiKeyFlag := flag.String("api-key", "", "Tenant credential (the jwt provisioning returned) presented to the control plane. Required with --plane; never printed.")
	agentAFlag := flag.String("agent-a", "agent_a", "Agent id the Global Brain sessions are pushed as")
	agentBFlag := flag.String("agent-b", "agent_b", "Agent id that compiles against the Global Brain")
	coldFlag := flag.String("cold-session", "testdata/session_code.json", "Session fixture for the cold-start scenario")
	sessionsFlag := flag.Int("global-sessions", 5, "Sequential agent_a sessions pushed to the Global Brain")
	modeFlag := flag.String("global-brain", globalBrainDistilled,
		`Global Brain push policy: "distilled" (one memory per session) or "full" (every memory of every session)`)
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		log.Fatal("Usage: benchmark [--budget N] [--plane URL --api-key JWT] <session_file>")
	}

	globalBrain, err := globalBrainOptionsFromFlags(*planeFlag, *apiKeyFlag, *agentAFlag, *agentBFlag, *modeFlag, *sessionsFlag)
	if err != nil {
		log.Fatal(err)
	}

	established, err := loadSession(args[0])
	if err != nil {
		log.Fatalf("Failed to load session: %v", err)
	}

	cold, err := loadSession(*coldFlag)
	if err != nil {
		log.Fatalf("Failed to load cold-start session %s: %v", *coldFlag, err)
	}

	cfg := config.DefaultConfig()

	// Apply budget override if the flag was explicitly set (-1 = not set)
	if *budgetFlag >= 0 {
		cfg.TokenBudget = *budgetFlag
	}

	// One embedder for the whole run. Loading the ONNX model is the slow part,
	// and every scenario has to score with the same model or the numbers are not
	// comparable. Real ONNX embeddings, not hash-based: the fallback is announced
	// out loud rather than swapped in silently.
	emb, err := embedder.NewEmbedder(cfg.EmbedderType, cfg.OpenAIAPIKey, cfg.ModelPath, "")
	if err != nil {
		log.Fatalf("Failed to create embedder: %v", err)
	}
	if _, ok := emb.(*embedder.ONNXEmbedder); !ok {
		log.Printf("WARNING: ONNX model not found at %s, benchmark is running against hash-based embeddings, NOT real semantic similarity", onnxModelPath)
	}
	if onnxEmb, ok := emb.(*embedder.ONNXEmbedder); ok {
		defer onnxEmb.Close()
	}

	p := &pipeline{cfg: cfg, embedder: emb}
	ctx := context.Background()

	establishedReduction, err := runEstablishedScenario(ctx, p, args[0], established)
	if err != nil {
		log.Fatalf("Scenario 1 failed: %v", err)
	}

	if err := runColdStartScenario(ctx, p, *coldFlag, cold); err != nil {
		log.Fatalf("Scenario 2 failed: %v", err)
	}

	// The Global Brain scenario needs a control plane, a tenant credential, and a
	// database behind them. Without --plane the run is exactly the local
	// benchmark it has always been.
	if globalBrain.enabled() {
		if err := runPlaneScenario(ctx, p, established, establishedReduction, globalBrain); err != nil {
			log.Fatalf("Scenario 3 failed: %v", err)
		}
	}
}

// runEstablishedScenario is scenario 1: an established conversation compiled
// against its own messages.
//
// It returns the reduction so scenario 3's uplift line can be measured against
// this run rather than against a number remembered from a README.
func runEstablishedScenario(ctx context.Context, p *pipeline, fixtureName string, fixture *Session) (float64, error) {
	fmt.Printf("=== scenario 1: v1 established (%s, fresh store) ===\n", fixtureName)

	result, err := p.runFixtureScenario(ctx, fixture.Messages)
	if err != nil {
		return 0, err
	}
	result.printDetail(-1)

	fmt.Printf("framing: candidates are this fixture's own %d scorable messages, embedded with the real model — the stand-in for an established session whose memories are already stored and scored.\n", result.Candidates)
	fmt.Println("framing: this is the steady-state number the README's headline quotes: what a long-running session compiles to.")

	reduction := result.Reduction()
	fmt.Printf("v1 established:  Raw: %d | Compiled: %d | Reduction: %.1f%% (target ≥%.0f%%)\n",
		result.RawTokens, result.CompiledTokens, reduction, v1EstablishedTarget)
	if reduction < v1EstablishedTarget {
		fmt.Printf("WARNING: v1 established reduction (%.1f%%) is below target (≥%.0f%%) — check the scoring weights, the classifier's confidence, and the fixture\n",
			reduction, v1EstablishedTarget)
	} else {
		fmt.Printf("SUCCESS: v1 established reduction (%.1f%%) meets target (≥%.0f%%).\n", reduction, v1EstablishedTarget)
	}

	return reduction, nil
}

// runColdStartScenario is scenario 2: a brand-new session with nothing behind it.
//
// It measures the same pipeline as scenario 1 on a shorter fixture, and the
// number is small for a structural reason rather than a tunable one -- so it
// prints the explanation instead of the 40% warning a low number would otherwise
// trip.
func runColdStartScenario(ctx context.Context, p *pipeline, fixtureName string, fixture *Session) error {
	fmt.Printf("\n=== scenario 2: v1 cold-start (%s, empty store) ===\n", fixtureName)

	result, err := p.runFixtureScenario(ctx, fixture.Messages)
	if err != nil {
		return err
	}
	result.printDetail(-1)

	fmt.Println("note: no prior memories exist yet — the store is fresh, so the compile can only draw on this session's own messages.")
	fmt.Println("note: cold-start reduction is lower by design and there is nothing to tune here; it grows as the conversation does, because there is more accumulated context to compress.")

	reduction := result.Reduction()
	fmt.Printf("v1 cold-start:   Raw: %d | Compiled: %d | Reduction: %.1f%% (expected ~%.0f%%)\n",
		result.RawTokens, result.CompiledTokens, reduction, expectedColdStartReduction)
	if reduction < expectedColdStartReduction-5 {
		fmt.Printf("note: %.1f%% is below the ~%.0f%% a cold start usually shows — small is expected, near-zero is not; check the fixture and the budget\n",
			reduction, expectedColdStartReduction)
	}

	return nil
}

// runPlaneScenario is scenario 3's wrapper: it prints the header and delegates
// the measurement to globalbrain.go, which owns that scenario's framing.
func runPlaneScenario(ctx context.Context, p *pipeline, fixture *Session, establishedReduction float64, opts globalBrainOptions) error {
	fmt.Printf("\n=== scenario 3: v2 Global Brain (%s) ===\n", opts.URL)

	return runGlobalBrain(ctx, p, fixture, establishedReduction, opts)
}
