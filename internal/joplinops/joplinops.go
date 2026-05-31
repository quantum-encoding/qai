// Package joplinops implements `qai joplin <sub>` — agent-friendly
// read commands for browsing Joplin programmatically without the
// scoping logic of `qai recall` or the write side of `qai note`.
//
// All subcommands honour --json so an agent can pipe JSON into jq or
// another tool. The human-readable default keeps output narrow enough
// for a 80-column terminal.
//
// The shape mirrors the legacy `use-joplin` Rust tool we ported from,
// but unified under one `qai joplin` namespace so the top-level
// command list stays tidy.
package joplinops

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// CmdJoplin is the dispatcher wired into cmd/qai/main.go.
func CmdJoplin(args []string) {
	if len(args) == 0 || isHelp(args[0]) {
		fmt.Println(helpJoplin)
		return
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "ping":
		cmdPing(rest)
	case "notebooks", "nb":
		cmdNotebooks(rest)
	case "notes":
		cmdNotes(rest)
	case "note":
		cmdNote(rest)
	case "events":
		cmdEvents(rest)
	case "search":
		cmdSearch(rest)
	case "tags":
		cmdTags(rest)
	case "tag":
		cmdTag(rest)
	default:
		fmt.Fprintf(os.Stderr, "qai joplin: unknown subcommand %q\n", sub)
		fmt.Fprintln(os.Stderr, "Run `qai joplin --help` for the list.")
		os.Exit(1)
	}
}

// ── shared helpers ──────────────────────────────────────────────────────

func isHelp(s string) bool {
	return s == "--help" || s == "-h" || s == "help"
}

// hasFlag returns true if name appears anywhere in args. Used for
// short boolean flags like --json that don't take a value.
func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

// stripFlag pulls "name value" from args, returning the value and the
// remaining args. For boolean flags use hasFlag instead.
func stripFlag(args []string, name string) (out []string, value string, present bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			present = true
			if i+1 < len(args) {
				value = args[i+1]
				out = append(out, args[:i]...)
				if i+2 <= len(args) {
					out = append(out, args[i+2:]...)
				}
				return
			}
			out = append(out, args[:i]...)
			return
		}
	}
	return args, "", false
}

// firstPositional returns the first non-flag arg, or "" if none.
func firstPositional(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// connect resolves the token + base URL and returns a wired client.
// All subcommands route through this so the error messages, env-var
// resolution, and default base URL stay consistent.
func connect() *joplin.Client {
	token, err := joplin.LoadDefaultToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin: %v\n", err)
		os.Exit(1)
	}
	base := os.Getenv("JOPLIN_URL")
	if base == "" {
		base = "http://127.0.0.1:41184"
	}
	return joplin.New(joplin.Config{BaseURL: base, Token: token})
}

// emitJSON marshals v with indent and prints. Used by every --json path.
func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ── ping ────────────────────────────────────────────────────────────────

func cmdPing(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println("qai joplin ping — check that the Joplin Web Clipper API is reachable")
		return
	}
	c := connect()
	if err := c.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin: ping failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ok")
}

// ── notebooks ───────────────────────────────────────────────────────────

func cmdNotebooks(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpNotebooks)
		return
	}
	jsonOut := hasFlag(args, "--json", "-j")
	tree := !hasFlag(args, "--flat") // default = tree

	c := connect()
	folders, err := c.ListFolders()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin: list folders: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		emitJSON(folders)
		return
	}

	if !tree {
		// Flat alphabetical list — useful for grep / fzf.
		sort.Slice(folders, func(i, j int) bool {
			return strings.ToLower(folders[i].Title) < strings.ToLower(folders[j].Title)
		})
		for _, f := range folders {
			fmt.Printf("%s  %s\n", f.ID[:8], f.Title)
		}
		return
	}

	printFolderTree(folders)
}

// printFolderTree builds the parent→children adjacency and walks it
// depth-first. Roots are folders whose ParentID is empty or unknown.
func printFolderTree(folders []joplin.Folder) {
	byID := make(map[string]joplin.Folder, len(folders))
	children := make(map[string][]joplin.Folder)
	for _, f := range folders {
		byID[f.ID] = f
		children[f.ParentID] = append(children[f.ParentID], f)
	}
	// Roots are anything with no parent OR whose parent doesn't exist
	// in our set (defensive against stale parent_id references).
	var roots []joplin.Folder
	for _, f := range folders {
		if f.ParentID == "" {
			roots = append(roots, f)
			continue
		}
		if _, ok := byID[f.ParentID]; !ok {
			roots = append(roots, f)
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		return strings.ToLower(roots[i].Title) < strings.ToLower(roots[j].Title)
	})
	for i, r := range roots {
		walkFolder(r, children, "", i == len(roots)-1)
	}
}

