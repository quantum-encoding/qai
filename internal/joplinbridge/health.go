package joplinbridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
)

// Health is Stage 3's load-bearing piece. Stage 4's agent-memory read
// verb calls this to decide whether to serve or refuse based on the
// bridge's freshness signals.
//
// Read the OK/Reason contract carefully — `OK==true` is necessary but
// NOT sufficient. A caller that only inspects OK and ignores Reason is
// buggy: HealthDegraded carries OK=true WITH a populated Reason that
// the caller MUST surface (warning banner / stderr line / log entry).
// See the Stage 3 spec for the full classification table.

// DefaultMaxLag is the freshness ceiling for tail's liveness signal
// (now - last_poll_at). Established by the Stage 2 spec and carried
// forward as an exported constant so CLI flags and Stage 4 callers
// both pick it up.
const DefaultMaxLag = 600 * time.Second

// schemaVersion is the stable version stamped on every HealthCheck.
// Bump on any field rename or removal. JSON-consuming tools key off
// this to refuse a newer payload they don't understand.
const schemaVersion = 1

// HealthStatus is the closed classification axis. New statuses bump
// schemaVersion.
type HealthStatus string

const (
	HealthHealthy   HealthStatus = "healthy"   // OK=true, Reason==""
	HealthDegraded  HealthStatus = "degraded"  // OK=true, Reason populated — see contract
	HealthStale     HealthStatus = "stale"     // lag > MaxLag, OK=false
	HealthError     HealthStatus = "error"     // tail_state == 'error', OK=false
	HealthBootstrap HealthStatus = "bootstrap" // sync in flight, OK=false (unless AllowBootstrap)
	HealthNoTail    HealthStatus = "no-tail"   // cursor set, last_poll_at null, OK=false
	HealthNeverRun  HealthStatus = "never-run" // no bridge_state / null cursor, OK=false
)

// BacklogState distinguishes a measured-zero backlog from a soft-failed
// probe (Joplin unreachable / parse error). Stage 4 callers should NOT
// refuse on BacklogUnknown alone; it doesn't mean the graph is stale,
// only that we couldn't compare it to Joplin's head.
type BacklogState string

const (
	BacklogOK      BacklogState = "ok"
	BacklogUnknown BacklogState = "unknown"
)

// Thresholds tunes Health's classifier. Zero MaxLag means "use
// DefaultMaxLag"; pass an explicit non-zero value to override.
type Thresholds struct {
	MaxLag         time.Duration
	AllowBootstrap bool
}

// StateSnapshot is the parsed bridge_state row — all datetimes already
// time.Parsed, so callers don't re-implement that. The pointer fields
// preserve null distinction: nil means "never written", non-nil means
// "stored and parsed cleanly".
type StateSnapshot struct {
	Cursor            *string    `json:"cursor"`
	LastPollAt        *time.Time `json:"last_poll_at"`
	LastEventAt       *time.Time `json:"last_event_at"`
	LastSyncCompleted *time.Time `json:"last_sync_completed"`
	TailState         *string    `json:"tail_state"`
	TailPID           *int       `json:"tail_pid"`
	EventsApplied     *int       `json:"events_applied"`
	LastError         *string    `json:"last_error"`
	BootstrapInFlight bool       `json:"bootstrap_in_flight"`
}

// HealthCheck is the structured result. JSON tags are part of the
// stable schema --json emits; rename = breaking change (bump
// schemaVersion).
type HealthCheck struct {
	OK            bool         `json:"ok"`
	Status        HealthStatus `json:"status"`
	Reason        string       `json:"reason,omitempty"`
	SchemaVersion int          `json:"schema_version"`

	State *StateSnapshot `json:"state,omitempty"`

	// Lag is now - last_poll_at (liveness); DataAge is now -
	// last_event_at (data freshness). Zero when the source timestamp
	// is null. Stage 4 reads Lag; DataAge is informational only.
	Lag     time.Duration `json:"lag_ns"`
	DataAge time.Duration `json:"data_age_ns"`

	Backlog      int64        `json:"backlog,omitempty"`
	BacklogState BacklogState `json:"backlog_state"`

	// Cold-start signal — see Stage 2 production run + Stage 3 spec's
	// carryover section. NOT factored into OK-ness; a freshly-
	// installed bridge with 0 tags is healthy if tail is alive.
	TagCount       int `json:"tag_count"`
	HasTagCount    int `json:"has_tag_count"`
	AmbiguousLinks int `json:"ambiguous_links"` // Stage 5 hook; always 0 until then
}

