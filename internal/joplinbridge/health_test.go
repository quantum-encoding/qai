package joplinbridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// ─── classifier table — acceptance criterion 1 + 2 ─────────────────────

// TestHealthClassifierEveryStatusReachable exercises every HealthStatus
// constant with a fixture that drives that branch. The rule table is
// load-bearing for Stage 4; if a future change skips a row, this test
// fails by missing-coverage.
func TestHealthClassifierEveryStatusReachable(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	tail5sAgo := now.Add(-5 * time.Second)
	tailHrAgo := now.Add(-1 * time.Hour)

	cases := []struct {
		name   string
		snap   *StateSnapshot
		thresh Thresholds
		want   HealthStatus
		ok     bool
	}{
		{
			name: "never_run_nil_snapshot",
			snap: nil, want: HealthNeverRun, ok: false,
		},
		{
			name: "never_run_null_cursor",
			snap: &StateSnapshot{},
			want: HealthNeverRun, ok: false,
		},
		{
			name: "error_state_takes_priority_over_lag",
			snap: &StateSnapshot{
				Cursor:     ptrStr("100"),
				TailState:  ptrStr("error"),
				LastError:  ptrStr("joplin GET: HTTP 400: cursor invalid"),
				LastPollAt: &tailHrAgo,
			},
			want: HealthError, ok: false,
		},
		{
			name: "bootstrap_in_flight_refuses",
			snap: &StateSnapshot{
				Cursor:            ptrStr("100"),
				BootstrapInFlight: true,
				LastPollAt:        &tail5sAgo,
			},
			want: HealthBootstrap, ok: false,
		},
		{
			name: "bootstrap_in_flight_allowed_falls_through",
			snap: &StateSnapshot{
				Cursor:            ptrStr("100"),
				BootstrapInFlight: true,
				LastPollAt:        &tail5sAgo,
				TailState:         ptrStr("running"),
			},
			thresh: Thresholds{AllowBootstrap: true},
			want:   HealthHealthy, ok: true,
		},
		{
			name: "no_tail_synced_but_never_polled",
			snap: &StateSnapshot{
				Cursor: ptrStr("100"),
				// LastPollAt nil
			},
			want: HealthNoTail, ok: false,
		},
		{
			name: "stale_lag_over_max",
			snap: &StateSnapshot{
				Cursor:     ptrStr("100"),
				LastPollAt: &tailHrAgo,
				TailState:  ptrStr("running"),
			},
			want: HealthStale, ok: false,
		},
		{
			name: "degraded_stopped_but_fresh",
			snap: &StateSnapshot{
				Cursor:     ptrStr("100"),
				LastPollAt: &tail5sAgo,
				TailState:  ptrStr("stopped"),
			},
			want: HealthDegraded, ok: true,
		},
		{
			name: "healthy_running_and_fresh",
			snap: &StateSnapshot{
				Cursor:     ptrStr("100"),
				LastPollAt: &tail5sAgo,
				TailState:  ptrStr("running"),
			},
			want: HealthHealthy, ok: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, reason := classify(c.snap, now, c.thresh)
			if got != c.want {
				t.Errorf("status: got %q want %q (reason=%q)", got, c.want, reason)
			}
			if ok != c.ok {
				t.Errorf("ok: got %v want %v (reason=%q)", ok, c.ok, reason)
			}
		})
	}
}

