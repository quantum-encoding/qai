// cmd.go — CLI dispatchers for `qai fleet` and `qai sessions`.
//
// Style matches the rest of qai: hand-rolled switch in CmdFoo, no cobra.
// The runner / spec / sessions library API does the work; these funcs
// just parse args and format output.

package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CmdFleet handles `qai fleet <action>`.
func CmdFleet(args []string) {
	if len(args) == 0 {
		fleetUsage()
		os.Exit(1)
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "help", "--help", "-h":
		fleetUsage()
	case "up":
		fleetUp(rest)
	case "down":
		fleetDown(rest)
	case "status":
		fleetStatus(rest)
	case "snapshot", "snap":
		fleetSnapshot(rest)
	case "attach":
		fleetAttach(rest)
	case "inbox":
		CmdInbox(rest)
	case "notifier":
		fleetNotifier(rest)
	case "bootstrap":
		fleetBootstrap()
	default:
		fmt.Fprintf(os.Stderr, "qai fleet: unknown action %q\n", action)
		fleetUsage()
		os.Exit(1)
	}
}

// CmdSessions handles `qai sessions <action>`.
func CmdSessions(args []string) {
	if len(args) == 0 {
		sessionsList(nil)
		return
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "help", "--help", "-h":
		sessionsUsage()
	case "list", "ls":
		sessionsList(rest)
	default:
		fmt.Fprintf(os.Stderr, "qai sessions: unknown action %q\n", action)
		sessionsUsage()
		os.Exit(1)
	}
}

// ─── fleet up ────────────────────────────────────────────────────────────

