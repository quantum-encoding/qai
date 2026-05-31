// Package joplinbridge implements `qai joplin bridge` — the Joplin-
// to-SurrealDB sync pipeline.
//
// Stage 1 (this file + sync.go + statements.go + classify.go) is the
// one-shot, resumable, idempotent bootstrap. Stages 2-5 (tail, status,
// graph context, link parser) ship separately.
//
// Schema lives in schema.surql and is embedded. Apply is part of sync's
// startup — there's no separate `init` verb to forget.
package joplinbridge

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// CmdBridge handles `qai joplin bridge <sub>`. Routed from joplinops.
func CmdBridge(args []string) {
	if len(args) == 0 || isHelp(args[0]) {
		fmt.Println(helpBridge)
		return
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "sync":
		cmdSync(rest)
	case "tail":
		cmdTail(rest)
	case "status":
		cmdStatus(rest)
	case "schema":
		fmt.Print(embeddedSchema)
	case "graph":
		fmt.Fprintln(os.Stderr,
			"qai joplin bridge graph: this verb moved to 'qai joplin graph'.")
		fmt.Fprintln(os.Stderr,
			"  → fix: run 'qai joplin graph context --help' for the read surface.")
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "qai joplin bridge: unknown subcommand %q\n", sub)
		fmt.Fprintln(os.Stderr, "Run `qai joplin bridge --help` for the list.")
		os.Exit(1)
	}
}

func isHelp(s string) bool { return s == "--help" || s == "-h" || s == "help" }

// hasFlag returns true if name appears in args.
func hasFlag(args []string, names ...string) bool {
	for _, n := range names {
		if slices.Contains(args, n) {
			return true
		}
	}
	return false
}

// stripFlag pulls "name value" from args, returning the value and the
// remaining args.
func stripFlag(args []string, name string) ([]string, string, bool) {
	for i := range args {
		if args[i] != name {
			continue
		}
		if i+1 < len(args) {
			return append(args[:i:i], args[i+2:]...), args[i+1], true
		}
		return args[:i:i], "", true
	}
	return args, "", false
}

// ─── sync subcommand ───────────────────────────────────────────────────────

func cmdSync(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpSync)
		return
	}
	var scope string
	args, scope, _ = stripFlag(args, "--notebook")
	_ = args

	// Joplin client — use the same auto-resolution as the other qai
	// joplin verbs so the user doesn't re-state JOPLIN_TOKEN here.
	jToken, err := joplin.LoadDefaultToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge sync: %v\n", err)
		os.Exit(1)
	}
	jBase := os.Getenv("JOPLIN_URL")
	if jBase == "" {
		jBase = "http://127.0.0.1:41184"
	}
	jc := joplin.New(joplin.Config{BaseURL: jBase, Token: jToken})
	if err := jc.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge sync: joplin: %v\n", err)
		os.Exit(1)
	}

	// SurrealDB client — default to notes_graph (override blast's
	// blast_radius default). Credential policy matches blast: no
	// compiled-in defaults for user/pass.
	sOpts := blast.DefaultOptions()
	if os.Getenv("QAI_SURREAL_DB") == "" {
		// User hasn't overridden — point at the bridge's own database
		// rather than blast's.
		sOpts.DB = "notes_graph"
	}
	if err := sOpts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge sync: %v\n", err)
		os.Exit(2)
	}
	sc := blast.NewClient(sOpts)

	// Resolve scope name → ID if the user passed a name rather than a
	// 32-hex Joplin folder ID. Same lookup style as `qai joplin notes`.
	if scope != "" && !looksLikeID(scope) {
		resolved, err := resolveNotebookID(jc, scope)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai joplin bridge sync: resolve %q: %v\n", scope, err)
			os.Exit(1)
		}
		scope = resolved
	}

	syncer := NewSyncer(jc, sc)
	syncer.NotebookScope = scope
	syncer.Logf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format, args...)
	}

	if err := syncer.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge sync: %v\n", err)
		fmt.Fprintln(os.Stderr,
			"  → bridge_state.last_error has been updated; re-run to resume from the last checkpoint.")
		os.Exit(1)
	}
	if scope != "" {
		fmt.Println("done (scoped; last_sync_completed not stamped — library not fully synced)")
	} else {
		fmt.Println("done")
	}
}

// looksLikeID — 32-char lowercase hex, matching Joplin's ID shape.
func looksLikeID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// resolveNotebookID translates a notebook title to its Joplin ID using
// a case-insensitive title match across the full folder list.
func resolveNotebookID(c *joplin.Client, name string) (string, error) {
	folders, err := c.ListFolders()
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if strings.EqualFold(strings.TrimSpace(f.Title), strings.TrimSpace(name)) {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("notebook %q not found", name)
}

// ─── help text ─────────────────────────────────────────────────────────────

const helpBridge = `qai joplin bridge — Joplin ↔ SurrealDB sync

The bridge populates a 'notes_graph' database in SurrealDB so an agent
can query Joplin content as a graph (notes × tags × notebooks, and
eventually wiki-links). Joplin remains the source of truth — Surreal
is the index.

USAGE
  qai joplin bridge sync [--notebook X]   Bootstrap or resume the full pull.
  qai joplin bridge tail [--once]         Long-running event-stream consumer.
  qai joplin bridge status                Health + lag (now - last_poll_at).
  qai joplin bridge schema                Print the embedded schema (no apply).

  qai joplin bridge graph                 Moved to 'qai joplin graph context'.

SCHEMA
  Namespace 'quantumencoding', database 'notes_graph' — both lazily
  created on first sync. Override with QAI_SURREAL_NS / _DB or
  --ns / --db (matches the qai blast / patterns convention).

CREDENTIALS
  QAI_SURREAL_USER   (required — no default)
  QAI_SURREAL_PASS   (required — no default)
  JOPLIN_TOKEN       (or read from Joplin Desktop settings.json)

  Connection defaults are the same as qai blast / patterns. There are
  no compiled-in user/pass defaults so a misconfigured target can't
  silently leak dev creds to a prod cluster.

EXAMPLES
  qai joplin bridge sync                   # full library
  qai joplin bridge sync --notebook qai    # qai/ subtree only
  qai joplin bridge schema | less`

const helpSync = `qai joplin bridge sync — bootstrap or resume the Joplin → SurrealDB pull

USAGE
  qai joplin bridge sync                     Full library
  qai joplin bridge sync --notebook X        Restrict to notebook X (+ descendants)
                                             X can be a notebook name OR a 32-hex ID.

BEHAVIOUR
  • Applies the embedded schema on every run (idempotent — every DEFINE
    has IF NOT EXISTS).
  • Reads bridge_state.bootstrap_progress on startup. If non-NONE,
    resumes from the last completed notebook. If NONE, walks from the
    start.
  • Each notebook is one Surreal transaction — atomic, so a mid-walk
    crash leaves the prior notebook's checkpoint as the resume point.
  • On clean completion, fetches the current /events cursor and writes
    it to bridge_state.cursor — the precondition for Stage 2 (tail).
  • --notebook X mode does NOT stamp last_sync_completed because the
    library as a whole isn't synced.

  Re-running an already-completed sync is a no-op — every write is an
  UPSERT keyed on the Joplin ID, with deterministic edge IDs.

PROJECT-TAG ALIAS WARNING
  Notes saved before the qai project-tag resolver shipped have no
  'project:<name>' tag. Stage 4 (graph context) will silently miss
  them. If that matters, set up '.qai/project' override files in the
  affected repos BEFORE running sync at scale — see 'qai project name
  --explain' to confirm what the resolver picks per repo.`
