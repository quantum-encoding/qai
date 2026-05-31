package joplinbridge

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// status is the read-side of bridge_state. Built on top of Health()
// — the same classifier the Stage 4 read verb will call — so the
// human output, --json, and --check can never drift apart on their
// interpretation of the bridge_state row.
//
// Two distinct timestamps surface to the user:
//   - last_poll_at   → liveness (heartbeat from tail, what Stage 4
//                      checks against MaxLag)
//   - last_event_at  → data freshness (informational only)
//
// A quiet night with no note changes legitimately shows hours of
// data age while the system is perfectly healthy.

// Exit codes for --check. Documented in the spec; constants here so
// the switch is grep-able and any future contributor can confirm the
// codes haven't drifted.
const (
	exitHealthy    = 0
	exitUnhealthy  = 1
	exitBootstrap  = 2
	exitInvocation = 3
)

func cmdStatus(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpStatus)
		return
	}

	var maxLagStr, watchStr string
	args, maxLagStr, _ = stripFlag(args, "--max-lag")
	args, watchStr, _ = stripFlag(args, "--watch")
	jsonMode := hasFlag(args, "--json")
	checkMode := hasFlag(args, "--check")
	allowBootstrap := hasFlag(args, "--allow-bootstrap")
	watchMode := hasFlag(args, "--watch") || watchStr != ""
	_ = args

	t := Thresholds{AllowBootstrap: allowBootstrap}
	if maxLagStr != "" {
		d, err := time.ParseDuration(maxLagStr)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "qai joplin bridge status: --max-lag invalid: %q\n", maxLagStr)
			os.Exit(exitInvocation)
		}
		t.MaxLag = d
	}
	// --watch interval. The flag form is `--watch 10s`; bare `--watch`
	// defaults to 5s. Parse first so an invalid value short-circuits
	// before we even build the Surreal client.
	watchInterval := 5 * time.Second
	if watchStr != "" {
		d, err := time.ParseDuration(watchStr)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "qai joplin bridge status: --watch interval invalid: %q\n", watchStr)
			os.Exit(exitInvocation)
		}
		watchInterval = d
	}

	// Mutually-exclusive sanity check. Mixing --check / --json /
	// --watch in one invocation would be ambiguous; refuse early.
	modes := 0
	for _, b := range []bool{jsonMode, checkMode, watchMode} {
		if b {
			modes++
		}
	}
	if modes > 1 {
		fmt.Fprintln(os.Stderr,
			"qai joplin bridge status: --json / --check / --watch are mutually exclusive")
		os.Exit(exitInvocation)
	}

	sOpts := blast.DefaultOptions()
	if os.Getenv("QAI_SURREAL_DB") == "" {
		sOpts.DB = "notes_graph"
	}
	if err := sOpts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge status: %v\n", err)
		os.Exit(exitInvocation)
	}
	sc := blast.NewClient(sOpts)

	// Joplin client is optional — backlog probe soft-fails when nil.
	// We try to load it; on failure (no token / Joplin not running)
	// we proceed with j=nil rather than refusing the whole command.
	jc := tryLoadJoplin()

	switch {
	case checkMode:
		runCheck(sc, jc, t)
	case jsonMode:
		if err := runJSON(os.Stdout, sc, jc, time.Now(), t); err != nil {
			fmt.Fprintf(os.Stderr, "qai joplin bridge status: %v\n", err)
			os.Exit(exitInvocation)
		}
	case watchMode:
		runWatch(os.Stdout, sc, jc, t, watchInterval)
	default:
		if err := printStatus(os.Stdout, sc, jc, time.Now(), t); err != nil {
			fmt.Fprintf(os.Stderr, "qai joplin bridge status: %v\n", err)
			os.Exit(exitInvocation)
		}
	}
}

