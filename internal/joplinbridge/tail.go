package joplinbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Tail is the long-running daemon that polls Joplin /events and
// applies them to the graph. One per qai joplin bridge tail process.
//
// Apply-then-advance crash consistency: every event is applied AND
// its cursor persisted BEFORE the next event runs. A SIGKILL between
// apply and cursor write means we re-apply that one event on
// restart — safe because every apply is idempotent (UPSERT on
// entities, INSERT RELATION ON DUPLICATE KEY UPDATE on edges, all
// keyed on deterministic IDs via the existing stmt… helpers).
//
// Heartbeat: last_poll_at is written after every successful poll
// regardless of event count. Stage 4 reads last_poll_at as the
// liveness signal; last_event_at is data freshness. The two are not
// the same and Stage 4 will get confused if you swap them. See the
// Stage 2 spec.
type Tail struct {
	J joplinAPI
	S surrealAPI

	// Now is the clock — tests pin it. Defaults to time.Now in NewTail.
	Now func() time.Time

	// Logf is the stderr sink. Tail must NEVER write to stdout (Stage 3
	// reserves it for --json status output). Defaults to a noop in
	// NewTail; the CmdBridge caller wires it to fmt.Fprintf(os.Stderr).
	Logf func(format string, args ...any)

	// Configuration — set by flags or test setup.
	Interval   time.Duration // poll cadence (default 5s)
	MaxBackoff time.Duration // error backoff ceiling (default 60s)
	BatchLimit int           // events per /events call (default 100)
	Once       bool          // single-pass mode; tail exits after one poll

	// Sleep is the function the loop calls between polls. Tests
	// replace it with a counting/cancelling version. Defaults to
	// time.Sleep.
	Sleep func(d time.Duration)

	// Stop is a channel the orchestrator closes on SIGINT/SIGTERM.
	// nil by default (created in Run if not pre-wired).
	Stop chan struct{}
}

// NewTail wraps the real joplin + blast clients with sane defaults.
// Tests construct Tail directly with fakes.
func NewTail(j *joplin.Client, s *blast.Client) *Tail {
	return &Tail{
		J:          j,
		S:          s,
		Now:        time.Now,
		Logf:       func(string, ...any) {},
		Interval:   5 * time.Second,
		MaxBackoff: 60 * time.Second,
		BatchLimit: 100,
		Sleep:      time.Sleep,
	}
}

// Run is the main entrypoint. Returns nil on graceful shutdown
// (--once completion or SIGINT/SIGTERM) and a non-nil error on a
// terminal failure (stale cursor, missing precondition).
func (t *Tail) Run() error {
	// Apply the schema first. Stage 1 sync applies it on every run; we
	// do the same here so tail picks up Stage 2's new bridge_state
	// fields (last_poll_at, tail_state, tail_pid, events_applied) on
	// a graph that bootstrapped under the older schema. Idempotent —
	// every DEFINE has IF NOT EXISTS.
	if err := t.applySchema(); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	// Precondition: bridge_state.cursor must be set. Stage 1 sync
	// guarantees this on clean completion; a null cursor means the
	// bootstrap never finished and tail has nothing to resume from.
	cursor, err := t.requireCursor()
	if err != nil {
		return err
	}
	if cursor == "" {
		return errors.New(
			"tail: bridge_state.cursor is null — run 'qai joplin bridge sync' first to bootstrap")
	}

	// Wire signal handling. The Stop channel propagates closes from
	// SIGINT/SIGTERM to the poll loop; tests pre-wire it themselves.
	if t.Stop == nil {
		t.Stop = make(chan struct{})
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigs
			close(t.Stop)
		}()
		defer signal.Stop(sigs)
	}

	// Stamp tail_state=running + tail_pid on startup so `bridge
	// status` shows the daemon is alive even before the first poll.
	if err := t.execState(stmtTailStart(os.Getpid())); err != nil {
		// Non-fatal — we can still apply events without this; log
		// and continue.
		t.Logf("warn: stamp tail_state=running failed: %v\n", err)
	}
	t.Logf("tail: cursor=%s interval=%s once=%v\n", cursor, t.Interval, t.Once)

	backoff := t.Interval
	for {
		newCursor, applied, err := t.pollAndApply(cursor)
		if err != nil {
			if isStaleCursorErr(err) {
				// Terminal — re-bootstrap is the only correct recovery.
				_ = t.execState(stmtTailError(
					"cursor stale — re-bootstrap with 'qai joplin bridge sync'"))
				return fmt.Errorf("tail: cursor %s stale: %w", cursor, err)
			}
			// Transient — record, back off, retry.
			t.Logf("tail: poll error: %v (backoff %s)\n", err, backoff)
			_ = t.execState(stmtSetError(err.Error()))
			if !t.sleepOrStop(backoff) {
				return t.shutdownClean(cursor)
			}
			backoff = nextBackoff(backoff, t.MaxBackoff)
			continue
		}

		// Success → reset backoff and update the heartbeat. THIS RUNS
		// EVEN IF applied==0 — that's the entire load-bearing point
		// of the heartbeat.
		backoff = t.Interval
		cursor = newCursor
		if err := t.execState(stmtPollSuccess(t.Now(), applied)); err != nil {
			// Heartbeat write failure is serious but not terminal —
			// the next successful poll will fix it. Log and continue.
			t.Logf("warn: heartbeat write failed: %v\n", err)
		}

		if t.Once {
			t.Logf("tail: --once complete (applied=%d, cursor=%s)\n", applied, cursor)
			return t.shutdownClean(cursor)
		}
		if !t.sleepOrStop(t.Interval) {
			return t.shutdownClean(cursor)
		}
	}
}

