// cmd_report.go — `qai report` worker-side command.
//
// Workers call this to post status to the fleet inbox. Pane name and
// fleet id are read from env vars set by the runner at spawn time:
//   QAI_FLEET_ID   — required (errors if unset)
//   QAI_FLEET_PANE — required (the worker's own pane name)
//
// Naming note: the brief specified `qai term report`, but putting it
// under `qai term` would create an import cycle (terminal → fleet → terminal).
// `qai report` at top level is one fewer keystroke and avoids the cycle.

package fleet

import (
	"fmt"
	"os"
	"strings"
)

// CmdReport handles `qai report --status <s> --message <m> [--important]`.
func CmdReport(args []string) {
	if len(args) == 0 || hasAny(args, "--help", "-h", "help") {
		reportUsage()
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}

	var status, message string
	var important bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--status", "-s":
			if i+1 < len(args) {
				status = args[i+1]
				i++
			}
		case "--message", "-m":
			if i+1 < len(args) {
				message = args[i+1]
				i++
			}
		case "--important":
			important = true
		default:
			fmt.Fprintf(os.Stderr, "qai report: unknown flag %q\n", args[i])
			reportUsage()
			os.Exit(1)
		}
	}

	if status == "" {
		fmt.Fprintln(os.Stderr, "qai report: --status required (done|blocked|progress|info)")
		os.Exit(1)
	}
	if !validStatus(status) {
		fmt.Fprintf(os.Stderr, "qai report: invalid status %q (use done|blocked|progress|info)\n", status)
		os.Exit(1)
	}
	if message == "" {
		fmt.Fprintln(os.Stderr, "qai report: --message required")
		os.Exit(1)
	}

	fleetID := os.Getenv("QAI_FLEET_ID")
	if fleetID == "" {
		fmt.Fprintln(os.Stderr, "qai report: QAI_FLEET_ID not set (this command runs inside a fleet-spawned pane)")
		os.Exit(1)
	}
	pane := os.Getenv("QAI_FLEET_PANE")
	if pane == "" {
		// Fallback: use $TMUX_PANE so a manually-spawned pane can still
		// report. The architect-side prompt won't have the friendly
		// pane name, but the report still lands in the inbox.
		pane = os.Getenv("TMUX_PANE")
	}
	if pane == "" {
		fmt.Fprintln(os.Stderr, "qai report: QAI_FLEET_PANE / TMUX_PANE both unset; cannot identify reporter")
		os.Exit(1)
	}

	if err := AppendReport(Report{
		FleetID:   fleetID,
		Pane:      pane,
		Status:    status,
		Message:   message,
		Important: important,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "qai report: %v\n", err)
		os.Exit(1)
	}
}

func reportUsage() {
	fmt.Fprint(os.Stderr, `qai report — post a status report to the fleet inbox

Usage:
  qai report --status <state> --message "<text>" [--important]

States:
  done      — task complete, architect should consider closing the pane
  blocked   — cannot proceed without intervention
  progress  — periodic status (suppressed from notifier nudges by default)
  info      — non-status update worth surfacing

Flags:
  --important   override the progress-status suppression. Forces a nudge
                even when status=progress.

Env (set by the fleet runner — do not set manually):
  QAI_FLEET_ID    fleet identifier
  QAI_FLEET_PANE  this worker's pane name

Examples:
  qai report --status done --message "Audit complete. 3 HIGH, 7 MED."
  qai report --status blocked --message "Cannot find facts.json"
  qai report --status progress --message "47/200 files audited"
  qai report --status progress --message "Stage 2 starting" --important
`)
}

func validStatus(s string) bool {
	switch s {
	case "done", "blocked", "progress", "info":
		return true
	}
	return false
}

func hasAny(args []string, want ...string) bool {
	for _, a := range args {
		for _, w := range want {
			if a == w {
				return true
			}
		}
	}
	_ = strings.Contains // keep strings imported for future use
	return false
}
