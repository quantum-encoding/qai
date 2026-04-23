# qai

Unified CLI for AI tools, code analysis, search, and media generation. Single binary, zero config needed, local-first.

## Install

```bash
# One-line installer (interactive, picks optional deps)
curl -sSL https://raw.githubusercontent.com/quantum-encoding/qai/main/install.sh | bash

# Or from source (requires Go 1.22+)
go install github.com/quantum-encoding/qai-cli@latest

# Or download a pre-built binary from Releases
```

| Platform | Binary |
|----------|--------|
| macOS Apple Silicon | `qai-darwin-arm64` |
| macOS Intel | `qai-darwin-amd64` |
| Linux x86_64 | `qai-linux-amd64` |
| Linux ARM64 | `qai-linux-arm64` |
| Windows x86_64 | `qai-windows-amd64.exe` |

```bash
# First-time setup
qai init
```

## Commands

### Code Analysis

```bash
# Compiler-accurate analysis (auto-detects language)
qai analyze <path>                    # JSON output
qai analyze <path> --format summary   # overview stats
qai analyze <path> --complexity       # complexity hotspots
qai analyze <path> --calls            # call graph
qai analyze <path> --types            # types + fields

# Compile codebase into single file for AI consumption
qai compile <path>                    # markdown
qai compile <path> --format xml       # XML with CDATA
qai compile <path> --format json      # JSON array
qai compile <path> -o -               # stdout (pipe to another tool)
qai compile <path> --diff             # only uncommitted/staged/untracked files
qai compile <path> --diff main        # only files changed since a git ref

# Code scanner — type extraction + structural diff
qai scan <path>                       # scan one codebase
qai scan <path1> <path2>              # scan + diff two codebases
qai scan <path1> <path2> <path3>      # scan + diff pairwise

# Visual call/dependency graphs
qai graph <path>                      # call graph SVG
qai graph <path> --type modules       # module dependency graph
qai graph <path> --type deps          # package dependency graph

# AI vulnerability scanner (static analysis, 14 langs, 40+ vuln types)
qai security <path>                   # scan for vulnerabilities
qai security <path> --severity high   # filter by severity
qai security <path> --format json     # JSON output

# LLM code audit (parallel per-file analysis via configurable profiles)
qai audit <path>                      # security red-team audit (default)
qai audit <path> --profile code-review       # code quality review
qai audit <path> --profile security-blueteam # defensive security review
qai audit <path> --profile documentation     # generate documentation
qai audit <path> --dry-run            # show files without calling LLM
qai audit <path> -c 10 -m gemini-3.1-pro-preview  # concurrency + model
```

### Compiler-Accurate Parsers

`qai analyze` auto-detects the language and uses the right compiler parser:

| Language | Parser | Method |
|----------|--------|--------|
| Go | `go/ast` | Native (stdlib) |
| Rust | `syn` | Via codebase_deity `--parser syn` |
| TypeScript | TS Compiler API | Shell to `node` |
| Python | `ast` module | Shell to `python3` |
| Swift | Swift script | Shell to `swift` |
| Kotlin | Regex (Python) | Shell to `python3` |

Each parser extracts: functions with typed parameters, structs/classes with fields, interfaces/protocols, imports, call graphs, and cyclomatic complexity. No regex guessing — uses each language's own compiler.

### Search

```bash
qai search "query"                    # search all knowledge bases
qai search --rag "query"              # Vertex AI RAG only
qai search --surreal "query"          # SurrealDB RAG only
qai search --joplin "query"           # Joplin notes only
qai web "query"                       # Brave web search
qai ask "question"                    # AI-grounded answer
qai context "query"                   # LLM-optimized content chunks
```

### Projects & Agent Store (Joplin)

qai persists per-project context and a portable agent "brain" in a local
Joplin instance. Notebooks become the shared surface between CLI sessions,
Claude Code hooks, and anything else that can POST to the Joplin Web Clipper
— so the "work behind the work" accumulates next to the notes, searchable
from anywhere and editable in the Joplin UI.

#### Projects

