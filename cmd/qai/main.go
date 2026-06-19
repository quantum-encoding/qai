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
	"sort"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/agent"
	"github.com/quantum-encoding/qai-cli/internal/analyze"
	"github.com/quantum-encoding/qai-cli/internal/audit"
	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/browser"
	"github.com/quantum-encoding/qai-cli/internal/chat"
	"github.com/quantum-encoding/qai-cli/internal/cj"
	"github.com/quantum-encoding/qai-cli/internal/compile"
	"github.com/quantum-encoding/qai-cli/internal/conduct"
	"github.com/quantum-encoding/qai-cli/internal/media"
	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/db"
	"github.com/quantum-encoding/qai-cli/internal/docs"
	"github.com/quantum-encoding/qai-cli/internal/doctor"
	"github.com/quantum-encoding/qai-cli/internal/fleet"
	"github.com/quantum-encoding/qai-cli/internal/graph"
	"github.com/quantum-encoding/qai-cli/internal/i18n"
	"github.com/quantum-encoding/qai-cli/internal/ingest"
	"github.com/quantum-encoding/qai-cli/internal/initcmd"
	"github.com/quantum-encoding/qai-cli/internal/joplinops"
	"github.com/quantum-encoding/qai-cli/internal/note"
	"github.com/quantum-encoding/qai-cli/internal/patterns"
	"github.com/quantum-encoding/qai-cli/internal/project"
	"github.com/quantum-encoding/qai-cli/internal/recall"
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
	chat.Cfg = cfg
	media.Cfg = cfg
	search.Cfg = cfg
	ingest.Cfg = cfg
	audit.Cfg = cfg
	db.Cfg = cfg
	project.Cfg = cfg
	agent.Cfg = cfg
	note.Cfg = cfg
	doctor.Cfg = cfg
	recall.Cfg = cfg

	if len(os.Args) < 2 {
		// Bare `qai` should print help to stdout and exit 0 — printing
		// help is a successful invocation, not a failure. Previously
		// exited 1, which broke pipe-friendly uses like `qai | head`.
		printUsage()
		return
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
	case "blast", "br":
		blast.Cmd(args) // handles its own --help
	case "patterns", "pat":
		patterns.Cmd(args) // handles its own --help
	case "i18n":
		i18n.Cmd(args) // handles its own --help
	case "clip":
		cmdClip(args)
	case "cj":
		cj.CmdCJ(args) // handles its own --help
	case "joplin":
		joplinops.CmdJoplin(args) // handles its own --help
	case "scrape":
		scrape.CmdScrape(args) // handles its own --help
	case "docs":
		docs.CmdDocs(args) // handles its own --help
	case "token":
		if helpFlag(args) {
			fmt.Println(helpToken)
			return
		}
		token.CmdToken(args)
	case "models":
		cmdModels(args)
	case "cheat":
		os.Stdout.WriteString(cheatSheet)
	case "plugins":
		cmdPlugins(args)
	case "chat":
		chat.CmdChat(args) // handles its own --help
	case "media":
		media.CmdMedia(args) // handles its own --help
	case "conduct", "c":
		conduct.CmdConduct(args) // handles its own --help
	case "project", "proj":
		project.CmdProject(args) // handles its own --help
	case "agent":
		agent.CmdAgent(args) // handles its own --help
	case "note", "n":
		note.CmdNote(args) // handles its own --help
	case "doctor":
		doctor.CmdDoctor(args) // handles its own --help
	case "recall", "r":
		recall.CmdRecall(args) // handles its own --help
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

// ─── cheat / plugins ────────────────────────────────────────────────────────

// cheatSheet is `qai cheat` — a one-screen summary of common usage
// across every subcommand. The intent: a fresh agent (or human) can
// read this once, in one turn, instead of running `qai foo` to get
// a usage hint then `qai foo --help` for detail then maybe
// `qai foo bar --help` for the subcommand. Probe-then-probe-help
// is the friction this closes.
const cheatSheet = `qai cheat — common usage at a glance (each command has --help for detail)

Media
  qai image "prompt"                        # Nano Banana Pro by default
  qai image "prompt" gpt                    # OpenAI alias (or grok, gemini-flash, ...)
  qai image --batch prompts.txt --parallel 4
  qai edit input.png "prompt"
  qai video "prompt"
  qai tts "text" [voice]
  qai music "prompt"

Search & knowledge
  qai search "query"                        # Joplin → RAG → web, in order
  qai web "query"                           # Brave web search
  qai ask "question"                        # AI-grounded answer + citations
  qai context "query"                       # Brave LLM-context chunks
  qai clip <url> [notebook] [title]         # save web page to Joplin

Notes (Joplin-backed memory)
  qai note "what you did"                   # → qai/sessions
  qai note --todo "human-only task"         # → qai/todos
  qai recall                                # read recent notes for THIS project
  qai recall --project '*'                  # cross-project firehose (opt-in)
  qai recall --session <id> --full          # one note, full body

Code
  qai analyze <path>                        # compiler-accurate (Go, Rust, TS, Py, Swift, Kotlin)
  qai scan <path>                           # multi-engine code scanner
  qai compile <target>
  qai graph <target>
  qai security <target>                     # CWE-mapped vuln scan (serial; don't fleet it)
  qai audit <path>                          # LLM code audit with profiles

Fleet & agents
  qai fleet up <manifest.yaml>              # N parallel claude panes
  qai fleet inbox --unread --json
  qai fleet down <manifest.yaml>
  qai term spawn <name>                     # ad-hoc subagent in a tmux pane
  qai term send <name|%paneID> "text"
  qai sessions list                         # discover Claude Code sessions

Browser (CDP — connect to running Chrome/Brave on :9222)
  qai browser audit <url>                   # one-shot: nav + network + console + perf + shot
  qai browser network --duration 10s -o reqs.json
  qai browser eval "js"                     # DevTools Console
  qai browser screenshot -o page.png
  qai browser pdf -o page.pdf

System
  qai doctor                                # health-check every dependency
  qai init                                  # first-time setup wizard
  qai models [filter]                       # model registry lookup
  qai token --check                         # GCP ADC status
  qai cheat                                 # this view
  qai plugins                               # list ~/.qai/commands/qai-* plugins

Workflow tips
  - Auth: ONE QAI_API_KEY unlocks media/search/audit (https://quantumencoding.ai)
  - Joplin: launch desktop + enable Web Clipper Service
  - Brave/GCP/Surreal: optional per-feature credentials, see qai doctor
`

// cmdPlugins handles 'qai plugins [list]' — lists qai-* executables in
// the sanctioned plugin directory (pluginDir = ~/.qai/commands/). This
// matches what dispatch (tryPlugin) actually resolves: anything outside
// pluginDir is not invocable as 'qai <verb>', so listing it would be
// misleading.
func cmdPlugins(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Println(helpPlugins)
			return
		}
	}
	entries, err := os.ReadDir(pluginDir)
	if err != nil || len(entries) == 0 {
		fmt.Printf("(no qai-* plugins in %s)\n", pluginDir)
		fmt.Printf("Drop an executable named qai-<verb> in %s to add a subcommand.\n", pluginDir)
		return
	}
	seen := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "qai-") || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			continue // not executable
		}
		short := strings.TrimPrefix(name, "qai-")
		seen[short] = filepath.Join(pluginDir, name)
	}
	if len(seen) == 0 {
		fmt.Printf("(no qai-* plugins in %s)\n", pluginDir)
		fmt.Printf("Drop an executable named qai-<verb> in %s to add a subcommand.\n", pluginDir)
		return
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  %-20s  %s\n", n, seen[n])
	}
}

