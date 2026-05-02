// qai — unified CLI for AI tools, search, code scanning, and media generation.
//
// Thin dispatcher that routes subcommands to internal packages. Each
// subcommand owns its own --help text, so `qai image --help` shows only
// image-generation help — not the full top-level menu.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/agent"
	"github.com/quantum-encoding/qai-cli/internal/analyze"
	"github.com/quantum-encoding/qai-cli/internal/audit"
	"github.com/quantum-encoding/qai-cli/internal/browser"
	"github.com/quantum-encoding/qai-cli/internal/compile"
	"github.com/quantum-encoding/qai-cli/internal/conduct"
	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/db"
	"github.com/quantum-encoding/qai-cli/internal/fleet"
	"github.com/quantum-encoding/qai-cli/internal/graph"
	"github.com/quantum-encoding/qai-cli/internal/ingest"
	"github.com/quantum-encoding/qai-cli/internal/initcmd"
	"github.com/quantum-encoding/qai-cli/internal/note"
	"github.com/quantum-encoding/qai-cli/internal/project"
	"github.com/quantum-encoding/qai-cli/internal/scrape"
	"github.com/quantum-encoding/qai-cli/internal/search"
	"github.com/quantum-encoding/qai-cli/internal/security"
	"github.com/quantum-encoding/qai-cli/internal/terminal"
	"github.com/quantum-encoding/qai-cli/internal/token"
)

var (
	home      = config.HomeDir()
	pluginDir = filepath.Join(home, ".qai", "commands")

	// External helper scripts retained for features that still shell out.
	clipToJoplinJS = filepath.Join(home, ".claude", "scripts", "clip-to-joplin.mjs")
	scanScript     = filepath.Join(home, ".claude", "scripts", "scan-codebase.sh")
	modelsCSV      = filepath.Join(home, ".claude", "models.csv")
)

func main() {
	cfg := config.Load()
	conduct.Cfg = cfg
	search.Cfg = cfg
	ingest.Cfg = cfg
	audit.Cfg = cfg
	db.Cfg = cfg
	project.Cfg = cfg
	agent.Cfg = cfg
	note.Cfg = cfg

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "help", "--help", "-h":
		printUsage()
	case "init":
		initcmd.CmdInit(args)
	case "search", "s":
		cmdSearch(args)
	case "image", "img":
		cmdImage(args)
	case "video", "vid":
		cmdVideo(args)
	case "tts", "speak":
		cmdTTS(args)
	case "music":
		cmdMusic(args)
	case "edit":
		cmdEdit(args)
	case "web", "w":
		cmdWeb(args)
	case "ask":
		cmdAsk(args)
	case "context", "ctx":
		cmdContext(args)
	case "ingest":
		if helpFlag(args) {
			fmt.Println(helpIngest)
			return
		}
		ingest.CmdIngest(args)
	case "scan":
		cmdScan(args)
	case "compile":
		if helpFlag(args) {
			fmt.Println(helpCompile)
			return
		}
		compile.CmdCompile(args)
	case "graph":
		if helpFlag(args) {
			fmt.Println(helpGraph)
			return
		}
		graph.CmdGraph(args)
	case "analyze":
		if helpFlag(args) {
			fmt.Println(helpAnalyze)
			return
		}
		analyze.CmdAnalyze(args)
	case "security":
		if helpFlag(args) {
			fmt.Println(helpSecurity)
			return
		}
		security.CmdSecurity(args)
	case "audit":
		if helpFlag(args) {
			fmt.Println(helpAudit())
			return
		}
		audit.CmdAudit(args)
	case "browser", "b":
		browser.CmdBrowser(args) // handles its own --help
	case "db":
		db.CmdDB(args) // handles its own --help
	case "clip":
		cmdClip(args)
	case "scrape":
		scrape.CmdScrape(args) // handles its own --help
	case "token":
		if helpFlag(args) {
			fmt.Println(helpToken)
			return
		}
		token.CmdToken(args)
	case "models":
		cmdModels(args)
	case "conduct", "c":
		conduct.CmdConduct(args) // handles its own --help
	case "project", "proj":
		project.CmdProject(args) // handles its own --help
	case "agent":
		agent.CmdAgent(args) // handles its own --help
	case "note", "n":
		note.CmdNote(args) // handles its own --help
	case "term", "terminal", "t":
		terminal.CmdTerminal(args) // handles its own --help
	case "fleet":
		fleet.CmdFleet(args) // handles its own --help
	case "sessions":
		fleet.CmdSessions(args) // handles its own --help
	case "report":
		fleet.CmdReport(args) // worker-side: post a report to the fleet inbox
	default:
		if !tryPlugin(cmd, args) {
			fmt.Fprintf(os.Stderr, "qai: unknown command %q\n", cmd)
			fmt.Fprintln(os.Stderr, "Run 'qai --help' for the command list.")
			os.Exit(1)
		}
	}
}

