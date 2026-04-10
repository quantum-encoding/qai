// qai — unified CLI for AI tools, search, code scanning, and media generation.
//
// Thin dispatcher that routes subcommands to existing tools. No reimplementation.
//
// Usage:
//
//	qai search "query"                     # search all knowledge bases
//	qai search --rag "query"               # Vertex AI RAG only
//	qai search --surreal "query" provider  # SurrealDB RAG only
//	qai search --joplin "query"            # Joplin only
//	qai image "prompt" [provider]          # generate image
//	qai video "prompt" [provider]          # generate video
//	qai tts "text" [voice]                 # text to speech
//	qai music "prompt"                     # generate music
//	qai scan <path> [path2...]             # code scanner + diff
//	qai clip <url> [notebook] [title]      # clip web page to Joplin
//	qai token [--identity --audience url]  # GCP token refresh
//	qai models [filter]                    # search model registry
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// homeDir returns the user's home directory, preferring os.UserHomeDir()
// over the HOME env var. Falls back to "." if neither works.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "."
}

var home = homeDir()

// Plugin directory — drop any executable here and it becomes a subcommand.
// e.g. ~/.qai/commands/qai-deploy → qai deploy
var pluginDir = filepath.Join(home, ".qai", "commands")

// Tool paths — only for features that still need external binaries.
var (
	clipToJoplinJS = filepath.Join(home, ".claude", "scripts", "clip-to-joplin.mjs") // fallback for web clip
	scanScript     = filepath.Join(home, ".claude", "scripts", "scan-codebase.sh")   // codebase scanner
	modelsCSV      = filepath.Join(home, ".claude", "models.csv")                    // model registry
)

func main() {
	cfg = loadConfig()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		cmdInit(args)
	case "search", "s":
		cmdSearch(args)
	case "image", "img":
		cmdMedia("image", args)
	case "video", "vid":
		cmdMedia("video", args)
	case "tts", "speak":
		cmdMedia("tts", args)
	case "music":
		cmdMedia("music", args)
	case "edit":
		cmdMediaEdit(args)
	case "web", "w":
		cmdWeb(args)
	case "ask":
		cmdAsk(args)
	case "context", "ctx":
		cmdContext(args)
	case "ingest":
		cmdIngest(args)
	case "scan":
		cmdScan(args)
	case "compile":
		cmdCompile(args)
	case "graph":
		cmdGraph(args)
	case "analyze":
		cmdAnalyze(args)
	case "security":
		cmdSecurity(args)
	case "audit":
		cmdAudit(args)
	case "browser", "b":
		cmdBrowser(args)
	case "db":
		cmdDB(args)
	case "clip":
		cmdClip(args)
	case "token":
		cmdToken(args)
	case "models":
		cmdModels(args)
	case "conduct", "c":
		cmdConduct(args)
	case "term", "terminal", "t":
		cmdTerminal(args)
	case "help", "--help", "-h":
		printUsage()
	default:
		// Check for plugin command.
		if !tryPlugin(cmd, args) {
			fmt.Fprintf(os.Stderr, "qai: unknown command %q\n", cmd)
			printUsage()
			os.Exit(1)
		}
	}
}

// ─── plugins ────────────────────────────────────────────────────────────────

// tryPlugin looks for an executable at ~/.qai/commands/qai-<cmd> and runs it.
func tryPlugin(cmd string, args []string) bool {
	bin := filepath.Join(pluginDir, "qai-"+cmd)
	if info, err := os.Stat(bin); err == nil && info.Mode()&0111 != 0 {
		run(bin, args...)
		return true
	}
	return false
}

