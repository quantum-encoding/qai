// Package note implements the `qai note` subcommand — the write-side
// of the Joplin-backed knowledge store.
//
// Two flavours, distinguished by destination notebook:
//
//   qai note "body"               → qai/sessions/<auto-titled note>
//                                   (session summaries, agent-emitted
//                                   work logs, anything you'd otherwise
//                                   write into a long closing message
//                                   the user has to skim through)
//
//   qai note --todo "body"        → qai/todos/<auto-titled note>
//                                   (things only the human can do —
//                                   "rotate this key", "review this PR",
//                                   "decide between A and B" — that
//                                   would otherwise get lost between
//                                   conversation turns)
//
// Both default to writing into the `qai/` parent notebook so qai-driven
// activity stays grouped together in the Joplin sidebar (consistent
// with the clip-to-joplin script's auto-prepend behaviour). Override
// the destination with --to <path-style/notebook>.
//
// Body sources: positional arg or --stdin. Stdin is the hook path —
// a Claude Code Stop hook reads the transcript and pipes the last
// assistant message in via `qai note --stdin`.

package note

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Cfg is injected from main, matching the pattern in project / agent.
var Cfg *config.Config

// Default notebook paths. Both are children of `qai/` so the sidebar
// stays organised.
const (
	sessionsFolder = "qai/sessions"
	todosFolder    = "qai/todos"
)

// CmdNote handles `qai note <args>`.
func CmdNote(args []string) {
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai note: %v\n", err)
		usage()
		os.Exit(1)
	}
	if opts.help {
		usage()
		return
	}

	body, err := resolveBody(opts)
	if err != nil {
		die(err)
	}
	if strings.TrimSpace(body) == "" {
		die(fmt.Errorf("note body is empty (pass body as argument or via --stdin)"))
	}

	folder := opts.toFolder
	if folder == "" {
		if opts.todo {
			folder = todosFolder
		} else {
			folder = sessionsFolder
		}
	}

	title := opts.title
	if title == "" {
		title = autoTitle(opts.todo)
	}

	client, err := newClient()
	if err != nil {
		die(err)
	}
	f, err := client.FindOrCreateFolderPath(folder)
	if err != nil {
		die(fmt.Errorf("resolve folder %q: %w", folder, err))
	}
	n, err := client.CreateNote(joplin.Note{
		Title:    title,
		Body:     body,
		ParentID: f.ID,
	})
	if err != nil {
		die(err)
	}
	// Apply any --tag flags. Each tag is find-or-created (Joplin uses
	// global tags; creating "work" twice is a no-op). Failures here
	// don't roll back the note — the note already exists; we just
	// emit a warning so the user knows the tag side didn't take.
	var applied []string
	for _, name := range opts.tags {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		tag, err := client.FindOrCreateTag(name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai note: tag %q find/create: %v\n", name, err)
			continue
		}
		if err := client.AttachTagToNote(tag.ID, n.ID); err != nil {
			fmt.Fprintf(os.Stderr, "qai note: tag %q attach: %v\n", name, err)
			continue
		}
		applied = append(applied, tag.Title)
	}
	if len(applied) > 0 {
		fmt.Printf("saved → %s/%s (note %s, tags: %s)\n",
			folder, n.Title, n.ID, strings.Join(applied, ", "))
	} else {
		fmt.Printf("saved → %s/%s (note %s)\n", folder, n.Title, n.ID)
	}
}

// ── arg parsing ─────────────────────────────────────────────────────────────

type options struct {
	body     string
	title    string
	toFolder string
	tags     []string
	todo     bool
	stdin    bool
	help     bool
}

