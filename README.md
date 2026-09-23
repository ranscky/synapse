# Synapse Context Compiler

[![CI](https://github.com/ranscky/synapse/actions/workflows/ci.yml/badge.svg)](https://github.com/ranscky/synapse/actions/workflows/ci.yml) [![Release](https://github.com/ranscky/synapse/actions/workflows/release.yml/badge.svg)](https://github.com/ranscky/synapse/actions/workflows/release.yml) [![License: BSL 1.1](https://img.shields.io/badge/License-BSL_1.1-blue.svg)](LICENSE)

Synapse compiles your conversation history into the smallest, most relevant context your model actually needs — instead of forwarding the raw, ever-growing message log every provider defaults to. A Go reverse proxy sits between your AI client and your model: it classifies intent, scores every candidate memory on four independent factors, deduplicates near-identical content, and packs what survives into an exact token budget before forwarding it upstream. Your model sees less, but better — and every decision is inspectable: what got selected, what got rejected, and why (see the [trace inspector](#api-reference)).

On an established multi-session conversation, that discipline verifiably cuts token usage by **46.4%** — measured with real semantic embeddings (all-MiniLM-L6-v2), not mock data. A brand-new session has no prior memory to draw from yet, so reduction grows as a conversation does — see [Benchmark numbers](#benchmark-numbers) to reproduce this number yourself.

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

**Status:** functional MVP with a self-hosted v2 control plane — Global Brain sync, a signed HMAC-SHA256 audit ledger, compliance surfaces, and an MCP server — all of it pre-v1. Single-developer project; see [Known limitations](#known-limitations) before relying on this in production.

## Benchmark numbers

Three scenarios, each printed with the framing its number needs:

| Scenario | Reduction | What it measures |
|---|---|---|
| v1 cold-start | **~15%** | A brand-new session with no history yet — expected, not a number to tune |
| v1 established | **46.4%** | `session_merged.json`, an established multi-session conversation (`Raw 5569 → Compiled 2999`) |
| v2 Global Brain | **93.2%** | Two agents sharing one Global Brain — cross-agent recall (`--global-brain=distilled`, the default) |

- **The cold start is expected.** On the first message of a brand-new session there is no prior memory to draw on, so the compile can only use the session's own text. Reduction grows as a conversation does: **46.4%** is the representative number for ongoing use, and it is the one the headline quotes.
- **The Global Brain number measures something different.** `Raw` is `agent_b`'s own session and `Compiled` is the context it builds from the shared brain, so the two sides are not the same text — 93.2% is cross-agent recall, not compression of one conversation. With `--global-brain=full` (every memory of every session) the pool exceeds the token budget, so the compile fills the budget and lands on 46.1%: budget-bound, not sync-bound.
- **The published 46.4% and the tool's 46.1% differ by 0.3 points.** The tool prints its own arithmetic for this fixture (`Raw: 5569 | Compiled: 2999 | Reduction: 46.1%`); 46.4% is the published headline figure. When the exact number matters, run the tool.

Reproduce all three:

```bash
go run ./cmd/benchmark testdata/session_merged.json
```

Shorter fixtures like `session_code.json` show a lower number — reduction grows with conversation length, since there is more accumulated context to compress. `session_merged.json` represents an established multi-session conversation, the scenario the headline number describes. [Utility tools](#utility-tools) covers the Global Brain run and its flags.

---

## Quickstart

```bash
brew tap ranscky/synapse
brew trust ranscky/synapse   # required once -- Homebrew now requires explicit trust for third-party taps
brew install synapse
synapse init                 # interactive: confirms config creation, picks your upstream provider (Ollama/OpenAI/Anthropic/OpenRouter/custom), offers to start at login
synapse --config ~/.config/synapse/synapse.yaml
```

Non-interactive (scripts/CI): `synapse init --yes` skips the prompts and writes Ollama-default config.

No Homebrew? See [Installation](#installation) for the pre-built archive and manual-build options. Full walkthrough: [QUICKSTART.md](QUICKSTART.md).

### Native Ollama support

`synapse init` points `upstream-url` at Ollama's default (`http://localhost:11434`). To route the interactive `ollama` CLI — or any Ollama-native tool — through Synapse, pick one of two options:

**Option A — point Ollama at Synapse with an environment variable**

```bash
export OLLAMA_HOST=http://127.0.0.1:8080
ollama
```

Add that `export` line to `~/.bashrc` or `~/.zshrc` to make it permanent.

**Option B — point the client at Synapse's Ollama-native endpoint directly**

A client with its own base-URL setting can be aimed at `http://127.0.0.1:8080/api/chat` instead. Synapse accepts Ollama's native `/api/chat` request shape and runs it through the same memory pipeline as `/v1/messages`; no environment variable is needed, and every other native endpoint (`/api/tags`, `/api/pull`, …) is forwarded upstream untouched.

Either way Synapse must be running: an Ollama client pointed at `:8080` fails to connect when Synapse is down rather than silently falling back to the model server. See [Native Ollama CLI](#native-ollama-cli) for exactly what is and isn't proxied.

---

## Table of contents

- [Benchmark numbers](#benchmark-numbers)
- [How it works](#how-it-works)
- [Installation](#installation)
- [Configuration](#configuration)
- [CLI flags](#cli-flags)
- [Connecting your AI client](#connecting-your-ai-client)
- [MCP setup](#mcp-setup)
- [Global Brain setup](#global-brain-setup)
- [API reference](#api-reference)
- [Utility tools](#utility-tools)
- [Security](#security)
- [EU AI Act compliance](#eu-ai-act-compliance)
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

> > **Note:** the macOS build currently targets Apple Silicon (arm64) only. Intel Mac support is on the roadmap, but GitHub's Intel-runner build queue has been unreliable, so it's not in the release matrix yet — Intel Mac users should use [Option 3 — Manual build](#option-3--manual-build) in the meantime. **The Apple Silicon build itself passes CI on GitHub's macos-latest (arm64) runners, but hasn't yet been run on real Apple Silicon hardware by the maintainer** (who doesn't have access to one) — if you try it, a report either way (works / doesn't) is genuinely useful. See [Known limitations](#known-limitations).

```
synapse                              # or synapse.exe on Windows
libonnxruntime.so                    # or .dylib / onnxruntime.dll
models/all-MiniLM-L6-v2/
  model.onnx
  vocab.txt
ui/
LICENSE
synapse.yaml.example
README.md
```

Extract and run:

```bash
./synapse --upstream https://api.anthropic.com
```

No separate ONNX Runtime install needed — the release archive ships the native library alongside the binary and Synapse resolves it automatically at startup.

### Option 2 — Setup script (build from source, recommended if you have Go)

```bash
git clone https://github.com/ranscky/synapse
cd synapse
bash setup.sh
```

`setup.sh` detects your OS/architecture, downloads and installs the matching ONNX Runtime native library, downloads the all-MiniLM-L6-v2 model (~90MB, one-time), builds the binary, and scaffolds `synapse.yaml` from the example file. Supports Linux (x86_64, aarch64), macOS (Intel, Apple Silicon), and Windows (Git Bash/WSL).

Then:

```bash
./synapse --config synapse.yaml
```

### Option 3 — Manual build

Requires Go 1.25.5+ (that floor is set by github.com/mark3labs/mcp-go v1.1.0, the MCP
server dependency added in Phase 22, and declared in go.mod), a C toolchain (the project uses cgo for both SQLite and ONNX Runtime bindings), and ONNX Runtime 1.27.0 available on your system library path (or set via `SYNAPSE_ORT_LIB_PATH`, see below).

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
weight-recency: 0.1
weight-importance: 0.3
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

#### Resetting the store

To start from a clean memory store, stop Synapse and delete **all three** SQLite files:

```bash
rm -f ~/.local/share/synapse/synapse.db \
      ~/.local/share/synapse/synapse.db-wal \
      ~/.local/share/synapse/synapse.db-shm
```

Deleting only `synapse.db` leaves SQLite in a broken state. The write-ahead log (`-wal`) and shared-memory (`-shm`) files are part of the database, not scratch files, and a database whose main file was replaced under a surviving WAL can fail to open or lose committed rows. If you script a reset, delete all three.

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

- **Cline** — API Provider: **OpenAI Compatible**. Base URL: `http://127.0.0.1:8080/v1` (include the `/v1` — Cline appends `/chat/completions` itself). Any placeholder API key works; Synapse doesn't validate it, but whatever value you use becomes the session-bucketing key, so keep it consistent.
- **Open WebUI** — set the Ollama/OpenAI base URL to `http://127.0.0.1:8080`
- **curl** — `curl http://127.0.0.1:8080/v1/messages -d '{"messages": [...]}'`

### Native Ollama CLI

Synapse also understands Ollama's native `/api/chat` shape, so the interactive `ollama` CLI itself can be captured — not just clients that speak the Anthropic/OpenAI shape:

```bash
export OLLAMA_HOST=http://127.0.0.1:8080
ollama
```

That is [Option A](#native-ollama-support) from the quickstart; [Option B](#native-ollama-support) points a client at `http://127.0.0.1:8080/api/chat` directly instead. To make Option A permanent rather than exporting it every session: add that `export` line to your shell rc file (`~/.bashrc` or `~/.zshrc`), and keep Synapse always running in the background with `brew services start synapse`. Once both are set, `ollama` transparently routes through Synapse with no per-session setup — though note that if Synapse isn't running, `ollama` will fail to connect rather than falling back to the real server directly, since it's now pointed explicitly at Synapse's port.

Every other native Ollama endpoint (`/api/tags`, `/api/show`, model pulls, `/api/status`, etc.) is forwarded straight through to your real Ollama server unmodified — only `/v1/messages`, `/api/chat`, and `/v1/chat/completions` run through the memory pipeline, so the rest of the CLI (model listing, launching, etc.) works exactly as it would without Synapse in the picture.

### Session Continuity

Synapse buckets conversation history into sessions by hashing whichever auth header your client sends — `Authorization` first, falling back to `x-api-key` (the standard Anthropic Messages API convention). Send the same header value on every request in a conversation, or memory recall silently resets on each call with no error — just an empty candidate pool every time.

---

## MCP setup

Synapse ships an MCP (Model Context Protocol) server, so Cline and other MCP-aware editors can compile and search memory as tools rather than as proxy traffic. Add `.cursor/mcp.json` to any project directory:

```json
{"mcpServers":{"synapse":{"command":"synapse","args":["--mcp"]}}}
```

Then restart Cline. Synapse appears in the MCP panel as a connected server, and its tools become callable:

| Tool | What it does |
|---|---|
| `synapse_compile` | Runs the full classify → retrieve → score → dedup → budget pipeline for a session and returns the compiled messages alongside the scoring that put each memory in — or kept it out |
| `synapse_search_memories` | Ranks the Global Brain's memories with the same 4-Factor model, returning `score_s` / `score_r` / `score_i` / `score_t` / `score_total` per memory |
| `synapse_write_memory` | Stores one memory, sanitized through the same pipeline the REST surface uses |

Two boot facts worth knowing before debugging a server that never appears:

- **The config must already exist.** `--mcp` starts *alongside* the normal proxy, and config validation runs first, so the template above only works where `synapse.yaml` (or the OS-standard config) is already in place.
- **The MCP server does not replace the HTTP proxy.** Both start in one process, so an editor-spawned instance whose config already binds `127.0.0.1:8080` exits on the bind clash and takes the MCP server with it. Give the MCP instance its own port: `"args": ["--mcp", "--port", "8098"]`.

The MCP surface is local-only by design — stdio, or Streamable HTTP on `127.0.0.1` when `--mcp-port` is given — and it carries no authentication of its own: anything that can spawn the process can call the tools. Loopback is the boundary.

### Coexistence with Mem0's OpenMemory MCP

Synapse and [OpenMemory MCP](https://github.com/mem0ai/mem0) are separate stores with no shared state. Enabling one does not populate, read, or migrate the other, and a project can run both without either seeing the other's memories. The practical difference is inspectability: `synapse_search_memories` returns the **score breakdown** (S/R/I/T plus the weighted total) and a `trace_id` for every memory it surfaces, so the editor can show *why* a memory ranked where it did. OpenMemory MCP returns memories without a scoring rationale. If your workflow depends on being able to audit retrieval, that is the difference that matters.

---

## Global Brain setup

The Global Brain is the v2 control plane: a memory written by one agent becomes retrievable by another. It is optional — leave `control-plane-url` blank and everything above behaves exactly as it does today — and it is self-hosted, so nothing leaves your machine except what you point at it.

### 1. Start the plane

```bash
cd deploy && docker compose up -d --build
curl http://127.0.0.1:9090/health
# {"status":"ok","version":"2.0.0","db":"connected"}
```

That brings up Postgres (pgvector) and the plane. With the three images already local, startup was measured at **12s warm** and **23s for a cached rebuild** — comfortably under two minutes. The first-ever run on a cold machine pulls ~240 MB of images (~6 minutes on the maintainer's connection); that cost is pull throughput, not the stack, and `docker compose pull` ahead of time avoids it.

Two deliberate deployment facts: the plane binds loopback only and refuses to start on `0.0.0.0`, and the compose service uses `network_mode: host` (Linux-only) because a bridge-published port cannot reach a loopback-bound container.

### 2. Point a node at it

Three lines in `synapse.yaml`:

```yaml
control-plane-url: "http://127.0.0.1:9090"
agent-id: "edge-01"                                        # this node's stable identity; required with a control plane
control-plane-api-key: "<the JWT that provisioning returned>"   # SYNAPSE_PLANE_KEY is the env override
```

With those set, the node keeps storing memories locally *and* syncs them to the plane, and its compiles pull candidates from the shared brain (`GET /v2/memories/search`) instead of only from its own store. A node whose plane is unreachable falls back to local candidates rather than failing the request.

The value in `control-plane-api-key` must be the **JWT** provisioning returned, not the plaintext `api_key`: the sync and search routes verify a signed token, and `api_key` is stored as a bcrypt row. Never commit a DSN or a key — `database-dsn` (or `SYNAPSE_DB_DSN`) carries a database password.

If the tenant's compliance tier is `enterprise`, the node also starts appending its compiled traces to the plane's audit ledger, and that writer needs the plane's `SYNAPSE_MASTER_KEY` (the same 64-hex-character value the plane runs with) to unwrap the tenant's signing secret. Without it the node logs `ledger: append failed ... SYNAPSE_MASTER_KEY is required to wrap tenant secrets` and serves the request anyway — a failed ledger append never fails a compile — but the chain that tenant's compliance report reads will have a hole in it.

### 3. Provision a tenant

```bash
curl -sS -X POST http://127.0.0.1:9090/v2/tenants \
  -H "Authorization: <admin-token>" \
  -H 'Content-Type: application/json' \
  -d '{"slug":"my-team","plan":"team","agent_id":"edge-01"}'
# 201 {"tenant_id":"…","jwt":"…","api_key":"…"}
```

`<admin-token>` is the plane's `admin-token` value, sent as the header itself — no `Bearer ` prefix. The response is the one and only time the plaintext `api_key` is shown; keep the `jwt` for `control-plane-api-key` above.

One current gap: provisioning always mints the `team` compliance tier, so a tenant created this way receives `403` from the compliance surfaces until an enterprise tier can be provisioned. See [EU AI Act compliance](#eu-ai-act-compliance) and [COMPLIANCE.md](COMPLIANCE.md).

---

## API reference

| Endpoint | Method | Description |
|---|---|---|
| `/v1/messages` | POST | Main proxy endpoint (Anthropic Messages API shape) — intercepts, compiles, forwards upstream |
| `/api/chat` | POST | Ollama-native chat endpoint (used by the `ollama` CLI and Ollama-native tools) — same pipeline as `/v1/messages`; responses are forced non-streaming so the full reply can be reliably captured |
| `/v1/chat/completions` | POST | OpenAI Chat Completions shape (used by Cline's "OpenAI Compatible" provider and most other coding-agent tools pointed at a self-hosted endpoint) — same pipeline as `/v1/messages`, also forced non-streaming |
| `/v1/compile` | POST | Compile a session without proxying (useful for testing/inspection) |
| `/v1/memories` | GET | List stored memories for a session |
| `/v1/memories` | DELETE | Clear memories for a session |
| `/v1/stats` | GET | Memory count, average compile time |
| `/openapi.yaml` | GET | Full OpenAPI specification |
| `/health` | GET | Health check — `{"status":"ok","memories_stored":N,"avg_compile_ms":N}` |
| `/ui` | GET | Web trace inspector |

Any other path (`/api/tags`, `/api/show`, `/api/pull`, etc.) is forwarded straight through to the upstream server unmodified, with no memory pipeline applied — this lets Ollama's own control-plane endpoints work without Synapse needing to whitelist each one individually.

Add header `X-Synapse-Trace: true` to any proxied request to get a base64-encoded trace manifest back in the `X-Synapse-Trace-Result` response header — shows exactly which memories were selected, scored, and why (and why others were excluded).

---

## Utility tools

Alongside the proxy itself, `cmd/` includes a few standalone tools:

| Command | Purpose |
|---|---|
| `cmd/benchmark` | Measure token reduction for an established session, a cold start, and — with a control plane — two agents sharing a Global Brain |
| `cmd/counttokens` | Exact tiktoken-based token count for a session JSON file |
| `cmd/mergesessions` | Merge multiple session JSON files into one, for constructing larger test fixtures |

The benchmark runs three scenarios and prints each with the framing its number needs:

```bash
go run ./cmd/benchmark testdata/session_merged.json
```

```text
v1 established:  Raw: 5569 | Compiled: 2999 | Reduction: 46.1% (target ≥40%)
v1 cold-start:   Raw: 3263 | Compiled: 2757 | Reduction: 15.5% (expected ~15%)
```

- **v1 established** is the headline scenario: an established multi-session conversation, compiled against its own messages.
- **v1 cold-start** is a brand-new session against an empty store. The number is low by design and there is nothing to tune: with no prior memories, the compile can only draw on the session's own messages. Reduction grows as a conversation does, which is why the established scenario is the one the headline quotes.
- **v2 Global Brain** runs only when a control plane is named:

```bash
go run ./cmd/benchmark testdata/session_merged.json \
  --plane http://127.0.0.1:9090 --api-key "$SYNAPSE_TENANT_JWT"
```

It pushes five sequential sessions as `agent_a`, then compiles as `agent_b` against the memories the plane answers with. `--api-key` is the **JWT** that provisioning returned — the plaintext `api_key` is a bcrypt row, not a credential the sync and search routes accept.

What `agent_a` pushes is a flag, because it decides what the number means:

- `--global-brain=distilled` (default) pushes one memory per session — the same last-user-message write-back `synapse_compile` performs. Measured: `Raw: 5569 | Compiled: 381 | Reduction: 93.2% (target ≥55%)`: a small pool of cross-agent memory answering the same task.
- `--global-brain=full` pushes every memory of every session, which is what a proxying edge node queues per turn. Measured: `Raw: 5569 | Compiled: 2999 | Reduction: 46.1%` — a pool larger than the token budget makes the compile fill the budget, so the reduction is budget-bound; the benchmark prints that explanation next to the number instead of leaving it to a README.

What the Global Brain number is *not*: `Raw` is `agent_b`'s own session while `Compiled` is the context it builds from the shared brain, so the two sides are not the same text. It measures cross-agent recall with the same arithmetic as the two v1 scenarios — not "better compression of one conversation".

---

## Security

- Binds to `127.0.0.1` by default — validation actively rejects any other host
- `Authorization` headers are forwarded upstream but **never logged**
- Config and database files are created with `0600` permissions
- Memory content is sanitized before storage (prompt-injection pattern detection)
- Upstream URLs can be restricted to an explicit allowlist (`allowed-upstream-hosts`); localhost/127.x is always implicitly permitted for local model servers like Ollama
- Traces are in-memory only by default; `--persist-traces` is required to write them to disk

---

## EU AI Act compliance

The v2 control plane keeps a signed, append-only ledger of every compilation an enterprise tenant performs. That ledger is the *evidence* layer for the transparency obligations a deployment inherits — it is deliberately not a compliance guarantee. [COMPLIANCE.md](COMPLIANCE.md) is the full obligation-by-obligation mapping.

### Dates that matter

| Obligation | Applies from | Source |
|---|---|---|
| **Article 50 — transparency obligations** | **2 August 2026** | Regulation (EU) 2024/1689, unchanged by the Digital Omnibus |
| **Annex III high-risk obligations** | **2 December 2027** | Digital Omnibus on AI — Regulation (EU) 2026/1744, in force 27 July 2026 |

The Digital Omnibus on AI moved the high-risk deadline out; it did **not** move Article 50 — which is why transparency evidence is the near-term work and the high-risk record-keeping calendar is the medium-term one.

### What the signed ledger provides

- **A tamper-evident trace of every compilation.** Each entry records what context an agent was given — the memories selected, the ones rejected and why, the detected intent, the S/R/I/T scoring per memory — signed, so a later edit is detectable rather than invisible.
- **HMAC-SHA256 chain integrity.** Every entry carries `prev_hash` and `hash_value`, making the ledger a hash chain: `GET /v2/compliance/chain-integrity` walks it and answers `chain_valid`, with `first_break_id` naming the earliest entry that failed.
- **A queryable audit history.** `GET /v2/compliance/audit` pages a tenant's own signed entries, newest first, over a caller-chosen window, and records an access row for every read — refused reads included.
- **A PDF compliance report.** `GET /v2/compliance/report?format=pdf` renders the JSON report — period, volume, memory-type mix, contradiction and supersession handling, chain verdict, Article 50 statement — as a print-ready A4 document.

#### The PDF needs an HTML-to-PDF renderer on the plane's host

The endpoint does not bundle a renderer. It looks for one on the host's `PATH`, in this order: `wkhtmltopdf`, `chromium`, `chromium-browser`, `google-chrome`, `google-chrome-stable`. With none installed it answers, rather than failing obscurely:

```json
503 {"error":"pdf_tool_unavailable","message":"install wkhtmltopdf to enable PDF reports"}
```

The self-hosted compose image does **not** include one, so `format=pdf` answers that 503 out of the box; the JSON report at the same route (`format=json`, the default) needs no renderer and always works. To get PDFs from the compose stack, either install `wkhtmltopdf`/`chromium` in the plane's runtime image, or run the plane binary directly on a host that already has a browser. Every actual render was measured against `google-chrome` and produced a two-page A4 document in 3.7–12.6 seconds, bounded by the endpoint's own 60-second ceiling.

### Retention

`ledger-retention-days` in `synapse-plane.yaml` (default `365`) declares the window your reporting and archival tooling treats as online history. The ledger table is append-only and is never updated or deleted, so this value never removes a row; a policy that needs longer must archive rather than rely on the plane. Recommended windows are in [COMPLIANCE.md](COMPLIANCE.md#retention-policy-recommendations).

### Two limits to know before relying on this

- **The compliance surfaces require an enterprise compliance tier**, and `POST /v2/tenants` currently mints `team` for every tenant. A tenant created through the public API therefore receives `403 {"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}` from all three surfaces until enterprise provisioning exists.
- **`avg_reduction_pct` is only as complete as metering.** A window covered by this node's metering rows answers with real numbers; an older window falls back to signed traces whose `reduction_pct` is `0`, and the report does not yet say which of the two it used.

### Disclaimer

> This report is generated by Synapse Context Compiler, which provides cryptographically signed, tamper-evident audit trails of all memory compilation decisions made by AI agents in this organisation. Each compilation event is logged with its full decision trace, memory provenance, scoring rationale, and chain integrity verification. DISCLAIMER: This report provides technical traceability infrastructure. It does not constitute legal advice or guarantee regulatory compliance with Regulation (EU) 2024/1689 or any other instrument. Consult qualified legal counsel regarding your organisation's specific obligations under the EU AI Act.

That text is `plane.Article50Statement`: the same constant every JSON and PDF report carries, verbatim.

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

### Testing a dev build: the Homebrew binary shadows it

If you installed Synapse with Homebrew, the release binary lives in Homebrew's prefix — `/opt/homebrew/bin/synapse` on Apple Silicon, `/home/linuxbrew/.linuxbrew/bin/synapse` on Linux, `/usr/local/bin/synapse` on Intel macOS — and that directory comes **before** the repository on `PATH`. A bare `synapse ...` therefore runs the *installed* release build, not the one you just produced with `go build -o synapse ./cmd/synapse`:

```bash
which synapse
# /home/linuxbrew/.linuxbrew/bin/synapse   <- not your dev build
```

The symptom is that a change appears to have no effect: a new flag is missing from `--help`, a log line never shows up, a fix doesn't take. Nothing is wrong with the build — it was never the binary that ran.

```bash
./synapse --config ./synapse.yaml    # run the dev build explicitly
# or put the repo first for this shell:
export PATH="$PWD:$PATH"
# or remove the installed copy entirely:
brew uninstall synapse
```

This bites hardest right after `brew install synapse` on a machine that is also a development checkout, because both binaries are valid and only one of them is new.

---

## Known limitations

Being upfront about what's not finished yet:

- **Cold starts are not the headline number.** A brand-new session has no prior memory to draw on, so reduction lands around **15%**. That is expected and there is nothing to tune; the representative number for ongoing use is the established **46.4%** — see [Benchmark numbers](#benchmark-numbers).
- **Memory supersession is one turn late.** When a new memory supersedes an older one ("we migrated to MongoDB" replacing "we use PostgreSQL"), the filtering that drops the superseded memory takes effect on the *next* compilation, so a superseded memory can appear in one additional compiled context. The compiled context is still correct and the chain is untouched — it is a property of when the filter runs, not a data-integrity bug. [COMPLIANCE.md](COMPLIANCE.md#memory-supersession-is-one-turn-late) spells out what it means for an audit.
- **OpenAI embedder is stubbed** — `embedder-type: openai` currently returns a placeholder, non-semantic embedding. Use `onnx` for real semantic similarity.
- **No true token-by-token streaming** — Synapse fully buffers the upstream response before returning it, so the full reply can be reliably captured for memory. Responses arrive as one block rather than typed out live; most noticeable with the interactive `ollama` CLI, which will pause and then print the whole reply at once.
- **Apple Silicon build is CI-tested only, not verified on real hardware** — it passes GitHub's macos-latest (arm64) runners, but the maintainer doesn't have access to a physical Apple Silicon Mac to confirm it on real hardware. Reports from real usage (either way) are welcome.
- **Intel macOS is not in the pre-built release matrix** — the release archives target Apple Silicon (arm64) only; Intel Mac users build manually per [Option 3 — Manual build](#option-3--manual-build).
- **A newly provisioned tenant cannot read the compliance surfaces.** `POST /v2/tenants` always mints the `team` compliance tier, so `/v2/compliance/audit`, `/v2/compliance/chain-integrity`, and `/v2/compliance/report` answer `403` for it. Minting an enterprise tier over HTTP is an open item — see [COMPLIANCE.md](COMPLIANCE.md#before-you-start-the-tier-gate).
- **Resetting the SQLite store means deleting three files** — `synapse.db`, `synapse.db-wal`, and `synapse.db-shm`. Deleting only the main file leaves SQLite broken. See [Resetting the store](#resetting-the-store).
- **A Homebrew install shadows a dev build.** `brew install synapse` puts a release binary in Homebrew's prefix, which precedes the repository on `PATH`, so a bare `synapse` can run the installed copy instead of the one you just built. See [Testing a dev build](#testing-a-dev-build-the-homebrew-binary-shadows-it).
- **Single-developer project, pre-v1** — no design partners or production deployments yet.
- **`allowed-upstream-hosts` allowlist** is optional and off by default; if security matters for your deployment, set it explicitly.

---

## Roadmap

- Show HN launch anchored to the token-reduction benchmark
- Community distribution via Cline, Ollama, and LocalLLaMA channels
- Bind to Ollama's default port directly (moving the real server to a non-standard port) for zero-config capture of any Ollama client, no `OLLAMA_HOST` needed — deferred for now since it requires OS-specific server reconfiguration and raises the blast radius if Synapse itself goes down
- 2–3 design partners post-v1
- ~~v2: enterprise managed-memory plane with a signed, auditable Memory Trace ledger~~ — shipped as Phases 1-30: a schema-per-tenant control plane, Global Brain sync, a signed HMAC-SHA256 ledger with compliance audit and report surfaces, usage metering, and an MCP server (see [Global Brain setup](#global-brain-setup) and [MCP setup](#mcp-setup)). The open items are listed in [Known limitations](#known-limitations)

---

## License

[Business Source License 1.1](LICENSE). Free for internal and production
use — the only restriction is offering Synapse (or a modified version) as
a competing hosted/managed service. Converts automatically to Apache 2.0
on 2030-07-06.