const helpPlugins = `qai plugins — list third-party qai-* plugins

Plugins are an extension mechanism: any executable named qai-<verb>
in ~/.qai/commands/ is invocable as 'qai <verb>'. This command
surfaces what's installed so agents (and humans) don't have to grep
the filesystem.

USAGE
  qai plugins             List installed plugins
  qai plugins --help      This help

HOW TO ADD A PLUGIN
  Drop an executable named qai-<verb> in ~/.qai/commands/. e.g.:
    install -m 755 /path/to/qai-deploy ~/.qai/commands/
    qai deploy           # now works`

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
  note      Save a session summary (qai/sessions) or user TODO (qai/todos) to Joplin
  recall    Read recent session notes + open TODOs (read-side of note)

Code:
  analyze   Compiler-accurate code analysis (Go, Rust, TS, Python, Swift, Kotlin)
  scan      Codebase scan + diff (external helper)
  compile   Compile codebase to one file for AI analysis
  graph     Call/module/dependency graph (SVG/DOT)
  security  Scan for vulnerabilities (CWE mapped)
  audit     LLM code audit with profiles
  chat      Single-shot chat (stdin-friendly; --template <profile>)
  media     Multimodal chat with cached uploads (video/audio Q&A)

System:
  browser   CDP browser automation (connects to Chrome/Brave)
  db        Local SurrealDB management
  term      tmux terminal management
  conduct   Multi-model API gateway (low-level access)
  project   Joplin-backed projects (list/create/set/show/notes)
  agent     Joplin-backed skills / agent profiles / instructions store
  doctor    Health-check every dependency qai talks to
  cheat     One-screen quickref of common usage across commands
  plugins   List qai-* plugins on PATH
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
  qai image "prompt" [model] [flags]