// TestHealthContractInvariant — acceptance criterion 2.
// THE contract: Reason == "" iff Status == HealthHealthy. Test by
// driving every status branch and asserting the invariant. If a future
// edit weakens this, Stage 4's "is OK enough" logic breaks silently.
func TestHealthContractInvariant(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	thr := Thresholds{}
	// One snapshot per status; reuse Rule 6's "stopped + fresh" /
	// Rule 7's "running + fresh" / etc.
	scenarios := map[HealthStatus]*StateSnapshot{
		HealthNeverRun:  nil,
		HealthError:     {Cursor: ptrStr("1"), TailState: ptrStr("error"), LastError: ptrStr("x")},
		HealthBootstrap: {Cursor: ptrStr("1"), BootstrapInFlight: true},
		HealthNoTail:    {Cursor: ptrStr("1")},
		HealthStale:     {Cursor: ptrStr("1"), LastPollAt: ptrTime(now.Add(-1 * time.Hour))},
		HealthDegraded:  {Cursor: ptrStr("1"), LastPollAt: &t5, TailState: ptrStr("stopped")},
		HealthHealthy:   {Cursor: ptrStr("1"), LastPollAt: &t5, TailState: ptrStr("running")},
	}
	for want, snap := range scenarios {
		status, _, reason := classify(snap, now, thr)
		if status != want {
			t.Errorf("scenario for %q produced %q — fix the fixture", want, status)
			continue
		}
		if want == HealthHealthy && reason != "" {
			t.Errorf("%q must have empty Reason; got %q", want, reason)
		}
		if want != HealthHealthy && reason == "" {
			t.Errorf("%q must have populated Reason; got empty", want)
		}
	}
}

// ─── Health() integration — acceptance criterion 9 (backlog) ──────────

func TestHealthBacklogJoplinAhead(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor:     &cursor,
			LastPollAt: &t5,
			TailState:  ptrStr("running"),
		},
	}
	j := &healthFakeJoplin{headCursor: "103"} // 3 ahead

	hc, err := Health(s, j, now, Thresholds{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if hc.BacklogState != BacklogOK {
		t.Errorf("backlog_state: got %q want %q", hc.BacklogState, BacklogOK)
	}
	if hc.Backlog != 3 {
		t.Errorf("backlog: got %d want 3", hc.Backlog)
	}
}

func TestHealthBacklogJoplinUnreachable(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor:     &cursor,
			LastPollAt: &t5,
			TailState:  ptrStr("running"),
		},
	}
	j := &healthFakeJoplin{eventsErr: errors.New("connection refused")}

	hc, err := Health(s, j, now, Thresholds{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if hc.BacklogState != BacklogUnknown {
		t.Errorf("backlog_state: got %q want %q", hc.BacklogState, BacklogUnknown)
	}
	if hc.Status != HealthHealthy {
		t.Errorf("backlog soft-fail should not affect Status: got %q", hc.Status)
	}
}

func TestHealthBacklogNilJoplinClient(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor:     &cursor,
			LastPollAt: &t5,
			TailState:  ptrStr("running"),
		},
	}

	hc, err := Health(s, nil, now, Thresholds{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if hc.BacklogState != BacklogUnknown {
		t.Errorf("backlog_state: got %q want %q", hc.BacklogState, BacklogUnknown)
	}
	if hc.Status != HealthHealthy {
		t.Errorf("nil j should not affect Status: got %q", hc.Status)
	}
}

// ─── --json schema stability — acceptance criterion 4 ─────────────────

// TestJSONSchemaHealthyGolden anchors the JSON shape for the healthy
// branch. Any field rename or removal fails this test — the wire
// shape is the Stage 4 contract.
func TestJSONSchemaHealthyGolden(t *testing.T) {
	now := time.Date(2026, 5, 31, 21, 0, 53, 436_000_000, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "8658"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor:     &cursor,
			LastPollAt: &t5,
			TailState:  ptrStr("running"),
			TailPID:    ptrInt(31035),
		},
		tagCount:    3,
		hasTagCount: 10,
	}
	var buf bytes.Buffer
	if err := runJSON(&buf, s, nil, now, Thresholds{}); err != nil {
		t.Fatalf("runJSON: %v", err)
	}
	got := buf.String()
	// Field-presence assertions — not byte-exact, so the test survives
	// a Go map iteration change but fails on any rename.
	mustContain(t, got, `"ok": true`)
	mustContain(t, got, `"status": "healthy"`)
	mustContain(t, got, `"schema_version": 1`)
	mustContain(t, got, `"state":`)
	mustContain(t, got, `"cursor": "8658"`)
	mustContain(t, got, `"tail_state": "running"`)
	mustContain(t, got, `"tail_pid": 31035`)
	mustContain(t, got, `"bootstrap_in_flight": false`)
	mustContain(t, got, `"lag_ns":`)
	mustContain(t, got, `"data_age_ns":`)
	mustContain(t, got, `"backlog_state": "unknown"`) // nil j
	mustContain(t, got, `"tag_count": 3`)
	mustContain(t, got, `"has_tag_count": 10`)
	mustContain(t, got, `"ambiguous_links": 0`)
	mustNotContain(t, got, `"reason":`) // omitempty + status==healthy
}

