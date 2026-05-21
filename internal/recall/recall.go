// Package recall implements `qai recall` — the read counterpart to
// `qai note`. Pulls recent session summaries + open TODOs for the
// current project out of Joplin and renders them as a briefing the
// agent (or human) can read to remember where they left off.
//
// Bounded by design — the unbounded-log problem we hit in qai_chat
// shouldn't recur here. Three bounds apply at the API level:
//
//   --sessions N   max session notes to include (default 5)
//   --days N       time window in days (default 14)
//   --budget N     output token budget; bodies truncated to fit
//                  (default 1500, rough chars/4 estimate)
//
// Cross-project access is GATED, not default. The default scope is
// "this cwd's project" — recall reads from a per-project notebook if
// one exists, plus the global qai/sessions and qai/todos filtered by
// cwd basename. To access the full firehose explicitly:
//
//   qai recall --project '*'   (cross-project union)
//
// SessionStart hook (scripts/qai-recall-start.sh) calls
// `qai recall --json` so the briefing lands in the agent's context
// at session boot without manual invocation.

package recall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Cfg is injected from main, matching the package pattern.
var Cfg *config.Config

const (
	sessionsRoot     = "qai/sessions"
	todosRoot        = "qai/todos"
	defaultSessions  = 5
	defaultDays      = 14
	defaultBudgetCh  = 6000 // ≈ 1500 tokens at the 4-char-per-token heuristic
)

type options struct {
	project    string // "*" = cross-project, "" = derive from cwd
	sessions   int
	days       int
	budgetCh   int
	full       bool
	noTodos    bool
	sessionID  string
	asJSON     bool
	help       bool
}

// CmdRecall is the `qai recall` entry point.
func CmdRecall(args []string) {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai recall: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: run 'qai recall --help' for flag syntax")
		os.Exit(1)
	}
	if opts.help {
		fmt.Print(help)
		return
	}

	if Cfg == nil || Cfg.Joplin.Token == "" {
		fmt.Fprintln(os.Stderr, "qai recall: JOPLIN_TOKEN not configured")
		fmt.Fprintln(os.Stderr, "  → fix: Tools → Options → Web Clipper in Joplin desktop, then export JOPLIN_TOKEN=<token>")
		os.Exit(1)
	}
	client := joplin.New(joplin.Config{BaseURL: Cfg.Joplin.BaseURL, Token: Cfg.Joplin.Token})
	if err := client.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "qai recall: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: launch Joplin desktop and enable Web Clipper")
		os.Exit(1)
	}

	// Single-note mode: fetch one specific note by id and print its body.
	if opts.sessionID != "" {
		printSingleNote(client, opts.sessionID, opts.asJSON)
		return
	}

	project, parentBase := resolveProject(opts.project)
	since := time.Now().Add(-time.Duration(opts.days) * 24 * time.Hour)
	sessions := collectSessions(client, project, parentBase, opts.sessions, since)
	var todos []joplin.Note
	if !opts.noTodos {
		todos = collectTodos(client, project)
	}

	// ListNotes intentionally lean — doesn't fetch body. Fill bodies
	// for the final bounded set via per-note GetNote calls. With the
	// default --sessions 5 + a typical handful of TODOs that's ~10
	// HTTP calls against a local Joplin clipper; not worth optimising.
	hydrateBodies(client, sessions)
	hydrateBodies(client, todos)

	if opts.asJSON {
		renderJSON(sessions, todos, project, opts)
	} else {
		renderMarkdown(sessions, todos, project, opts)
	}
}

// ── arg parsing ─────────────────────────────────────────────────────────────

func parseArgs(args []string) (options, error) {
	o := options{
		sessions: defaultSessions,
		days:     defaultDays,
		budgetCh: defaultBudgetCh,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--help", "-h":
			o.help = true
		case "--project":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--project requires a name or '*'")
			}
			i++
			o.project = args[i]
		case "--sessions":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--sessions requires N")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &o.sessions); err != nil {
				return o, fmt.Errorf("bad --sessions %q", args[i])
			}
		case "--days":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--days requires N")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &o.days); err != nil {
				return o, fmt.Errorf("bad --days %q", args[i])
			}
		case "--budget":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--budget requires N (tokens)")
			}
			i++
			var tok int
			if _, err := fmt.Sscanf(args[i], "%d", &tok); err != nil {
				return o, fmt.Errorf("bad --budget %q", args[i])
			}
			o.budgetCh = tok * 4 // 4 chars ≈ 1 token rough estimate
		case "--full":
			o.full = true
		case "--no-todos":
			o.noTodos = true
		case "--session":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--session requires a note id")
			}
			i++
			o.sessionID = args[i]
		case "--json":
			o.asJSON = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return o, fmt.Errorf("unknown flag %q", args[i])
			}
		}
	}
	return o, nil
}

