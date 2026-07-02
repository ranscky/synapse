# Synapse Context Compiler

[![CI](https://github.com/ranscky/synapse/actions/workflows/ci.yml/badge.svg)](https://github.com/ranscky/synapse/actions/workflows/ci.yml)

A Go binary that acts as a reverse proxy between AI clients (Cline, Ollama, any OpenAI-compatible tool) and upstream models. It intercepts API calls, scores and prunes conversation history using a 4-factor model, and returns a compiled, token-budgeted context to the model instead of the raw noisy history.

## Features

- **Reverse Proxy**: Transparently sits between AI clients and upstream models
- **Context Compilation**: Intelligently reduces conversation history to essential context
- **4-Factor Scoring**: Uses Semantic Similarity, Recency, Importance, and Task Alignment metrics
- **Token Budget Management**: Ensures responses fit within model token limits
- **Multiple Embedding Backends**: Supports ONNX (local) and OpenAI embeddings
- **Structured Logging**: Built-in structured logging with slog
- **Health Monitoring**: Built-in health check endpoint
- **Memory Trace Inspector**: Web UI for inspecting context compilation decisions
- **Cross-Platform**: Works on Linux, macOS, and Windows

## Architecture

```
┌─────────────┐    ┌──────────────────┐    ┌─────────────────┐
│ AI Clients  │───▶│ Synapse Proxy    │───▶│ Upstream Models │
│ (Cline, etc)│    │ (this binary)    │    │ (Ollama, etc)   │
└─────────────┘    └──────────────────┘    └─────────────────┘
                            │
                            ▼
                   ┌──────────────────┐
                   │ Memory Store     │
                   │ (SQLite + Vec)   │
                   └──────────────────┘
```

## 4-Factor Sieve Scoring

Synapse uses a 4-factor model to score and rank conversation memories:

**S (Semantic Similarity)**: How semantically similar is this memory to the current query?
- Uses embedding vectors and cosine similarity
- Weight: 0.4 (default)

**R (Recency)**: How recent is this memory?
- Newer memories get higher scores
- Normalized 1/(1+hours_since) decay function
- Weight: 0.2 (default)

**I (Importance)**: What type of memory is this?
- Decision: 1.0 (highest priority)
- Error: 0.9
- Fact: 0.7
- Context: 0.5
- Preference: 0.3
- Weight: 0.2 (default)

**T (Task Alignment)**: How relevant is this memory type to the current task?
- Determined by intent classification (debug, code, plan, write, generic)
- Different weights per intent type
- Weight: 0.2 (default)

**Total Score Formula**:
```
Total = S × wS + R × wR + I × wI + T × wT
```

## Trace Inspector

Synapse includes a web-based trace inspector for understanding context compilation decisions:

- Visit `http://localhost:8080/ui` to access the inspector
- View detailed scoring breakdown for each memory
- See which memories were included/excluded and why
- Monitor token usage and budget compliance

## Weight Tuning Guide

Adjust weights in `synapse.yaml` for different use cases:

**For Debugging Sessions** (prioritize errors and decisions):
```yaml
weight-semantic-similarity: 0.3
weight-recency: 0.2
weight-importance: 0.3
weight-task-alignment: 0.2
```

**For Code Generation** (prioritize decisions and facts):
```yaml
weight-semantic-similarity: 0.5
weight-recency: 0.1
weight-importance: 0.2
weight-task-alignment: 0.2
```

**For Creative Writing** (prioritize context and preferences):
```yaml
weight-semantic-similarity: 0.3
weight-recency: 0.3
weight-importance: 0.1
weight-task-alignment: 0.3
```

## Quick Start

### Installation

```bash
go install github.com/ranscky/synapse/cmd/synapse@latest
```

### Configuration

Create a `synapse.yaml` file:

```yaml
# See synapse.yaml.example for all configuration options
upstream-url: "http://localhost:11434"  # Ollama default
listen-addr: "127.0.0.1:8080"
token-budget: 3000
embedder-type: "onnx"  # or "openai"
```

### Running

```bash
# Basic usage
synapse --config synapse.yaml

# Override upstream URL
synapse --config synapse.yaml --upstream http://localhost:11434

# Override port
synapse --config synapse.yaml --port 9090
```

## API Endpoints

- `POST /v1/messages` - Main proxy endpoint for chat completions
- `GET /health` - Health check endpoint returning `{"status":"ok"}`

## Configuration

All configuration options can be set in `synapse.yaml`:

| Key | Default | Description |
|-----|---------|-------------|
| `upstream-url` | Required | Upstream model server URL |
| `listen-addr` | `127.0.0.1:8080` | Bind address for the proxy |
| `token-budget` | `3000` | Maximum tokens in compiled context |
| `embedder-type` | `"onnx"` | Embedding backend (`"onnx"` or `"openai"`) |
| `openai-api-key` | From `OPENAI_API_KEY` env | OpenAI API key for embeddings |
| `weight-semantic-similarity` | `0.4` | Weight for semantic similarity scoring |
| `weight-recency` | `0.2` | Weight for recency scoring |
| `weight-importance` | `0.2` | Weight for importance scoring |
| `weight-task-alignment` | `0.2` | Weight for task alignment scoring |
| `log-level` | `"info"` | Logging level (`debug`, `info`, `warn`, `error`) |

## Security

- Binds to `127.0.0.1` by default (never `0.0.0.0`)
- Never logs Authorization headers
- All config files created with secure `0600` permissions
- Memory traces are in-memory only by default

## Development

### Building

```bash
go build -o synapse ./cmd/synapse
```

### Testing

```bash
go test ./...
```

### Dependencies

- Go 1.22+
- Chi v5 (HTTP router)
- go-yaml v3 (config parsing)
- sqlite-vec (memory storage)
- ONNX Runtime (local embeddings)
- tiktoken-go (token counting)

## License

MIT