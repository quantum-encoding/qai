# Getting Started

This guide covers everything you need to do once between *cloned the repo*
and *first useful command*.

## TL;DR for the impatient

```bash
# 1. Build (you already have Go installed, right?)
go install ./cmd/qai

# 2. Get a key
#    See: https://quantumencoding.ai
export QAI_API_KEY="qai_..."

# 3. First command
qai image "an isometric server rack at sunset"
```

If that produced a PNG path on stdout, you're done. The rest of this guide
is for unlocking specific subcommands that need their own credentials.

## How auth works in qai

`qai` is a CLI client. By default it talks to a managed broker at
`https://api.quantumencoding.ai` that handles fan-out to xAI, OpenAI,
Google, Anthropic, and friends on your behalf. **One `QAI_API_KEY`
authenticates you against every provider qai supports** — you don't need
five separate vendor keys for the AI surfaces.

The model registry (`qai models`) shows which models route through the
broker. Almost everything generative does.

A few subcommands talk to non-AI services and need their own credentials.
Those are documented per-feature below — none are required for a working
qai install, just for specific features.

## The one key that matters: `QAI_API_KEY`

Get it from **<https://quantumencoding.ai>** — sign in, generate a key, copy it.

Once you have it, set it however your shell prefers:

```bash
# zsh / bash — add to ~/.zshrc or ~/.bashrc
export QAI_API_KEY="qai_..."

# fish
set -gx QAI_API_KEY "qai_..."

# or persist it in qai's own config (interactive wizard):
qai init
```

`QAI_API_KEY` unlocks:

| Subcommand              | What it does                                         |
|-------------------------|------------------------------------------------------|
| `qai image`             | xAI Grok Imagine, OpenAI GPT-Image, Gemini 3 Pro     |
| `qai video`             | Grok Imagine video, Veo, Sora                        |
| `qai tts`               | OpenAI TTS, Gemini TTS                               |
| `qai music`             | Generative audio                                     |
| `qai edit`              | Image edits (single, parallel, Vertex batch)         |
| `qai conduct`           | Direct multi-model API gateway (low-level)           |
| `qai audit`             | LLM code audit with profiles                         |
| `qai ask` / `qai context` | Brave AI-grounded answers (also see Brave below)   |
| `qai analyze`           | Compiler-accurate code analysis with LLM summaries   |

If a command says `QAI_API_KEY not set`, it needs this key.

## Self-hosting the broker

If you'd rather run your own gateway (or point at a staging instance):

```bash
export QAI_BASE_URL="https://your-gateway.example.com"
export QAI_API_KEY="whatever-your-gateway-expects"
```

You'll need to provide the upstream provider keys to *your* gateway —
that's outside qai's scope. Most users don't want this; the managed
broker exists so you don't have to.

## Optional: feature-specific credentials

These are only needed if you use the corresponding subcommand. None
break the rest of qai if missing.

### Joplin — for `clip`, `project`, `agent`, knowledge search

`qai clip <url>`, `qai project`, `qai agent`, and the Joplin path of
`qai search` all talk to the local Joplin desktop app's Web Clipper
service.

**Setup:**

1. Install [Joplin desktop](https://joplinapp.org/).
2. Tools → Options → Web Clipper → enable it. Copy the API token shown.
3. Export it:

```bash
export JOPLIN_TOKEN="copied-from-joplin-options"
# Optional — only if you've moved the clipper off port 41184:
export JOPLIN_BASE_URL="http://127.0.0.1:41184"
```

Joplin must be running for any of these commands to succeed. If it isn't,
qai prints an actionable error.

### Brave Search — for `web`, `ask`, `context`

These three commands query Brave's API directly (not via the qai broker)
because Brave is cheap, the rate limits are generous, and the data is
fresh.

**Setup:**

1. Get a key from https://api.search.brave.com/ (free tier covers most
   personal use).
2. Export it:

```bash
export BRAVE_SEARCH_API_KEY="..."
```

### GCP — for `qai token`, Vertex AI RAG, `qai search --rag`

`qai search --rag` and the Vertex AI integrations talk to Google Cloud
Vertex AI directly using your gcloud credentials.

**Setup:**

```bash
# Install the gcloud CLI: https://cloud.google.com/sdk/docs/install
gcloud auth application-default login

# Optional: pin the project + region qai uses
export GCP_PROJECT="your-project-id"
export GCP_REGION="us-central1"
export GCS_BUCKET="your-bucket-for-rag-uploads"
```

