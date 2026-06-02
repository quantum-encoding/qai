package browser

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempSlotsPath redirects the slot file to a temp path for the
// test's lifetime and restores it on cleanup. Keeps the user's real
// ~/.qai/browser-slots.json untouched.
func withTempSlotsPath(t *testing.T) {
	t.Helper()
	prev := slotsPath
	dir := t.TempDir()
	slotsPath = filepath.Join(dir, "slots.json")
	t.Cleanup(func() { slotsPath = prev })
}

// TestSaveLoadRoundTrip — basic write + read.
func TestSaveLoadRoundTrip(t *testing.T) {
	withTempSlotsPath(t)
	if err := SaveSlot("TAB1", "abc123"); err != nil {
		t.Fatalf("SaveSlot: %v", err)
	}
	if err := SaveSlot("main", "def456"); err != nil {
		t.Fatalf("SaveSlot: %v", err)
	}
	m, err := LoadSlots(nil)
	if err != nil {
		t.Fatalf("LoadSlots: %v", err)
	}
	if m["TAB1"] != "abc123" || m["main"] != "def456" {
		t.Errorf("round-trip: got %+v", m)
	}
}

// TestPruneDropsStaleEntries — stale tab IDs are removed AND written
// back so subsequent reads are clean. The load-bearing contract that
// keeps slots accurate after manual tab closes.
func TestPruneDropsStaleEntries(t *testing.T) {
	withTempSlotsPath(t)
	_ = SaveSlot("TAB1", "abc123") // still live
	_ = SaveSlot("TAB2", "stale-id-not-in-browser")

	live := map[string]bool{"abc123": true}
	m, err := LoadSlots(live)
	if err != nil {
		t.Fatalf("LoadSlots: %v", err)
	}
	if _, ok := m["TAB1"]; !ok {
		t.Errorf("TAB1 was pruned; should remain")
	}
	if _, ok := m["TAB2"]; ok {
		t.Errorf("TAB2 stale entry should have been pruned")
	}

	// Re-read without the live map — TAB2 must be gone from disk too.
	m2, _ := LoadSlots(nil)
	if _, ok := m2["TAB2"]; ok {
		t.Errorf("TAB2 still present on disk after prune; expected write-back")
	}
}

// TestUnpinSlot + TestUnpinTabID.
func TestUnpin(t *testing.T) {
	withTempSlotsPath(t)
	_ = SaveSlot("TAB1", "abc")
	_ = SaveSlot("main", "abc") // both pinned to same tab

	if err := UnpinSlot("TAB1"); err != nil {
		t.Fatalf("UnpinSlot: %v", err)
	}
	m, _ := LoadSlots(nil)
	if _, ok := m["TAB1"]; ok {
		t.Errorf("TAB1 still present after UnpinSlot")
	}
	if m["main"] != "abc" {
		t.Errorf("main was incidentally removed: %+v", m)
	}

	// UnpinTabID drops every slot pinned to that tab
	if err := UnpinTabID("abc"); err != nil {
		t.Fatalf("UnpinTabID: %v", err)
	}
	m2, _ := LoadSlots(nil)
	if len(m2) != 0 {
		t.Errorf("expected empty map after UnpinTabID, got %+v", m2)
	}
}

// TestLooksLikeSlotName — the addressing dispatcher's load-bearing
// check. A full 32-hex tab ID must NOT be mistaken for a slot name
// (else `--tab abc...` would unsupported-collide with the slot file).
// A short hex prefix passes through too — existing scripts use those.
func TestLooksLikeSlotName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"TAB1", true},
		{"main", true},
		{"cj-research", true},
		{"abc123", true},                            // short hex prefix — treated as slot, but prefix-match downstream resolves it
		{"abc123def456abc123def456abc123gh", true},  // 32 chars but contains 'g'/'h' so not hex
		{"71BD792058B2801E21011A7EDE2D62B6", false}, // exact 32-hex ID
		{"71bd792058b2801e21011a7ede2d62b6", false}, // lowercase 32-hex
		{"", true},                                  // edge case; LookupResolver handles empty separately
	}
	for _, c := range cases {
		got := LooksLikeSlotName(c.in)
		if got != c.want {
			t.Errorf("LooksLikeSlotName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNoSlotsFileReturnsEmpty — first invocation on a fresh machine
// has no slots file. LoadSlots must return an empty map, not error.
func TestNoSlotsFileReturnsEmpty(t *testing.T) {
	withTempSlotsPath(t)
	// Don't write anything.
	m, err := LoadSlots(nil)
	if err != nil {
		t.Fatalf("LoadSlots on missing file: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %+v", m)
	}

	// Verify the file actually doesn't exist (we didn't accidentally
	// create it via a Save during the test).
	if _, err := os.Stat(slotsPath); !os.IsNotExist(err) {
		t.Errorf("file should not exist after read-only LoadSlots: %v", err)
	}
}

// TestFormatSlotList is stable (alpha-sorted) so two consecutive
// runs produce identical output — useful for diff-friendly logs.
func TestFormatSlotList(t *testing.T) {
	m := SlotMap{
		"TAB2": "deadbeefcafebabe",
		"TAB1": "abc12345",
		"main": "ff0011223344",
	}
	got := FormatSlotList(m)
	want := "TAB1=abc12345  TAB2=deadbeef  main=ff001122"
	if got != want {
		t.Errorf("FormatSlotList:\n got: %q\nwant: %q", got, want)
	}
}