// pollAndApply does one /events fetch, applies each event in turn
// (apply-then-advance), and returns the new cursor + how many events
// the apply layer actually committed. An event that's skipped (e.g.
// non-note item_type, or a 404 on a note that was deleted before
// apply) advances the cursor but does NOT count toward `applied`.
func (t *Tail) pollAndApply(cursor string) (string, int, error) {
	resp, err := t.J.GetEvents(cursor, t.BatchLimit)
	if err != nil {
		return cursor, 0, err
	}
	applied := 0
	for _, ev := range resp.Items {
		res, err := ApplyEvent(t.J, t.S, ev)
		if err != nil {
			// Per-event error — surface up. The outer loop treats
			// this as transient and backs off. We DO NOT advance the
			// cursor past a failed event; the apply-then-advance
			// invariant guarantees that a re-run re-attempts this
			// exact event.
			return cursor, applied, fmt.Errorf("event %d (note %s): %w", ev.ID, ev.ItemID, err)
		}
		// Persist cursor BEFORE moving to next event so a crash mid-
		// batch leaves a re-applicable record, not a silent gap.
		if err := t.execState(stmtAdvanceCursor(fmt.Sprintf("%d", ev.ID), res.LastEventAt)); err != nil {
			return cursor, applied, fmt.Errorf("advance cursor for event %d: %w", ev.ID, err)
		}
		cursor = fmt.Sprintf("%d", ev.ID)
		if res.Applied {
			applied++
		}
	}
	// If the response carries a newer cursor (e.g., an empty batch
	// where Joplin advanced past skipped items), honour it.
	if resp.Cursor != "" {
		cursor = resp.Cursor
	}
	return cursor, applied, nil
}

// applySchema applies the embedded schema. Idempotent — every
// DEFINE has IF NOT EXISTS so this is a no-op against a graph that
// already has the Stage 1 + Stage 2 tables. The same schema file is
// applied by sync.go; running tail against a fresh DB or one that
// last saw an older schema picks up any new fields here.
func (t *Tail) applySchema() error {
	results, err := t.S.Exec(embeddedSchema)
	if err != nil {
		return err
	}
	return blast.FirstError(results)
}

// requireCursor reads bridge_state.cursor on startup. Returns ("", nil)
// when bridge_state exists but cursor is null (the precondition-failed
// case). Returns an error only on actual query failure.
func (t *Tail) requireCursor() (string, error) {
	results, err := t.S.Exec("SELECT cursor FROM " + bridgeStateID + ";")
	if err != nil {
		return "", err
	}
	if err := blast.FirstError(results); err != nil {
		return "", err
	}
	if len(results) == 0 || len(results[0].Result) == 0 {
		return "", nil
	}
	var rows []struct {
		Cursor *string `json:"cursor"`
	}
	if err := json.Unmarshal(results[0].Result, &rows); err != nil {
		return "", err
	}
	if len(rows) == 0 || rows[0].Cursor == nil {
		return "", nil
	}
	return *rows[0].Cursor, nil
}

// execState wraps the surreal Exec for short bridge_state writes.
// Shares the same error-surfacing as the apply layer.
func (t *Tail) execState(stmt string) error {
	results, err := t.S.Exec(stmt + ";")
	if err != nil {
		return err
	}
	return blast.FirstError(results)
}

// sleepOrStop returns false when t.Stop has been closed during the
// sleep — that's the signal to begin a clean shutdown. Otherwise
// returns true to continue the loop. The Sleep callback is what's
// called; tests substitute it.
func (t *Tail) sleepOrStop(d time.Duration) bool {
	// Wrap the configured Sleep in a select so the Stop channel can
	// short-circuit a long interval. Tests that pre-close Stop see
	// immediate return.
	if t.Sleep == nil {
		t.Sleep = time.Sleep
	}
	done := make(chan struct{})
	go func() {
		t.Sleep(d)
		close(done)
	}()
	select {
	case <-t.Stop:
		return false
	case <-done:
		return true
	}
}