func walkFolder(f joplin.Folder, children map[string][]joplin.Folder, prefix string, last bool) {
	connector := "├── "
	nextPrefix := prefix + "│   "
	if last {
		connector = "└── "
		nextPrefix = prefix + "    "
	}
	fmt.Printf("%s%s%s  (%s)\n", prefix, connector, f.Title, f.ID[:8])
	kids := children[f.ID]
	sort.Slice(kids, func(i, j int) bool {
		return strings.ToLower(kids[i].Title) < strings.ToLower(kids[j].Title)
	})
	for i, k := range kids {
		walkFolder(k, children, nextPrefix, i == len(kids)-1)
	}
}

// ── notes ───────────────────────────────────────────────────────────────

func cmdNotes(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpNotes)
		return
	}
	jsonOut := hasFlag(args, "--json", "-j")
	var limitStr string
	args, limitStr, _ = stripFlag(args, "--limit")
	limit := 50
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	target := firstPositional(args)
	c := connect()

	// Resolve folder ID — accept a name (case-insensitive title match
	// across the full folder list) OR a 32-char hex ID directly.
	folderID := ""
	if target != "" {
		folderID = resolveFolderID(c, target)
		if folderID == "" {
			fmt.Fprintf(os.Stderr, "qai joplin notes: notebook %q not found\n", target)
			os.Exit(1)
		}
	}

	notes, err := c.ListNotes(folderID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin notes: %v\n", err)
		os.Exit(1)
	}

	// Sort by user_updated_time desc so recency wins.
	sort.Slice(notes, func(i, j int) bool {
		return notes[i].UserUpdatedTime > notes[j].UserUpdatedTime
	})
	if len(notes) > limit {
		notes = notes[:limit]
	}

	if jsonOut {
		emitJSON(notes)
		return
	}
	for _, n := range notes {
		fmt.Printf("%s  %s  %s\n", n.ID[:8], formatTime(n.UserUpdatedTime), truncate(n.Title, 80))
	}
}

// resolveFolderID accepts a 32-char Joplin ID OR a case-insensitive
// title match against the full folder list. Returns "" on miss.
func resolveFolderID(c *joplin.Client, s string) string {
	if len(s) == 32 && isHex(s) {
		return s
	}
	folders, err := c.ListFolders()
	if err != nil {
		return ""
	}
	for _, f := range folders {
		if strings.EqualFold(f.Title, s) {
			return f.ID
		}
	}
	return ""
}

// ── note (single) ───────────────────────────────────────────────────────

func cmdNote(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpNote)
		return
	}
	jsonOut := hasFlag(args, "--json", "-j")
	full := hasFlag(args, "--full")

	id := firstPositional(args)
	if id == "" {
		fmt.Fprintln(os.Stderr, "qai joplin note: missing note ID")
		os.Exit(1)
	}

	c := connect()
	fields := []string{"id", "title", "parent_id", "user_created_time", "user_updated_time", "source_url"}
	if full {
		fields = append(fields, "body")
	}
	n, err := c.GetNote(id, fields...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin note: %v\n", err)
		os.Exit(1)
	}
	// Tags are surfaced for every note — they're metadata Joplin
	// renders separately from the note body, and any agent reading a
	// note via this command will want to see them.
	tags, _ := c.GetNoteTags(id)

	if jsonOut {
		emitJSON(struct {
			*joplin.Note
			Tags []joplin.Tag `json:"tags,omitempty"`
		}{n, tags})
		return
	}
	fmt.Printf("ID:         %s\n", n.ID)
	fmt.Printf("Title:      %s\n", n.Title)
	if n.ParentID != "" {
		fmt.Printf("Parent:     %s\n", n.ParentID)
	}
	if n.UserCreatedTime > 0 {
		fmt.Printf("Created:    %s\n", formatTime(n.UserCreatedTime))
	}
	if n.UserUpdatedTime > 0 {
		fmt.Printf("Updated:    %s\n", formatTime(n.UserUpdatedTime))
	}
	if n.SourceURL != "" {
		fmt.Printf("Source URL: %s\n", n.SourceURL)
	}
	if len(tags) > 0 {
		names := make([]string, len(tags))
		for i, t := range tags {
			names[i] = t.Title
		}
		fmt.Printf("Tags:       %s\n", strings.Join(names, ", "))
	}
	if full {
		fmt.Println()
		fmt.Println(n.Body)
	}
}

// ── events ──────────────────────────────────────────────────────────────

