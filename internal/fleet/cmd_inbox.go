// cmd_inbox.go — `qai fleet inbox` architect-side reader.
//
// --unread        : drains everything past the cursor and advances it.
// --watch         : block until at least one new report arrives, or
//                   --timeout fires. Drain-before-wait pattern (returns
//                   immediately if there's already pending data).
// --json          : machine-readable output for the architect agent.
//
// Pairs with the worker-side `qai report` command and the notifier
// daemon. The architect calls this on each [FLEET] nudge to consume
// the queued reports.

package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// CmdInbox handles `qai fleet inbox [--unread] [--watch] [--json] [--timeout N]`.
func CmdInbox(args []string) {
	if hasAny(args, "--help", "-h", "help") {
		inboxUsage()
		return
	}
	unread := false
	watch := false
	jsonOut := false
	timeout := 5 * time.Minute

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--unread":
			unread = true
		case "--watch":
			watch = true
		case "--json":
			jsonOut = true
		case "--timeout":
			if i+1 < len(args) {
				d, err := time.ParseDuration(args[i+1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "qai fleet inbox: invalid --timeout %q: %v\n", args[i+1], err)
					os.Exit(1)
				}
				timeout = d
				i++
			}
		default:
			fmt.Fprintf(os.Stderr, "qai fleet inbox: unknown flag %q\n", args[i])
			inboxUsage()
			os.Exit(1)
		}
	}

	fleetID, err := ResolveActiveFleet()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet inbox: %v\n", err)
		os.Exit(1)
	}

	read := func() ([]Report, error) {
		if unread {
			return ReadUnread(fleetID, CursorArchitect)
		}
		return ReadAll(fleetID)
	}

	reports, err := read()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet inbox: %v\n", err)
		os.Exit(1)
	}

	if len(reports) > 0 || !watch {
		emit(reports, jsonOut)
		return
	}

	// --watch with no pending data: block on fsnotify until inbox grows.
	got, err := waitForNewReport(fleetID, timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet inbox: %v\n", err)
		os.Exit(1)
	}
	if !unread {
		// In watch mode without --unread, return only what just arrived.
		emit(got, jsonOut)
		return
	}
	// --watch + --unread: cursor was already advanced by the read above
	// (which returned 0 records); re-read to advance again over the new
	// records that just arrived.
	fresh, err := ReadUnread(fleetID, CursorArchitect)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai fleet inbox: %v\n", err)
		os.Exit(1)
	}
	if len(fresh) == 0 {
		// Watcher fired but the records arrived between reads (rare).
		fresh = got
	}
	emit(fresh, jsonOut)
}

func emit(reports []Report, jsonOut bool) {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(reports)
		return
	}
	if len(reports) == 0 {
		fmt.Println("(no reports)")
		return
	}
	for _, r := range reports {
		ts := r.TS.Local().Format("15:04:05")
		fmt.Printf("[%s] %-10s %-25s %s\n", ts, r.Status, r.Pane, r.Message)
	}
}

// waitForNewReport blocks until inbox.jsonl gains at least one new line,
// up to timeout. Returns the new records (parsed since cursor).
func waitForNewReport(fleetID string, timeout time.Duration) ([]Report, error) {
	dir := FleetDir(fleetID)
	if _, err := EnsureFleetDir(fleetID); err != nil {
		return nil, err
	}
	inboxPath := filepath.Join(dir, "inbox.jsonl")

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher: %v", err)
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		return nil, fmt.Errorf("watch %s: %v", dir, err)
	}

	// Drain-before-wait: a record may have arrived between our caller's
	// last read and watcher setup.
	if reports, _ := PeekUnread(fleetID, CursorArchitect); len(reports) > 0 {
		// Advance cursor for these — caller path expects "what arrived
		// while we were waiting" semantics.
		if _, err := ReadUnread(fleetID, CursorArchitect); err != nil {
			return nil, err
		}
		return reports, nil
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errors.New("inbox watch timeout")
		}
		select {
		case <-time.After(remaining):
			return nil, errors.New("inbox watch timeout")
		case ev, ok := <-w.Events:
			if !ok {
				return nil, errors.New("watcher closed")
			}
			if filepath.Clean(ev.Name) != inboxPath {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			reports, err := ReadUnread(fleetID, CursorArchitect)
			if err != nil {
				return nil, err
			}
			if len(reports) > 0 {
				return reports, nil
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil, errors.New("watcher errors closed")
			}
			return nil, fmt.Errorf("watch error: %v", err)
		}
	}
}

// ResolveActiveFleet returns the current fleet id. Order of resolution:
//
//  1. $QAI_FLEET_ID env var (set by the runner in spawned panes).
//  2. ~/.qai/fleet/active — written by `qai fleet up`, points at the
//     fleet currently running. Lets the architect call `qai fleet inbox`
//     without remembering ids.
//
// Errors if neither is set.
func ResolveActiveFleet() (string, error) {
	if v := os.Getenv("QAI_FLEET_ID"); v != "" {
		return v, nil
	}
	data, err := os.ReadFile(activeFleetPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("no active fleet (run `qai fleet up <manifest>`, or set QAI_FLEET_ID)")
		}
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("active-fleet pointer is empty")
	}
	return id, nil
}

// SetActiveFleet writes ~/.qai/fleet/active so subsequent `qai fleet inbox`
// invocations from the architect resolve to this fleet without args.
func SetActiveFleet(fleetID string) error {
	if err := os.MkdirAll(filepath.Dir(activeFleetPath()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(activeFleetPath(), []byte(fleetID+"\n"), 0o600)
}

// ClearActiveFleet removes the ~/.qai/fleet/active pointer. Called from
// Down() so subsequent sessions start with a clean slate rather than
// inheriting a stale pointer to a torn-down fleet. Idempotent: missing
// file is not an error.
func ClearActiveFleet() error {
	err := os.Remove(activeFleetPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// activeFleetPath returns ~/.qai/fleet/active.
func activeFleetPath() string {
	// FleetDir("") returns ~/.qai/fleet/ (trailing slash via filepath.Join
	// of the empty id); use Dir to get the parent reliably.
	return filepath.Join(filepath.Dir(FleetDir("placeholder")), "active")
}

func inboxUsage() {
	fmt.Fprint(os.Stderr, `qai fleet inbox — read worker reports

Usage:
  qai fleet inbox [--unread] [--watch] [--timeout 5m] [--json]

Modes:
  (default)   Print all reports ever written.
  --unread    Print reports past the cursor; advance the cursor.
  --watch     If nothing pending, block until a new report arrives or
              --timeout fires. Drain-before-wait: returns immediately if
              there's already pending data.
  --json      Machine-readable output (an array of {ts, fleet_id, pane,
              status, message, important}).

Resolution:
  Reads the active fleet from $QAI_FLEET_ID or ~/.qai/fleet/active.
  `+"`"+`qai fleet up`+"`"+` writes the latter on bringup.

Examples:
  qai fleet inbox --unread --json     # architect drain on a [FLEET] nudge
  qai fleet inbox --watch --timeout 10m   # block until something arrives
  qai fleet inbox                     # everything
`)
}