func TestJSONSchemaDegradedGolden(t *testing.T) {
	now := time.Date(2026, 5, 31, 21, 0, 53, 436_000_000, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "8658"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor:     &cursor,
			LastPollAt: &t5,
			TailState:  ptrStr("stopped"),
			TailPID:    ptrInt(31035),
		},
	}
	var buf bytes.Buffer
	if err := runJSON(&buf, s, nil, now, Thresholds{}); err != nil {
		t.Fatalf("runJSON: %v", err)
	}
	got := buf.String()
	mustContain(t, got, `"ok": true`)
	mustContain(t, got, `"status": "degraded"`)
	mustContain(t, got, `"reason": "tail daemon stopped`)
	mustContain(t, got, `"tail_state": "stopped"`)
}

func TestJSONSchemaStaleGolden(t *testing.T) {
	now := time.Date(2026, 5, 31, 21, 0, 53, 436_000_000, time.UTC)
	old := now.Add(-1 * time.Hour)
	cursor := "8658"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor:     &cursor,
			LastPollAt: &old,
			TailState:  ptrStr("running"),
		},
	}
	var buf bytes.Buffer
	if err := runJSON(&buf, s, nil, now, Thresholds{}); err != nil {
		t.Fatalf("runJSON: %v", err)
	}
	got := buf.String()
	mustContain(t, got, `"ok": false`)
	mustContain(t, got, `"status": "stale"`)
	mustContain(t, got, `"reason": "liveness lag`)
}

func TestJSONSchemaNeverRunGolden(t *testing.T) {
	now := time.Date(2026, 5, 31, 21, 0, 53, 436_000_000, time.UTC)
	s := &healthFakeSurreal{snap: nil}
	var buf bytes.Buffer
	if err := runJSON(&buf, s, nil, now, Thresholds{}); err != nil {
		t.Fatalf("runJSON: %v", err)
	}
	got := buf.String()
	mustContain(t, got, `"ok": false`)
	mustContain(t, got, `"status": "never-run"`)
	mustContain(t, got, `"reason": "no cursor`)
}

// TestJSONDecodes confirms the emitted bytes round-trip through
// encoding/json without panic — defensive coverage against any
// future field added with a non-JSON-serializable type.
func TestJSONDecodes(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor: &cursor, LastPollAt: &t5, TailState: ptrStr("running"),
		},
	}
	var buf bytes.Buffer
	if err := runJSON(&buf, s, nil, now, Thresholds{}); err != nil {
		t.Fatalf("runJSON: %v", err)
	}
	var hc HealthCheck
	if err := json.Unmarshal(buf.Bytes(), &hc); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, buf.String())
	}
	if hc.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d want 1", hc.SchemaVersion)
	}
	if hc.Status != HealthHealthy {
		t.Errorf("status round-trip: got %q", hc.Status)
	}
}

// ─── default human output — backlog row + warning line ────────────────

func TestRenderHumanIncludesBacklogRow(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor: &cursor, LastPollAt: &t5, TailState: ptrStr("running"),
		},
	}
	j := &healthFakeJoplin{headCursor: "103"}
	var buf bytes.Buffer
	if err := printStatus(&buf, s, j, now, Thresholds{}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := buf.String()
	mustContain(t, got, "backlog:")
	mustContain(t, got, "3 events behind (Joplin at cursor 103)")
}