func cmdEvents(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpEvents)
		return
	}
	jsonOut := hasFlag(args, "--json", "-j")
	var cursor, limitStr string
	args, cursor, _ = stripFlag(args, "--cursor")
	args, limitStr, _ = stripFlag(args, "--limit")
	_ = args
	limit := 0
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	c := connect()
	resp, err := c.GetEvents(cursor, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin events: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		emitJSON(resp)
		return
	}
	fmt.Printf("events:    %d\n", len(resp.Items))
	fmt.Printf("has_more:  %v\n", resp.HasMore)
	if resp.Cursor != "" {
		fmt.Printf("cursor:    %s\n", resp.Cursor)
		fmt.Printf("           (pass via --cursor on the next call to resume)\n")
	}
	if len(resp.Items) == 0 {
		fmt.Println("(no changes since last cursor)")
		return
	}
	fmt.Println()
	for _, e := range resp.Items {
		fmt.Printf("  %-7s %-8s %s  (event#%d)\n",
			joplin.EventTypeName(e.EventType),
			joplin.ItemTypeName(e.ItemType),
			e.ItemID,
			e.ID,
		)
	}
}

// ── search ──────────────────────────────────────────────────────────────

func cmdSearch(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpSearch)
		return
	}
	jsonOut := hasFlag(args, "--json", "-j")
	body := hasFlag(args, "--body", "-b")
	var limitStr string
	args, limitStr, _ = stripFlag(args, "--limit")
	limit := 20
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	q := firstPositional(args)
	if q == "" {
		fmt.Fprintln(os.Stderr, "qai joplin search: missing query")
		os.Exit(1)
	}

	c := connect()
	fields := []string{"id", "title", "parent_id", "user_updated_time"}
	if body {
		fields = append(fields, "body")
	}
	results, err := c.SearchNotes(q, limit, fields...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin search: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		emitJSON(results)
		return
	}
	if len(results) == 0 {
		fmt.Println("(no matches)")
		return
	}
	for _, r := range results {
		fmt.Printf("%s  %s  %s\n", r.ID[:8], formatTime(r.UserUpdatedTime), truncate(r.Title, 80))
		if body && r.Body != "" {
			fmt.Println(indentBody(r.Body, 4, 6))
		}
	}
}

// ── tags (list) ─────────────────────────────────────────────────────────

func cmdTags(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpTags)
		return
	}
	jsonOut := hasFlag(args, "--json", "-j")

	c := connect()
	tags, err := c.ListTags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tags: %v\n", err)
		os.Exit(1)
	}

	if jsonOut {
		emitJSON(tags)
		return
	}
	sort.Slice(tags, func(i, j int) bool {
		return strings.ToLower(tags[i].Title) < strings.ToLower(tags[j].Title)
	})
	for _, t := range tags {
		fmt.Printf("%s  %s\n", t.ID[:8], t.Title)
	}
}

// ── tag (show / mutate) ─────────────────────────────────────────────────
//
// One verb, mode-flagged. The positional arg is always the tag name
// (case-insensitive). Mode flags pick the verb:
//
//	(none)            show tag info + its notes
//	--add-to <id>     attach tag to note (creates tag if missing)
//	--rm-from <id>    detach tag from note
//	--delete          delete tag globally (with confirmation when interactive)

func cmdTag(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpTag)
		return
	}
	jsonOut := hasFlag(args, "--json", "-j")
	yes := hasFlag(args, "--yes", "-y")
	doDelete := hasFlag(args, "--delete")
	var addTo, rmFrom, limitStr string
	args, addTo, _ = stripFlag(args, "--add-to")
	args, rmFrom, _ = stripFlag(args, "--rm-from")
	args, limitStr, _ = stripFlag(args, "--limit")

	name := firstPositional(args)
	if name == "" {
		fmt.Fprintln(os.Stderr, "qai joplin tag: missing tag name (use --help for syntax)")
		os.Exit(1)
	}

	c := connect()

	switch {
	case addTo != "":
		tagAdd(c, name, addTo, jsonOut)
	case rmFrom != "":
		tagRemove(c, name, rmFrom, jsonOut)
	case doDelete:
		tagDelete(c, name, yes, jsonOut)
	default:
		tagShow(c, name, limitStr, jsonOut)
	}
}