```bash
qai project                           # same as --list
qai project --list            [-j]    # list notebooks (active one marked *)
qai project --create "<name>"         # create + set active
qai project --set    "<name>"         # switch active project
qai project --current         [-j]    # show active project + notebook id
qai project --show   "<name>" [-j]    # details + recent notes (newest first)
qai project --notes           [-j]    # list notes in the active project
qai project --copy <dir>      [-n]    # recursively copy a folder into the
                                      # active project as one note per file
```

The active project is persisted to `~/.qai/config.yaml` so future
subcommands and Claude Code hooks can target it without re-asking.

`--copy` walks the tree, skips hidden dirs (`.git`, `.venv`, `.next`, …)
plus `node_modules`, `dist`, `build`, `target`, `__pycache__`, `venv`,
`env`. Each text file becomes a note titled by its relative path with a
fenced code block. Re-runs update existing notes in place (matched by
title) so you can `--copy` after every commit without duplicates. Files
over 1 MiB or with a NUL byte in the first 512 bytes are skipped as binary.

#### Agent store

```bash
qai agent init                # create AGENT notebook + seed from ~/.claude
qai agent init --deep         # also walk ~/work for SKILL.md/CLAUDE.md/agents/*.md
qai agent sync                # alias for init (re-runs are idempotent)
qai agent list        [-j]    # list stored skills / agents / instructions
qai agent add <file>  [-n]    # classify a single file and add it
qai agent status      [-j]    # show notebook id + counts
```

Creates a top-level `AGENT` notebook with three sub-notebooks:

- `Skills/` — `SKILL.md` files and standalone `skills/<name>.md`.
  Reference files inside a skill's tree (e.g. `skills/foo/references/*.md`)
  roll into their parent SKILL.md instead of spawning separate notes.
- `Agents/` — direct children of any `agents/` directory. Files nested
  deeper are treated as support material for the profile above them.
- `Instructions/` — `CLAUDE.md` files (global from `~/.claude/`, plus
  per-project ones when `--deep` is set).

Think of it as a Joplin-hosted `~/.claude/`: the knowledge every project
depends on but isn't tied to any single one. Portable across machines,
searchable via `qai search --joplin "<term>"`, editable in the Joplin UI.

#### Environment

| Var               | Default                    | Notes |
|-------------------|----------------------------|-------|
| `JOPLIN_TOKEN`    | —                          | Required. Web Clipper API token (Joplin → Tools → Options → Web Clipper) |
| `JOPLIN_BASE_URL` | `http://127.0.0.1:41184`   | Override Joplin endpoint (e.g. running on a different port) |
| `QAI_PROJECT`     | —                          | Override the active project name for a single session |

### Media Generation

```bash
qai image "prompt" [provider]         # image generation
qai video "prompt" [provider]         # video generation
qai tts "text" [voice]                # text-to-speech
qai music "prompt"                    # music generation
qai edit input.png "prompt"           # image editing
```

### Conductor (Multi-Model API Gateway)

Replaces MCP servers with direct CLI commands. Requires `QAI_API_KEY`.

```bash
qai conduct chat <model> "message"    # multi-model LLM chat
qai conduct image "prompt"            # generate image
qai conduct video "prompt"            # queue video generation
qai conduct tts "text"                # text to speech
qai conduct search "query"            # RAG knowledge search
qai conduct web "query"               # Brave web search
qai conduct models                    # list models + pricing
qai conduct balance                   # check credit balance
```

### Terminal (tmux Management)

Replaces MCP servers with direct CLI commands. Requires `tmux`.

```bash
qai term list                         # list active terminals
qai term spawn "name" [--cwd /path]   # create terminal pane
qai term send "name" "input"          # send input to terminal
qai term read "name" [--lines 50]     # read terminal output
qai term close "name"                 # close terminal
qai term snapshot                     # overview all terminals
```

#### Delegation and parallel agents

The commands above are the mechanical surface. The capability they unlock is bigger than the syntax suggests: they let a parent agent (Claude, Gemini, a local model, or a human) spawn subagents that **inherit the parent's live conversation context** rather than starting fresh with a briefing document.