// ─── help helpers ───────────────────────────────────────────────────────────

// helpFlag reports whether args request help (--help, -h, help).
func helpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			return true
		}
	}
	return false
}

// ─── plugins ────────────────────────────────────────────────────────────────

func tryPlugin(cmd string, args []string) bool {
	bin := filepath.Join(pluginDir, "qai-"+cmd)
	if info, err := os.Stat(bin); err == nil && info.Mode()&0111 != 0 {
		run(bin, args...)
		return true
	}
	return false
}

func listPlugins() []string {
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "qai-") {
			info, err := e.Info()
			if err == nil && info.Mode()&0111 != 0 {
				names = append(names, strings.TrimPrefix(name, "qai-"))
			}
		}
	}
	return names
}

// ─── search ─────────────────────────────────────────────────────────────────

func cmdSearch(args []string) {
	if len(args) == 0 || helpFlag(args) {
		fmt.Println(helpSearch)
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}

	mode := "all"
	query := ""
	var extra []string

	for i, a := range args {
		switch a {
		case "--rag":
			mode = "rag"
		case "--surreal":
			mode = "surreal"
		case "--local":
			mode = "local"
		case "--text":
			mode = "text"
		case "--similar":
			mode = "similar"
		case "--read":
			mode = "read"
		case "--list":
			mode = "list"
		case "--joplin":
			mode = "joplin"
		case "--json":
			// reserved for future unified JSON output
		default:
			if query == "" {
				query = a
			} else {
				extra = append(extra, args[i:]...)
				goto done
			}
		}
	}
done:

	switch mode {
	case "rag":
		ragArgs := append([]string{query}, extra...)
		search.RAGSearch(ragArgs)

	case "surreal":
		provider := ""
		limit := 5
		if len(extra) > 0 {
			provider = extra[0]
		}
		if len(extra) > 1 {
			fmt.Sscanf(extra[1], "%d", &limit)
		}
		search.CloudSurreal(query, provider, limit)

	case "local":
		provider := ""
		limit := 5
		if len(extra) > 0 {
			provider = extra[0]
		}
		if len(extra) > 1 {
			fmt.Sscanf(extra[1], "%d", &limit)
		}
		search.LocalSurreal(query, provider, limit)

	case "text":
		provider := ""
		limit := 10
		if len(extra) > 0 {
			provider = extra[0]
		}
		if len(extra) > 1 {
			fmt.Sscanf(extra[1], "%d", &limit)
		}
		search.LocalText(query, provider, limit)

	case "similar":
		provider := ""
		limit := 10
		if len(extra) > 0 {
			provider = extra[0]
		}
		if len(extra) > 1 {
			fmt.Sscanf(extra[1], "%d", &limit)
		}
		search.LocalSimilar(query, provider, limit)

	case "read":
		provider := ""
		if len(extra) > 0 {
			provider = extra[0]
		}
		search.ReadLocalFile(query, provider)

	case "list":
		search.ListLocalFiles(query)

	case "joplin":
		limit := 10
		if len(extra) > 0 {
			fmt.Sscanf(extra[0], "%d", &limit)
		}
		search.JoplinSearch(query, limit)

	case "all":
		fmt.Println("━━━ Joplin ━━━")
		search.JoplinSearch(query, 5)
		if search.Cfg.Surreal.CloudURL != "" {
			fmt.Println("\n━━━ SurrealDB RAG ━━━")
			search.CloudSurreal(query, "", 5)
		} else {
			fmt.Println("\n━━━ Local RAG ━━━")
			search.LocalSurreal(query, "", 5)
		}
		if search.APIBase() != "" {
			fmt.Println("\n━━━ Vertex AI RAG ━━━")
			search.RAGSearch([]string{query})
		}
	}
}