func fleetUp(args []string) {
	manifest, dryRun := parseFleetArgs(args, "up")
	spec, err := LoadFile(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if dryRun {
		printPlan(spec)
		return
	}

	fmt.Fprintf(os.Stderr, "qai fleet: bringing %d panes up in parallel…\n", len(spec.Panes))
	ctx := context.Background()
	fleetID, err := Up(ctx, spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "qai fleet: all panes up. fleet-id: %s\n", fleetID)
	if spec.Defaults.Reporting.Enabled {
		fmt.Fprintf(os.Stderr, "qai fleet: notifier running. Workers will report via `qai report`.\n")
		fmt.Fprintf(os.Stderr, "           Architect: on `[FLEET]` nudges, run `qai fleet inbox --unread --json`.\n")
	}
}

func printPlan(spec *Spec) {
	fmt.Printf("Plan (%d panes):\n", len(spec.Panes))
	for _, p := range spec.Panes {
		cmd, err := buildCommand(p)
		if err != nil {
			cmd = fmt.Sprintf("(%v)", err)
		}
		fmt.Printf("  %s\n", p.Name)
		fmt.Printf("    cwd:    %s\n", p.Cwd)
		fmt.Printf("    cmd:    %s\n", cmd)
		if p.WaitFor != "" {
			fmt.Printf("    wait:   %s\n", p.WaitFor)
		}
		fmt.Printf("    prompt: %s\n", truncForPlan(p.Prompt))
	}
}

func truncForPlan(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	if len(s) > 100 {
		return s[:97] + "…"
	}
	return s
}

// ─── fleet down ──────────────────────────────────────────────────────────

func fleetDown(args []string) {
	manifest, _ := parseFleetArgs(args, "down")
	spec, err := LoadFile(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := Down(spec); err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet down: %v\n", err)
		os.Exit(1)
	}
	if id, err := ResolveActiveFleet(); err == nil {
		_ = stopNotifier(id)
	}
	fmt.Fprintf(os.Stderr, "qai fleet: %d panes torn down.\n", len(spec.Panes))
}

// ─── fleet status ────────────────────────────────────────────────────────

func fleetStatus(args []string) {
	manifest, _ := parseFleetArgs(args, "status")
	jsonOut := hasFlag(args, "--json")

	spec, err := LoadFile(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	statuses, err := Status(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet status: %v\n", err)
		os.Exit(1)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(statuses)
		return
	}
	fmt.Printf("%-25s %-6s %-8s %-15s %s\n", "NAME", "ALIVE", "ID", "CMD", "CWD")
	fmt.Println(strings.Repeat("-", 100))
	for _, s := range statuses {
		alive := "yes"
		if !s.Alive {
			alive = "no"
		}
		fmt.Printf("%-25s %-6s %-8s %-15s %s\n", s.Name, alive, s.ID, s.Cmd, s.Cwd)
	}
}

// ─── fleet snapshot ──────────────────────────────────────────────────────

func fleetSnapshot(args []string) {
	manifest, _ := parseFleetArgs(args, "snapshot")
	spec, err := LoadFile(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	snaps, err := Snapshot(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet snapshot: %v\n", err)
		os.Exit(1)
	}
	for _, s := range snaps {
		fmt.Printf("━━━ %s ━━━\n", s.Title)
		if s.Output == "" {
			fmt.Println("(no output)")
		} else {
			fmt.Println(s.Output)
		}
		fmt.Println()
	}
}

// ─── fleet attach ────────────────────────────────────────────────────────

func fleetAttach(args []string) {
	if len(args) == 0 || hasFlag(args, "--help") {
		fmt.Fprintln(os.Stderr, "usage: qai fleet attach <pane-name>")
		os.Exit(1)
	}
	name := args[0]
	if err := exec.Command("tmux", "select-pane", "-t", name).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet attach: %v\n", err)
		os.Exit(1)
	}
}

// ─── sessions list ───────────────────────────────────────────────────────

func sessionsList(args []string) {
	cwdFilter := ""
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cwd":
			if i+1 < len(args) {
				cwdFilter = args[i+1]
				i++
			}
		case "--json":
			jsonOut = true
		}
	}
	sessions, err := ListSessions(cwdFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai sessions: %v\n", err)
		os.Exit(1)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sessions)
		return
	}
	fmt.Printf("%-38s %-6s %-8s %-30s %s\n", "SESSION ID", "LIVE", "STATUS", "UPDATED", "CWD")
	fmt.Println(strings.Repeat("-", 120))
	for _, s := range sessions {
		live := "no"
		if s.Live {
			live = "yes"
		}
		updated := s.UpdatedAt.Format(time.RFC3339)
		fmt.Printf("%-38s %-6s %-8s %-30s %s\n", s.ID, live, s.Status, updated, s.Cwd)
	}
}

// ─── fleet notifier ──────────────────────────────────────────────────────

func fleetNotifier(args []string) {
	if len(args) == 0 || hasFlag(args, "--help") {
		fmt.Fprintln(os.Stderr, "usage: qai fleet notifier <fleet-id>")
		fmt.Fprintln(os.Stderr, "Long-running daemon: watches inbox and nudges the architect on new reports.")
		fmt.Fprintln(os.Stderr, "Started automatically by `qai fleet up` when manifest enables reporting.")
		os.Exit(1)
	}
	if err := RunNotifier(args[0], DefaultNotifierConfig()); err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet notifier: %v\n", err)
		os.Exit(1)
	}
}

// ─── fleet bootstrap ─────────────────────────────────────────────────────

func fleetBootstrap() {
	fmt.Print(architectPromptProtocol)
}

