package joplinbridge

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
)

// status is the read-side of bridge_state. Prints liveness lag
// (now - last_poll_at, the heartbeat from tail) distinctly from data
// age (now - last_event_at, the freshness signal). This distinction
// is the Stage 4 precondition — agent memory queries refuse on stale
// liveness, not stale data.
//
// Output goes to stdout (the human is reading it interactively); only
// machine-readable composition (Stage 3's --json) needs the stricter
// stderr/stdout discipline tail enforces.

type statusRow struct {
	Cursor             *string  `json:"cursor"`
	LastEventAt        *string  `json:"last_event_at"`
	LastSyncCompleted  *string  `json:"last_sync_completed"`
	LastPollAt         *string  `json:"last_poll_at"`
	LastError          *string  `json:"last_error"`
	TailState          *string  `json:"tail_state"`
	TailPID            *int     `json:"tail_pid"`
	EventsApplied      *int     `json:"events_applied"`
	BootstrapProgress  any      `json:"bootstrap_progress"`
}

func cmdStatus(args []string) {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(helpStatus)
		return
	}

	sOpts := blast.DefaultOptions()
	if os.Getenv("QAI_SURREAL_DB") == "" {
		sOpts.DB = "notes_graph"
	}
	if err := sOpts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge status: %v\n", err)
		os.Exit(2)
	}
	sc := blast.NewClient(sOpts)

	if err := printStatus(os.Stdout, sc, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin bridge status: %v\n", err)
		os.Exit(1)
	}
}

// printStatus is the testable seam. Pure-ish: reads bridge_state via
// the injected surrealAPI, formats to the writer using the injected
// "now" so tests can pin the wall clock.
func printStatus(w io.Writer, s surrealAPI, now time.Time) error {
	state, err := readStatus(s)
	if err != nil {
		return err
	}
	tagCount, hasTagCount, _ := readTagCounts(s)

	stateStr := "(never run)"
	if state.TailState != nil {
		stateStr = *state.TailState
	}
	fmt.Fprintf(w, "state:               %s", stateStr)
	if state.TailPID != nil {
		fmt.Fprintf(w, "   (pid %d)", *state.TailPID)
	}
	fmt.Fprintln(w)

	// last_poll_at + liveness lag — the Stage 4 precondition signal.
	fmt.Fprintf(w, "last_poll_at:        %s   (lag = %s)\n",
		formatOpt(state.LastPollAt), formatLag(state.LastPollAt, now))

	// last_event_at + data age — informational, NOT the liveness check.
	fmt.Fprintf(w, "last_event_at:       %s   (data age = %s)\n",
		formatOpt(state.LastEventAt), formatLag(state.LastEventAt, now))

	fmt.Fprintf(w, "last_sync_completed: %s\n", formatOpt(state.LastSyncCompleted))
	fmt.Fprintf(w, "cursor:              %s\n", formatOptString(state.Cursor))
	fmt.Fprintf(w, "events_applied:      %s\n", formatOptInt(state.EventsApplied))

	// tags / has_tag — cold-start signal flagged in the Stage 1
	// production run. Two near-zero numbers means tag-based queries
	// won't surface much; Stage 4 should lean on contains + links_to
	// (when Stage 5 ships) until tags accumulate via qai note --tag.
	fmt.Fprintf(w, "tags / has_tag:      %d / %d\n", tagCount, hasTagCount)

	fmt.Fprintf(w, "last_error:          %s\n", formatOptString(state.LastError))

	// Bootstrap-in-flight hint — useful if the user kills sync midway
	// and forgets a resume run is needed.
	if state.BootstrapProgress != nil {
		fmt.Fprintln(w, "bootstrap:           in-flight (run 'qai joplin bridge sync' to resume)")
	}
	return nil
}

func readStatus(s surrealAPI) (*statusRow, error) {
	results, err := s.Exec("SELECT * FROM " + bridgeStateID + ";")
	if err != nil {
		return nil, err
	}
	if err := blast.FirstError(results); err != nil {
		return nil, err
	}
	if len(results) == 0 || len(results[0].Result) == 0 {
		return &statusRow{}, nil
	}
	var rows []statusRow
	if err := json.Unmarshal(results[0].Result, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &statusRow{}, nil
	}
	return &rows[0], nil
}

// readTagCounts fires two cheap COUNTs for the cold-start signal row.
// Errors are non-fatal — we surface zeros rather than blowing up the
// whole status print.
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

// formatOpt renders a *string datetime as "value" or "(never)".
func formatOpt(p *string) string {
	if p == nil || *p == "" {
		return "(never)"
	}
	return *p
}

func formatOptString(p *string) string {
	if p == nil || *p == "" {
		return "(none)"
	}
	return *p
}

func formatOptInt(p *int) string {
	if p == nil {
		return "0"
	}
	return fmt.Sprintf("%d", *p)
}

// formatLag computes now - timestamp and renders a human-friendly
// duration ("4s", "33m", "2h12m", "1d3h"). Returns "(unknown)" on a
// nil/unparseable input.
func formatLag(p *string, now time.Time) string {
	if p == nil || *p == "" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339Nano, *p)
	if err != nil {
		// Try the truncated form Surreal sometimes returns.
		t, err = time.Parse(time.RFC3339, *p)
		if err != nil {
			return "unknown"
		}
	}
	return formatDuration(now.Sub(t))
}

// formatDuration trims time.Duration output to the resolution that
// matters here. Sub-second values round to seconds (1s minimum).
func formatDuration(d time.Duration) string {
	if d < 0 {
		// Clock skew between now and last_poll_at — should be tiny.
		// Render as "0s" rather than confusing negative output.
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
  qai joplin bridge status              Print the current state.

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

  tags / has_tag       Cold-start signal. Until tags accumulate via
                       'qai note --tag' and manual tagging, the useful
                       query surface is the notebook hierarchy
                       (contains edges), not has_tag.

  last_error           Most recent transient error, or '(none)'. Tail
                       sets this on backoff and clears it on the next
                       successful poll.`