// ─── media (image, video, tts, music) ───────────────────────────────────────

func cmdImage(args []string) {
	if helpFlag(args) {
		fmt.Println(helpImage)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai image \"prompt\" [provider]")
		fmt.Fprintln(os.Stderr, "Run 'qai image --help' for details.")
		os.Exit(1)
	}
	conduct.CmdConduct(append([]string{"image"}, args...))
}

func cmdVideo(args []string) {
	if helpFlag(args) {
		fmt.Println(helpVideo)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai video \"prompt\" [provider]")
		fmt.Fprintln(os.Stderr, "Run 'qai video --help' for details.")
		os.Exit(1)
	}
	conduct.CmdConduct(append([]string{"video"}, args...))
}

func cmdTTS(args []string) {
	if helpFlag(args) {
		fmt.Println(helpTTS)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai tts \"text\" [voice]")
		fmt.Fprintln(os.Stderr, "Run 'qai tts --help' for details.")
		os.Exit(1)
	}
	conduct.CmdConduct(append([]string{"tts"}, args...))
}

func cmdMusic(args []string) {
	if helpFlag(args) {
		fmt.Println(helpMusic)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai music \"prompt\"")
		os.Exit(1)
	}
	conduct.CmdConduct(append([]string{"music"}, args...))
}

func cmdEdit(args []string) {
	if helpFlag(args) {
		fmt.Println(helpEdit)
		return
	}

	// Batch mode: qai edit --batch [--parallel N] "prompt" <input-dir> <output-dir>
	for _, a := range args {
		if a == "--batch" {
			rest := make([]string, 0, len(args))
			for _, x := range args {
				if x != "--batch" {
					rest = append(rest, x)
				}
			}
			conduct.CmdEditBatch(rest)
			return
		}
	}

	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai edit <input.png> \"prompt\" [--model M]")
		fmt.Fprintln(os.Stderr, "       qai edit --batch [--parallel N] \"prompt\" <input-dir> <output-dir>")
		fmt.Fprintln(os.Stderr, "Run 'qai edit --help' for details.")
		os.Exit(1)
	}
	conduct.CmdConduct(append([]string{"image-edit"}, args...))
}

// ─── web (Brave → display + optional Joplin cache) ──────────────────────────

func cmdWeb(args []string) {
	if helpFlag(args) {
		fmt.Println(helpWeb)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai web \"query\" [--clip] [-n count] [--freshness day|week|month]")
		os.Exit(1)
	}

	clip := false
	count := 10
	freshness := ""
	var query string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--clip":
			clip = true
		case "-n", "--count":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &count)
				i++
			}
		case "--freshness":
			if i+1 < len(args) {
				freshness = args[i+1]
				i++
			}
		default:
			if query == "" {
				query = args[i]
			}
		}
	}

	search.BraveSearch(query, count, freshness)

	if clip {
		jsonData := search.BraveSearchJSON(query, 3)
		if jsonData == nil {
			return
		}
		var resp struct {
			Web struct {
				Results []struct {
					URL   string `json:"url"`
					Title string `json:"title"`
				} `json:"results"`
			} `json:"web"`
		}
		if json.Unmarshal(jsonData, &resp) != nil {
			return
		}
		notebook := "Search_Cache"
		for _, r := range resp.Web.Results {
			if r.URL == "" {
				continue
			}
			fmt.Fprintf(os.Stderr, "clipping: %s → %s\n", r.URL, notebook)
			exec.Command("node", clipToJoplinJS, r.URL, notebook, r.Title).Run()
		}
		fmt.Fprintf(os.Stderr, "cached %d results to Joplin/%s\n", len(resp.Web.Results), notebook)
	}
}

