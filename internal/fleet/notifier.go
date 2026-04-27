// notifier.go — debounced safe-fire daemon.
//
// One process per fleet. Watches inbox.jsonl for new reports, debounces,
// suppresses status=progress unless flagged --important, and fires a
// short sentinel-prefixed nudge into the architect's pane only when the
// architect's input line is empty (the human isn't mid-typing).
//
// Cost-control, not correctness:
//   The corruption tests (1, 3) and your direct experience confirm that
//   Anthropic's input handler queues messages cleanly across tool calls
//   and serialises rapid arrivals. So the "no spinner" / mid-response
//   check is unnecessary. The only remaining stomping mode is the human
//   typing into the architect's input — that's the single check below.
//
// Lifecycle:
//   - `qai fleet up` writes architect-pane and active-fleet pointers,
//     starts this notifier as a detached background process.
//   - `qai fleet down` reads notifier.pid and SIGTERMs it.
//   - SIGTERM removes the pidfile and exits cleanly.

package fleet

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/quantum-encoding/qai-cli/internal/terminal"
)

// NotifierConfig tunes the debouncer + safety checks. Defaults match the
// design discussion (10s debounce, 10-msg batch ceiling, 30s max wait,
// 5min hold cap before "fire anyway").
type NotifierConfig struct {
	DebounceQuiet time.Duration // reset on each new report
	MaxBatch      int           // flush early when pending reaches this
	MaxWait       time.Duration // flush even if reports keep arriving
	SafetyRecheck time.Duration // re-poll architect pane while held
	HoldCap       time.Duration // after this long held, fire anyway
}

// DefaultNotifierConfig returns the v1 tuning.
func DefaultNotifierConfig() NotifierConfig {
	return NotifierConfig{
		DebounceQuiet: 10 * time.Second,
		MaxBatch:      10,
		MaxWait:       30 * time.Second,
		SafetyRecheck: 5 * time.Second,
		HoldCap:       5 * time.Minute,
	}
}