func tagShow(c *joplin.Client, name, limitStr string, jsonOut bool) {
	tag, err := c.FindTagByName(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: %v\n", err)
		os.Exit(1)
	}
	if tag == nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: tag %q not found\n", name)
		os.Exit(1)
	}
	notes, err := c.GetTagNotes(tag.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: list notes: %v\n", err)
		os.Exit(1)
	}
	sort.Slice(notes, func(i, j int) bool {
		return notes[i].UserUpdatedTime > notes[j].UserUpdatedTime
	})
	limit := 50
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	if len(notes) > limit {
		notes = notes[:limit]
	}

	if jsonOut {
		emitJSON(struct {
			Tag   joplin.Tag    `json:"tag"`
			Notes []joplin.Note `json:"notes"`
		}{*tag, notes})
		return
	}
	fmt.Printf("Tag:   %s  (%s)\n", tag.Title, tag.ID)
	fmt.Printf("Notes: %d\n", len(notes))
	if len(notes) == 0 {
		return
	}
	fmt.Println()
	for _, n := range notes {
		fmt.Printf("  %s  %s  %s\n", n.ID[:8], formatTime(n.UserUpdatedTime), truncate(n.Title, 80))
	}
}

func tagAdd(c *joplin.Client, name, noteID string, jsonOut bool) {
	if !looksLikeID(noteID) {
		fmt.Fprintf(os.Stderr, "qai joplin tag: --add-to expects a 32-char note ID (got %q)\n", noteID)
		os.Exit(1)
	}
	tag, err := c.FindOrCreateTag(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: find-or-create %q: %v\n", name, err)
		os.Exit(1)
	}
	if err := c.AttachTagToNote(tag.ID, noteID); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: attach %q to %s: %v\n", name, noteID, err)
		os.Exit(1)
	}
	if jsonOut {
		emitJSON(map[string]any{"attached": true, "tag": tag, "note_id": noteID})
		return
	}
	fmt.Printf("attached %q (%s) to %s\n", tag.Title, tag.ID[:8], noteID)
}

func tagRemove(c *joplin.Client, name, noteID string, jsonOut bool) {
	if !looksLikeID(noteID) {
		fmt.Fprintf(os.Stderr, "qai joplin tag: --rm-from expects a 32-char note ID (got %q)\n", noteID)
		os.Exit(1)
	}
	tag, err := c.FindTagByName(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: %v\n", err)
		os.Exit(1)
	}
	if tag == nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: tag %q not found (nothing to detach)\n", name)
		os.Exit(1)
	}
	if err := c.DetachTagFromNote(tag.ID, noteID); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: detach %q from %s: %v\n", name, noteID, err)
		os.Exit(1)
	}
	if jsonOut {
		emitJSON(map[string]any{"detached": true, "tag": tag, "note_id": noteID})
		return
	}
	fmt.Printf("detached %q (%s) from %s\n", tag.Title, tag.ID[:8], noteID)
}

func tagDelete(c *joplin.Client, name string, yes, jsonOut bool) {
	tag, err := c.FindTagByName(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: %v\n", err)
		os.Exit(1)
	}
	if tag == nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: tag %q not found\n", name)
		os.Exit(1)
	}
	notes, _ := c.GetTagNotes(tag.ID)
	if !yes && isInteractive() {
		fmt.Fprintf(os.Stderr,
			"About to delete tag %q (%s). %d note(s) currently carry it; deletion\n"+
				"removes the tag entirely (notes themselves are untouched).\n"+
				"  → re-run with --yes to confirm, or detach individual notes with --rm-from.\n",
			tag.Title, tag.ID[:8], len(notes))
		os.Exit(1)
	}
	if err := c.DeleteTag(tag.ID); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin tag: delete %q: %v\n", name, err)
		os.Exit(1)
	}
	if jsonOut {
		emitJSON(map[string]any{"deleted": true, "tag": tag, "detached_from": len(notes)})
		return
	}
	fmt.Printf("deleted tag %q (was on %d note(s))\n", tag.Title, len(notes))
}

// looksLikeID is the same 32-char hex test used for note IDs.
func looksLikeID(s string) bool {
	return len(s) == 32 && isHex(s)
}

// isInteractive reports whether stderr is a TTY. Confirmation prompts
// only fire when there's a human watching — pipelines and scripts get
// the safer behaviour of refusing to act without --yes.
func isInteractive() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ── tiny utility helpers ────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func formatTime(ms int64) string {
	if ms == 0 {
		return "          "
	}
	return time.UnixMilli(ms).Format("2006-01-02")
}

