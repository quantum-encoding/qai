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

**macOS users downloading a release tarball directly:** strip the
quarantine xattr after extracting, or Gatekeeper will silently kill
the binary on first run (exit 137, no output):

```bash
xattr -c ~/.local/bin/qai     # or wherever you put it
```

The `install.sh` script does this automatically; only manual extracts
need it.

```bash
# First-time setup
qai init
```

> **New here?** [`docs/getting-started.md`](docs/getting-started.md) walks
> through the auth surface end-to-end — the one `QAI_API_KEY` that powers
> the AI features, plus the optional secondary credentials (Joplin, Brave,
> GCP, SurrealDB) for the non-AI surfaces.
>
> **Working with an agent?** [`docs/notes.md`](docs/notes.md) covers
> `qai note` for saving session summaries + user TODOs to Joplin —
> the agent-shorter-message pattern, plus the optional Claude Code
> Stop-hook for automatic capture.

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
| `JOPLIN_TIMEOUT`  | `60s`                      | HTTP client timeout (clip POSTs of multi-MB body_html need this) |
| `QAI_PROJECT`     | —                          | Override the active project name for a single session |

#### Cross-repo edit-target tagging

When the agent is launched from repo A but does all its work in repo
B (typical Claude Code "port the hot tub site to the new dropship
project" sessions), the cwd-based tag resolver picks A and pollutes
your graph. The edit-target hook fixes this: a Claude Code
`PostToolUse` hook on `Edit|Write|MultiEdit` writes the last-edited
file's path to `~/.qai/target-edit-path`; `projectid.Resolve` reads
it first and anchors resolution on the target dir instead. Failure-
redirecting (never breaks single-repo sessions).

Setup walkthrough + hook script: [`docs/edit-target-hook.md`](docs/edit-target-hook.md).

### Joplin Bridge & Graph

Syncs your Joplin library into SurrealDB as a typed graph (notes ×
notebooks × tags × wiki-links) and exposes an agent-memory read verb
that hydrates a fresh session with relevant context.

```bash
# write/maintain
qai joplin bridge sync                  # one-shot full pull, resumable
qai joplin bridge tail                  # event-stream daemon
qai joplin bridge status [--watch|--json|--check]
qai joplin bridge schema                # print the embedded schema

# read
qai joplin graph context [query]        # agent-memory bundle as JSON
qai joplin graph context --project X    # notes tagged project:X
qai joplin graph context --tag A --with B   # tag intersection
```

`bridge status` exposes a structured `HealthCheck` (seven-state
classifier, OK/Reason contract) consumed both by the human CLI and by
`graph context`. The freshness gate is policy-by-design — serve-
stale-with-label by default, `--strict` opt-in for cron consumers
that prefer empty-and-fail. Full-text recall via BM25 over note title
+ excerpt; graph neighbourhood (`contains`, `nested_in`, `has_tag`,
`links_to`) attaches around the search hits.

**Full coverage:** [`docs/joplin-bridge.md`](docs/joplin-bridge.md) —
sync resumability, tail heartbeats, health classification table,
gate decision matrix, payload schema, SurrealDB schema notes.

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
qai term send --json [file]           # batch: shared body + per-pane messages
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

#### Batch send — one payload, shared body, per-pane messages

The example above sends three prompts with three separate calls, and each
prompt re-types the same shared context. `qai term send --json` collapses
that into a single invocation: write the common context once, give each pane
its own message. It reads a JSON payload from stdin (a heredoc) or a file.

```bash
qai term send --json <<'EOF'
{
  "shared": "Constraints we already agreed in this conversation: no marketing posts, no LinkedIn, arXiv preferred. House voice we've been refining all session.",
  "shared_suffix": "Report back in this pane when done.",
  "enter": true,
  "messages": [
    { "pane": "research", "message": "Gather recent papers on agent observability; write findings to facts.json." },
    { "pane": "writer",   "message": "When facts.json exists, draft article.md; no claims outside facts.json." },
    { "pane": "redteam",  "message": "Read article.md and flag every sentence not supported by a line in facts.json." }
  ]
}
EOF
```

Each pane receives `shared` + its `message` + `shared_suffix`. Two template
tokens are substituted in the merged text: `{{message}}` (the per-pane
message — if present in `shared`, the message lands there inline instead of
being prepended) and `{{pane}}` (the pane name/id). `enter` sets the default
submit behaviour; a per-message `enter` overrides it (useful for a pane you
want to stage but not yet fire).

The batch is **atomic**: every pane is resolved up front, and if any one is
unknown the command reports all the failures and exits without sending a
single message — a typo in the last entry won't half-drive the earlier panes.
Sends are serial and each uses a per-pane tmux *named* buffer (`qai_send_<id>`,
deleted after paste), so a long run can't collide with your own clipboard.
Pass `--fail-fast` to abort on the first send error instead of attempting the
rest. The command reports `✓`/`✗` per pane and exits non-zero if any failed.

#### Two mitigations worth building in

1. **Ground-truth checkpoint before the fork.** Context inheritance scales failures as well as successes. If the parent has accumulated a wrong assumption, every subagent forking from it will confidently repeat the error in parallel. Before spawning the crew, have the parent run a command, read a document or verify a test — something external to the conversation — to pin a claim to reality.
2. **Heterogeneous crew.** Don't spawn N identical workers; spawn N-1 workers and one adversary. Ideally the adversary uses a different model family (e.g. Claude-writes, Gemini-red-teams) so it brings a different training distribution's error profile. `qai conduct chat` is model-agnostic — each pane is free to call whichever model it wants.

#### Why tmux and not a bespoke dashboard

tmux is already the primitive. Session persistence across disconnects, SSH-from-anywhere, keyboard-driven pane navigation, scrollback search, working clipboard, integration with every terminal emulator. A custom agent-supervision GUI reinvents 90% of that badly. If your observability surface can't be attached from an SSH session on a phone, it's a status page, not an observability surface.

### Fleet (declarative parallel agent panes)

`qai fleet` is the next layer up from `qai term`. Where `qai term spawn` is the imperative one-pane-at-a-time primitive, `qai fleet up <manifest.yaml>` brings up an entire fleet of agents from a single declarative file: `N` panes, each with its own working directory, agent kind (fresh or resumed), and prompt. Workers report status into a per-fleet inbox; a notifier daemon nudges the architect's pane when reports land.

```bash
qai fleet up        <manifest.yaml> [--dry]   # spawn every pane; --dry validates
qai fleet down      <manifest.yaml>           # tear down + stop notifier
qai fleet status    <manifest.yaml> [--json]  # alive/dead per pane
qai fleet snapshot  <manifest.yaml>           # tail every pane's last lines
qai fleet inbox     [--unread] [--watch] [--json]  # read worker reports
qai fleet bootstrap                           # print architect prompt protocol

qai sessions list   [--cwd <path>] [--json]   # discover live + historical Claude sessions
qai report          --status <s> --message m  # called by workers from inside their pane
```

Manifest schema (v1):

```yaml
version: 1

defaults:
  cwd: /Users/me/work
  startup_timeout: 45s
  reporting:
    enabled: true   # auto-injects a "qai report when done/blocked" block
                    # into every worker's prompt

panes:
  - name: gs-security-audit
    cwd: /Users/me/work/guardian
    agent:
      kind: fresh                       # cold start
      cmd: claude
      args: ["--permission-mode", "auto"]
    prompt: |
      Audit this Zig codebase for memory safety issues.

  - name: chronos-specialist
    cwd: /Users/me/work/chronos_engine
    agent:
      kind: resume                      # claude --resume <uuid>
      cmd: claude
      session: "@chronos_engine"        # @alias resolves against ~/.claude/
    prompt: |
      Pick up where we left off on the timestamp serialisation bug.

  - name: writer
    cwd: /Users/me/work/article
    agent: { kind: fresh, cmd: claude }
    wait_for: /Users/me/work/article/facts.json   # only orchestration primitive
    prompt: |
      Draft article.md from facts.json in the house voice.
```

The two flavours are the load-bearing distinction:

- **Fresh agent** — cold start. Prompt is the entire context. Used for tasks that don't need history: security audits, doc generation, scans, anything throwaway.
- **Resumed agent** — `claude --resume <session-id>`. The specialist Claude that's already been working in that repo for days, has been corrected, has the taste. Prompt is a nudge ("pick up where we left off"), not a briefing.

`@alias` for resumed sessions is matched against the on-disk Claude session store at `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl` — most-recent session whose `cwd` basename matches the alias wins. `qai sessions list --cwd <path>` is the discovery command.

#### Worker → architect reporting

The architect (you, with Claude in the top-left tmux pane) doesn't poll worker output by hand. Workers call `qai report --status <state> --message "<text>"` when they finish or get stuck — `QAI_FLEET_ID` and `QAI_FLEET_PANE` env vars are set by `qai fleet up` so the worker knows which inbox to write to.

A notifier daemon (`qai fleet notifier <id>`, started automatically by `up`) watches `~/.qai/fleet/<id>/inbox.jsonl`, debounces 10s, and fires a single human-voiced nudge into the architect's pane: `[FLEET] check your inbox, N new reports waiting — you have permission`. The architect's prompt protocol (`qai fleet bootstrap | tee -a ~/.claude/CLAUDE.md`) teaches it to drain via `qai fleet inbox --unread --json` and act per-report.

The notifier holds the nudge while the architect's input box is non-empty (you're mid-typing) and resumes once it goes idle, so a worker finishing while you're composing a reply doesn't stomp your input.