func parseArgs(args []string) (options, error) {
	var o options
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--help", "-h":
			o.help = true
		case "--todo":
			o.todo = true
		case "--stdin":
			o.stdin = true
		case "--title", "-T":
			i++
			if i >= len(args) {
				return o, fmt.Errorf("--title requires a value")
			}
			o.title = args[i]
		case "--to":
			i++
			if i >= len(args) {
				return o, fmt.Errorf("--to requires a notebook path")
			}
			o.toFolder = args[i]
		case "--tag":
			// Comma-separated list, e.g. --tag work,research. Repeating
			// the flag accumulates: --tag work --tag research is the
			// same as --tag work,research. Empty entries get dropped
			// when applied (TrimSpace + skip).
			i++
			if i >= len(args) {
				return o, fmt.Errorf("--tag requires a comma-separated list")
			}
			for _, t := range strings.Split(args[i], ",") {
				if t = strings.TrimSpace(t); t != "" {
					o.tags = append(o.tags, t)
				}
			}
		default:
			if strings.HasPrefix(a, "-") {
				return o, fmt.Errorf("unknown flag %q", a)
			}
			positional = append(positional, a)
		}
	}
	if len(positional) > 1 {
		return o, fmt.Errorf("expected one body arg, got %d (quote multi-word bodies)", len(positional))
	}
	if len(positional) == 1 {
		o.body = positional[0]
	}
	return o, nil
}

func resolveBody(opts options) (string, error) {
	if opts.stdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	return opts.body, nil
}

// autoTitle derives a sensible note title from the current cwd basename
// and a timestamp. Picks "TODO" or "Session" prefix per kind so the
// notebook view scans cleanly.
//
// The cwd basename is the project being worked on — meaningful for
// future-you scrolling through a list of session notes.
func autoTitle(isTodo bool) string {
	cwd, err := os.Getwd()
	base := ""
	if err == nil {
		base = filepath.Base(cwd)
		if base == "/" || base == "." {
			base = ""
		}
	}
	ts := time.Now().Format("2006-01-02 15:04")
	prefix := "Session"
	if isTodo {
		prefix = "TODO"
	}
	if base == "" {
		return fmt.Sprintf("%s %s", prefix, ts)
	}
	return fmt.Sprintf("%s %s — %s", prefix, ts, base)
}

// ── helpers (mirror project.go's pattern) ───────────────────────────────────

func newClient() (*joplin.Client, error) {
	if Cfg == nil || Cfg.Joplin.Token == "" {
		return nil, fmt.Errorf(
			"Joplin token not configured. Set one with either:\n" +
				"  export JOPLIN_TOKEN=...   (Tools → Options → Web Clipper in Joplin)\n" +
				"  or add joplin.token to ~/.qai/config.yaml")
	}
	c := joplin.New(joplin.Config{
		BaseURL: Cfg.Joplin.BaseURL,
		Token:   Cfg.Joplin.Token,
	})
	if err := c.Ping(); err != nil {
		return nil, err
	}
	return c, nil
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "qai note: %v\n", err)
	os.Exit(1)
}

func usage() {
	fmt.Println(`qai note — write a note to Joplin.

USAGE
  qai note "body"                       Save a session summary to qai/sessions
  qai note --todo "body"                Save a TODO for the human to qai/todos
  qai note --title "..." "body"         Override the auto-generated title
  qai note --to "qai/custom/path" "body"  Override the destination notebook
  qai note --stdin                      Read body from stdin (for hooks)

WHY TWO NOTEBOOKS

  qai/sessions   Things you did. Long agent-emitted work logs, build
                 summaries, debug recaps, anything the agent would
                 otherwise stuff into a closing message that the human
                 has to skim. Writing here means the closing reply can
                 be one line ("Done. Notes in Joplin.") and the detail
                 lives in a searchable place.

  qai/todos      Things only the human can do. Key rotations, manual
                 reviews, decisions, things the agent surfaced but
                 cannot complete autonomously. Read this notebook when
                 you have time; tackle items at your own pace.

EXAMPLES
  qai note "Fixed the macOS Gatekeeper install issue. cp → install(1) is
            the actual fix. Pushed as 6a67cee."

  qai note --todo "Rotate CLAUDE_ADMIN_API_KEY — leaked into a transcript
                   on 2026-04-28."

  echo "..." | qai note --stdin --title "Session summary"

ENV
  JOPLIN_TOKEN     Joplin Web Clipper token (required).
  JOPLIN_BASE_URL  Override the Joplin endpoint (default http://127.0.0.1:41184).

NOTEBOOKS
  Both default destinations live under a "qai" parent folder so qai-
  driven activity stays grouped in the Joplin sidebar. Override the
  destination with --to. Path-style names ("qai/foo/bar") create the
  whole chain (matches the behaviour of the clip-to-joplin script).

HOOK
  See scripts/qai-session-hook.sh for a Claude Code Stop-hook that
  auto-saves the last assistant message to qai/sessions on every stop.`)
}