// Health is the entrypoint. Pure-ish: side effects are SELECTs against
// Surreal and one /events?limit=1 probe against Joplin for backlog.
// Passing j==nil reports BacklogUnknown without failing — useful for
// callers that don't have a Joplin client wired (e.g., --check on a
// cron without JOPLIN_TOKEN exported).
func Health(s surrealAPI, j joplinAPI, now time.Time, t Thresholds) (HealthCheck, error) {
	if t.MaxLag <= 0 {
		t.MaxLag = DefaultMaxLag
	}
	snap, err := readSnapshot(s)
	if err != nil {
		return HealthCheck{SchemaVersion: schemaVersion}, fmt.Errorf("read bridge_state: %w", err)
	}
	tagCount, hasTagCount, _ := readTagCounts(s)

	status, ok, reason := classify(snap, now, t)
	hc := HealthCheck{
		OK:             ok,
		Status:         status,
		Reason:         reason,
		SchemaVersion:  schemaVersion,
		State:          snap,
		TagCount:       tagCount,
		HasTagCount:    hasTagCount,
		AmbiguousLinks: 0,
	}
	if snap != nil && snap.LastPollAt != nil {
		hc.Lag = max(0, now.Sub(*snap.LastPollAt))
	}
	if snap != nil && snap.LastEventAt != nil {
		hc.DataAge = max(0, now.Sub(*snap.LastEventAt))
	}
	hc.Backlog, hc.BacklogState = probeBacklog(j, snap)
	return hc, nil
}

// classify is the rule table — first match wins. Pure function over
// the parsed snapshot + thresholds. Kept separate from Health() so
// tests can drive every branch with hand-rolled snapshots without
// standing up a surrealAPI fake.
//
// MaxLag <= 0 is treated as DefaultMaxLag, so the zero-value
// Thresholds{} is meaningful (and tests / Stage 4 callers don't have
// to remember to set it).
func classify(snap *StateSnapshot, now time.Time, t Thresholds) (HealthStatus, bool, string) {
	if t.MaxLag <= 0 {
		t.MaxLag = DefaultMaxLag
	}
	// Rule 1: no row OR null cursor → never run.
	if snap == nil || snap.Cursor == nil || *snap.Cursor == "" {
		return HealthNeverRun, false,
			"no cursor — run 'qai joplin bridge sync' first to bootstrap"
	}
	// Rule 2: tail recorded a terminal error.
	if snap.TailState != nil && *snap.TailState == "error" {
		reason := "tail reported an error"
		if snap.LastError != nil && *snap.LastError != "" {
			reason = *snap.LastError
		}
		return HealthError, false, reason
	}
	// Rule 3: sync in flight, caller didn't opt in.
	if snap.BootstrapInFlight && !t.AllowBootstrap {
		return HealthBootstrap, false,
			"bootstrap in flight — re-run 'qai joplin bridge sync' to resume"
	}
	// Rule 4: cursor set but tail never ran (synced-but-no-tail) — the
	// distinct state spec review flagged. Don't tell the user to
	// re-sync a populated graph; tell them to start the daemon.
	if snap.LastPollAt == nil {
		return HealthNoTail, false,
			"synced but tail daemon never started — run 'qai joplin bridge tail'"
	}
	// Rule 5: lag exceeds the freshness ceiling.
	lag := max(time.Duration(0), now.Sub(*snap.LastPollAt))
	if lag > t.MaxLag {
		return HealthStale, false,
			fmt.Sprintf("liveness lag %s exceeds %s", formatDuration(lag), formatDuration(t.MaxLag))
	}
	// Rule 6: daemon stopped but data still fresh. OK=true WITH a
	// populated Reason — the contract Stage 4 must surface. See the
	// spec's "THE CONTRACT" section.
	if snap.TailState != nil && *snap.TailState == "stopped" {
		return HealthDegraded, true,
			"tail daemon stopped — data fresh now but staleness is climbing"
	}
	// Rule 7: nothing tripped — fully healthy.
	return HealthHealthy, true, ""
}