func cmdAsk(args []string) {
	if helpFlag(args) {
		fmt.Println(helpAsk)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai ask \"question\"")
		os.Exit(1)
	}
	search.BraveAsk(args[0])
}

func cmdContext(args []string) {
	if helpFlag(args) {
		fmt.Println(helpContext)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai context \"query\"")
		os.Exit(1)
	}
	search.BraveContext(args[0])
}

// ─── scan / clip / models ───────────────────────────────────────────────────

func cmdScan(args []string) {
	if helpFlag(args) {
		fmt.Println(helpScan)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai scan <path> [path2] [path3]")
		os.Exit(1)
	}
	if _, err := os.Stat(scanScript); err != nil {
		fmt.Fprintf(os.Stderr, "qai scan: script not found at %s\n", scanScript)
		fmt.Fprintln(os.Stderr, "The scan command requires the scan-codebase.sh helper script.")
		fmt.Fprintln(os.Stderr, "Use 'qai analyze' for built-in code analysis instead.")
		os.Exit(1)
	}
	run("bash", append([]string{scanScript}, args...)...)
}

func cmdClip(args []string) {
	if helpFlag(args) {
		fmt.Println(helpClip)
		return
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai clip <url> [notebook] [title]")
		os.Exit(1)
	}
	if _, err := exec.LookPath("node"); err != nil {
		fmt.Fprintln(os.Stderr, "qai clip: node not found on PATH (required for Joplin clipping)")
		fmt.Fprintln(os.Stderr, "Install Node.js from https://nodejs.org/")
		os.Exit(1)
	}
	if _, err := os.Stat(clipToJoplinJS); err != nil {
		fmt.Fprintf(os.Stderr, "qai clip: script not found at %s\n", clipToJoplinJS)
		os.Exit(1)
	}
	run("node", append([]string{clipToJoplinJS}, args...)...)
}

func cmdModels(args []string) {
	if helpFlag(args) {
		fmt.Println(helpModels)
		return
	}
	filter := ""
	if len(args) > 0 {
		filter = strings.ToLower(strings.Join(args, " "))
	}

	data, err := os.ReadFile(modelsCSV)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai models: model registry not found at %s\n", modelsCSV)
		fmt.Fprintln(os.Stderr, "Place a models.csv file there, or use 'qai conduct models' for API model listing.")
		os.Exit(1)
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 {
		fmt.Println(lines[0]) // header
	}
	for _, line := range lines[1:] {
		if filter == "" || strings.Contains(strings.ToLower(line), filter) {
			if line != "" {
				fmt.Println(line)
			}
		}
	}
}

// ─── exec helpers ───────────────────────────────────────────────────────────

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "qai: %v\n", err)
		os.Exit(1)
	}
}

// ─── top-level usage ────────────────────────────────────────────────────────