// listPlugins returns the names of available plugin commands.
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
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai search [--rag|--surreal|--local|--joplin] <query> [provider] [limit]")
		os.Exit(1)
	}

	// Parse flags.
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
		case "--joplin":
			mode = "joplin"
		case "--json":
			// TODO: unified JSON output
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
		// Vertex AI RAG search (native Go).
		ragArgs := []string{query}
		ragArgs = append(ragArgs, extra...)
		ragSearch(ragArgs)

	case "surreal":
		// SurrealDB cloud vector search (native Go).
		provider := ""
		limit := 5
		if len(extra) > 0 {
			provider = extra[0]
		}
		if len(extra) > 1 {
			fmt.Sscanf(extra[1], "%d", &limit)
		}
		searchCloudSurreal(query, provider, limit)

	case "local":
		// Local SurrealDB vector search (native Go).
		provider := ""
		limit := 5
		if len(extra) > 0 {
			provider = extra[0]
		}
		if len(extra) > 1 {
			fmt.Sscanf(extra[1], "%d", &limit)
		}
		searchLocalSurreal(query, provider, limit)

	case "joplin":
		// Joplin REST API search (native Go).
		limit := 10
		if len(extra) > 0 {
			fmt.Sscanf(extra[0], "%d", &limit)
		}
		joplinSearch(query, limit)

	case "all":
		// Search all available sources in sequence.
		fmt.Println("━━━ Joplin ━━━")
		joplinSearch(query, 5)
		if cfg.Surreal.CloudURL != "" {
			fmt.Println("\n━━━ SurrealDB RAG ━━━")
			searchCloudSurreal(query, "", 5)
		} else {
			fmt.Println("\n━━━ Local RAG ━━━")
			searchLocalSurreal(query, "", 5)
		}
		if ragAPIBase() != "" {
			fmt.Println("\n━━━ Vertex AI RAG ━━━")
			ragSearch([]string{query})
		}
	}
}

// ─── media ──────────────────────────────────────────────────────────────────

// cmdMedia routes to qai conduct for media generation (no shell scripts).
func cmdMedia(mediaType string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: qai %s \"prompt\" [provider]\n", mediaType)
		os.Exit(1)
	}
	// Route through conduct subcommands which handle the API calls natively.
	conductArgs := append([]string{mediaType}, args...)
	cmdConduct(conductArgs)
}

func cmdMediaEdit(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai edit <input.png> \"prompt\" [provider]")
		os.Exit(1)
	}
	conductArgs := append([]string{"edit"}, args...)
	cmdConduct(conductArgs)
}

// ─── web search (Brave → display + cache to Joplin) ─────────────────────────

func cmdWeb(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai web \"query\" [--clip] [-n count] [--freshness day|week|month]")
		os.Exit(1)
	}

	// Parse flags.
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

	braveSearch(query, count, freshness)

	// If --clip, get JSON and clip top URLs via browser.
	if clip {
		jsonData := braveSearchJSON(query, 3)
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
			// Use browser clip if available, fall back to node script
			exec.Command("node", clipToJoplinJS, r.URL, notebook, r.Title).Run()
		}
		fmt.Fprintf(os.Stderr, "cached %d results to Joplin/%s\n", len(resp.Web.Results), notebook)
	}
}

func cmdAsk(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai ask \"question\"")
		os.Exit(1)
	}
	braveAsk(args[0])
}

func cmdContext(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai context \"query\"")
		os.Exit(1)
	}
	braveContext(args[0])
}

// ─── scan ───────────────────────────────────────────────────────────────────