// resolveProject derives the project identifier from --project (if set)
// or from the cwd. Returns (basename, parent/basename) where the second
// is the per-project notebook path convention from joplin-session-note.sh.
//
// Special case: "*" means cross-project — both returns are empty so
// collectSessions skips the filter.
func resolveProject(override string) (basename, parentBase string) {
	if override == "*" {
		return "", ""
	}
	if override != "" {
		// User passed an explicit name. Treat it as a basename; we don't
		// know the parent dir, so per-project notebook lookup will miss.
		return override, ""
	}
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return "", ""
	}
	basename = filepath.Base(cwd)
	parent := filepath.Base(filepath.Dir(cwd))
	if parent == "/" || parent == "." || parent == "" {
		parentBase = basename
	} else {
		parentBase = parent + "/" + basename
	}
	return basename, parentBase
}

// ── note collection ─────────────────────────────────────────────────────────

// collectSessions returns up to `limit` session notes for `project`,
// preferring notes within `since`. If fewer than `limit` notes exist
// within the window, returns the most recent regardless of age — so
// projects you context-switched away from still yield something
// useful when you come back.
//
// Sources (union, then dedup by ID, then sort by updated_time desc):
//  1. Per-project notebook at parent/basename (if it exists)
//  2. Global qai/sessions filtered by "— <basename>" in title
func collectSessions(client *joplin.Client, basename, parentBase string, limit int, since time.Time) []joplin.Note {
	all := map[string]joplin.Note{}

	if parentBase != "" {
		if f, _ := client.FindFolderByTitle(parentBase); f != nil {
			notes, _ := client.ListNotes(f.ID)
			for _, n := range notes {
				all[n.ID] = n
			}
		}
	}

	// Cross-project firehose: skip qai/sessions filter, take everything.
	if basename == "" && parentBase == "" {
		if f, _ := walkPath(client, sessionsRoot); f != nil {
			notes, _ := client.ListNotes(f.ID)
			for _, n := range notes {
				all[n.ID] = n
			}
		}
	} else if f, _ := walkPath(client, sessionsRoot); f != nil {
		notes, _ := client.ListNotes(f.ID)
		suffix := "— " + basename
		for _, n := range notes {
			if strings.Contains(n.Title, suffix) {
				all[n.ID] = n
			}
		}
	}

	out := make([]joplin.Note, 0, len(all))
	for _, n := range all {
		out = append(out, n)
	}
	// Sort by updated time, descending (most recent first).
	sort.Slice(out, func(i, j int) bool { return out[i].UserUpdatedTime > out[j].UserUpdatedTime })

	// Apply combined bounds: at least `limit` notes, OR everything from
	// `since` onward, whichever yields more.
	sinceMs := since.UnixMilli()
	cut := limit
	for cut < len(out) && out[cut].UserUpdatedTime >= sinceMs {
		cut++
	}
	if cut > len(out) {
		cut = len(out)
	}
	return out[:cut]
}

// collectTodos pulls TODO notes filtered the same way: by basename when
// the project is scoped, full notebook contents when "*".
func collectTodos(client *joplin.Client, basename string) []joplin.Note {
	f, _ := walkPath(client, todosRoot)
	if f == nil {
		return nil
	}
	notes, err := client.ListNotes(f.ID)
	if err != nil {
		return nil
	}
	if basename == "" {
		return notes
	}
	suffix := "— " + basename
	out := notes[:0]
	for _, n := range notes {
		if strings.Contains(n.Title, suffix) {
			out = append(out, n)
		}
	}
	// Most recent first.
	sort.Slice(out, func(i, j int) bool { return out[i].UserUpdatedTime > out[j].UserUpdatedTime })
	return out
}

// hydrateBodies fills in the Body field on each note via GetNote.
// joplin.ListNotes returns notes without bodies (the /folders/<id>/notes
// endpoint's default field set excludes body); recall needs them for the
// markdown rendering. Modifies the slice in place. Silent on individual
// fetch failures — a missing body just renders as an empty section.
func hydrateBodies(client *joplin.Client, notes []joplin.Note) {
	for i := range notes {
		if notes[i].Body != "" {
			continue
		}
		full, err := client.GetNote(notes[i].ID, "id", "title", "body", "user_updated_time")
		if err != nil || full == nil {
			continue
		}
		notes[i].Body = full.Body
	}
}

