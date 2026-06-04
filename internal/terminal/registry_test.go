package terminal

import (
	"testing"
	"time"
)

// withTempRegistry points config.ConfigDir at a temp dir (via QAI_CONFIG_HOME)
// so registry reads/writes never touch the real ~/.qai.
func withTempRegistry(t *testing.T) {
	t.Helper()
	t.Setenv("QAI_CONFIG_HOME", t.TempDir())
}

func TestRegistryRoundTrip(t *testing.T) {
	withTempRegistry(t)

	if err := RegisterPane("worker-1", "%5", "/work/qai"); err != nil {
		t.Fatalf("RegisterPane: %v", err)
	}
	if err := RegisterPane("worker-2", "%6", "/work/other"); err != nil {
		t.Fatalf("RegisterPane: %v", err)
	}

	recs := AllRecords()
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs["worker-1"].PaneID != "%5" || recs["worker-1"].Cwd != "/work/qai" {
		t.Errorf("worker-1 record wrong: %+v", recs["worker-1"])
	}
	if recs["worker-1"].SpawnedAt == 0 {
		t.Errorf("worker-1 SpawnedAt not set")
	}

	// Session attaches only to an existing record.
	if err := SetPaneSession("worker-1", "abc-123-def"); err != nil {
		t.Fatalf("SetPaneSession: %v", err)
	}
	if got := AllRecords()["worker-1"].SessionID; got != "abc-123-def" {
		t.Errorf("session not stored, got %q", got)
	}
	if err := SetPaneSession("ghost", "x"); err != nil {
		t.Fatalf("SetPaneSession on absent name should be a no-op error-free: %v", err)
	}
	if _, ok := AllRecords()["ghost"]; ok {
		t.Errorf("SetPaneSession created a record for an unregistered name")
	}

	// recordsByID indexes on pane id.
	byID := recordsByID()
	if byID["%6"].Name != "worker-2" {
		t.Errorf("recordsByID[%%6] = %+v", byID["%6"])
	}
}

func TestUnregisterByNameAndID(t *testing.T) {
	withTempRegistry(t)
	RegisterPane("a", "%1", "")
	RegisterPane("b", "%2", "")

	// Remove by name.
	if err := UnregisterPaneRef("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := AllRecords()["a"]; ok {
		t.Errorf("record a still present after UnregisterPaneRef by name")
	}
	// Remove by id.
	if err := UnregisterPaneRef("%2"); err != nil {
		t.Fatal(err)
	}
	if len(AllRecords()) != 0 {
		t.Errorf("record b still present after UnregisterPaneRef by id")
	}
	// Idempotent.
	if err := UnregisterPaneRef("nope"); err != nil {
		t.Errorf("UnregisterPaneRef on absent ref should be no-op: %v", err)
	}
}

func TestLookupPaneIDLivenessAndPrune(t *testing.T) {
	withTempRegistry(t)
	RegisterPane("alive", "%10", "")
	RegisterPane("dead", "%11", "")

	// Stub the liveness probe: %10 lives, %11 is gone.
	orig := livePaneIDsFn
	livePaneIDsFn = func() map[string]bool { return map[string]bool{"%10": true} }
	defer func() { livePaneIDsFn = orig }()

	if id, ok := lookupPaneID("alive"); !ok || id != "%10" {
		t.Errorf("lookupPaneID(alive) = %q,%v; want %%10,true", id, ok)
	}
	// Dead pane: not resolved, and its stale record is pruned.
	if id, ok := lookupPaneID("dead"); ok || id != "" {
		t.Errorf("lookupPaneID(dead) = %q,%v; want \"\",false", id, ok)
	}
	if _, ok := AllRecords()["dead"]; ok {
		t.Errorf("stale 'dead' record was not pruned by lookup")
	}
	// Unknown name resolves to nothing.
	if _, ok := lookupPaneID("missing"); ok {
		t.Errorf("lookupPaneID(missing) should be false")
	}
}

func TestPruneDead(t *testing.T) {
	withTempRegistry(t)
	RegisterPane("x", "%20", "")
	RegisterPane("y", "%21", "")
	orig := livePaneIDsFn
	livePaneIDsFn = func() map[string]bool { return map[string]bool{"%20": true} }
	defer func() { livePaneIDsFn = orig }()

	PruneDead()
	recs := AllRecords()
	if _, ok := recs["x"]; !ok {
		t.Errorf("live pane x was pruned")
	}
	if _, ok := recs["y"]; ok {
		t.Errorf("dead pane y was not pruned")
	}
}

func TestRCURLRegex(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"app.claude.com canonical",
			"  Remote Control active\n  Open https://app.claude.com/rc/817b1c30-48a3-93af to connect\n",
			"https://app.claude.com/rc/817b1c30-48a3-93af",
		},
		{
			"claude.ai/code alt form",
			"link: https://claude.ai/code/abc123-def from your phone",
			"https://claude.ai/code/abc123-def",
		},
		{
			"status line with no url",
			"                         Remote Control active",
			"",
		},
		{
			"unrelated url is ignored",
			"see https://example.com/rc/nope",
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rcURLRe.FindString(c.in); got != c.want {
				t.Errorf("FindString(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEncodeProjectPath(t *testing.T) {
	cases := map[string]string{
		"/Users/director/work/qai":  "-Users-director-work-qai",
		"/Users/x/work/go-programs": "-Users-x-work-go-programs",
		"/Users/x/my.app_v2":        "-Users-x-my-app-v2",
		"relative/path":             "relative-path",
	}
	for in, want := range cases {
		if got := encodeProjectPath(in); got != want {
			t.Errorf("encodeProjectPath(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestPickEarliestUnclaimed(t *testing.T) {
	base := time.Date(2026, 6, 4, 19, 0, 0, 0, time.UTC)
	born := map[string]time.Time{
		"old":    base.Add(-time.Hour), // born before the spawn window
		"first":  base.Add(2 * time.Second),
		"second": base.Add(5 * time.Second),
	}
	since := base.Add(-1 * time.Second) // small skew tolerance before spawn

	// Earliest birth at/after `since`, nothing claimed → "first".
	if got := pickEarliestUnclaimed(born, since, nil); got != "first" {
		t.Errorf("pick = %q; want first", got)
	}
	// "first" already claimed by an earlier pane → next earliest is "second".
	if got := pickEarliestUnclaimed(born, since, map[string]bool{"first": true}); got != "second" {
		t.Errorf("pick(first claimed) = %q; want second", got)
	}
	// Everything born after `since` is claimed → "" (the pre-existing "old" is
	// excluded by the since cutoff, not chosen).
	claimed := map[string]bool{"first": true, "second": true}
	if got := pickEarliestUnclaimed(born, since, claimed); got != "" {
		t.Errorf("pick(all recent claimed) = %q; want \"\"", got)
	}
	// Empty input → "".
	if got := pickEarliestUnclaimed(map[string]time.Time{}, since, nil); got != "" {
		t.Errorf("pick(empty) = %q; want \"\"", got)
	}
}

func TestShortUUID(t *testing.T) {
	if got := shortUUID("817b1c30-48a3-93af-fdd0"); got != "817b1c30" {
		t.Errorf("shortUUID = %q; want 817b1c30", got)
	}
	if got := shortUUID("nohyphen"); got != "nohyphen" {
		t.Errorf("shortUUID(nohyphen) = %q", got)
	}
	if got := shortUUID(""); got != "" {
		t.Errorf("shortUUID(empty) = %q", got)
	}
}