func cmdScan(args []string) {
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

// ─── clip ───────────────────────────────────────────────────────────────────

func cmdClip(args []string) {
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

// ─── token (now native in token.go) ─────────────────────────────────────────

// ─── models ─────────────────────────────────────────────────────────────────

func cmdModels(args []string) {
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

func runQuiet(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run() // don't exit on error — continue to next search
}

// ─── usage ──────────────────────────────────────────────────────────────────

func printUsage() {
	fmt.Print(`qai — unified CLI for AI tools

Search:
  qai search "query"                    Search all knowledge bases (Joplin + SurrealDB + Vertex RAG)
  qai search --rag "query"              Vertex AI RAG only (language specs, stdlib)
  qai search --surreal "query" [provider]  SurrealDB RAG only (API docs)
  qai search --joplin "query"           Joplin notes only

Media:
  qai image "prompt" [provider]         Generate image (xai, openai_gpt_image1, gemini_flash)
  qai video "prompt" [provider]         Generate video (grok_video, veo_genai, sora)
  qai tts "text" [voice]                Text to speech (alloy, echo, fable, onyx, nova, shimmer)
  qai music "prompt"                    Generate music
  qai edit input.png "prompt" [provider]  Edit image

Web (Brave Search):
  qai web "query"                       Web search via Brave
  qai web "query" --clip                Web search + clip top results to Joplin cache
  qai ask "question"                    AI-grounded answer with citations
  qai context "query"                   LLM-optimized context chunks

RAG Ingest:
  qai ingest <provider> <path>           Embed + store docs in SurrealDB (Gemini Embedding 2)

Code:
  qai analyze <path>                    Compiler-accurate code analysis (Go, Rust, TS, Python, Swift, Kotlin)
  qai analyze <path> --complexity       Complexity hotspots
  qai analyze <path> --calls            Call graph
  qai scan <path> [path2] [path3]       Scan codebases + diff (types, fields, functions, call graph)
  qai compile <path> [-o out.md]        Compile codebase into single file (md/xml/json) for AI analysis
  qai compile <path> --diff [ref]       Only compile git-changed files (uncommitted, or since ref)
  qai graph <path> [--type calls]       Generate call/module/dependency graph (SVG/DOT)
  qai security <path>                   Scan for vulnerabilities (14 langs, 40+ types, CWE mapped)
  qai audit <path> [--profile p] [-m model]  LLM code audit (profiles: ` + strings.Join(profileNames(), ", ") + `)
  qai audit <path> --dry-run            Show files that would be analyzed

Knowledge:
  qai clip <url> [notebook] [title]     Clip web page to Joplin via Playwright
  qai models [filter]                   Search model registry (pricing, context, IDs)

Conductor (API — replaces ai-conductor-mcp):
  qai conduct chat <model> "message"    Multi-model LLM gateway
  qai conduct image "prompt"            Generate image
  qai conduct video "prompt"            Queue video generation
  qai conduct tts "text"                Text to speech
  qai conduct search "query"            RAG knowledge search
  qai conduct web "query"               Brave web search
  qai conduct models                    List models + pricing
  qai conduct balance                   Check credit balance
  qai conduct --help                    Full list of conductor actions

Terminal (tmux — replaces terminal-mcp):
  qai term list                         List active terminals
  qai term spawn "name" [--cwd /path]   Create terminal pane
  qai term send "name" "input"          Send input to terminal
  qai term read "name" [--lines 50]     Read terminal output
  qai term close "name"                 Close terminal
  qai term snapshot                     Overview all terminals
  qai term --help                       Full list of terminal actions

Browser (CDP — connects to your Chrome/Brave debug port):
  qai browser launch                    Start browser with debug port enabled
  qai browser list                      List open tabs
  qai browser open <url>                Navigate to URL
  qai browser extract [--html]          Get page text or HTML
  qai browser screenshot [-o file.png]  Capture screenshot
  qai browser click <selector|x y>     Click element or coordinates
  qai browser type "text"               Type text
  qai browser eval "js"                 Evaluate JavaScript
  qai browser clip [notebook] [title]   Extract + save to Joplin
  qai browser scrape <urls.csv>        Batch scrape text/HTML/screenshots from CSV
  qai browser --help                    Full list of browser actions

Database (local SurrealDB knowledge base):
  qai db start                          Start SurrealDB in background
  qai db stop                           Stop the running instance
  qai db status                         Check status + provider stats
  qai db info                           List providers, chunks, sizes
  qai db shell                          Open interactive SurrealQL shell

Auth:
  qai token                             Get GCP access token
  qai token --identity --audience <url> Get GCP identity token
  qai token --check                     Check if ADC is valid

Setup:
  qai init                              Interactive first-time configuration wizard

Plugins (` + pluginDir + `):
  Drop any executable as qai-<name> to add a subcommand.
`)

	plugins := listPlugins()
	if len(plugins) > 0 {
		fmt.Println("  Installed plugins:")
		for _, p := range plugins {
			fmt.Printf("    qai %s\n", p)
		}
	}
}

// Unused but available for --json mode later.
func jsonOutput(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