func printUsage() {
	fmt.Print(`qai — unified CLI for AI tools

Usage: qai <command> [args]
       qai <command> --help    # detailed help for that command

Media:
  image     Generate image (xai, openai, gemini)
  video     Generate video (grok-imagine, veo, sora)
  tts       Text to speech
  music     Generate music
  edit      Edit an image (single, parallel, or Vertex batch)

Search & Knowledge:
  search    Unified search — Joplin + SurrealDB RAG + Vertex RAG
  web       Brave web search (+ optional Joplin clip)
  ask       Brave AI-grounded answer with citations
  context   Brave LLM-optimized context chunks
  clip      Clip a web page to Joplin
  scrape    Pluggable product scraper (Amazon + presets, batch from CSV)
  ingest    Embed + store docs in SurrealDB

Code:
  analyze   Compiler-accurate code analysis (Go, Rust, TS, Python, Swift, Kotlin)
  scan      Codebase scan + diff (external helper)
  compile   Compile codebase to one file for AI analysis
  graph     Call/module/dependency graph (SVG/DOT)
  security  Scan for vulnerabilities (CWE mapped)
  audit     LLM code audit with profiles

System:
  browser   CDP browser automation (connects to Chrome/Brave)
  db        Local SurrealDB management
  term      tmux terminal management
  conduct   Multi-model API gateway (low-level access)
  project   Joplin-backed projects (list/create/set/show/notes)
  agent     Joplin-backed skills / agent profiles / instructions store
  note      Save a session summary or user TODO to Joplin
  token     GCP token refresh / identity tokens
  models    Search model registry (pricing, context, IDs)
  init      Interactive first-time configuration wizard

Plugins (` + pluginDir + `):
  Drop any executable as qai-<name> to add a subcommand.
`)

	plugins := listPlugins()
	if len(plugins) > 0 {
		fmt.Println("\n  Installed plugins:")
		for _, p := range plugins {
			fmt.Printf("    qai %s\n", p)
		}
	}
}

// ─── per-subcommand help ────────────────────────────────────────────────────

const helpImage = `qai image — generate an image

Usage:
  qai image "prompt" [provider] [flags]

Providers (via 'qai image "prompt" <provider>' — routed through conduct):
  grok-imagine-image        xAI Grok Imagine (default)
  gpt-image-1               OpenAI GPT-Image
  gemini-2.5-flash-image    Google Gemini 2.5 Flash Image

Flags:
  --model <id>       Override model id
  --count <n>        Number of images (default 1)
  --aspect <ratio>   Aspect ratio, e.g. 16:9
  --size <WxH>       Explicit pixel size, e.g. 1024x1024

Output:
  Images saved to ~/Pictures/generated/ (paths printed to stdout).

Examples:
  qai image "a lighthouse at dusk"
  qai image "isometric cityscape" --aspect 16:9 --count 4
  qai image "logo sketch" --model gpt-image-1

Related:
  qai edit    — edit an existing image (single or batch)
  qai conduct image --help   — low-level conduct access`

const helpVideo = `qai video — generate a video

Usage:
  qai video "prompt" [provider] [flags]

Providers:
  grok-imagine-video   xAI Grok Imagine (default)
  veo-genai            Google Veo
  sora                 OpenAI Sora

Flags:
  --model <id>       Override model id
  --duration <sec>   Duration in seconds (default 8)

Output:
  Video job is queued; job id printed. Poll with: qai conduct job <id>

Example:
  qai video "drone shot over icy mountains" --duration 10

Related:
  qai conduct job <id>   — check job state
  qai conduct video --help`

const helpTTS = `qai tts — text to speech

Usage:
  qai tts "text" [voice] [flags]

Built-in voices (OpenAI):
  alloy, echo, fable, onyx, nova, shimmer

Flags:
  --model <id>    Override TTS model id
  --voice <id>    Cloned voice id (see: qai conduct clone-voice)

Output:
  Audio file saved to ~/Pictures/generated/ (path printed to stdout).

Examples:
  qai tts "hello world" nova
  qai tts "narration text" --voice <cloned-id>`

const helpMusic = `qai music — generate music

Usage:
  qai music "prompt" [flags]

Flags:
  --model <id>       Override model id
  --duration <sec>   Duration in seconds

Example:
  qai music "ambient strings, reverberant, slow build"`

