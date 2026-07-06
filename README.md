# Synapse Context Compiler

> **46.4% token reduction** on real multi-session conversations — verified with semantic embeddings (all-MiniLM-L6-v2), not mock data.

A Go reverse proxy that sits between your AI client and your model. It intercepts every API call, scores and prunes conversation history using a 4-factor model, and forwards a token-budgeted, task-aware context instead of the raw, noisy history. Your model sees less, but better.

```
┌─────────────┐    ┌──────────────────┐    ┌─────────────────┐
│ AI Clients  │───▶│ Synapse Proxy    │───▶│ Upstream Models │
│ Cline, etc. │    │ :8080            │    │ Ollama, etc.    │
└─────────────┘    └──────────────────┘    └─────────────────┘
                            │
                   ┌────────▼─────────┐
                   │  Memory Store    │
                   │  SQLite +        │
                   │  in-Go cosine    │
                   │  similarity      │
                   └──────────────────┘
```

**Status:** functional MVP, single-developer project, pre-v1. See [Known limitations](#known-limitations) before relying on this in production.

---

## Table of contents

- [How it works](#how-it-works)
- [Installation](#installation)
- [Configuration](#configuration)
- [CLI flags](#cli-flags)
- [Connecting your AI client](#connecting-your-ai-client)
- [API reference](#api-reference)
- [Utility tools](#utility-tools)
- [Security](#security)
- [Development](#development)
- [Known limitations](#known-limitations)
- [Roadmap](#roadmap)
- [License](#license)

---

## How it works

Every message sent through the proxy goes through a four-step pipeline before reaching your model.

**1. Classify intent.** Synapse detects one of five intents from the message content — `debug`, `plan`, `code`, `write`, or `generic` — and uses that to shift scoring weights toward what actually matters for that kind of work.

**2. Score memories.** Each candidate memory is scored on four independent factors and combined into a single weighted score:

```
Score = S·w_s + R·w_r + I·w_i + T·w_t
```

| Factor | What it measures | Source |
|---|---|---|
| **S** — Semantic Similarity | Cosine similarity between the current query and stored memory embeddings | Real ONNX inference via all-MiniLM-L6-v2, WordPiece tokenization verified against `transformers.BertTokenizer` |
| **R** — Recency | Time-decayed, normalized across the current candidate set | — |
| **I** — Importance | Lookup table by memory type | `decision: 1.0`, `error: 0.9`, `fact: 0.7`, `context: 0.5`, `preference: 0.3` |
| **T** — Task Alignment | Intent × memory-type weight matrix, confidence-blended so a low-confidence intent guess doesn't fully override the other factors | See table below |

Task-alignment weight matrix (`internal/scorer/weights.go`):

| Intent | decision | error | fact | preference | context |
|---|---|---|---|---|---|
| `debug` | 1.0 | 1.0 | 0.6 | 0.1 | 0.4 |
| `plan` | 1.0 | 0.3 | 0.8 | 0.3 | 0.7 |
| `code` | 0.8 | 0.6 | 0.7 | 0.2 | 0.6 |
| `write` | 0.4 | 0.1 | 0.6 | 0.7 | 0.8 |
| `generic` | 0.5 | 0.5 | 0.5 | 0.5 | 0.5 |

This is the core novelty of the project: providers generally compete on making context windows bigger, not on making what goes into them smarter. Task-aware weight shifting is a bet that signal quality matters more than raw window size.

**3. Deduplicate.** Near-duplicate memories (cosine similarity > 0.92) are collapsed before scoring, so repeated content doesn't crowd out genuine signal.

**4. Budget.** Top-ranked memories are packed into a token budget (default 3000 tokens, counted exactly via `tiktoken-go`, not estimated) and compiled into the final context forwarded upstream.

---

## Installation

### Option 1 — Pre-built release (recommended if you don't have Go installed)

Download the archive for your platform from the Releases page. Each archive bundles everything needed to run:

```
synapse                              # or synapse.exe on Windows
libonnxruntime.so                    # or .dylib / onnxruntime.dll
models/all-MiniLM-L6-v2/
  model.onnx
  vocab.txt
ui/
```

Extract and run:

```bash
./synapse --upstream https://api.anthropic.com
```

No separate ONNX Runtime install needed — the release archive ships the native library alongside the binary and Synapse resolves it automatically at startup.

### Option 2 — Setup script (build from source, recommended if you have Go)

```bash
git clone https://github.com/<yourhandle>/synapse
cd synapse
bash setup.sh
```

`setup.sh` detects your OS/architecture, downloads and installs the matching ONNX Runtime native library, downloads the all-MiniLM-L6-v2 model (~90MB, one-time), builds the binary, and scaffolds `synapse.yaml` from the example file. Supports Linux (x86_64, aarch64), macOS (Intel, Apple Silicon), and Windows (Git Bash/WSL).

Then:

```bash
./synapse --config synapse.yaml
```

### Option 3 — Manual build

Requires Go 1.22+, a C toolchain (the project uses cgo for both SQLite and ONNX Runtime bindings), and ONNX Runtime 1.27.0 available on your system library path (or set via `SYNAPSE_ORT_LIB_PATH`, see below).

```bash
go build -o synapse ./cmd/synapse
```

---

## Configuration

Create `synapse.yaml` (use `synapse.yaml.example` as a starting point):

```yaml
upstream-url: "http://localhost:11434"    # Your model server (Ollama default). Required.
allowed-upstream-hosts: []                  # Optional allowlist; localhost/127.x is always permitted
listen-addr: "127.0.0.1:8080"              # Must be 127.0.0.1 or localhost — enforced at validation time
token-budget: 3000                          # Tokens allocated to compiled context (tiktoken-exact count)
embedder-type: "onnx"                       # "onnx" (local, real semantic similarity) or "openai"
model-path: "models/all-MiniLM-L6-v2/model.onnx"
db-path: ""                                 # Leave blank to use the OS-standard data directory (see below)
openai-api-key: ""                          # Also settable via OPENAI_API_KEY env var
weight-semantic-similarity: 0.4
weight-recency: 0.2
weight-importance: 0.2
weight-task-alignment: 0.2
deduplication-threshold: 0.92               # Cosine similarity above which memories are collapsed
log-level: "info"
```

### Data location

If `db-path` is left blank, Synapse resolves a stable per-OS data directory rather than writing next to the binary — important since a distributed release binary may be launched from anywhere:

| OS | Default path |
|---|---|
| Linux | `~/.local/share/synapse/synapse.db` (or `$XDG_DATA_HOME/synapse/synapse.db`) |
| macOS | `~/Library/Application Support/synapse/synapse.db` |
| Windows | `%APPDATA%\synapse\synapse.db` |

Set `db-path` explicitly to override — including `:memory:` for a fully ephemeral, non-persistent store.

### Environment variables

| Variable | Purpose |
|---|---|
| `SYNAPSE_ORT_LIB_PATH` | Override the ONNX Runtime shared library location. Only needed if you've moved `synapse` away from its bundled native lib, or are running a source build with ORT installed somewhere non-standard. |
| `OPENAI_API_KEY` | Used only when `embedder-type: openai`. |

---

## CLI flags

| Flag | Default | Description |
|---|---|---|
| `--config` | `synapse.yaml` | Path to configuration file |
| `--upstream` | — | Override `upstream-url` from config |
| `--port` | — | Override the port in `listen-addr` |
| `--persist-traces` | `false` | Persist memory traces to disk (they're in-memory-only by default) |

---

## Connecting your AI client

Point any OpenAI-compatible client at `http://127.0.0.1:8080`:

- **Cline** — set the API base URL in settings to `http://127.0.0.1:8080`
- **Open WebUI** — set the Ollama/OpenAI base URL to `http://127.0.0.1:8080`
- **curl** — `curl http://127.0.0.1:8080/v1/messages -d '{"messages": [...]}'`

---

## API reference

| Endpoint | Method | Description |
|---|---|---|
| `/v1/messages` | POST | Main proxy endpoint — intercepts, compiles, forwards upstream |
| `/v1/compile` | POST | Compile a session without proxying (useful for testing/inspection) |
| `/v1/memories` | GET | List stored memories for a session |
| `/v1/memories` | DELETE | Clear memories for a session |
| `/v1/stats` | GET | Memory count, average compile time |
| `/openapi.yaml` | GET | Full OpenAPI specification |
| `/health` | GET | Health check — `{"status":"ok","memories_stored":N,"avg_compile_ms":N}` |
| `/ui` | GET | Web trace inspector |

Add header `X-Synapse-Trace: true` to any proxied request to get a base64-encoded trace manifest back in the `X-Synapse-Trace-Result` response header — shows exactly which memories were selected, scored, and why (and why others were excluded).

---

## Utility tools

Alongside the proxy itself, `cmd/` includes a few standalone tools:

| Command | Purpose |
|---|---|
| `cmd/benchmark` | Run the scoring pipeline against a test-fixture session and report token reduction vs. raw history |
| `cmd/counttokens` | Exact tiktoken-based token count for a session JSON file |
| `cmd/mergesessions` | Merge multiple session JSON files into one, for constructing larger test fixtures |

```bash
go run ./cmd/benchmark testdata/session_code.json
```

---

## Security

- Binds to `127.0.0.1` by default — validation actively rejects any other host
- `Authorization` headers are forwarded upstream but **never logged**
- Config and database files are created with `0600` permissions
- Memory content is sanitized before storage (prompt-injection pattern detection)
- Upstream URLs can be restricted to an explicit allowlist (`allowed-upstream-hosts`); localhost/127.x is always implicitly permitted for local model servers like Ollama
- Traces are in-memory only by default; `--persist-traces` is required to write them to disk

---

## Development

```bash
# Run tests (matches CI: ubuntu-latest, macos-latest, windows-latest)
go test -v ./...

# Run a benchmark against a test fixture
go run ./cmd/benchmark testdata/session_merged.json

# Build
go build -o synapse ./cmd/synapse
```

CI runs on every push/PR to `main` across all three OSes, installs ONNX Runtime and downloads the real model so the ONNX inference path is actually exercised rather than silently falling back to hash-based embeddings. Tagged pushes (`v*`) trigger the release workflow, which builds and bundles a self-contained archive per platform.

---

## Known limitations

Being upfront about what's not finished yet:

- **OpenAI embedder is stubbed** — `embedder-type: openai` currently returns a placeholder, non-semantic embedding. Use `onnx` for real semantic similarity.
- **Single-developer project, pre-v1** — no design partners or production deployments yet.
- **`allowed-upstream-hosts` allowlist** is optional and off by default; if security matters for your deployment, set it explicitly.

---

## Roadmap

- Show HN launch anchored to the token-reduction benchmark
- Community distribution via Cline, Ollama, and LocalLLaMA channels
- 2–3 design partners post-v1
- v2: enterprise managed-memory plane with a signed, auditable Memory Trace ledger

---

## License

MIT