// tryLoadJoplin attempts to wire a Joplin client; returns nil on any
// failure so the backlog probe degrades to BacklogUnknown rather than
// blocking the whole status command. Status is read-only — there's no
// reason to refuse it just because the user hasn't exported JOPLIN_TOKEN.
func tryLoadJoplin() joplinAPI {
	token, err := joplin.LoadDefaultToken()
	if err != nil {
		return nil
	}
	base := os.Getenv("JOPLIN_URL")
	if base == "" {
		base = "http://127.0.0.1:41184"
	}
	c := joplin.New(joplin.Config{BaseURL: base, Token: token})
	if err := c.Ping(); err != nil {
		return nil
	}
	return c
}

// printStatus is the human-readable path. Same testable seam shape
// as Stage 2: takes io.Writer + interfaces + clock so tests can pin
// every variable. Built on Health() so the displayed status matches
// what --json / --check / Stage 4 would compute against the same row.
func printStatus(w io.Writer, s surrealAPI, j joplinAPI, now time.Time, t Thresholds) error {
	hc, err := Health(s, j, now, t)
	if err != nil {
		return err
	}
	renderHuman(w, hc, now)
	return nil
}

// renderHuman writes the Stage 2 baseline + Stage 3 additions (backlog
// row, degraded warning line) to the writer. Pure: no I/O beyond w.
func renderHuman(w io.Writer, hc HealthCheck, now time.Time) {
	state := hc.State
	if state == nil {
		state = &StateSnapshot{}
	}

	stateStr := "(never run)"
	if state.TailState != nil {
		stateStr = *state.TailState
	}
	fmt.Fprintf(w, "state:               %s", stateStr)
	if state.TailPID != nil {
		fmt.Fprintf(w, "   (pid %d)", *state.TailPID)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "last_poll_at:        %s   (lag = %s)\n",
		formatTimePtr(state.LastPollAt), formatDurationPtr(state.LastPollAt, now))

	fmt.Fprintf(w, "last_event_at:       %s   (data age = %s)\n",
		formatTimePtr(state.LastEventAt), formatDurationPtr(state.LastEventAt, now))

	fmt.Fprintf(w, "last_sync_completed: %s\n", formatTimePtr(state.LastSyncCompleted))
	fmt.Fprintf(w, "cursor:              %s\n", formatStrPtr(state.Cursor))
	fmt.Fprintf(w, "events_applied:      %s\n", formatIntPtr(state.EventsApplied))

	fmt.Fprintf(w, "backlog:             %s\n", formatBacklog(hc))

	fmt.Fprintf(w, "tags / has_tag:      %d / %d\n", hc.TagCount, hc.HasTagCount)

	fmt.Fprintf(w, "last_error:          %s\n", formatStrPtr(state.LastError))

	if state.BootstrapInFlight {
		fmt.Fprintln(w, "bootstrap:           in-flight (run 'qai joplin bridge sync' to resume)")
	}

	// Degraded → human form of the OK/Reason contract. Anyone reading
	// the default output sees the warning, not just programmatic
	// consumers checking the JSON Reason field.
	if hc.Status == HealthDegraded && hc.Reason != "" {
		fmt.Fprintf(w, "warning:             %s\n", hc.Reason)
	}
}

// formatBacklog renders the backlog row per the spec. Three branches:
// soft-failed probe, caught up, behind by N events.
func formatBacklog(hc HealthCheck) string {
	if hc.BacklogState == BacklogUnknown {
		return "unknown (Joplin unreachable)"
	}
	if hc.Backlog == 0 {
		return "0 events behind (caught up)"
	}
	// Surface Joplin's head cursor when we know it — derivable as
	// stored + delta. Spec example: "3 events behind (Joplin at cursor 8661)".
	head := hc.Backlog
	if hc.State != nil && hc.State.Cursor != nil {
		if stored, err := parseInt64(*hc.State.Cursor); err == nil {
			head = stored + hc.Backlog
		}
	}
	return fmt.Sprintf("%d events behind (Joplin at cursor %d)", hc.Backlog, head)
}

// runJSON pretty-prints the HealthCheck with the same two-space
// indent emitJSON in joplinops uses. Single object per invocation;
// schema_version is the version key.
func runJSON(w io.Writer, s surrealAPI, j joplinAPI, now time.Time, t Thresholds) error {
	hc, err := Health(s, j, now, t)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(hc)
}