func TestRenderHumanBacklogUnknown(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor: &cursor, LastPollAt: &t5, TailState: ptrStr("running"),
		},
	}
	var buf bytes.Buffer
	if err := printStatus(&buf, s, nil, now, Thresholds{}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	mustContain(t, buf.String(), "backlog:             unknown (Joplin unreachable)")
}

func TestRenderHumanDegradedWarningLine(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor: &cursor, LastPollAt: &t5, TailState: ptrStr("stopped"),
		},
	}
	var buf bytes.Buffer
	if err := printStatus(&buf, s, nil, now, Thresholds{}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	got := buf.String()
	mustContain(t, got, "warning:")
	mustContain(t, got, "tail daemon stopped")
}

func TestRenderHumanCaughtUp(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor: &cursor, LastPollAt: &t5, TailState: ptrStr("running"),
		},
	}
	j := &healthFakeJoplin{headCursor: "100"}
	var buf bytes.Buffer
	if err := printStatus(&buf, s, j, now, Thresholds{}); err != nil {
		t.Fatalf("printStatus: %v", err)
	}
	mustContain(t, buf.String(), "0 events behind (caught up)")
}

// ─── --watch single-tick — acceptance criterion 5 ─────────────────────

func TestRenderWatchTickHasHeaderAndBody(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	s := &healthFakeSurreal{
		snap: &StateSnapshot{
			Cursor: &cursor, LastPollAt: &t5, TailState: ptrStr("running"),
		},
	}
	var buf bytes.Buffer
	renderWatchTick(&buf, s, nil, now, Thresholds{})
	got := buf.String()
	mustContain(t, got, "\033[2J\033[H")
	mustContain(t, got, "qai joplin bridge status — 2026-05-31T12:00:00Z")
	mustContain(t, got, "state:")
}

// ─── --max-lag plumbing — acceptance criterion 7 ──────────────────────

func TestMaxLagOverrideTighter(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t30 := now.Add(-30 * time.Second)
	cursor := "100"
	snap := &StateSnapshot{
		Cursor: &cursor, LastPollAt: &t30, TailState: ptrStr("running"),
	}
	// Default 10m → fresh.
	if status, _, _ := classify(snap, now, Thresholds{}); status != HealthHealthy {
		t.Errorf("default: got %q want healthy", status)
	}
	// Custom 10s → stale.
	if status, _, _ := classify(snap, now, Thresholds{MaxLag: 10 * time.Second}); status != HealthStale {
		t.Errorf("tight max-lag: got %q want stale", status)
	}
}

// TestAllowBootstrapFlipsBranch — acceptance criterion 8.
func TestAllowBootstrapFlipsBranch(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t5 := now.Add(-5 * time.Second)
	cursor := "100"
	snap := &StateSnapshot{
		Cursor: &cursor, LastPollAt: &t5,
		TailState: ptrStr("running"), BootstrapInFlight: true,
	}
	// Without override → bootstrap branch.
	if status, ok, _ := classify(snap, now, Thresholds{}); status != HealthBootstrap || ok {
		t.Errorf("default: got %q ok=%v", status, ok)
	}
	// With override → falls through to healthy.
	if status, ok, _ := classify(snap, now, Thresholds{AllowBootstrap: true}); status != HealthHealthy || !ok {
		t.Errorf("allow-bootstrap: got %q ok=%v", status, ok)
	}
}

// ─── helpers ───────────────────────────────────────────────────────────

func mustContain(t *testing.T, got, needle string) {
	t.Helper()
	if !strings.Contains(got, needle) {
		t.Errorf("output missing %q\nfull output:\n%s", needle, got)
	}
}
func mustNotContain(t *testing.T, got, needle string) {
	t.Helper()
	if strings.Contains(got, needle) {
		t.Errorf("output unexpectedly contains %q\nfull output:\n%s", needle, got)
	}
}

