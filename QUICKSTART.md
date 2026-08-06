# Synapse Quickstart

Get a working Synapse proxy in front of your local model in about two minutes.

## 1. Install

**macOS / Linux (Homebrew):**
```bash
brew install ranscky/synapse/synapse
```

**Anything else:** grab the archive for your platform from the [latest release](https://github.com/ranscky/synapse/releases/latest), extract it, and skip to step 3 — the archive doesn't need `init`, it ships its own `synapse.yaml.example` ready to copy.

## 2. Create a config

```bash
synapse init
```

This creates a config file and tells you exactly where. Open it and set `upstream-url` to your model server — if you're running [Ollama](https://ollama.com) locally, the default (`http://localhost:11434`) already works and you can skip this.

## 3. Run it

**As a background service (Homebrew installs):**
```bash
brew services start synapse
```

**In the foreground (any install):**
```bash
synapse --config ~/.config/synapse/synapse.yaml
```

You should see a line like:
That confirms real semantic scoring is active, not the degraded hash-based fallback.

## 4. Point your client at it

Any OpenAI- or Anthropic-compatible client, pointed at `http://127.0.0.1:8080` instead of directly at your model, now gets its conversation history compiled through Synapse automatically. No code changes on the client side.

- **Cline** — set the API base URL to `http://127.0.0.1:8080`
- **Open WebUI** — set the Ollama/OpenAI base URL to `http://127.0.0.1:8080`
- **curl** — `curl http://127.0.0.1:8080/v1/messages -d '{"model": "...", "max_tokens": 1024, "messages": [...]}'`

## 5. See what it's doing

Open `http://127.0.0.1:8080/ui` in a browser — this is the trace inspector, showing which memories get selected, scored, and why, for any recent request.

## Something not working?

Check the [README's Known limitations](README.md#known-limitations) and [Configuration](README.md#configuration) sections, or open an issue.