Two cursors are tracked independently per fleet: an `architect-cursor` advanced by `qai fleet inbox --unread`, and a `notifier-cursor` advanced by the notifier when it fires a nudge. Reports persist in the inbox file until the architect explicitly drains them; the notifier never consumes records on the architect's behalf.

#### What lands at the end

For a 15-pane mission: 15 `findings.md` (or whatever artefact the prompt asked for) committed in their respective repos, an `~/.qai/fleet/<id>/inbox.jsonl` with every report, and a sequence of `[FLEET]` nudges that gave the architect a single drain-call per batch instead of N polling rounds.

#### Operator workflow guide

For the day-to-day playbook (mental model, common bugs hit during iteration, state-file layout, when to use `qai term` vs `qai fleet`), see [`docs/AGENTS.md`](docs/AGENTS.md).

### Browser Automation (CDP)

Drives your existing Chrome/Brave via the DevTools Protocol debug port.
No headless browser, no Playwright, no Node.js — every command attaches
to the browser you already have open, so your cookies, auth, and
extensions are all live.

```bash
qai browser launch                      # auto-detect Brave/Chrome, start with debug port
qai browser list                        # tabs + slot pinning + summary footer

# navigate + interact
qai browser open <url> [--wait <css>] [--theme light|dark] [--timeout 30s]
qai browser screenshot [--selector <css>] [-o file.png]
qai browser emulate <device> <url> [--selector] [--wait] [--theme]
qai browser extract|source|pdf|click|type|eval|wait|clip|tab|scrape …

# multi-tab management (NEW)
qai browser new [url] [--slot <name>]   # spawn in background, optionally pin
qai browser close <slot-or-id>          # close by slot name or hex (prefix ok)
qai browser open … --tab TAB1           # address pinned tab by slot name
```

