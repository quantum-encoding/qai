// Package project implements the `qai project` subcommand.
//
// Projects map 1:1 to Joplin notebooks. The CLI tracks an "active" project
// (name + notebook id) in ~/.qai/config.yaml; future subcommands like
// `qai note` can write straight into it without re-prompting the user.
//
// Command shape (flag-driven to match user's stated preference):
//
//	qai project --list
//	qai project --create "Vibing with Grok"
//	qai project --set    "Vibing with Grok"
//	qai project --current
//	qai project --show   "Vibing with Grok"
//	qai project --notes                    (list notes in active project)
//
// A bare `qai project` runs `--list`. `qai project --help` prints usage.
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Cfg is injected from main. Keeping it package-global matches the pattern
// other qai subpackages use (search.Cfg, conduct.Cfg, etc.).
var Cfg *config.Config

// CmdProject is the entry point.
func CmdProject(args []string) {
	action := parseAction(args)
	switch action.kind {
	case "help":
		usage()
	case "list":
		doList(action.json)
	case "current":
		doCurrent(action.json)
	case "create":
		doCreate(action.value)
	case "set":
		doSet(action.value)
	case "show":
		doShow(action.value, action.json)
	case "notes":
		doNotes(action.json)
	case "copy":
		doCopy(action.value, action.dryRun)
	default:
		usage()
		os.Exit(1)
	}
}

// ── arg parsing ─────────────────────────────────────────────────────────────

type parsedAction struct {
	kind   string // "list" | "create" | "set" | "current" | "show" | "notes" | "copy" | "help"
	value  string // e.g. project name for --create / --set / --show, dir for --copy
	json   bool
	dryRun bool
}

// parseAction accepts both flag-style (`--list`, `--set "X"`) and positional
// (`list`, `set X`) shapes so the command is comfortable under either style.
// If no action is given, defaults to --list.
func parseAction(args []string) parsedAction {
	var out parsedAction
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--help", "-h", "help":
			out.kind = "help"
			return out
		case "--json", "-j":
			out.json = true
		case "--dry-run", "-n":
			out.dryRun = true
		case "--copy":
			out.kind = "copy"
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				out.value = args[i]
			}
		case "--list", "-l", "list", "ls":
			out.kind = "list"
		case "--current", "--show-current", "current":
			out.kind = "current"
		case "--notes", "notes":
			out.kind = "notes"
		case "--create", "create":
			out.kind = "create"
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				out.value = args[i]
			}
		case "--set", "set", "use":
			out.kind = "set"
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				out.value = args[i]
			}
		case "--show", "show":
			out.kind = "show"
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				out.value = args[i]
			}
		default:
			// Bare project name as sole positional: treat as `--set <name>`.
			// Covers `qai project "My Project"`.
			if out.kind == "" && !strings.HasPrefix(a, "-") {
				out.kind = "set"
				out.value = a
			}
		}
	}
	if out.kind == "" {
		out.kind = "list"
	}
	return out
}

// ── actions ─────────────────────────────────────────────────────────────────

func doList(asJSON bool) {
	client, err := newClient()
	if err != nil {
		die(err)
	}
	folders, err := client.ListFolders()
	if err != nil {
		die(err)
	}
	sort.Slice(folders, func(i, j int) bool {
		return strings.ToLower(folders[i].Title) < strings.ToLower(folders[j].Title)
	})
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(folders)
		return
	}
	activeID := Cfg.Project.JoplinNotebookID
	if len(folders) == 0 {
		fmt.Println("No Joplin notebooks found.")
		return
	}
	for _, f := range folders {
		marker := " "
		if f.ID == activeID {
			marker = "*"
		}
		fmt.Printf(" %s  %s\n", marker, strings.TrimSpace(f.Title))
	}
	if activeID == "" {
		fmt.Println()
		fmt.Println("  (no active project — run: qai project --set \"<name>\")")
	}
}

func doCurrent(asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(Cfg.Project)
		return
	}
	if Cfg.Project.Name == "" {
		fmt.Println("No active project. Set one with: qai project --set \"<name>\"")
		return
	}
	fmt.Printf("Active project: %s\n", Cfg.Project.Name)
	if Cfg.Project.JoplinNotebookID != "" {
		fmt.Printf("Joplin notebook: %s\n", Cfg.Project.JoplinNotebookID)
	}
}

func doCreate(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		die(fmt.Errorf("project --create requires a name: qai project --create \"My Project\""))
	}
	client, err := newClient()
	if err != nil {
		die(err)
	}
	// Path-style names ("qai/scans/foo") create the whole chain. Flat names
	// keep the legacy single-folder behaviour so existing "Project Name"
	// usages don't change shape.
	if strings.Contains(name, "/") {
		f, err := client.FindOrCreateFolderPath(name)
		if err != nil {
			die(err)
		}
		fmt.Printf("Created project: %s (%s)\n", name, f.ID)
		setActive(name, f.ID)
		return
	}
	existing, err := client.FindFolderByTitle(name)
	if err != nil {
		die(err)
	}
	if existing != nil {
		fmt.Printf("Project already exists: %s (%s)\n", name, existing.ID)
		setActive(name, existing.ID)
		return
	}
	f, err := client.CreateFolder(name, "")
	if err != nil {
		die(err)
	}
	fmt.Printf("Created project: %s (%s)\n", name, f.ID)
	setActive(name, f.ID)
}