// walkPath resolves a "qai/sessions"-style path to a folder by walking
// the chain. Returns nil (no error) if any segment is missing — the
// caller treats "missing notebook" as "no notes" rather than an error.
func walkPath(client *joplin.Client, path string) (*joplin.Folder, error) {
	segments := joplin.SplitFolderPath(path)
	if len(segments) == 0 {
		return nil, nil
	}
	all, err := client.ListFolders()
	if err != nil {
		return nil, err
	}
	parentID := ""
	var leaf *joplin.Folder
	for _, want := range segments {
		var found *joplin.Folder
		for i := range all {
			if all[i].ParentID == parentID && strings.TrimSpace(all[i].Title) == want {
				found = &all[i]
				break
			}
		}
		if found == nil {
			return nil, nil
		}
		leaf = found
		parentID = found.ID
	}
	return leaf, nil
}

// ── rendering ───────────────────────────────────────────────────────────────

func renderMarkdown(sessions, todos []joplin.Note, project string, opts options) {
	header := project
	if header == "" {
		header = "all projects"
	}
	fmt.Printf("# %s — last %d sessions / %d days\n\n", header, len(sessions), opts.days)

	if len(sessions) == 0 {
		fmt.Println("(no session notes found for this scope)")
		fmt.Println()
	}

	// Budget-aware truncation: trim each body to a per-note cap that
	// keeps the total under opts.budgetCh.
	perNoteCap := opts.budgetCh
	if !opts.full && len(sessions) > 0 {
		perNoteCap = opts.budgetCh / len(sessions)
		if perNoteCap < 200 {
			perNoteCap = 200
		}
	}

	for _, n := range sessions {
		ts := formatTime(n.UserUpdatedTime)
		fmt.Printf("## %s — %s\n", ts, strings.TrimSpace(n.Title))
		body := strings.TrimSpace(n.Body)
		if !opts.full {
			body = firstParagraphOrTrunc(body, perNoteCap)
		}
		if body != "" {
			fmt.Println(body)
		}
		fmt.Println()
	}

	if !opts.noTodos && len(todos) > 0 {
		fmt.Printf("## Open TODOs (%d)\n\n", len(todos))
		for _, t := range todos {
			body := strings.TrimSpace(t.Body)
			line := firstLine(body)
			if line == "" {
				line = strings.TrimSpace(t.Title)
			}
			fmt.Printf("- %s\n", line)
		}
		fmt.Println()
	}

	if !opts.full && len(sessions) > 0 {
		fmt.Println("(qai recall --full to expand bodies; --session <id> for one note)")
	}
}

type recallJSON struct {
	Project  string          `json:"project"`
	Days     int             `json:"days"`
	Sessions []sessionRecord `json:"sessions"`
	Todos    []todoRecord    `json:"todos,omitempty"`
}

type sessionRecord struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	When  string `json:"when"`
	Body  string `json:"body"`
}

type todoRecord struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	When  string `json:"when"`
	Body  string `json:"body"`
}

