package joplingraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
	"github.com/quantum-encoding/qai-cli/internal/joplinbridge"
)

// ────────────────────────────────────────────────────────────────────────
// argparse table — every flag + every reject branch
// ────────────────────────────────────────────────────────────────────────

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		raw     []string
		want    Args
		wantErr string
	}{
		{
			name: "defaults",
			raw:  []string{},
			want: Args{Hops: 1, Limit: 20},
		},
		{
			name: "positional_query",
			raw:  []string{"wasm"},
			want: Args{Query: "wasm", Hops: 1, Limit: 20},
		},
		{
			name: "project_bare",
			raw:  []string{"--project"},
			want: Args{ProjectFlag: true, Hops: 1, Limit: 20},
		},
		{
			name: "project_with_value",
			raw:  []string{"--project", "qai-cli"},
			want: Args{ProjectFlag: true, Project: "qai-cli", Hops: 1, Limit: 20},
		},
		{
			name: "tag_only",
			raw:  []string{"--tag", "decision"},
			want: Args{Tag: "decision", Hops: 1, Limit: 20},
		},
		{
			name: "tag_intersection",
			raw:  []string{"--tag", "decision", "--with", "qai-cli"},
			want: Args{Tag: "decision", With: "qai-cli", Hops: 1, Limit: 20},
		},
		{
			name: "all_flags",
			raw:  []string{"wasm", "--hops", "2", "--limit", "5", "--include-bodies",
				"--max-lag", "1h", "--strict", "--explain"},
			want: Args{
				Query: "wasm", Hops: 2, Limit: 5, IncludeBody: true,
				MaxLag: time.Hour, Strict: true, Explain: true,
			},
		},
		{
			name:    "with_without_tag",
			raw:     []string{"--with", "foo"},
			wantErr: "--with requires --tag",
		},
		{
			name:    "two_positional_queries",
			raw:     []string{"alpha", "beta"},
			wantErr: "only one positional",
		},
		{
			name:    "limit_over_cap",
			raw:     []string{"--limit", "999"},
			wantErr: "exceeds hard cap",
		},
		{
			name:    "limit_negative",
			raw:     []string{"--limit", "-1"},
			wantErr: "positive integer",
		},
		{
			name:    "hops_out_of_range",
			raw:     []string{"--hops", "3"},
			wantErr: "must be 0, 1, or 2",
		},
		{
			name:    "max_lag_invalid",
			raw:     []string{"--max-lag", "notaduration"},
			wantErr: "--max-lag invalid",
		},
		{
			name:    "unknown_flag",
			raw:     []string{"--nope"},
			wantErr: "unknown flag",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseArgs(c.raw)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err=%v want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// SQL builders — pin the shapes per the spec
// ────────────────────────────────────────────────────────────────────────

func TestBuildFullTextSQL(t *testing.T) {
	sql := buildFullTextSQL("wasm", 10)
	for _, need := range []string{
		`title @1@ "wasm"`,
		`excerpt @2@ "wasm"`,
		"search::score(1) + search::score(2)",
		"ORDER BY score DESC",
		"LIMIT 10",
	} {
		if !strings.Contains(sql, need) {
			t.Errorf("SQL missing %q\nfull:\n%s", need, sql)
		}
	}
}

func TestBuildFullTextSQLEscapesQuery(t *testing.T) {
	// A query containing a double-quote must be safely escaped via the
	// quote() JSON round-trip; otherwise it would terminate the literal
	// and Surreal would parse-error.
	sql := buildFullTextSQL(`he said "hi"`, 10)
	if !strings.Contains(sql, `"he said \"hi\""`) {
		t.Errorf("quote() escape failed; SQL:\n%s", sql)
	}
}

func TestBuildProjectSQL(t *testing.T) {
	sql := buildProjectSQL("qai-cli", 5)
	for _, need := range []string{
		`kind = 'project'`,
		`title = "qai-cli"`,
		// VALUE id (not bare id) — without it the IN clause silently
		// matches zero rows because Surreal compares wrapped {id: ...}
		// objects against the scalar in-list. Live-fire regression.
		"SELECT VALUE id FROM tag",
		"ORDER BY updated_at DESC",
		"LIMIT 5",
	} {
		if !strings.Contains(sql, need) {
			t.Errorf("SQL missing %q\nfull:\n%s", need, sql)
		}
	}
}

func TestBuildTagSQL(t *testing.T) {
	sql := buildTagSQL("decision", 7)
	for _, need := range []string{
		`title = "decision"`,
		"SELECT VALUE id FROM tag", // same VALUE-id trap as the project path
		"ORDER BY updated_at DESC",
		"LIMIT 7",
	} {
		if !strings.Contains(sql, need) {
			t.Errorf("SQL missing %q\nfull:\n%s", need, sql)
		}
	}
	// The single-tag path must NOT pin kind — operator wants tags
	// classified as kind/freeform/concept to match too.
	if strings.Contains(sql, "kind = ") {
		t.Errorf("single-tag SQL should not pin kind; SQL:\n%s", sql)
	}
}

func TestBuildTagIntersectionSQL(t *testing.T) {
	sql := buildTagIntersectionSQL("decision", "qai-cli", 20)
	for _, need := range []string{
		"LET $a =",
		"LET $b =",
		"id IN $a AND id IN $b",
		`title = "decision"`,
		`title = "qai-cli"`,
	} {
		if !strings.Contains(sql, need) {
			t.Errorf("SQL missing %q\nfull:\n%s", need, sql)
		}
	}
}

func TestBuildPrimarySQLResolution(t *testing.T) {
	cases := []struct {
		name string
		args Args
		want LookupKind
	}{
		{"fulltext_wins", Args{Query: "x", ProjectFlag: true, Tag: "y", Limit: 1}, KindFullText},
		{"project_wins_over_tag", Args{ProjectFlag: true, Tag: "y", Limit: 1}, KindProject},
		{"tag_alone", Args{Tag: "y", Limit: 1}, KindTag},
		{"tag_intersection", Args{Tag: "a", With: "b", Limit: 1}, KindTag},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, k := buildPrimarySQL(c.args)
			if k != c.want {
				t.Errorf("kind: got %q want %q", k, c.want)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// Health gate — the four-row table from the spec
// ────────────────────────────────────────────────────────────────────────

// TestHealthGate drives every row of the spec's 4-row gate table and
// asserts the resulting exit code + stderr line + stale flag. This is
// load-bearing for the agent-memory contract — silent-serve on !OK is
// the failure mode this test prevents.
func TestHealthGate(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-1 * time.Hour)

	cases := []struct {
		name       string
		snap       *joplinbridge.StateSnapshot
		strict     bool
		wantExit   int
		wantStderr string
		wantStale  bool
	}{
		{
			name:     "healthy_clean_serve",
			snap:     &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
			wantExit: exitOK,
		},
		{
			name:       "degraded_serves_with_warning",
			snap:       &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("stopped")},
			wantExit:   exitOK,
			wantStderr: "warning: tail daemon stopped",
		},
		{
			name:       "stale_no_strict_serves_with_label",
			snap:       &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &stale, TailState: ptrStr("running")},
			wantExit:   exitOK,
			wantStderr: "warning: serving stale graph",
			wantStale:  true,
		},
		{
			name:       "stale_strict_refuses",
			snap:       &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &stale, TailState: ptrStr("running")},
			strict:     true,
			wantExit:   exitRefused,
			wantStderr: "unhealthy: stale",
		},
		{
			name:       "bootstrap_strict_refuses_exit2",
			snap:       &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), BootstrapInFlight: true, LastPollAt: &fresh},
			strict:     true,
			wantExit:   exitBootstrap,
			wantStderr: "unhealthy: bootstrap",
		},
		{
			name:       "never_run_strict_refuses",
			snap:       nil,
			strict:     true,
			wantExit:   exitRefused,
			wantStderr: "unhealthy: never-run",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &fakeSurreal{snap: c.snap, primary: []map[string]any{}, expansion: emptyExpansion()}
			var out, errb bytes.Buffer
			rc := &runContext{
				Surreal: s,
				Joplin:  nil,
				Args:    Args{Query: "x", Hops: 0, Limit: 1, Strict: c.strict},
				Now:     now,
				Out:     &out,
				Err:     &errb,
			}
			exit := runContext_run(rc)
			if exit != c.wantExit {
				t.Errorf("exit: got %d want %d\nstderr:%s\nstdout:%s",
					exit, c.wantExit, errb.String(), out.String())
			}
			if c.wantStderr != "" && !strings.Contains(errb.String(), c.wantStderr) {
				t.Errorf("stderr missing %q\nstderr:\n%s", c.wantStderr, errb.String())
			}
			if exit == exitOK && out.Len() > 0 {
				var p ContextPayload
				if err := json.Unmarshal(out.Bytes(), &p); err != nil {
					t.Fatalf("payload not valid JSON: %v\n%s", err, out.String())
				}
				if p.Stale != c.wantStale {
					t.Errorf("stale: got %v want %v", p.Stale, c.wantStale)
				}
			}
		})
	}
}

