package fleet

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

// inboxIsolation redirects FleetDir to a tempdir for the duration of a
// test. Returns a cleanup func.
func inboxIsolation(t *testing.T, fleetID string) func() {
	t.Helper()
	tmp := t.TempDir()
	prev := config.Home
	config.Home = tmp
	if err := os.MkdirAll(filepath.Join(tmp, ".qai", "fleet", fleetID), 0o755); err != nil {
		t.Fatal(err)
	}
	return func() { config.Home = prev }
}

func TestAppendAndReadAll(t *testing.T) {
	cleanup := inboxIsolation(t, "f1")
	defer cleanup()

	for _, msg := range []string{"first", "second", "third"} {
		if err := AppendReport(Report{
			FleetID: "f1", Pane: "worker-1", Status: "info", Message: msg,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadAll("f1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 reports, got %d", len(got))
	}
	if got[0].Message != "first" || got[2].Message != "third" {
		t.Errorf("order wrong: %+v", got)
	}
}

func TestReadUnreadAdvancesCursor(t *testing.T) {
	cleanup := inboxIsolation(t, "f2")
	defer cleanup()

	for i := 1; i <= 3; i++ {
		AppendReport(Report{FleetID: "f2", Pane: "w", Status: "info", Message: string(rune('0' + i))})
	}

	first, err := ReadUnread("f2")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("first read: want 3, got %d", len(first))
	}

	second, err := ReadUnread("f2")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("second read should be empty (cursor advanced), got %d", len(second))
	}

	AppendReport(Report{FleetID: "f2", Pane: "w", Status: "info", Message: "fresh"})
	third, err := ReadUnread("f2")
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 || third[0].Message != "fresh" {
		t.Fatalf("third read: want [fresh], got %+v", third)
	}
}

func TestPeekUnreadDoesNotAdvance(t *testing.T) {
	cleanup := inboxIsolation(t, "f3")
	defer cleanup()

	AppendReport(Report{FleetID: "f3", Pane: "w", Status: "done", Message: "x"})

	a, err := PeekUnread("f3")
	if err != nil || len(a) != 1 {
		t.Fatalf("first peek: %v %+v", err, a)
	}
	b, err := PeekUnread("f3")
	if err != nil || len(b) != 1 {
		t.Fatalf("second peek should still see the same record: %v %+v", err, b)
	}
}

// TestConcurrentAppend confirms O_APPEND atomicity: 50 goroutines each
// posting 4 reports must produce 200 well-formed records, no torn lines.
func TestConcurrentAppend(t *testing.T) {
	cleanup := inboxIsolation(t, "f4")
	defer cleanup()

	const G, N = 50, 4
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				_ = AppendReport(Report{
					FleetID: "f4",
					Pane:    "w",
					Status:  "progress",
					// long-ish message to maximise the chance of a torn write if O_APPEND were broken
					Message: "this is a deliberately wordy message to inflate the record so concurrent appenders have a chance to interleave bytes if the platform's O_APPEND semantics betray us",
				})
			}
		}(g)
	}
	wg.Wait()

	got, err := ReadAll("f4")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != G*N {
		t.Fatalf("want %d reports, got %d (some torn / lost)", G*N, len(got))
	}
}

func TestAdvanceCursorMissingFile(t *testing.T) {
	cleanup := inboxIsolation(t, "f5")
	defer cleanup()
	// No inbox file yet — should not error.
	if err := AdvanceCursor("f5"); err != nil {
		t.Fatalf("AdvanceCursor on missing inbox: %v", err)
	}
}

func TestAppendCreatesInboxIfMissing(t *testing.T) {
	cleanup := inboxIsolation(t, "f6")
	defer cleanup()
	// Removed dir — Append should re-create.
	os.RemoveAll(FleetDir("f6"))
	if err := AppendReport(Report{FleetID: "f6", Pane: "w", Status: "info", Message: "x"}); err != nil {
		t.Fatal(err)
	}
}