// runCheck is the pure exit-code health probe — silent on healthy and
// degraded, one stderr line otherwise. Maps HealthStatus → exit code
// per the table in the spec.
//
// Note the degraded branch: exit 0 (servable right now). The fact
// that staleness is climbing reaches the operator via --json's Reason
// or the default output's warning line — not via the exit code. Same
// decision as the Health API's OK=true on degraded; recorded once,
// applied here.
func runCheck(s surrealAPI, j joplinAPI, t Thresholds) {
	hc, err := Health(s, j, time.Now(), t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge status --check: %v\n", err)
		os.Exit(exitInvocation)
	}
	switch hc.Status {
	case HealthHealthy, HealthDegraded:
		os.Exit(exitHealthy)
	case HealthBootstrap:
		fmt.Fprintf(os.Stderr, "unhealthy: %s — %s\n", hc.Status, hc.Reason)
		os.Exit(exitBootstrap)
	default:
		fmt.Fprintf(os.Stderr, "unhealthy: %s — %s\n", hc.Status, hc.Reason)
		os.Exit(exitUnhealthy)
	}
}

// runWatch clears the screen and redraws once per interval until
// SIGINT. ANSI escape codes are inline string constants — no curses
// dependency. The inner per-tick work is renderWatchTick, kept
// separate so tests can drive a single tick without standing up the
// loop or the signal handler.
func runWatch(w io.Writer, s surrealAPI, j joplinAPI, t Thresholds, interval time.Duration) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	renderWatchTick(w, s, j, time.Now(), t)
	for {
		select {
		case <-sigs:
			fmt.Fprintln(w)
			return
		case <-ticker.C:
			renderWatchTick(w, s, j, time.Now(), t)
		}
	}
}

// renderWatchTick is one redraw — clear screen + reposition cursor +
// timestamp header + status body. ANSI sequences are \033[2J (clear
// screen) and \033[H (home cursor). Errors in Health() surface as a
// single line in the watch view rather than crashing the loop.
func renderWatchTick(w io.Writer, s surrealAPI, j joplinAPI, now time.Time, t Thresholds) {
	fmt.Fprint(w, "\033[2J\033[H")
	fmt.Fprintf(w, "qai joplin bridge status — %s\n", now.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "─────────────────────────────────────────────────")
	hc, err := Health(s, j, now, t)
	if err != nil {
		fmt.Fprintf(w, "error: %v\n", err)
		return
	}
	renderHuman(w, hc, now)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parseInt64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// readTagCounts fires two cheap COUNTs for the cold-start signal row.
// Errors are non-fatal — surface zeros rather than failing Health().
// Same shape as the Stage 2 version; just moved here so health.go can
// import it without status.go importing health.go (one direction).
func readTagCounts(s surrealAPI) (int, int, error) {
	sql := "SELECT count() FROM tag GROUP ALL; SELECT count() FROM has_tag GROUP ALL;"
	results, err := s.Exec(sql)
	if err != nil {
		return 0, 0, err
	}
	if err := blast.FirstError(results); err != nil {
		return 0, 0, err
	}
	parse := func(raw json.RawMessage) int {
		if len(raw) == 0 {
			return 0
		}
		var rows []struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return 0
		}
		if len(rows) == 0 {
			return 0
		}
		return rows[0].Count
	}
	if len(results) < 2 {
		return 0, 0, nil
	}
	return parse(results[0].Result), parse(results[1].Result), nil
}

// formatTimePtr renders a *time.Time as the canonical RFC3339Nano
// form, or "(never)" on nil. Matches Stage 2's formatOpt output.
func formatTimePtr(p *time.Time) string {
	if p == nil {
		return "(never)"
	}
	return p.UTC().Format(time.RFC3339Nano)
}

func formatStrPtr(p *string) string {
	if p == nil || *p == "" {
		return "(none)"
	}
	return *p
}

func formatIntPtr(p *int) string {
	if p == nil {
		return "0"
	}
	return fmt.Sprintf("%d", *p)
}