// TestHealthContractInvariantSurfaced — every !healthy status MUST land
// either as a payload freshness_warning OR as a refusal stderr line.
// No silent-serve branch. Iterates every HealthStatus constant.
func TestHealthContractInvariantSurfaced(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-1 * time.Hour)
	scenarios := map[joplinbridge.HealthStatus]*joplinbridge.StateSnapshot{
		joplinbridge.HealthHealthy:   {Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
		joplinbridge.HealthDegraded:  {Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("stopped")},
		joplinbridge.HealthStale:     {Cursor: ptrStr("1"), LastPollAt: &stale, TailState: ptrStr("running")},
		joplinbridge.HealthError:     {Cursor: ptrStr("1"), TailState: ptrStr("error"), LastError: ptrStr("x")},
		joplinbridge.HealthBootstrap: {Cursor: ptrStr("1"), BootstrapInFlight: true},
		joplinbridge.HealthNoTail:    {Cursor: ptrStr("1")},
		joplinbridge.HealthNeverRun:  nil,
	}
	for status, snap := range scenarios {
		s := &fakeSurreal{snap: snap, primary: []map[string]any{}, expansion: emptyExpansion()}
		var out, errb bytes.Buffer
		rc := &runContext{
			Surreal: s, Joplin: nil,
			Args: Args{Query: "x", Hops: 0, Limit: 1},
			Now:  now, Out: &out, Err: &errb,
		}
		exit := runContext_run(rc)
		if status == joplinbridge.HealthHealthy {
			if errb.Len() > 0 {
				t.Errorf("healthy emitted stderr: %s", errb.String())
			}
			continue
		}
		// Either the payload carries a warning OR stderr does. Never silent.
		if exit == exitOK {
			var p ContextPayload
			if err := json.Unmarshal(out.Bytes(), &p); err != nil {
				t.Errorf("%s: payload invalid JSON: %v", status, err)
				continue
			}
			if p.FreshnessWarning == "" && errb.Len() == 0 {
				t.Errorf("%s: silent serve — no warning AND no stderr", status)
			}
		} else if errb.Len() == 0 {
			t.Errorf("%s: refused but emitted no stderr", status)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// --explain — runs gate, prints SQL, does NOT execute the query
// ────────────────────────────────────────────────────────────────────────

// TestExplainSkipsQuery confirms --explain runs the Health probe (one
// Exec for bridge_state, one for tag counts) but never runs the
// primary-lookup SQL. Counts Exec calls as proof.
func TestExplainSkipsQuery(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	s := &fakeSurreal{
		snap:      &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
		primary:   []map[string]any{},
		expansion: emptyExpansion(),
	}
	var out, errb bytes.Buffer
	rc := &runContext{
		Surreal: s, Joplin: nil,
		Args: Args{Query: "wasm", Hops: 0, Limit: 5, Explain: true},
		Now:  now, Out: &out, Err: &errb,
	}
	exit := runContext_run(rc)
	if exit != exitOK {
		t.Fatalf("exit: got %d want 0\nstderr:%s", exit, errb.String())
	}
	if !strings.Contains(errb.String(), "--explain (kind=fulltext)") {
		t.Errorf("stderr missing --explain header\nstderr:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "title @1@") {
		t.Errorf("stderr missing emitted SQL")
	}
	// Health does TWO Execs: bridge_state SELECT + tag counts.
	// No primary lookup → total stays at 2.
	if s.execCount > 2 {
		t.Errorf("Exec called %d times — --explain must skip primary lookup", s.execCount)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Full payload round-trip + per-branch goldens
// ────────────────────────────────────────────────────────────────────────

// TestFulltextPayload — primary path with scores, neighbourhood
// expansion, links_to no-op-safe. Asserts every load-bearing payload
// field.
func TestFulltextPayload(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	s := &fakeSurreal{
		snap: &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
		primary: []map[string]any{
			{"id": "note:`abc123`", "title": "WASM intro", "excerpt": "intro to wasm", "score": 5.4},
			{"id": "note:`def456`", "title": "wasm runtimes", "excerpt": "runtimes overview", "score": 4.1},
		},
		expansion: expansionWith(
			[]edgeRow{{A: "notebook:`nb1`", B: "note:`abc123`"}, {A: "notebook:`nb1`", B: "note:`def456`"}},
			[]notebookDetailRow{{ID: "notebook:`nb1`", Title: "concepts"}},
			[]edgeRow{{A: "note:`abc123`", B: "tag:`t1`"}},
			[]tagDetailRow{{ID: "tag:`t1`", Title: "wasm", Kind: "concept"}},
			nil, nil, nil, nil,
		),
		notebookPaths: map[string]notebookPathRow{
			"nb1": {Title: "concepts", ParentID: nil},
		},
	}
	var out, errb bytes.Buffer
	rc := &runContext{
		Surreal: s, Joplin: nil,
		Args: Args{Query: "wasm", Hops: 1, Limit: 20},
		Now:  now, Out: &out, Err: &errb,
	}
	exit := runContext_run(rc)
	if exit != exitOK {
		t.Fatalf("exit %d\nstderr:%s\nstdout:%s", exit, errb.String(), out.String())
	}
	var p ContextPayload
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	if p.SchemaVersion != schemaVersion {
		t.Errorf("schema_version=%d want %d", p.SchemaVersion, schemaVersion)
	}
	if p.Query.Kind != KindFullText || p.Query.Value != "wasm" {
		t.Errorf("query: %+v", p.Query)
	}
	if p.Stale {
		t.Errorf("healthy → stale must be false")
	}
	if p.Counts.Primary != 2 || p.Counts.Total != 2 {
		t.Errorf("counts: %+v", p.Counts)
	}
	if len(p.Notes) != 2 {
		t.Fatalf("notes: got %d want 2", len(p.Notes))
	}
	if p.Notes[0].Score == 0 {
		t.Errorf("fulltext path must populate score; got %v", p.Notes[0])
	}
	if p.Notes[0].Match != "primary" {
		t.Errorf("match: %q want primary", p.Notes[0].Match)
	}
	if p.Notes[0].Notebook.Title != "concepts" {
		t.Errorf("notebook title: %q", p.Notes[0].Notebook.Title)
	}
	if p.Notes[0].Notebook.Path != "concepts" {
		t.Errorf("notebook path: %q want concepts", p.Notes[0].Notebook.Path)
	}
	// links_to is empty today. Must be a non-nil slice (so jq .links_out
	// doesn't reach into null) but zero-length.
	if p.Notes[0].LinksOut == nil {
		t.Errorf("links_out must be non-nil")
	}
	if len(p.Notes[0].LinksOut) != 0 {
		t.Errorf("links_out: empty today; got %d", len(p.Notes[0].LinksOut))
	}
}

// TestStage5Readiness — inject a fake links_to row and assert the
// payload's links_out populates with no code change to the builders.
// Stage-5-readiness proof.
func TestStage5Readiness(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	s := &fakeSurreal{
		snap: &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
		primary: []map[string]any{
			{"id": "note:`abc123`", "title": "src", "excerpt": "x"},
		},
		expansion: expansionWith(
			nil, nil, nil, nil,
			[]edgeRow{{A: "note:`abc123`", B: "note:`xyz789`"}},
			[]noteIDTitleRow{{ID: "note:`xyz789`", Title: "linked target"}},
			nil, nil,
		),
	}
	var out, errb bytes.Buffer
	rc := &runContext{
		Surreal: s, Joplin: nil,
		Args: Args{Query: "src", Hops: 1, Limit: 5},
		Now:  now, Out: &out, Err: &errb,
	}
	if exit := runContext_run(rc); exit != exitOK {
		t.Fatalf("exit %d\nstderr:%s", exit, errb.String())
	}
	var p ContextPayload
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Notes) == 0 || len(p.Notes[0].LinksOut) != 1 {
		t.Fatalf("links_out should have 1 entry; got payload:\n%s", out.String())
	}
	if p.Notes[0].LinksOut[0].ID != "xyz789" || p.Notes[0].LinksOut[0].Title != "linked target" {
		t.Errorf("links_out[0]: %+v", p.Notes[0].LinksOut[0])
	}
}

// TestHopsZeroSkipsExpansion — --hops 0 returns primary only, no
// neighbourhood SELECT issued. We assert via Exec count: 2 for Health
// + 1 for primary = 3 total when expansion is skipped.
func TestHopsZeroSkipsExpansion(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	s := &fakeSurreal{
		snap: &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
		primary: []map[string]any{
			{"id": "note:`a`", "title": "a", "excerpt": "x"},
		},
		expansion: emptyExpansion(),
	}
	var out, errb bytes.Buffer
	rc := &runContext{
		Surreal: s, Joplin: nil,
		Args: Args{Query: "x", Hops: 0, Limit: 5},
		Now:  now, Out: &out, Err: &errb,
	}
	if exit := runContext_run(rc); exit != exitOK {
		t.Fatalf("exit %d", exit)
	}
	// Health: bridge_state SELECT + tag counts = 2 Execs.
	// Primary lookup = 1 Exec. No expansion.
	if s.execCount != 3 {
		t.Errorf("Exec count=%d want 3 (health=2 + primary=1, no expansion)", s.execCount)
	}
}

// TestEmptyPrimarySkipsExpansion — when the primary lookup returns
// zero rows, neighbourhood expansion is also skipped (nothing to
// expand). Healthy fixture, fulltext path against an unknown term.
func TestEmptyPrimarySkipsExpansion(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	s := &fakeSurreal{
		snap:      &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
		primary:   []map[string]any{},
		expansion: emptyExpansion(),
	}
	var out, errb bytes.Buffer
	rc := &runContext{
		Surreal: s, Joplin: nil,
		Args: Args{Query: "unknown", Hops: 1, Limit: 5},
		Now:  now, Out: &out, Err: &errb,
	}
	if exit := runContext_run(rc); exit != exitOK {
		t.Fatalf("exit %d", exit)
	}
	var p ContextPayload
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(p.Notes) != 0 || p.Counts.Total != 0 {
		t.Errorf("expected empty notes; got %+v", p)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Notebook path resolver — caching assertion
// ────────────────────────────────────────────────────────────────────────

// TestNotebookPathCache — N notes sharing one parent walk the chain
// once. Asserts via call-counting fake.
func TestNotebookPathCache(t *testing.T) {
	s := &countingNotebookSurreal{
		paths: map[string]notebookPathRow{
			"leaf":   {Title: "sessions", ParentID: ptrStr("mid")},
			"mid":    {Title: "qai", ParentID: ptrStr("root")},
			"root":   {Title: "work", ParentID: nil},
		},
	}
	cache := map[string]string{}
	// First walk — three SELECTs.
	if got := resolveNotebookPath(s, "leaf", cache); got != "work/qai/sessions" {
		t.Errorf("path: got %q", got)
	}
	first := s.execCount
	// Second walk — same leaf, must hit cache, ZERO new SELECTs.
	if got := resolveNotebookPath(s, "leaf", cache); got != "work/qai/sessions" {
		t.Errorf("cached path: got %q", got)
	}
	if s.execCount != first {
		t.Errorf("cache miss: execCount went %d → %d", first, s.execCount)
	}
}

// ────────────────────────────────────────────────────────────────────────
// helpers
// ────────────────────────────────────────────────────────────────────────

func ptrStr(s string) *string { return &s }

// fakeSurreal handles every SELECT graph context issues:
//   - bridge_state:current SELECT (Stage 3 Health)
//   - tag count GROUP ALL (Stage 3 Health)
//   - the primary lookup (one of fulltext/project/tag/intersection)
//   - the neighbourhood expansion (8 SELECTs concatenated)
//   - notebook path SELECTs (one per unique leaf, repeated per walk)
//
// Dispatching on string-match of the SQL: cheap, brittle, fine for
// tests. snap routes through the same snapshotToRaw shape Stage 3
// uses; primary/expansion are pre-encoded via setters.
type fakeSurreal struct {
	snap          *joplinbridge.StateSnapshot
	primary       []map[string]any
	expansion     [8]json.RawMessage // results[0..7] of expandNeighbourhood
	notebookPaths map[string]notebookPathRow
	execCount     int
}

func (f *fakeSurreal) Exec(sql string) ([]blast.StatementResult, error) {
	f.execCount++
	trimmed := strings.TrimSpace(sql)

	switch {
	case strings.HasPrefix(trimmed, "SELECT * FROM bridge_state:current"):
		return f.bridgeStateResult(), nil
	case strings.Contains(sql, "FROM tag GROUP ALL"):
		return f.tagCountsResult(), nil
	case strings.HasPrefix(trimmed, "SELECT title, parent_id FROM notebook:"):
		return f.notebookPathResult(sql), nil
	case strings.HasPrefix(trimmed, "SELECT in AS notebook_id"):
		return f.expansionResult(), nil
	case strings.HasPrefix(trimmed, "SELECT id, title, excerpt"),
		strings.HasPrefix(trimmed, "LET $a ="):
		return f.primaryResult(), nil
	}
	return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`null`)}}, nil
}

func (f *fakeSurreal) bridgeStateResult() []blast.StatementResult {
	if f.snap == nil {
		return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}
	}
	// Encode StateSnapshot to the rawStateRow on-wire shape (via the
	// public type's JSON tags — pointer fields with RFC3339Nano for
	// the datetimes). Stage 3 tests do this via snapshotToRaw, but
	// here we hand-roll it to avoid a cross-package dependency on the
	// unexported helper.
	type raw struct {
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
	r := raw{
		Cursor: f.snap.Cursor, TailState: f.snap.TailState, TailPID: f.snap.TailPID,
		EventsApplied: f.snap.EventsApplied, LastError: f.snap.LastError,
	}
	if f.snap.LastPollAt != nil {
		v := f.snap.LastPollAt.UTC().Format(time.RFC3339Nano)
		r.LastPollAt = &v
	}
	if f.snap.LastEventAt != nil {
		v := f.snap.LastEventAt.UTC().Format(time.RFC3339Nano)
		r.LastEventAt = &v
	}
	if f.snap.LastSyncCompleted != nil {
		v := f.snap.LastSyncCompleted.UTC().Format(time.RFC3339Nano)
		r.LastSyncCompleted = &v
	}
	if f.snap.BootstrapInFlight {
		r.BootstrapProgress = map[string]any{"in_flight": true}
	}
	raws, _ := json.Marshal([]raw{r})
	return []blast.StatementResult{{Status: "OK", Result: raws}}
}

func (f *fakeSurreal) tagCountsResult() []blast.StatementResult {
	t, _ := json.Marshal([]map[string]int{{"count": 0}})
	h, _ := json.Marshal([]map[string]int{{"count": 0}})
	return []blast.StatementResult{
		{Status: "OK", Result: t},
		{Status: "OK", Result: h},
	}
}

func (f *fakeSurreal) primaryResult() []blast.StatementResult {
	rows, _ := json.Marshal(f.primary)
	// Tag-intersection emits LET $a + LET $b + SELECT — 3 results.
	// All other paths emit a single SELECT. The decoder takes the LAST
	// result so a fixed 3-result reply works for both shapes.
	null := json.RawMessage(`null`)
	return []blast.StatementResult{
		{Status: "OK", Result: null},
		{Status: "OK", Result: null},
		{Status: "OK", Result: rows},
	}
}

func (f *fakeSurreal) expansionResult() []blast.StatementResult {
	out := make([]blast.StatementResult, 8)
	for i := 0; i < 8; i++ {
		raw := f.expansion[i]
		if len(raw) == 0 {
			raw = json.RawMessage(`[]`)
		}
		out[i] = blast.StatementResult{Status: "OK", Result: raw}
	}
	return out
}

func (f *fakeSurreal) notebookPathResult(sql string) []blast.StatementResult {
	// Extract the notebook ID from the SQL — "SELECT title, parent_id
	// FROM notebook:`<id>` LIMIT 1;". We trim greedily on backtick.
	start := strings.Index(sql, "notebook:`")
	if start < 0 {
		return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}
	}
	rest := sql[start+len("notebook:`"):]
	end := strings.Index(rest, "`")
	if end < 0 {
		return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}
	}
	id := rest[:end]
	row, ok := f.notebookPaths[id]
	if !ok {
		return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}
	}
	rows, _ := json.Marshal([]notebookPathRow{row})
	return []blast.StatementResult{{Status: "OK", Result: rows}}
}

// emptyExpansion is the all-empty 8-result expansion (no contains,
// no tags, no links).
func emptyExpansion() [8]json.RawMessage {
	out := [8]json.RawMessage{}
	for i := range out {
		out[i] = json.RawMessage(`[]`)
	}
	return out
}

// expansionWith pre-encodes the 8 SELECTs of expandNeighbourhood from
// hand-rolled rows. Pass nil for slots that should be empty.
func expansionWith(
	contains []edgeRow,
	notebooks []notebookDetailRow,
	tagEdges []edgeRow,
	tags []tagDetailRow,
	linksOut []edgeRow,
	linksOutTargets []noteIDTitleRow,
	linksIn []edgeRow,
	linksInSources []noteIDTitleRow,
) [8]json.RawMessage {
	encEdges := func(rs []edgeRow, aField, bField string) json.RawMessage {
		if rs == nil {
			return json.RawMessage(`[]`)
		}
		arr := make([]map[string]any, 0, len(rs))
		for _, r := range rs {
			arr = append(arr, map[string]any{aField: r.A, bField: r.B})
		}
		b, _ := json.Marshal(arr)
		return b
	}
	encAny := func(v any) json.RawMessage {
		if v == nil {
			return json.RawMessage(`[]`)
		}
		b, _ := json.Marshal(v)
		return b
	}
	return [8]json.RawMessage{
		encEdges(contains, "notebook_id", "note_id"),
		encAny(notebooks),
		encEdges(tagEdges, "note_id", "tag_id"),
		encAny(tags),
		encEdges(linksOut, "note_id", "target_id"),
		encAny(linksOutTargets),
		encEdges(linksIn, "source_id", "note_id"),
		encAny(linksInSources),
	}
}

// countingNotebookSurreal — minimal fake for the path-resolver test.
// Only handles SELECT title, parent_id FROM notebook:<id>. Other
// queries panic so a test that strays gets a clear failure.
type countingNotebookSurreal struct {
	paths     map[string]notebookPathRow
	execCount int
}

func (s *countingNotebookSurreal) Exec(sql string) ([]blast.StatementResult, error) {
	s.execCount++
	start := strings.Index(sql, "notebook:`")
	if start < 0 {
		return nil, errors.New("unexpected SQL in path-resolver test: " + sql)
	}
	rest := sql[start+len("notebook:`"):]
	end := strings.Index(rest, "`")
	if end < 0 {
		return nil, errors.New("malformed record id in SQL")
	}
	id := rest[:end]
	row, ok := s.paths[id]
	if !ok {
		return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}, nil
	}
	rows, _ := json.Marshal([]notebookPathRow{row})
	return []blast.StatementResult{{Status: "OK", Result: rows}}, nil
}