`qai token` will refresh ADC tokens automatically; it's the helper to
use when calling identity-token-protected Cloud Run services from
scripts.

### SurrealDB — for the local-first RAG store

`qai db`, `qai ingest --surreal`, and `qai search --surreal` use a
SurrealDB instance for embeddings + structured knowledge. You can run
this locally or against SurrealDB Cloud.

**Local SurrealDB:** install per https://surrealdb.com/install , and
qai will speak to it on the default port. No env vars needed.

**SurrealDB Cloud:**

```bash
export SURREAL_CLOUD_URL="wss://...surreal.cloud"
export SURREAL_CLOUD_NS="your-namespace"
export SURREAL_CLOUD_DB="your-database"
export SURREAL_CLOUD_USER="..."
export SURREAL_CLOUD_PASS="..."
```

### Embeddings — for `qai ingest` and SurrealDB-backed search

The first time you run `qai init`, you'll be asked to pick an embedding
provider. Four options:

1. **Ollama** — fully local, no key, zero cost. Requires Ollama
   installed + the `nomic-embed-text` model pulled. Best for privacy
   and offline work.
2. **Google Gemini** — free tier (1500 req/day). Get a key from
   https://aistudio.google.com.
3. **OpenAI** — `text-embedding-3-small`. Get a key from
   https://platform.openai.com.
4. **QAI Managed** — embeddings via your existing `QAI_API_KEY`.
   Simplest if you've already done step 2 of the TL;DR.

The wizard validates your choice and writes the result to
`~/.qai/config.yaml`. You can switch later by re-running `qai init`
or editing the file directly.

## What needs no credentials at all

These work out of the box on a fresh install:

| Subcommand | Notes |
|---|---|
| `qai scan`      | Local Go-types and Rust tree-sitter scanner |
| `qai compile`   | Codebase-to-single-file dump |
| `qai graph`     | Call-graph SVG/DOT generator |
| `qai security`  | Local CWE-mapped vuln scanner |
| `qai term`      | tmux pane management |
| `qai fleet`     | declarative parallel agent panes |
| `qai sessions`  | discover Claude Code sessions on disk |
| `qai report`    | worker → architect inbox writer |
| `qai models`    | model registry + pricing lookup |
| `qai browser`   | CDP browser automation (just needs Chrome) |

## The init wizard

`qai init` is interactive and walks you through:

1. Setting `QAI_API_KEY` (or pasting it).
2. Picking an embedding provider.
3. Validating both work (live key check).
4. Writing `~/.qai/config.yaml`.

You can always re-run it. The wizard never overwrites an existing key
without asking.

## Troubleshooting

**`QAI_API_KEY not set`** — see "The one key that matters" above. The
key needs to be in the environment of the shell that runs `qai`, which
also includes any tmux pane or shell where Claude Code (or another
agent) calls qai.

**`joplin: connection refused`** — Joplin desktop isn't running, or
Web Clipper isn't enabled in Tools → Options → Web Clipper.

**`gcloud auth application-default login`** — gcloud's ADC token
expired. Run that command and try again. `qai token --check` is the
quickest way to verify.

**`exit code 137` on macOS, no output, freshly copied binary** —
Gatekeeper rejected the binary because `cp` inherited a
`com.apple.provenance` xattr. Fix:

```bash
xattr -d com.apple.provenance /Users/<you>/.local/bin/qai
# or, more aggressive:
xattr -c /Users/<you>/.local/bin/qai
```

If the issue keeps coming back after every reinstall, prefer
`go install` directly (writes to `~/go/bin/qai`, no Gatekeeper friction)
or `install -m 755 ~/go/bin/qai ~/.local/bin/qai` instead of `cp`.

**Most other errors are actionable** — qai's error messages name the
env var or config field you need to set. If something says "X not set"
or "Y not configured", that's literally the fix.

## What's next

- Read [docs/AGENTS.md](AGENTS.md) for the day-to-day fleet workflow.
- Read [docs/mcp-investigation.md](mcp-investigation.md) if you want to
  know why qai doesn't ship any MCP servers loaded by default.
- Run `qai <command> --help` for the surface of any subcommand. The
  intent is that lazy `--help` is enough — no upfront manifest needed.