// formatDurationPtr is the lag/data-age formatter. Stage 2's formatLag
// took *string and parsed inline; this version trusts the parsed
// snapshot — one parse per read, not three.
func formatDurationPtr(p *time.Time, now time.Time) string {
	if p == nil {
		return "unknown"
	}
	return formatDuration(max(time.Duration(0), now.Sub(*p)))
}

// formatDuration trims time.Duration output to the resolution that
// matters here. Sub-second values round to seconds (1s minimum).
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

const helpStatus = `qai joplin bridge status — Joplin → Surreal bridge health

USAGE
  qai joplin bridge status                Human-readable (default).
  qai joplin bridge status --json         HealthCheck struct as JSON.
  qai joplin bridge status --watch [Ns]   Refresh in place every N (default 5s).
  qai joplin bridge status --check        Exit code only — cron / monit / launchd.

MODIFIERS
  --max-lag <dur>      Override the freshness threshold (default 10m).
  --allow-bootstrap    Treat a bootstrap-in-flight as OK (operator mode).

OUTPUT (read this carefully — two distinct timestamps)

  state                The tail daemon's self-reported state:
                       'running' / 'stopped' / 'error' / '(never run)'.

  last_poll_at         LIVENESS heartbeat — updated every successful
                       poll regardless of whether new events came back.
                       The 'lag = Ns' is now - last_poll_at and is what
                       Stage 4 (graph context) reads to decide whether
                       the graph is fresh enough to serve.

  last_event_at        DATA FRESHNESS — the most recent applied event's
                       timestamp. The 'data age = N' is now - this,
                       and is purely informational. A quiet night with
                       no note changes legitimately shows hours of
                       data age while the system is perfectly healthy.

  backlog              How many events behind Joplin's current head.
                       'unknown' when Joplin is unreachable — backlog
                       is nice-to-have, not load-bearing.

  tags / has_tag       Cold-start signal. Until tags accumulate via
                       'qai note --tag' and manual tagging, the useful
                       query surface is the notebook hierarchy
                       (contains edges), not has_tag.

  last_error           Most recent transient error, or '(none)'. Tail
                       sets this on backoff and clears it on the next
                       successful poll.

  warning              Shown only when status is 'degraded': the data
                       is still fresh but the tail daemon has stopped,
                       so staleness is climbing.

--json SCHEMA (schema_version = 1)

  {
    "ok":            bool,     true iff servable right now.
    "status":        string,   healthy|degraded|stale|error|bootstrap|no-tail|never-run
    "reason":        string,   "" iff status==healthy; populated otherwise.
                               NB: ok==true with non-empty reason ("degraded")
                               means the caller MUST surface it (warning).
    "schema_version": int,     bump on field rename / removal.
    "state":         object,   parsed bridge_state row (see status default
                               output for field meanings).
    "lag_ns":        int,      now - last_poll_at (nanoseconds).
    "data_age_ns":   int,      now - last_event_at (nanoseconds).
    "backlog":       int,      events behind Joplin's head.
    "backlog_state": string,   ok | unknown.
    "tag_count":     int,      cold-start signal — tag rows.
    "has_tag_count": int,      cold-start signal — has_tag edges.
    "ambiguous_links": int,    Stage 5 hook; always 0 until then.
  }

--check EXIT CODES

  0  healthy OR degraded — data is servable right now. Degraded prints
                           nothing; the climbing-staleness fact reaches
                           callers via --json's reason field, not via
                           the exit code.
  1  stale | error | no-tail | never-run — operator action needed.
  2  bootstrap — sync in flight; retry later rather than alert.
  3  invocation error (bad flags / Surreal unreachable / etc.)

EXAMPLES
  qai joplin bridge status                       # default human view
  qai joplin bridge status --json | jq .         # machine-readable
  qai joplin bridge status --check && echo ok    # cron-friendly probe
  qai joplin bridge status --watch 2s            # live operator view
  qai joplin bridge status --max-lag 30m         # looser threshold`