Every multi-agent framework on the market — LangChain, AutoGen, CrewAI, and the long tail behind them — treats a subagent as a new process that receives a prompt. The parent serialises its accumulated understanding into a system prompt, the subagent decodes that summary into an approximation of the context, and every delegation is lossy. Ten minutes of human correction of the parent agent gets flattened into a paragraph before the subagent ever runs.

`qai term` gives you a different pattern. The parent agent spawns a named tmux pane and writes instructions into it as if it were talking to a teammate who was already in the room when the decisions were made — because it is, in the sense that matters. The parent composes the subagent's instructions *after* the human has been iterating with the parent for an hour, so the instructions encode not just the task but every correction the human has made, every blind alley the parent has recognised, every taste judgement the parent and the human arrived at together.

The human can attach to any pane in their terminal emulator (iTerm, Ghostty, Terminal.app, SSH session, phone), read the subagent's live output, type a correction directly to it, and detach. The subagent carries on with the correction applied. This capability does not exist as a first-class surface in any of the mainstream agent frameworks.

Example — three subagents running in parallel, each forked from the same parent conversation:

```bash
# Parent Claude in your current chat spawns three panes
qai term spawn research --cwd /work/project
qai term spawn writer   --cwd /work/project
qai term spawn redteam  --cwd /work/project

qai term send research "gather recent papers on agent observability,
  write findings to facts.json; constraints we already agreed in this
  conversation: no marketing posts, no LinkedIn, arXiv preferred."

qai term send writer   "when facts.json exists, draft article.md in
  the house voice we've been refining; no claims outside facts.json."

qai term send redteam  "read article.md and flag every sentence that
  makes a claim not supported by a specific line in facts.json."

# At any point, the human or the parent can snapshot all panes,
# read a specific subagent's reasoning, and inject a correction:
qai term snapshot
qai term read writer --lines 200
qai term send redteam "sections 1 and 3 are approved; focus on 2 and 4"
```

#### Two mitigations worth building in

1. **Ground-truth checkpoint before the fork.** Context inheritance scales failures as well as successes. If the parent has accumulated a wrong assumption, every subagent forking from it will confidently repeat the error in parallel. Before spawning the crew, have the parent run a command, read a document or verify a test — something external to the conversation — to pin a claim to reality.
2. **Heterogeneous crew.** Don't spawn N identical workers; spawn N-1 workers and one adversary. Ideally the adversary uses a different model family (e.g. Claude-writes, Gemini-red-teams) so it brings a different training distribution's error profile. `qai conduct chat` is model-agnostic — each pane is free to call whichever model it wants.

#### Why tmux and not a bespoke dashboard

tmux is already the primitive. Session persistence across disconnects, SSH-from-anywhere, keyboard-driven pane navigation, scrollback search, working clipboard, integration with every terminal emulator. A custom agent-supervision GUI reinvents 90% of that badly. If your observability surface can't be attached from an SSH session on a phone, it's a status page, not an observability surface.

### Browser Automation (CDP)

Connects to your existing Chrome/Brave via the DevTools Protocol debug port. No headless browser, no Playwright, no Node.js — uses your real browser session with all cookies, auth, and fingerprints intact.

```bash
qai browser launch                    # start browser with debug port (auto-detects Brave/Chrome)
qai browser list                      # list open tabs
qai browser open <url>                # navigate to URL
qai browser extract [--html]          # get page text or HTML
qai browser screenshot [-o file.png]  # capture screenshot
qai browser click <selector>          # click element by CSS selector
qai browser click <x> <y>            # click at coordinates
qai browser type "text"               # type text character by character
qai browser eval "js expression"      # evaluate JavaScript
qai browser clip [notebook] [title]   # extract page + save to Joplin
qai browser wait <selector> [timeout] # wait for element to appear
qai browser source                    # get full page HTML
qai browser pdf [-o file.pdf]         # print page to PDF
qai browser tab <id>                  # activate a specific tab
qai browser scrape <urls.csv>         # batch extract text from each URL
qai browser scrape <urls.csv> --screenshot  # batch screenshot each URL
qai browser scrape <urls.csv> --html  # batch extract HTML from each URL
```

