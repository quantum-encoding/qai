package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForFile_ImmediateExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ready")
	if err := os.WriteFile(path, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := WaitForFile(path, 5*time.Second); err != nil {
		t.Fatalf("WaitForFile: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("returned slowly for already-existing file")
	}
}

func TestWaitForFile_AppearsLater(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "facts.json")

	go func() {
		time.Sleep(150 * time.Millisecond)
		os.WriteFile(path, []byte("{}"), 0644)
	}()

	start := time.Now()
	if err := WaitForFile(path, 3*time.Second); err != nil {
		t.Fatalf("WaitForFile: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned too quickly (%v) — fsnotify may not be working", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("returned too slowly (%v) — should have been notified ~150ms in", elapsed)
	}
}

func TestWaitForFile_Timeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "never-arrives")

	start := time.Now()
	err := WaitForFile(path, 200*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond || elapsed > 800*time.Millisecond {
		t.Errorf("timeout duration off: %v (wanted ~200ms)", elapsed)
	}
}

func TestWaitForFile_ParentAppearsLate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	subdir := filepath.Join(dir, "subdir-not-yet")
	target := filepath.Join(subdir, "target")

	go func() {
		time.Sleep(100 * time.Millisecond)
		os.MkdirAll(subdir, 0755)
		time.Sleep(100 * time.Millisecond)
		os.WriteFile(target, []byte("ok"), 0644)
	}()

	if err := WaitForFile(target, 3*time.Second); err != nil {
		t.Fatalf("WaitForFile: %v", err)
	}
}