Models (positional or --model, aliases accepted):
  nano-banana-pro / gemini-pro / gemini-3-pro-image-preview
      Google Gemini 3 Pro Image — "Nano Banana Pro" (DEFAULT, strongest
      realistic output, best text rendering)
  nano-banana-2 / gemini-flash-2 / gemini-3.1-flash-image-preview
      Google Gemini 3.1 Flash Image — "Nano Banana 2" (newer flash,
      between flash and pro on quality)
  nano-banana / gemini-flash / gemini-2.5-flash-image
      Google Gemini 2.5 Flash Image — "Nano Banana" (original; fast,
      cheap)
  grok / grok-imagine / grok-imagine-image
      xAI Grok Imagine — standard (stylised, fast, ~$0.02/img)
  grok-pro / grok-quality / grok-imagine-image-quality
      xAI Grok Imagine Quality — higher resolution / quality
      (~$0.05/img 1K, ~$0.07/img 2K)
  gpt / openai / gpt-image-2
      OpenAI GPT-Image 2 (DEFAULT for OpenAI alias — newest, best)
  gpt-1.5 / gpt-image-1.5
      OpenAI GPT-Image 1.5
  gpt-1 / gpt-image-1
      OpenAI GPT-Image 1
  gpt-mini / gpt-image-1-mini
      OpenAI GPT-Image 1 Mini (cheapest)
  chatgpt / chatgpt-image
      ChatGPT image generator (latest)

Flags (provider-neutral — translated per model):
  --model <id>          Override model id (aliases above also accepted here)
  --count <n>           Number of images (default 1)
  --aspect <ratio>      Aspect ratio, e.g. 16:9, 9:16, 1:1, 2:3, 3:2
                        Gemini: native field. OpenAI: maps to nearest size enum.
                        Grok: dropped (model has no aspect field).
  --size <value>        Image size — meaning depends on the model:
                          Gemini Pro:  1K | 2K           (resolution tier)
                          Gemini Flash: (no resolution; flag dropped)
                          OpenAI:      1024x1024 | 1024x1536 | 1536x1024 | auto
                          Pixel input on Gemini Pro snaps to nearest tier;
                          off-spec WxH on OpenAI snaps to nearest enum value.
  --quality <q>         OpenAI only: low | medium | high | auto
  --background <bg>     OpenAI only: transparent | opaque | auto
  --format <fmt>        OpenAI only: png | jpeg | webp
  --batch <file>        Generate from a file of prompts (one per line; '-' for stdin).
                        All flags apply to every prompt.
  --parallel <n>        With --batch, number of concurrent generations (default 4).