CSV format: first column is the URL, optional second column is a label. Header row auto-detected.

Options: `--delay <ms>` (default 1000), `-o <dir>` (output directory), `--json` (write manifest).

Global flags: `--port <n>` (default 9222 or `QAI_BROWSER_PORT`), `--tab <id>`, `--json`.

#### Security Perimeter

Four-layer defense against prompt injection attacks that try to exfiltrate data from authenticated browser sessions:

| Layer | Protection | Example |
|-------|-----------|---------|
| **Pattern block** | Hard-deny dangerous JS before it reaches the browser | `document.cookie`, `localStorage`, `fetch(`, `eval(`, `XMLHttpRequest`, `sendBeacon` |
| **Domain protection** | Flag sensitive domains (AWS, GitHub, banking, SSO, cloud consoles) | `console.aws.amazon.com`, `github.com`, `dash.cloudflare.com` |
| **TTY confirmation** | Require human `[y/N]` approval on sensitive domains; deny when non-interactive | Piped/automated input is denied by default |
| **Audit log** | JSONL trail of every command at `~/.qai/browser-audit.log` | Logged regardless of allow/deny |

User-configurable via `~/.qai/browser-policy.yaml`:

```yaml
# Add your org's sensitive domains
sensitive_domains:
  - "*.internal.mycompany.com"
  - "grafana.mycompany.com"

# Additional blocked eval patterns (regex)
blocked_patterns:
  - "internalAPI\\.secret"

# Domains that skip confirmation
trusted_domains:
  - "localhost"

# Require confirmation for ALL domains
strict_mode: false
```

Stealth injection removes `navigator.webdriver` and spoofs browser fingerprints (plugins, WebGL, permissions API) to avoid bot detection on legitimate automation tasks.

### RAG Ingestion & Vector Search

```bash
# Standard ingest (chunk + embed + store)
qai ingest --local my-docs ~/Documents/

# Load pre-computed embeddings (e.g. from Qwen3-8B, any dimension)
qai ingest --precomputed --local zig-std data/raw-embeddings/zig-std-0.16/

# Search (auto-filters by matching vector dimension)
qai search --local "memory allocator"

# Database management
qai db start                          # start local SurrealDB
qai db info                           # show providers, dimensions, chunk counts
qai db shell                          # interactive SurrealQL
```

Mixed embedding dimensions coexist in the same database — 768-dim (Ollama), 4096-dim (Qwen3-8B), or anything else. Search automatically matches query dimension to stored vectors.

### Product Scraper (`qai scrape`)

A pluggable, real-browser scraping pipeline for affiliate / research
workflows. Drives `qai clip` (Playwright → Joplin) to bypass bot
detection, parses the clipped note with a per-site preset, and emits a
JSON brief ready to hand to a review-writing agent.

```bash
# Single URL — preset auto-detected from hostname
qai scrape https://www.amazon.co.uk/dp/B0BT9R5XNN

# Explicit preset, custom image directory
qai scrape --amazon https://www.amazon.com/dp/B0GR1K7LHC \
           --image-dir ./site/public/product-images

# Build URL from product ID
qai scrape --preset amazon --id B0BT9R5XNN

# Batch from CSV, JSONL output, 3 workers, resumable
qai scrape --csv products.csv -o briefs.jsonl --parallel 3 --resume
```

CSV format is dead simple — one URL per line is enough; optional
`url,preset,notebook` header row for per-row overrides.

#### Two-stage: scout → scrape

For building a review library you rarely know all the URLs up front —
you have a category query. `--scout` turns a search / listing page into
a CSV of product URLs, which then feeds straight back into the normal
batch scraper.

```bash
# Stage 1 — scout a listing (~2s for 15 ASINs, direct HTTP fetch)
qai scrape --scout "https://www.amazon.co.uk/s?k=amd+threadripper" \
           --max 30 -o threadrippers.csv

# Stage 2 — scrape each product (real browser, anti-bot bypass)
qai scrape --csv threadrippers.csv --parallel 3 -o briefs.jsonl --resume
```