const helpEdit = `qai edit — edit an image

Single mode:
  qai edit <input.png> "prompt" [--model M]

Parallel mode (bounded workers against /qai/v1/images/edit):
  qai edit --batch --parallel N [--model M] [--provider P] \
           "prompt" <input-dir> <output-dir>

Vertex batch mode (Gemini image, default when --batch without --parallel):
  qai edit --batch [--model M] [--gcs-bucket B] [--region R] \
           "prompt" <input-dir> <output-dir>

Flags (batch):
  --parallel N       Use N concurrent edit calls instead of Vertex batch.
  --model <id>       Model id (default gemini-2.5-flash-image).
  --gcs-bucket <b>   GCS bucket (defaults to vertex.bucket in config).
  --region <r>       Vertex region (default us-central1).
  --provider <p>     Parallel-mode provider override (e.g. gemini).

Notes:
  - Landscape inputs are auto-rotated to portrait for document scans.
  - Vertex batch requires ADC and GCS write access: run 'qai token --check'.
  - Supported input ext: .png, .jpg, .jpeg, .webp

Examples:
  qai edit photo.png "remove background, keep subject"
  qai edit --batch --parallel 4 "colorize" ./scans ./out
  qai edit --batch "restore faded text" ./scans ./out`

const helpSearch = `qai search — unified knowledge search

Modes (pick one; defaults to --all):
  qai search "query"                    Search Joplin + SurrealDB RAG + Vertex RAG
  qai search --joplin "query" [limit]   Joplin notes only
  qai search --surreal "query" [prov] [limit]  SurrealDB cloud vector search
  qai search --local "query" [prov] [limit]    Local SurrealDB vector (needs matching embed dim)
  qai search --text "query" [prov] [limit]     Local SurrealDB keyword search (no embedding)
  qai search --similar "file" [prov] [limit]   Find similar files using stored vectors
  qai search --read "file" [prov]              Read full source of a file from SurrealDB
  qai search --list [prov]                     List all files in a provider
  qai search --rag "query"                     Vertex AI RAG (language specs, stdlib)

Providers (for --surreal / --local / --text):
  xai, claude, surrealdb, openai, deepseek, gemini (depends on what you've ingested)

Examples:
  qai search "async runtimes"
  qai search --text "Allocator" zig
  qai search --similar "Allocator.zig" zig 5
  qai search --rag "tokio select macro"

Related:
  qai ingest <provider> <path>   — add docs to the RAG corpus
  qai db info                    — see what's in the local DB`

const helpWeb = `qai web — Brave web search

Usage:
  qai web "query" [flags]

Flags:
  --clip                 After search, clip top 3 results to Joplin/Search_Cache
  -n, --count <n>        Number of results (default 10)
  --freshness <window>   day | week | month | year

Env:
  BRAVE_SEARCH_API_KEY   required

Examples:
  qai web "rust 1.94 release notes" --freshness week
  qai web "surrealdb full-text index" --clip

Related:
  qai ask "…"       — AI-grounded answer with citations
  qai context "…"   — LLM-optimized context chunks`

const helpAsk = `qai ask — Brave AI-grounded answer with citations

Usage:
  qai ask "question"

Returns a concise answer citing the sources Brave used.

Env:
  BRAVE_SEARCH_API_KEY   required

Example:
  qai ask "what changed in Go 1.26 iterators?"`

const helpContext = `qai context — Brave LLM-optimized context chunks

Usage:
  qai context "query"

Returns pre-extracted web content (text, tables, code) tuned for LLM input.
Use this when you're building a RAG/agentic prompt rather than skimming a SERP.

Env:
  BRAVE_SEARCH_API_KEY   required`

const helpScan = `qai scan — codebase scan + diff (external helper)

Usage:
  qai scan <path>                       Scan one codebase (types, fields, funcs, call graph)
  qai scan <path1> <path2>              Scan + diff two codebases
  qai scan <path1> <path2> <path3>      Scan + pairwise diff

IMPORTANT: pass DIRECTORIES, not individual files. Scans recursively.

Requires the helper script at:
  ~/.claude/scripts/scan-codebase.sh

Use 'qai analyze' for the built-in, dependency-free Go/Rust/TS/Python/Swift/Kotlin analysis.`

const helpClip = `qai clip — clip a web page to Joplin via Playwright

Usage:
  qai clip <url> [notebook] [title]

Defaults notebook to "Web_Clippings".
Requires Node.js on PATH and the helper at ~/.claude/scripts/clip-to-joplin.mjs.

Examples:
  qai clip https://example.com/post
  qai clip https://docs.rs/foo Notebook "foo crate docs"`