// ────────────────────────────────────────────────────────────────────────
// joplin fake for the --include-bodies fetch loop
// ────────────────────────────────────────────────────────────────────────

type fakeJoplin struct {
	bodies map[string]string
	errs   map[string]error
	mu     sync.Mutex
	calls  int
}

func (f *fakeJoplin) GetNote(id string, _ ...string) (*joplin.Note, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if err, ok := f.errs[id]; ok {
		return nil, err
	}
	body, ok := f.bodies[id]
	if !ok {
		return nil, fmt.Errorf("HTTP 404")
	}
	return &joplin.Note{ID: id, Body: body}, nil
}

func (f *fakeJoplin) GetEvents(_ string, _ int) (*joplin.EventsResponse, error) {
	return &joplin.EventsResponse{Cursor: ""}, nil
}

// TestIncludeBodiesFetchesParallel — assert bodies populate when
// --include-bodies is set, fetch errors land as null + a stderr line.
func TestIncludeBodiesFetchesParallel(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Second)
	s := &fakeSurreal{
		snap: &joplinbridge.StateSnapshot{Cursor: ptrStr("1"), LastPollAt: &fresh, TailState: ptrStr("running")},
		primary: []map[string]any{
			{"id": "note:`a`", "title": "A", "excerpt": "ex"},
			{"id": "note:`b`", "title": "B", "excerpt": "ex"},
			{"id": "note:`c`", "title": "C", "excerpt": "ex"},
		},
		expansion: emptyExpansion(),
	}
	j := &fakeJoplin{
		bodies: map[string]string{"a": "body a", "c": "body c"},
		errs:   map[string]error{"b": errors.New("simulated 500")},
	}
	var out, errb bytes.Buffer
	rc := &runContext{
		Surreal: s, Joplin: j,
		Args: Args{Query: "x", Hops: 0, Limit: 5, IncludeBody: true},
		Now:  now, Out: &out, Err: &errb,
	}
	if exit := runContext_run(rc); exit != exitOK {
		t.Fatalf("exit %d", exit)
	}
	var p ContextPayload
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bodies := map[string]*string{}
	for _, n := range p.Notes {
		bodies[n.ID] = n.Body
	}
	if got := bodies["a"]; got == nil || *got != "body a" {
		t.Errorf("a body: %v", got)
	}
	if got := bodies["b"]; got != nil {
		t.Errorf("b body: expected nil on fetch error, got %v", got)
	}
	if got := bodies["c"]; got == nil || *got != "body c" {
		t.Errorf("c body: %v", got)
	}
	if !strings.Contains(errb.String(), "fetch body b") {
		t.Errorf("stderr missing fetch error line; got:\n%s", errb.String())
	}
}

// ────────────────────────────────────────────────────────────────────────
// Sanity: payload always emits non-nil neighbour slices so jq paths
// don't reach into JSON null.
// ────────────────────────────────────────────────────────────────────────

func TestNoteSlicesNeverNull(t *testing.T) {
	n := convertNoteRow(noteRow{ID: "note:`x`", Title: "x"}, "primary")
	if n.Tags == nil || n.LinksOut == nil || n.LinksIn == nil {
		t.Errorf("zero-value Note had nil slice: tags=%v out=%v in=%v",
			n.Tags == nil, n.LinksOut == nil, n.LinksIn == nil)
	}
}