func ptrStr(s string) *string     { return &s }
func ptrInt(i int) *int           { return &i }
func ptrTime(t time.Time) *time.Time { return &t }

// healthFakeSurreal serves the SELECTs Health() issues by encoding a
// hand-rolled StateSnapshot back into the Surreal wire shape (a JSON
// array of one row). readSnapshot decodes it the same way it would a
// real response. The tag counts come from the two GROUP ALL SELECTs.
//
// Tests against classify() go directly (bypassing this fake); tests
// that exercise Health() end-to-end use this.
type healthFakeSurreal struct {
	snap        *StateSnapshot
	tagCount    int
	hasTagCount int
}

func (f *healthFakeSurreal) Exec(sql string) ([]blast.StatementResult, error) {
	if strings.HasPrefix(strings.TrimSpace(sql), "SELECT * FROM bridge_state:current") {
		if f.snap == nil {
			return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}, nil
		}
		row := snapshotToRaw(f.snap)
		raw, _ := json.Marshal([]rawStateRow{row})
		return []blast.StatementResult{{Status: "OK", Result: raw}}, nil
	}
	if strings.Contains(sql, "FROM tag GROUP ALL") {
		t, _ := json.Marshal([]map[string]int{{"count": f.tagCount}})
		h, _ := json.Marshal([]map[string]int{{"count": f.hasTagCount}})
		return []blast.StatementResult{
			{Status: "OK", Result: t},
			{Status: "OK", Result: h},
		}, nil
	}
	return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`null`)}}, nil
}

// snapshotToRaw converts a hand-rolled snapshot back to the on-wire
// shape so readSnapshot can re-parse it. Mirrors rawStateRow.toSnapshot.
func snapshotToRaw(s *StateSnapshot) rawStateRow {
	r := rawStateRow{
		Cursor:        s.Cursor,
		TailState:     s.TailState,
		TailPID:       s.TailPID,
		EventsApplied: s.EventsApplied,
		LastError:     s.LastError,
	}
	if s.LastPollAt != nil {
		v := s.LastPollAt.UTC().Format(time.RFC3339Nano)
		r.LastPollAt = &v
	}
	if s.LastEventAt != nil {
		v := s.LastEventAt.UTC().Format(time.RFC3339Nano)
		r.LastEventAt = &v
	}
	if s.LastSyncCompleted != nil {
		v := s.LastSyncCompleted.UTC().Format(time.RFC3339Nano)
		r.LastSyncCompleted = &v
	}
	if s.BootstrapInFlight {
		r.BootstrapProgress = map[string]any{"in_flight": true}
	}
	return r
}

// healthFakeJoplin satisfies joplinAPI just enough for backlog
// probing. Returns headCursor on GetEvents; returns eventsErr if set.
type healthFakeJoplin struct {
	headCursor string
	eventsErr  error
}

func (f *healthFakeJoplin) ListFolders() ([]joplin.Folder, error) { return nil, nil }
func (f *healthFakeJoplin) ListNotesFull(_ string, _ []string) ([]joplin.Note, error) {
	return nil, nil
}
func (f *healthFakeJoplin) ListTags() ([]joplin.Tag, error)               { return nil, nil }
func (f *healthFakeJoplin) GetNoteTags(_ string) ([]joplin.Tag, error)    { return nil, nil }
func (f *healthFakeJoplin) GetNote(_ string, _ ...string) (*joplin.Note, error) {
	return nil, errors.New("not used")
}
func (f *healthFakeJoplin) GetFolder(_ string) (*joplin.Folder, error) {
	return nil, errors.New("not used")
}
func (f *healthFakeJoplin) GetEvents(_ string, _ int) (*joplin.EventsResponse, error) {
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	return &joplin.EventsResponse{Cursor: f.headCursor}, nil
}