func renderJSON(sessions, todos []joplin.Note, project string, opts options) {
	out := recallJSON{Project: project, Days: opts.days}
	perNoteCap := opts.budgetCh
	if !opts.full && len(sessions) > 0 {
		perNoteCap = opts.budgetCh / len(sessions)
		if perNoteCap < 200 {
			perNoteCap = 200
		}
	}
	for _, n := range sessions {
		body := strings.TrimSpace(n.Body)
		if !opts.full {
			body = firstParagraphOrTrunc(body, perNoteCap)
		}
		out.Sessions = append(out.Sessions, sessionRecord{
			ID:    n.ID,
			Title: strings.TrimSpace(n.Title),
			When:  formatTime(n.UserUpdatedTime),
			Body:  body,
		})
	}
	for _, t := range todos {
		out.Todos = append(out.Todos, todoRecord{
			ID:    t.ID,
			Title: strings.TrimSpace(t.Title),
			When:  formatTime(t.UserUpdatedTime),
			Body:  strings.TrimSpace(t.Body),
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// printSingleNote fetches one note by id and prints its body verbatim.
// Useful for `qai recall --session <id>` when one of the truncated
// session summaries looked interesting.
func printSingleNote(client *joplin.Client, id string, asJSON bool) {
	n, err := client.GetNote(id, "id", "title", "body", "user_updated_time")
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai recall: cannot fetch note %s: %v\n", id, err)
		fmt.Fprintln(os.Stderr, "  → fix: run 'qai recall' to list available note ids in this scope")
		os.Exit(1)
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(n)
		return
	}
	fmt.Printf("# %s\n%s\n\n%s\n", strings.TrimSpace(n.Title), formatTime(n.UserUpdatedTime), n.Body)
}

// ── helpers ─────────────────────────────────────────────────────────────────

func firstParagraphOrTrunc(body string, maxChars int) string {
	body = skipYAMLFrontmatter(body)
	// First paragraph = up to first double newline.
	if idx := strings.Index(body, "\n\n"); idx > 0 && idx < maxChars {
		return strings.TrimSpace(body[:idx])
	}
	if len(body) <= maxChars {
		return body
	}
	// Try to cut at a word boundary.
	cut := maxChars
	for cut > maxChars-20 && cut < len(body) && body[cut] != ' ' && body[cut] != '\n' {
		cut--
	}
	if cut <= 0 {
		cut = maxChars
	}
	return strings.TrimSpace(body[:cut]) + "…"
}

// skipYAMLFrontmatter strips leading metadata from a note body before
// summarisation: blank lines, markdown headers (`# ...`), and YAML
// frontmatter blocks (`---\n...\n---\n`).
//
// The legacy ~/.claude/hooks/joplin-session-note.sh prefixes every
// note with:
//
//	# Session 2026-04-27 — <sid>
//
//	---
//	project: work/qai
//	cwd: ...
//	session_id: ...
//	---
//
//	<actual content>
//
// All of that is metadata; surfacing it as the "summary" of the note
// is exactly the opposite of useful. This skipper handles arbitrary
// ordering of these prefixes (a header before frontmatter, after, or
// neither) and yields the first real content paragraph.
func skipYAMLFrontmatter(body string) string {
	lines := strings.Split(body, "\n")
	i := 0
	yamlSeen := false
	for i < len(lines) {
		trim := strings.TrimSpace(lines[i])
		switch {
		case trim == "":
			i++
		case strings.HasPrefix(trim, "#"):
			// Markdown header line — keep skipping. Legacy notes
			// have an `# Session ...` H1 generated by the hook.
			i++
		case trim == "---" && !yamlSeen:
			// First `---` line = open of YAML frontmatter; consume
			// through its matching close.
			yamlSeen = true
			closed := false
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == "---" {
					i = j + 1
					closed = true
					break
				}
			}
			if !closed {
				// Unbalanced — stop here rather than discard everything.
				return strings.Join(lines[i:], "\n")
			}
		case trim == "---" && yamlSeen:
			// Lone `---` horizontal rule (or section separator from
			// the joplin-session-note.sh append-style format) AFTER
			// the YAML block. Skip a single one; if we hit more
			// content quickly after, that's where the real body
			// begins.
			i++
		default:
			// Real content found.
			return strings.Join(lines[i:], "\n")
		}
	}
	return ""
}

func firstLine(body string) string {
	if idx := strings.Index(body, "\n"); idx > 0 {
		return strings.TrimSpace(body[:idx])
	}
	return strings.TrimSpace(body)
}

func formatTime(ms int64) string {
	if ms <= 0 {
		return "                "
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

// ── help ────────────────────────────────────────────────────────────────────

const help = `qai recall — read recent session notes + open TODOs from Joplin.

The read counterpart to 'qai note'. Bounded by design: defaults won't
dump the full dead-letter office into your context. Three bounds apply:

  --sessions N   max session notes to include (default 5)
  --days N       time window in days (default 14)
  --budget N     token budget; bodies truncated to fit (default 1500)

The default scope is "this cwd's project" — recall reads from a
per-project notebook (if you have one via the joplin-session-note.sh
hook) plus the global qai/sessions / qai/todos filtered by the cwd
basename. Cross-project access is opt-in.

USAGE
  qai recall                       Briefing for the current project
  qai recall --project <name>      Specific project by basename
  qai recall --project '*'         All projects (cross-project firehose)
  qai recall --sessions N          Override session count
  qai recall --days N              Override time window
  qai recall --budget N            Override token budget
  qai recall --full                Full bodies, no truncation
  qai recall --no-todos            Skip the open-todos section
  qai recall --session <id>        Print one specific note's full body
  qai recall --json                Machine-readable (for hooks + scripts)

EXAMPLES
  qai recall                       Default briefing (markdown)
  qai recall --json                JSON, suitable for SessionStart hook
  qai recall --session abc123 --full   One note in full

HOOK
  scripts/qai-recall-start.sh is a Claude Code SessionStart-hook that
  injects 'qai recall --json' output into the agent's context at
  session boot. Wire it into ~/.claude/settings.json under
  hooks.SessionStart.

ENV
  JOPLIN_TOKEN     Joplin Web Clipper token (required).
  JOPLIN_BASE_URL  Override the Joplin endpoint (default http://127.0.0.1:41184).
`