// probeBacklog hits Joplin /events?limit=1 to read the current head
// cursor, then computes head - stored. Both cursors are numeric event
// IDs serialized as strings. Soft-fails to BacklogUnknown when Joplin
// is unreachable, when j is nil, or when either cursor isn't parseable
// — the backlog signal is nice-to-have, not load-bearing.
func probeBacklog(j joplinAPI, snap *StateSnapshot) (int64, BacklogState) {
	if j == nil || snap == nil || snap.Cursor == nil || *snap.Cursor == "" {
		return 0, BacklogUnknown
	}
	stored, err := strconv.ParseInt(*snap.Cursor, 10, 64)
	if err != nil {
		return 0, BacklogUnknown
	}
	resp, err := j.GetEvents("", 1)
	if err != nil || resp == nil {
		return 0, BacklogUnknown
	}
	// Prefer the explicit Cursor field; fall back to the highest event
	// ID we saw in items (Joplin's head pointer on a limit=1 call).
	headStr := resp.Cursor
	if headStr == "" && len(resp.Items) > 0 {
		headStr = fmt.Sprintf("%d", resp.Items[0].ID)
	}
	if headStr == "" {
		return 0, BacklogUnknown
	}
	head, err := strconv.ParseInt(headStr, 10, 64)
	if err != nil {
		return 0, BacklogUnknown
	}
	return max(int64(0), head-stored), BacklogOK
}

// readSnapshot promotes the date-parse out of formatLag into a single
// read layer. Stage 2's printStatus parsed timestamps inline in every
// format call; Stage 3 parses once during decode so Health() can
// classify without redoing the work.
func readSnapshot(s surrealAPI) (*StateSnapshot, error) {
	results, err := s.Exec("SELECT * FROM " + bridgeStateID + ";")
	if err != nil {
		return nil, err
	}
	if err := blast.FirstError(results); err != nil {
		return nil, err
	}
	if len(results) == 0 || len(results[0].Result) == 0 {
		return nil, nil
	}
	var rows []rawStateRow
	if err := json.Unmarshal(results[0].Result, &rows); err != nil {
		return nil, fmt.Errorf("decode bridge_state: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0].toSnapshot(), nil
}

// rawStateRow is the on-the-wire shape. Decoded once, then converted
// into a StateSnapshot whose datetimes are parsed. Keeping this
// internal means callers never see the *string-of-RFC3339 form.
type rawStateRow struct {
	Cursor            *string `json:"cursor"`
	LastEventAt       *string `json:"last_event_at"`
	LastSyncCompleted *string `json:"last_sync_completed"`
	LastPollAt        *string `json:"last_poll_at"`
	LastError         *string `json:"last_error"`
	TailState         *string `json:"tail_state"`
	TailPID           *int    `json:"tail_pid"`
	EventsApplied     *int    `json:"events_applied"`
	BootstrapProgress any     `json:"bootstrap_progress"`
}

func (r rawStateRow) toSnapshot() *StateSnapshot {
	return &StateSnapshot{
		Cursor:            r.Cursor,
		LastPollAt:        parseSurrealTime(r.LastPollAt),
		LastEventAt:       parseSurrealTime(r.LastEventAt),
		LastSyncCompleted: parseSurrealTime(r.LastSyncCompleted),
		TailState:         r.TailState,
		TailPID:           r.TailPID,
		EventsApplied:     r.EventsApplied,
		LastError:         r.LastError,
		BootstrapInFlight: r.BootstrapProgress != nil,
	}
}

// parseSurrealTime tolerates both RFC3339Nano and the truncated RFC3339
// form Surreal returns under some serialisers. Returns nil on nil/empty
// input rather than zero-valued Time, so callers can distinguish
// "never set" from "Unix epoch" cleanly.
func parseSurrealTime(p *string) *time.Time {
	if p == nil || *p == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, *p); err == nil {
		return &t
	}
	if t, err := time.Parse(time.RFC3339, *p); err == nil {
		return &t
	}
	return nil
}