const helpModels = `qai models — search the local model registry CSV

Usage:
  qai models [filter]

Reads ~/.claude/models.csv. Prints the header then any row matching the
case-insensitive filter (matches any column). For the live API model list,
use 'qai conduct models'.

Examples:
  qai models claude
  qai models "1m context"`

const helpToken = `qai token — GCP token refresh & identity tokens

Usage:
  qai token                              Print a fresh access token from ADC
  qai token --json                       Token + metadata as JSON
  qai token --check                      Verify ADC is valid (exit 0 if OK)
  qai token --identity --audience <url>  Identity token for a Cloud Run target
  qai token --identity --audience <url> --json

Falls back to a Keychain-stored service-account key when the ADC RAPT expires.

Examples:
  curl -H "Authorization: Bearer $(qai token)" https://api.example.com/…
  curl -H "Authorization: Bearer $(qai token --identity --audience https://svc-xxx.run.app)" …`

const helpIngest = `qai ingest — embed + store docs in SurrealDB

Usage:
  qai ingest <provider> <path>                  Standard ingest (chunks + embeds via config provider)
  qai ingest <provider> --precomputed <dir>     Store precomputed Vertex embedding archive

Embedding provider/model come from ~/.qai/config.yaml (ollama/gemini/openai/qai).
Use 'qai init' to configure.

Examples:
  qai ingest zig ~/docs/zig-stdlib
  qai ingest rust --precomputed ~/embeddings/rust-archive

Related:
  qai db info             — see chunk counts, sizes per provider
  qai search --local "…"  — vector search against what you ingested`

const helpCompile = `qai compile — compile a codebase into a single AI-readable file

Usage:
  qai compile <path> [flags]

Flags:
  -o <file>       Output path (default: stdout or a sensible default)
  --format md|xml|json   Output format
  --diff [ref]    Only compile git-changed files (uncommitted, or since ref)
  --exclude <glob>       Additional ignore patterns

Examples:
  qai compile ./src -o src.md
  qai compile ./src --diff HEAD~5 -o changes.md`

const helpGraph = `qai graph — call, module, or dependency graph

Usage:
  qai graph <path> [flags]

Flags:
  --type calls|modules|deps   Graph type
  -o <file>                   Output file (SVG or DOT)
  --format svg|dot            Output format (default svg)

Examples:
  qai graph ./cmd --type calls -o calls.svg
  qai graph . --type modules --format dot -o modules.dot`

const helpAnalyze = `qai analyze — compiler-accurate code analysis

Usage:
  qai analyze <path> [flags]

Flags:
  --complexity   Complexity hotspots (cyclomatic / cognitive)
  --calls        Call graph summary
  --json         Machine-readable JSON output

Languages: Go, Rust, TypeScript, Python, Swift, Kotlin.

Examples:
  qai analyze ./internal --complexity
  qai analyze ./src --calls --json > calls.json`

const helpSecurity = `qai security — vulnerability scan

Usage:
  qai security <path> [flags]

Covers 14 languages, 40+ vulnerability classes, CWE-mapped.
Scans for: injection, SSRF, hardcoded secrets, weak crypto, unsafe deserialization,
path traversal, XSS, insecure RNG, and more.

Flags:
  --json         JSON output for CI pipelines

Example:
  qai security ./src
  qai security ./src --json > security.json`

func helpAudit() string {
	return `qai audit — LLM-assisted code audit

Usage:
  qai audit <path> [flags]

Flags:
  --profile <name>   Audit profile (default: code-review)
  -m, --model <id>   Model override (default from config)
  --dry-run          Show files that would be analyzed, don't call the model
  -o <file>          Write audit to file

Available profiles: ` + strings.Join(audit.ProfileNames(), ", ") + `

Examples:
  qai audit ./src --profile security-redteam
  qai audit ./src --dry-run`
}