func doSet(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		die(fmt.Errorf("project --set requires a name: qai project --set \"My Project\""))
	}
	client, err := newClient()
	if err != nil {
		die(err)
	}
	// Path-style names walk the chain; flat names keep the legacy
	// case-sensitive title match. We deliberately do NOT fall back from
	// path → flat: if the user typed slashes, surfacing "no project at
	// that path" is more useful than silently matching a flat folder
	// whose literal title happens to contain the same slashes.
	if strings.Contains(name, "/") {
		f, err := client.FindOrCreateFolderPath(name)
		if err != nil {
			die(err)
		}
		setActive(name, f.ID)
		fmt.Printf("Active project: %s\n", name)
		return
	}
	f, err := client.FindFolderByTitle(name)
	if err != nil {
		die(err)
	}
	if f == nil {
		die(fmt.Errorf("no project named %q — create it first with: qai project --create %q", name, name))
	}
	setActive(name, f.ID)
	fmt.Printf("Active project: %s\n", name)
}

func doShow(name string, asJSON bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = Cfg.Project.Name
	}
	if name == "" {
		die(fmt.Errorf("no project specified and no active project set"))
	}
	client, err := newClient()
	if err != nil {
		die(err)
	}
	f, err := client.FindFolderByTitle(name)
	if err != nil {
		die(err)
	}
	if f == nil {
		die(fmt.Errorf("no project named %q", name))
	}
	notes, err := client.ListNotes(f.ID)
	if err != nil {
		die(err)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"notebook": f, "notes": notes})
		return
	}
	fmt.Printf("Project: %s\n", f.Title)
	fmt.Printf("Notebook ID: %s\n", f.ID)
	fmt.Printf("Notes: %d\n\n", len(notes))
	// Sort newest first.
	sort.Slice(notes, func(i, j int) bool { return notes[i].UserUpdatedTime > notes[j].UserUpdatedTime })
	for i, n := range notes {
		if i >= 20 {
			fmt.Printf("  … and %d more\n", len(notes)-20)
			break
		}
		fmt.Printf("  %s  %s\n", formatTime(n.UserUpdatedTime), strings.TrimSpace(n.Title))
	}
}

func doNotes(asJSON bool) {
	if Cfg.Project.JoplinNotebookID == "" {
		die(fmt.Errorf("no active project set. Run: qai project --set \"<name>\""))
	}
	client, err := newClient()
	if err != nil {
		die(err)
	}
	notes, err := client.ListNotes(Cfg.Project.JoplinNotebookID)
	if err != nil {
		die(err)
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].UserUpdatedTime > notes[j].UserUpdatedTime })
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(notes)
		return
	}
	if len(notes) == 0 {
		fmt.Println("(no notes in this project yet)")
		return
	}
	for _, n := range notes {
		fmt.Printf("  %s  %s\n", formatTime(n.UserUpdatedTime), strings.TrimSpace(n.Title))
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// newClient returns a Joplin client and errors out with an actionable message
// when the user hasn't configured a token.
func newClient() (*joplin.Client, error) {
	if Cfg.Joplin.Token == "" {
		return nil, fmt.Errorf(
			"Joplin token not configured. Set one with either:\n" +
				"  export JOPLIN_TOKEN=...            (Tools → Options → Web Clipper in Joplin)\n" +
				"  or add joplin.token to %s", config.ConfigPath())
	}
	c := joplin.New(joplin.Config{
		BaseURL: Cfg.Joplin.BaseURL,
		Token:   Cfg.Joplin.Token,
	})
	// Cheap probe so the user gets a clear error message if Joplin isn't
	// running instead of a generic connection refused on the next request.
	if err := c.Ping(); err != nil {
		return nil, err
	}
	return c, nil
}

func setActive(name, notebookID string) {
	Cfg.Project.Name = strings.TrimSpace(name)
	Cfg.Project.JoplinNotebookID = notebookID
	if err := config.Save(Cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save active project: %v\n", err)
	}
}

func formatTime(ms int64) string {
	if ms <= 0 {
		return "                   "
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04:05")
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "qai project: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Println(`qai project — manage Joplin-backed projects (notebooks).

USAGE
  qai project                         # list projects (same as --list)
  qai project --list            [-j]  # list all Joplin notebooks
  qai project --create <name>         # create a new project and set it active
  qai project --set    <name>         # set the active project
  qai project --current         [-j]  # show the currently active project
  qai project --show   <name>   [-j]  # notebook + recent notes
  qai project --notes           [-j]  # list notes in the active project
  qai project --copy   <dir>    [-n]  # copy a folder's files into the active
                                      # project as one note per file. Skips
                                      # hidden dirs (.git, .venv, …) plus
                                      # node_modules, dist, build, target, etc.
                                      # Re-runs update existing notes by title.

FLAGS
  -j, --json       Emit machine-readable JSON.
  -n, --dry-run    (for --copy) preview without writing to Joplin.
  -h, --help       Print this help.

ENV
  JOPLIN_TOKEN         Joplin Web Clipper token (required).
  JOPLIN_BASE_URL      Override the Joplin endpoint (default http://127.0.0.1:41184).
  QAI_PROJECT          Override the active project name for this session.

The active project is persisted to ~/.qai/config.yaml so other qai
subcommands (and future Claude Code hooks) can target it automatically.`)
}