// architectPromptProtocol is the instruction block the human pastes into
// their CLAUDE.md (or hands to the architect at session start) so the
// architect knows what `[FLEET]` sentinels mean and how to act on them.
const architectPromptProtocol = `# Fleet Architect Protocol

You are the lead architect of a fleet of worker agents running in
sibling tmux panes. Workers report status to a shared inbox. A notifier
daemon nudges you with messages prefixed ` + "`[FLEET]`" + ` when there are unread
reports to consume.

When you receive a message starting with ` + "`[FLEET]`" + `:

1. It is a machine-generated nudge, not the human asking a question.
   Do not respond conversationally as if to the human.

2. Run ` + "`qai fleet inbox --unread --json`" + ` to drain the inbox. The
   tool returns an array of {ts, fleet_id, pane, status, message,
   important} objects.

3. For each report, decide:
   - status=done       → if the work is complete, run
                         ` + "`qai term close <pane>`" + ` to clean up the pane.
                         Mention the worker by name in your reply so the
                         human can see what landed.
   - status=blocked    → either send unblock instructions via
                         ` + "`qai term send <pane> \"<answer>\"`" + ` or
                         escalate to the human.
   - status=info       → acknowledge in your reply; act if the info
                         changes plans.
   - status=progress   → routine; usually just note silently. The
                         notifier suppresses these unless flagged
                         --important, so if one arrives, it matters.

4. Human messages are NOT prefixed with ` + "`[FLEET]`" + `. Respond to those
   normally.

5. Do not call ` + "`qai fleet inbox --unread`" + ` unprompted. The notifier
   tells you when there's something new — quiet inbox = nothing to do.

# Sending the fleet up

To bring a fleet up, run ` + "`qai fleet up <manifest.yaml>`" + ` from this
pane. The runner detects this pane's id (via $TMUX_PANE) and persists
it as the architect pane; the notifier will direct ` + "`[FLEET]`" + ` nudges here.
`

// ─── helpers ─────────────────────────────────────────────────────────────

func parseFleetArgs(args []string, action string) (manifestPath string, dryRun bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry", "--dry-run":
			dryRun = true
		case "--help", "-h":
			fleetUsage()
			os.Exit(0)
		default:
			if !strings.HasPrefix(args[i], "-") && manifestPath == "" {
				manifestPath = args[i]
			}
		}
	}
	if manifestPath == "" {
		fmt.Fprintf(os.Stderr, "usage: qai fleet %s <manifest.yaml> [--dry]\n", action)
		os.Exit(1)
	}
	return manifestPath, dryRun
}

func hasFlag(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func fleetUsage() {
	fmt.Fprint(os.Stderr, `qai fleet — declarative parallel agent panes

  qai fleet up        <manifest.yaml> [--dry]   Spawn every pane in parallel
  qai fleet down      <manifest.yaml>           Kill named panes + notifier
  qai fleet status    <manifest.yaml> [--json]  Alive/dead per manifest pane
  qai fleet snapshot  <manifest.yaml>           Tail every pane's last lines
  qai fleet attach    <pane-name>               Focus a pane in tmux
  qai fleet inbox     [--unread] [--watch]      Read worker reports
  qai fleet notifier  <fleet-id>                Run the notifier daemon (auto-started by 'up')
  qai fleet bootstrap                           Print the architect prompt-protocol block

Manifest schema (v1):
  version: 1
  defaults:        # optional
    cwd: /path
    startup_timeout: 30s
  panes:
    - name: <id>          # [a-zA-Z0-9_-]+
      cwd: /path
      agent:
        kind: fresh|resume
        cmd: claude
        args: ["--model", "opus-4.7"]   # optional
        session: "@alias-or-uuid"        # required when kind=resume
      prompt: |
        opaque text, sent byte-for-byte
      wait_for: /path/to/file            # optional, single orchestration primitive

Resume aliases:
  session: "@chronos_engine" — resolves to the most recent session whose cwd
                               basename matches "chronos_engine".
  session: "@" or omitted    — for kind=resume, uses the latest session in
                               the pane's own cwd.
`)
}

func sessionsUsage() {
	fmt.Fprint(os.Stderr, `qai sessions — Claude Code session discovery

  qai sessions list [--cwd <path>] [--json]   List live + historical sessions

Live sessions come from ~/.claude/sessions/<pid>.json.
Historical sessions come from ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl.
Use the IDs as input to a fleet manifest's agent.session field, or directly
with `+"`claude --resume <id>`"+`.
`)
}
