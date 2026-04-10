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

### Other

```bash
qai clip <url> [notebook] [title]     # clip web page to Joplin
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

## Plugins

Drop any executable as `~/.qai/commands/qai-<name>` to add a subcommand:

```bash
qai deploy    # runs ~/.qai/commands/qai-deploy
```

## License

MIT