// RunNotifier is the daemon entrypoint. Blocks until SIGTERM/SIGINT.
//
// fleetID identifies which inbox to watch and which architect-pane file
// to read. Re-entrancy is prevented via a pidfile lock — a second
// invocation for the same fleetID will refuse to start.
func RunNotifier(fleetID string, cfg NotifierConfig) error {
	if fleetID == "" {
		return errors.New("RunNotifier: fleet id required")
	}
	dir, err := EnsureFleetDir(fleetID)
	if err != nil {
		return err
	}

	if err := acquirePidfile(filepath.Join(dir, "notifier.pid")); err != nil {
		return err
	}
	defer releasePidfile(filepath.Join(dir, "notifier.pid"))

	architectPane, err := readArchitectPane(fleetID)
	if err != nil {
		return fmt.Errorf("notifier: %v", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watcher: %v", err)
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		return fmt.Errorf("watch %s: %v", dir, err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	d := &debouncer{
		cfg:           cfg,
		fleetID:       fleetID,
		architectPane: architectPane,
	}

	for {
		select {
		case <-sigs:
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			if filepath.Base(ev.Name) != "inbox.jsonl" {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			d.notify()
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("watcher errors closed")
			}
			fmt.Fprintf(os.Stderr, "notifier: watcher: %v\n", err)
		}
	}
}

// debouncer collapses bursts of inbox writes into a single nudge.
//
// State machine:
//   IDLE     —  no pending reports. notify() peeks the inbox, transitions
//               to ARMED when there's something to nudge about.
//   ARMED    —  pending count > 0, debounce timer running. notify() resets
//               the timer until DebounceQuiet of quiet OR MaxBatch reached
//               OR MaxWait elapsed since first arrival.
//   FLUSHING —  one delivery in flight; further notify() calls accumulate
//               into a fresh ARMED batch the moment FLUSHING completes.
//
// The implementation skips the explicit state machine and just (re)starts
// a timer goroutine on every notify(); cancellation is via mu + timerGen.
type debouncer struct {
	cfg           NotifierConfig
	fleetID       string
	architectPane string

	mu        sync.Mutex
	armedAt   time.Time
	timerGen  int  // incremented each notify, killing earlier scheduled flushes
	flushing  bool // serialises deliveries
}

// notify reacts to a new inbox event. It either schedules a fresh flush
// or extends the current debounce window.
func (d *debouncer) notify() {
	d.mu.Lock()
	now := time.Now()
	first := false
	if d.armedAt.IsZero() {
		d.armedAt = now
		first = true
	}
	d.timerGen++
	gen := d.timerGen
	cfg := d.cfg
	armedAt := d.armedAt
	d.mu.Unlock()

	pending, _ := PeekUnread(d.fleetID)
	pending = filterReportable(pending)
	if len(pending) == 0 {
		// All pending reports filtered out (e.g. all progress without
		// --important). Drop the cursor forward so they don't pile up,
		// then idle.
		_ = AdvanceCursor(d.fleetID)
		d.mu.Lock()
		d.armedAt = time.Time{}
		d.mu.Unlock()
		return
	}

	// Early flush: batch ceiling.
	if len(pending) >= cfg.MaxBatch {
		d.attemptFlush(gen)
		return
	}

	// Time-based flush: pick whichever expires first — debounce-quiet
	// from now, OR max-wait from first arrival.
	debounceFire := time.Until(now.Add(cfg.DebounceQuiet))
	maxWaitFire := time.Until(armedAt.Add(cfg.MaxWait))
	wait := debounceFire
	if maxWaitFire < wait {
		wait = maxWaitFire
	}
	if wait <= 0 {
		d.attemptFlush(gen)
		return
	}

	if first {
		go d.scheduleFlush(gen, wait)
	} else {
		// notify() in an existing window: cancel any older scheduled
		// flush by bumping timerGen (already done above) and start a
		// fresh one with the new wait. The earlier goroutine will see
		// the gen mismatch on wakeup and exit.
		go d.scheduleFlush(gen, wait)
	}
}

func (d *debouncer) scheduleFlush(gen int, wait time.Duration) {
	time.Sleep(wait)
	d.mu.Lock()
	if gen != d.timerGen {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	d.attemptFlush(gen)
}

// attemptFlush drains pending reports, runs the safety check loop, and
// (eventually) sends a nudge. Concurrent attempts are serialised via the
// flushing flag — a second flush is skipped because the first will pick
// up the freshly-arrived records.
func (d *debouncer) attemptFlush(gen int) {
	d.mu.Lock()
	if d.flushing || gen != d.timerGen {
		d.mu.Unlock()
		return
	}
	d.flushing = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.flushing = false
		d.armedAt = time.Time{}
		d.mu.Unlock()
	}()

	// Drain *into* the read so the cursor advances atomically with the
	// delivery decision.
	reports, err := ReadUnread(d.fleetID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "notifier: read inbox: %v\n", err)
		return
	}
	reports = filterReportable(reports)
	if len(reports) == 0 {
		return
	}

	// Safety: hold while human is mid-typing in the architect pane.
	holdStart := time.Now()
	for {
		busy, reason, err := architectInputBusy(d.architectPane)
		if err != nil {
			// Architect pane gone? Log and break — don't lose reports
			// (cursor already advanced past them, but they're in the
			// inbox file forever).
			fmt.Fprintf(os.Stderr, "notifier: architect pane probe failed: %v — firing anyway\n", err)
			break
		}
		if !busy {
			break
		}
		if time.Since(holdStart) >= d.cfg.HoldCap {
			fmt.Fprintf(os.Stderr, "notifier: held %s waiting on architect (%s) — firing anyway\n",
				d.cfg.HoldCap, reason)
			break
		}
		time.Sleep(d.cfg.SafetyRecheck)
	}

	// Fire one short sentinel line + Enter. The architect's prompt
	// protocol teaches it to call `qai fleet inbox --unread --json` on
	// receipt of this sentinel.
	nudge := fmt.Sprintf("[FLEET] %d new report%s — qai fleet inbox --unread --json",
		len(reports), pluralise(len(reports)))
	if err := terminal.Send(d.architectPane, nudge, true); err != nil {
		fmt.Fprintf(os.Stderr, "notifier: send to architect: %v\n", err)
	}
}

// filterReportable drops status=progress records that aren't flagged
// --important. Done/blocked/info always survive.
func filterReportable(in []Report) []Report {
	out := in[:0]
	for _, r := range in {
		if r.Status == "progress" && !r.Important {
			continue
		}
		out = append(out, r)
	}
	return out
}

// architectInputBusy reports whether the architect pane shows the human
// mid-typing. Reads the bottom 10 lines of the pane, finds the
// `shift+tab` footer (input-box ready marker), walks up to the `❯ ` prompt
// line, and checks for non-whitespace chars after the prompt char.
//
// Returns busy=true if any of:
//   - The footer marker is missing entirely (architect not at its input
//     box: still booting, mid-response, or in a slash-command modal).
//   - The input line has characters past `❯ `.
//
// Returns an error only if reading the pane fails.
func architectInputBusy(pane string) (bool, string, error) {
	buf, err := terminal.Read(pane, 10)
	if err != nil {
		return true, "read failed", err
	}
	lines := strings.Split(buf, "\n")
	footerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "shift+tab") {
			footerIdx = i
			break
		}
	}
	if footerIdx < 0 {
		return true, "footer marker not found (architect mid-response or modal)", nil
	}
	for i := footerIdx - 1; i >= 0; i-- {
		line := lines[i]
		idx := strings.Index(line, "❯")
		if idx < 0 {
			continue
		}
		// Everything after `❯ ` should be empty (only whitespace) for
		// the input to count as idle.
		rest := strings.TrimSpace(line[idx+len("❯"):])
		if rest != "" {
			return true, "human typing in input", nil
		}
		return false, "input line empty", nil
	}
	return true, "no input line above footer", nil
}

// readArchitectPane reads ~/.qai/fleet/<id>/architect-pane.
func readArchitectPane(fleetID string) (string, error) {
	path := filepath.Join(FleetDir(fleetID), "architect-pane")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read architect-pane file: %v (was the fleet brought up via `qai fleet up`?)", err)
	}
	pane := strings.TrimSpace(string(data))
	if pane == "" {
		return "", errors.New("architect-pane file is empty")
	}
	return pane, nil
}

// acquirePidfile creates pidfile or refuses if a live process owns it.
// Stale pidfiles (process gone) are silently overwritten.
func acquirePidfile(path string) error {
	if data, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			if err := syscall.Kill(pid, 0); err == nil {
				return fmt.Errorf("notifier already running (pid %d)", pid)
			}
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func releasePidfile(path string) {
	_ = os.Remove(path)
}

func pluralise(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