// shutdownClean stamps tail_state=stopped + final cursor and returns
// nil so the daemon process exits 0.
func (t *Tail) shutdownClean(cursor string) error {
	if err := t.execState(stmtTailStop(cursor, t.Now())); err != nil {
		t.Logf("warn: shutdown state write failed: %v\n", err)
	}
	t.Logf("tail: shutdown (cursor=%s)\n", cursor)
	return nil
}

// ---------------------------------------------------------------------------
// bridge_state statements specific to tail
// ---------------------------------------------------------------------------

// stmtTailStart stamps the daemon-start state. Idempotent — if a prior
// tail crashed without setting state=stopped, we just overwrite.
func stmtTailStart(pid int) string {
	return fmt.Sprintf(
		"UPSERT %s SET tail_state = %s, tail_pid = %d, last_error = NONE",
		bridgeStateID, jsonQuote("running"), pid,
	)
}

// stmtTailStop stamps the clean-shutdown state. Cursor is included
// because shutdownClean may be the only persistence step that follows
// a partial poll batch.
func stmtTailStop(cursor string, now time.Time) string {
	return fmt.Sprintf(
		"UPSERT %s SET tail_state = %s, cursor = %s, last_poll_at = %s",
		bridgeStateID,
		jsonQuote("stopped"),
		jsonQuote(cursor),
		datetimeLiteral(now),
	)
}

// stmtTailError stamps the terminal-error state — used for the stale-
// cursor branch only. Transient errors flow through stmtSetError.
func stmtTailError(msg string) string {
	return fmt.Sprintf(
		"UPSERT %s SET tail_state = %s, last_error = %s",
		bridgeStateID, jsonQuote("error"), jsonQuote(msg),
	)
}

// stmtSetError records a transient last_error without changing
// tail_state — the daemon is still running, just backing off.
func stmtSetError(msg string) string {
	return fmt.Sprintf("UPSERT %s SET last_error = %s", bridgeStateID, jsonQuote(msg))
}

// stmtAdvanceCursor writes the per-event cursor advance. last_event_at
// updates iff the event has a non-zero apply timestamp — a no-op event
// (skipped type, deleted-before-apply 404) leaves data-freshness alone.
func stmtAdvanceCursor(cursor string, lastEventAt time.Time) string {
	if lastEventAt.IsZero() {
		return fmt.Sprintf("UPSERT %s SET cursor = %s",
			bridgeStateID, jsonQuote(cursor))
	}
	return fmt.Sprintf("UPSERT %s SET cursor = %s, last_event_at = %s",
		bridgeStateID, jsonQuote(cursor), datetimeLiteral(lastEventAt))
}

// stmtPollSuccess writes the heartbeat: last_poll_at + counter
// increment + clear last_error. CRITICALLY runs every successful poll
// regardless of event count — this is what Stage 4's lag-for-trust
// check reads.
func stmtPollSuccess(now time.Time, applied int) string {
	return fmt.Sprintf(
		"UPSERT %s SET last_poll_at = %s, events_applied = (events_applied ?? 0) + %d, last_error = NONE, tail_state = %s",
		bridgeStateID,
		datetimeLiteral(now),
		applied,
		jsonQuote("running"),
	)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// jsonQuote wraps a string as a SurrealQL string literal — same trick
// as patterns/blast (json.Marshal round trip).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// datetimeLiteral formats a time.Time as Surreal's <datetime>"..." form.
func datetimeLiteral(t time.Time) string {
	return fmt.Sprintf("<datetime>%q", t.UTC().Format("2006-01-02T15:04:05.000Z"))
}

// nextBackoff doubles the current backoff up to the ceiling.
func nextBackoff(current, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// ---------------------------------------------------------------------------
// cmdTail — flag parser + client wiring for `qai joplin bridge tail`
// ---------------------------------------------------------------------------

func cmdTail(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpTail)
		return
	}

	var intervalStr, maxBackoffStr, batchStr string
	args, intervalStr, _ = stripFlag(args, "--interval")
	args, maxBackoffStr, _ = stripFlag(args, "--max-backoff")
	args, batchStr, _ = stripFlag(args, "--batch")
	once := hasFlag(args, "--once")
	_ = args

	interval := 5 * time.Second
	if intervalStr != "" {
		d, err := time.ParseDuration(intervalStr)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "qai joplin bridge tail: --interval invalid: %q\n", intervalStr)
			os.Exit(2)
		}
		interval = d
	}
	maxBackoff := 60 * time.Second
	if maxBackoffStr != "" {
		d, err := time.ParseDuration(maxBackoffStr)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "qai joplin bridge tail: --max-backoff invalid: %q\n", maxBackoffStr)
			os.Exit(2)
		}
		maxBackoff = d
	}
	batch := 100
	if batchStr != "" {
		var n int
		if _, err := fmt.Sscanf(batchStr, "%d", &n); err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "qai joplin bridge tail: --batch invalid: %q\n", batchStr)
			os.Exit(2)
		}
		batch = n
	}

	// Joplin client — same auto-resolution as the other verbs.
	jToken, err := joplin.LoadDefaultToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge tail: %v\n", err)
		os.Exit(1)
	}
	jBase := os.Getenv("JOPLIN_URL")
	if jBase == "" {
		jBase = "http://127.0.0.1:41184"
	}
	jc := joplin.New(joplin.Config{BaseURL: jBase, Token: jToken})
	if err := jc.Ping(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge tail: joplin: %v\n", err)
		os.Exit(1)
	}

	// SurrealDB client — default DB → notes_graph (override only if
	// QAI_SURREAL_DB explicitly set).
	sOpts := blast.DefaultOptions()
	if os.Getenv("QAI_SURREAL_DB") == "" {
		sOpts.DB = "notes_graph"
	}
	if err := sOpts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge tail: %v\n", err)
		os.Exit(2)
	}
	sc := blast.NewClient(sOpts)

	t := NewTail(jc, sc)
	t.Interval = interval
	t.MaxBackoff = maxBackoff
	t.BatchLimit = batch
	t.Once = once
	t.Logf = func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format, args...)
	}

	if err := t.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge tail: %v\n", err)
		os.Exit(1)
	}
}