`qai browser clip` writes `body_html` to Joplin Desktop's `/notes`
endpoint — the same endpoint the Joplin Web Clipper extension uses —
so the resulting note is bit-compatible with what the extension
produces. Stdout is the new note's ID for clean pipelining.

Layered security perimeter (pattern block, hard-deny schemes, ~30
builtin sensitive-domain list, TTY confirmation, list redaction, batch
pre-flight, audit log). User-configurable via
`~/.qai/browser-policy.yaml`.

**Full coverage:** [`docs/browser.md`](docs/browser.md) — every
subcommand, the tab/slot system end-to-end, the security model in
detail, and the determinism notes (the `--theme` renderer-barrier
fix, the React-grid wait-selector trap that bit the CJ workflow, etc.)

### CJ Dropshipping research (`qai cj`)

The CJ intelligence dashboard + catalog search are React SPAs behind
a bot wall — neither headless scraping nor the CJ Open Platform API
surface the demand data the dashboard shows. `qai cj` codifies the
"human web-clips → agent extracts → boards downstream" workflow so
no future session has to re-derive the regex.

```bash
qai cj extract <clip.md>             # parse a clipped CJ page → JSON
qai cj extract -                     # same, from stdin
qai cj extract --joplin <id|title>   # fetch + parse a Joplin note
qai cj batch <urls.csv>              # navigate → wait → clip → extract × N
```

Handles two CJ page shapes (intelligence dashboard + catalog search)
through the same `CJProduct[]` output, with PID-dedup. `batch` mode
drives the full chain per row, with throttle-aware retry
(exponential backoff + jitter, default 3 attempts) and a `--soft-
wait` escape hatch for when CJ's anti-bot leaves the page in skeleton
state.

**Full coverage:** [`docs/cj.md`](docs/cj.md) — the three edge cases
the parser survives (literal `|` in titles, NBSP days cell, trailing
"Details" anchor), useful jq one-liners for boardable sorting, the
honest anti-bot reality + workarounds.

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