func indentBody(body string, indent, maxLines int) string {
	pad := strings.Repeat(" ", indent)
	lines := strings.Split(body, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines = append(lines, "(…body truncated; use --json for full content)")
	}
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ── help text ───────────────────────────────────────────────────────────

const helpJoplin = `qai joplin — read Joplin programmatically

Agent-friendly read commands for browsing notebooks, notes, and the
change-event stream. All subcommands support --json for scripting.
The write side lives in 'qai note' (sessions / TODOs); the bounded
session-context read side lives in 'qai recall'.

USAGE
  qai joplin ping                       Connectivity check
  qai joplin notebooks [--flat] [--json]  List notebooks (tree by default)
  qai joplin notes [<nb>] [--limit N] [--json]
                                        List notes in a notebook (or all)
  qai joplin note <id> [--full] [--json]
                                        Get a note's metadata (and body with --full)
  qai joplin events [--cursor C] [--limit N] [--json]
                                        Incremental change stream — pass the
                                        returned cursor on the next call to
                                        resume from where you left off.
  qai joplin search <query> [--limit N] [--body] [--json]
                                        Full-text search across all notes.
  qai joplin tags [--json]               List every tag.
  qai joplin tag <name> [...]            Show / attach / detach / delete a tag.
                                        See 'qai joplin tag --help'.

EXAMPLES
  qai joplin notebooks
  qai joplin notes ProductResearch --limit 10
  qai joplin note f7e0f0aa62f74156a645289bfe2982bc --full
  qai joplin events                     # current snapshot
  qai joplin events --cursor abc123     # only what's changed
  qai joplin search "dropship" --body --json`

const helpNotebooks = `qai joplin notebooks — list notebooks (tree by default)

USAGE
  qai joplin notebooks                 Hierarchical tree of notebooks
  qai joplin notebooks --flat          Alphabetical list with short IDs
  qai joplin notebooks --json          Raw JSON of every folder

The tree view shows nesting via box-drawing characters and each
folder's 8-char ID prefix in parentheses — enough to disambiguate
without flooding the terminal.`

const helpNotes = `qai joplin notes — list notes in a notebook

USAGE
  qai joplin notes                      All notes across all notebooks
  qai joplin notes ProductResearch      Notes in a named notebook
  qai joplin notes <32-hex-id>          Notes by notebook ID
  qai joplin notes ... --limit 50       Cap result count (default 50)
  qai joplin notes ... --json           Raw JSON

Output is sorted by user_updated_time descending (recent first).
Default columns: 8-char note ID, YYYY-MM-DD updated, title (truncated to 80).`

const helpNote = `qai joplin note — get a single note's content

USAGE
  qai joplin note <id>                  Metadata only (title, dates, source URL)
  qai joplin note <id> --full           Include the note body
  qai joplin note <id> --json           Raw JSON shape

ID must be a 32-char hex (the full Joplin note ID, not a prefix).`

const helpEvents = `qai joplin events — incremental change stream

USAGE
  qai joplin events                     Latest events (no cursor)
  qai joplin events --cursor <C>        Only events newer than cursor C
  qai joplin events --limit 200         Override page size (default 100)
  qai joplin events --json              Raw JSON including new cursor

The cursor lets you poll for changes without re-listing everything:

  cursor=$(qai joplin events --json | jq -r .cursor)
  # later
  qai joplin events --cursor "$cursor" --json | jq .items

Item types: note, folder, resource, tag, note_tag, search, alarm,
master_key, item_change, note_resource, resource_local_state, revision,
migration. Event types: create, update, delete.`

const helpTags = `qai joplin tags — list every tag

USAGE
  qai joplin tags                Alpha-sorted list with short IDs
  qai joplin tags --json         Raw JSON

Tags are global in Joplin (not per-notebook).`

const helpTag = `qai joplin tag — show, attach, detach, or delete a tag

USAGE
  qai joplin tag <name>                       Show notes carrying this tag
  qai joplin tag <name> --limit N             Cap shown notes (default 50)
  qai joplin tag <name> --add-to <note-id>    Attach (creates tag if missing)
  qai joplin tag <name> --rm-from <note-id>   Detach
  qai joplin tag <name> --delete [--yes]      Delete tag globally
  qai joplin tag ... --json                   Raw JSON

Tag names are case-insensitive. Add and remove operate on 32-char note
IDs (the form qai joplin notes / search prints). Delete refuses to
proceed without --yes when run from a TTY; pipelines always require it.`

const helpSearch = `qai joplin search — full-text search across all notes

USAGE
  qai joplin search "query"             Top 20 matches, ordered recent first
  qai joplin search "query" --body      Include note bodies in output
  qai joplin search "query" --limit 50  Override result cap
  qai joplin search "query" --json      Raw JSON

The query supports Joplin's search syntax: plain words, exact phrases
in quotes, field qualifiers (title:foo, body:bar), Boolean operators
(AND, OR, -term to exclude). See https://joplinapp.org/help/#searching`