The two stages deliberately use different engines because they face
different threat models:

- **Scout** is a plain `GET` with a realistic `User-Agent`. Marketplace
  search pages are server-rendered and ship every `/dp/ASIN` in the
  initial HTML, so a real browser buys nothing. Clipping them also
  reliably kills Playwright's `networkidle` wait — search pages poll
  analytics indefinitely. Direct fetch is both faster and actually
  works (clip path timed out at 30s; scout returns in ~2s).
- **Scrape** (per-product) still goes through `qai clip` — a real
  browser with your cookies is where Amazon's anti-bot actually bites,
  and where that machinery earns its keep.

`--max N` caps to the first N products (body order mirrors listing
order, so top N ≈ top results). Output is a headered CSV (`url,preset`)
that feeds back in via `--csv` with no massaging.

#### Per-URL pipeline

1. `qai clip <url>` — Playwright drives a real browser, writes to Joplin
2. Fetch the note via Joplin Data API (direct-by-ID, no search lag)
3. Preset parser extracts title, price, bullets, spec table, related IDs
4. Walk note's embedded images in body order, return the first that
   passes the preset's hero filters (dims, aspect, format)
5. Save hero as `<image-dir>/<ID>.jpg`
6. Emit JSON brief

Presets ship as Go packages — each owns its `ExtractID`, `Parse`,
`BuildURL`, and `ImageFilters`, with optional `Scout(body, searchURL)`
and `ScoutTitle(searchURL)` for listing extraction. The clip/fetch/hero
pipeline is shared. Amazon is the reference preset; add new ones by
calling `scrape.RegisterPreset(...)` from a package `init()`.

### Other

```bash
qai clip <url> [notebook] [title]     # clip web page to Joplin (raw)
qai models [filter]                   # search model registry
qai token                             # GCP access token
qai token --check                     # check ADC validity
```

## Scanner Features

The `qai scan` command provides:

- **6 language parsers** — Go, Rust, TypeScript, Python, Swift, Kotlin + OpenAPI YAML
- **Type alias detection** — `typealias`, `type =`, top-level assignments
- **Convention-aware diff** — case-insensitive matching (`TTSRequest` ↔ `TtsRequest`)
- **Changelog tracking** — cached baselines, shows delta on rescan
- **Field-level mismatch detection** — `qai embed` finds types with different field sets across codebases

## Audit Profiles

The `qai audit` command sends each source file to an LLM for analysis. Built-in profiles:

| Profile | Purpose |
|---------|---------|
| `security-redteam` | Find exploitable vulnerabilities (default) |
| `security-blueteam` | Defense-in-depth review, hardening, compliance |
| `code-review` | Bugs, performance, maintainability, best practices |
| `documentation` | Generate docs: API, architecture, usage notes |

Features: parallel workers (`-c`), exponential backoff on rate limits, per-file markdown output, JSON manifest with token counts.

## Dependencies

- **Required**: Go 1.26+
- **For `qai scan`/`qai graph`**: [codebase_deity](https://github.com/quantum-encoding/codebase_deity) on PATH
- **For `qai analyze` (TypeScript)**: Node.js + `typescript` npm package
- **For `qai analyze` (Swift)**: Xcode Command Line Tools
- **For `qai analyze` (Python/Kotlin)**: Python 3.10+
- **For `qai graph` (SVG)**: graphviz (`brew install graphviz`)
- **For `qai security`**: [rust-security-detector](https://github.com/quantum-encoding/rust-security-detector) on PATH
- **For `qai audit`**: `QAI_API_KEY` environment variable
- **For `qai conduct`**: `QAI_API_KEY` environment variable
- **For `qai term`**: tmux
- **For `qai browser`**: Chrome, Brave, or Edge with `--remote-debugging-port`
- **For `qai project` / `qai agent`**: Joplin Desktop with Web Clipper Service enabled, plus `JOPLIN_TOKEN` env var

## Plugins

Drop any executable as `~/.qai/commands/qai-<name>` to add a subcommand:

```bash
qai deploy    # runs ~/.qai/commands/qai-deploy
```

## License

MIT