Unsupported flags for the target model are warn-and-dropped (stderr) so
batches that swap models keep running. Use --model explicitly to see
exactly which flag is being dropped where.

Output:
  Images saved to ~/Pictures/generated/ (paths printed to stdout).

Examples:
  qai image "a lighthouse at dusk"                       # Nano Banana Pro, 2K default
  qai image "moody street" --aspect 16:9 --size 1K       # Pro at 1K tier
  qai image "isometric cityscape" --aspect 16:9 --count 4
  qai image "logo sketch" gpt --size 1024x1024 --background transparent
  qai image "portrait" gpt --aspect 9:16                 # → size 1024x1536
  qai image --batch prompts.txt --aspect 16:9            # all 16:9 across providers
  qai image --batch - --parallel 2 < prompts.txt
  printf "a fox\\na koi pond\\na zen rock garden\\n" | qai image --batch -

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

Flags:
  --model <id>    TTS model or provider alias (default: tts-1)
                  Aliases: xai|grok → grok-tts (best quality)
                           gemini    → gemini-2.5-flash-preview-tts
                           openai    → tts-1
                           eleven    → eleven_multilingual_v2
  --voice <id>    Voice (see per-provider list below).
  --language <c>  Language code. xAI: BCP-47 or "auto" (default). Set it
                  for natural non-English speech — an English default
                  pronounces other languages with English phonetics
                  (the "robotic" failure). Gemini: ISO 639-1.
  --speed <n>     Speech speed multiplier, xAI range 0.7–1.5 (1.0 normal).
  --format <fmt>  mp3 (default), wav, opus, aac, flac, pcm.
  --sample-rate <hz>  xAI sample rate: 8000/16000/22050/24000/44100/48000.
                      grok defaults to 44100 (CD) when unset.
  --bit-rate <bps>    xAI bit rate, e.g. 128000 or 192000.

xAI voices (--model xai), case-insensitive:
  eve  energetic/upbeat (default)   ara  warm/friendly
  rex  confident/business           sal  smooth/balanced
  leo  authoritative/instructional
  + custom clones: qai conduct clone-voice "name" sample.mp3
OpenAI voices: alloy, echo, fable, onyx, nova, shimmer.
ElevenLabs: friendly name or voice id (qai conduct voices).

xAI languages (--language): auto (default), en, ar-EG, ar-SA, ar-AE,
  bn, zh, fr, de, hi, id, it, ja, ko, pt-BR, pt-PT, ru, es-MX, es-ES,
  tr, vi. (Spanish is es-ES / es-MX — NOT "es".)

xAI speech tags (inside the text, forwarded verbatim):
  inline  [pause] [laugh] …            — a one-off expression
  wrap    <whisper>…</whisper> …       — change delivery of a span
                (also <soft>, <build-intensity>, pitch/speed/volume tags)

Output:
  Audio file saved to ~/Music/generated/ (path printed to stdout).

Speech → text (the reverse):
  qai conduct transcribe <audio> [flags]
    --model xai|gemini|whisper   provider (default whisper-1; .opus/.ogg
                                 auto-route to xai grok-stt, which accepts
                                 them — whisper-1 does not).
    --language <c>               ISO 639-1; enables number/currency
                                 formatting (xAI ITN).
    --keyterm <term>             bias toward a proper noun/product name;
                                 repeat the flag for several (xAI).
    --diarize                    label each word with a speaker index (xAI).
  Formats: wav, mp3, ogg, opus, flac, aac, mp4, m4a, webm. Transcript → stdout.

Examples:
  qai tts "hello world" nova
  qai tts "Hola, ¿qué tal?" --model xai --language es-ES --voice ara
  qai tts "So I walked in [pause] and <whisper>there it was.</whisper>" --model xai
  qai conduct transcribe ~/Downloads/voice-note.opus           # → grok-stt
  qai conduct transcribe meeting.mp3 --model xai --diarize --keyterm "Dinahosting"`

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