const helpTail = `qai joplin bridge tail — long-running Joplin → Surreal event consumer

USAGE
  qai joplin bridge tail                     Run as a foreground daemon
                                             (5s poll interval default).
  qai joplin bridge tail --once              Single poll-apply pass, then exit.
                                             Cron-friendly; the seam tests drive.
  qai joplin bridge tail --interval 10s      Override poll cadence.
  qai joplin bridge tail --max-backoff 2m    Override error backoff ceiling.
  qai joplin bridge tail --batch 200         Events per /events call.

BEHAVIOUR
  • Refuses to start when bridge_state.cursor is null — run sync first.
  • Polls Joplin /events?cursor=N, applies each event to the graph,
    advances the cursor AFTER each apply lands. A crash between apply
    and advance re-applies that one event on restart — safe because
    every apply is idempotent.
  • Writes bridge_state.last_poll_at on every successful poll, even
    one that returns zero events. THIS IS THE HEARTBEAT — Stage 4
    reads it as the liveness signal. last_event_at is data freshness.
  • SIGINT/SIGTERM: finish the in-flight event, persist cursor +
    last_poll_at + tail_state='stopped', exit 0.
  • Stale cursor (Joplin pruned the events window past the stored
    point): refuses to silently skip — sets tail_state='error' and
    exits non-zero with a re-bootstrap instruction.

JOPLIN /events SCOPE LIMITATION

  Joplin's /events stream (verified against the running version)
  emits item_type=1 (note) ONLY — folder renames, new empty folders,
  folder moves, tag renames, and tag deletions do NOT appear. Tail
  catches new tags lazily when a note carries them in, and lazily
  materialises a parent notebook the first time a note references one.
  But notebook renames and structural cleanups drift until the next
  full 'qai joplin bridge sync'.

  Recommended structural backstop: a nightly cron running
    qai joplin bridge sync
  (it's a no-op against an unchanged library after the first ~36s
  walk of GetNoteTags HTTP roundtrips on the user's 6.8k-note library).

CREDENTIALS
  Same env as 'qai joplin bridge sync':
    QAI_SURREAL_USER / QAI_SURREAL_PASS  (no compiled-in defaults)
    JOPLIN_TOKEN                          (or settings.json fallback)

EXAMPLES
  qai joplin bridge tail --once             # cron / smoke test
  qai joplin bridge tail --interval 10s     # gentler polling
  qai joplin bridge tail 2>tail.log &       # background, log stderr`

// isStaleCursorErr matches Joplin's reject for a cursor too old to
// service. Joplin's exact wording varies by version; we match the
// generic 4xx + "cursor" pattern so any new variant still trips this.
//
// HTTP 400/410 carrying "cursor" → stale.
//
// We deliberately do NOT match generic transient 5xx — those flow
// through the backoff path.
func isStaleCursorErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "cursor") {
		return false
	}
	return strings.Contains(msg, "http 400") ||
		strings.Contains(msg, "http 410") ||
		strings.Contains(msg, "stale") ||
		strings.Contains(msg, "invalid") ||
		strings.Contains(msg, "rejected")
}
