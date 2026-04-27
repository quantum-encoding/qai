// watcher.go — wait_for resolution via filesystem events.
//
// `wait_for: <path>` is the only orchestration primitive in a fleet
// manifest. A pane with this field set holds back its prompt until the
// path appears, then sends. Filesystem is the IPC bus.
//
// Implementation watches the parent directory with fsnotify rather than
// polling, so 14 panes (or 200) waiting on different files is cheap. If
// the parent itself doesn't exist yet, we poll for it briefly before
// giving up — a fleet manifest writer can always pre-create the parent.

package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WaitForFile blocks until path exists (file or dir) or timeout elapses.
// Returns nil on success.
//
// Order of checks:
//   1. If the path exists already, return immediately (TOCTOU friendly).
//   2. If the parent directory exists, fsnotify-watch it for Create/Write/Rename.
//   3. If the parent doesn't exist, poll for it every 250ms up to 1/4 of the
//      timeout, then re-evaluate. A fleet usually pre-creates parents, so
//      this is a fallback.
func WaitForFile(path string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %q: %v", path, err)
	}

	deadline := time.Now().Add(timeout)
	if _, err := os.Stat(abs); err == nil {
		return nil
	}

	parent := filepath.Dir(abs)
	if err := waitParentExists(parent, deadline); err != nil {
		return err
	}

	// Re-stat: parent now exists; the target may have appeared in the
	// gap between stat() and watcher setup.
	if _, err := os.Stat(abs); err == nil {
		return nil
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watcher: %v", err)
	}
	defer w.Close()
	if err := w.Add(parent); err != nil {
		return fmt.Errorf("watch %q: %v", parent, err)
	}

	// One more stat after Add — the file might have appeared between our
	// re-stat and Add. fsnotify only notifies on events strictly after
	// Add returns.
	if _, err := os.Stat(abs); err == nil {
		return nil
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timeout waiting for %q", path)
		}
		select {
		case <-time.After(remaining):
			return fmt.Errorf("timeout waiting for %q", path)
		case ev, ok := <-w.Events:
			if !ok {
				return fmt.Errorf("watcher closed unexpectedly")
			}
			// We get notified for siblings too; verify the target.
			if filepath.Clean(ev.Name) != abs {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
				if _, err := os.Stat(abs); err == nil {
					return nil
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return fmt.Errorf("watcher error channel closed")
			}
			return fmt.Errorf("watcher error: %v", err)
		}
	}
}

// waitParentExists polls for the parent directory to appear. Cheap-and-
// cheerful — only runs while the parent dir is missing, which is rare.
func waitParentExists(parent string, deadline time.Time) error {
	for {
		if info, err := os.Stat(parent); err == nil && info.IsDir() {
			return nil
		}
		// Cap parent-poll budget to a quarter of the remaining timeout so
		// we still have time to fsnotify-watch the file once the parent
		// shows up.
		budget := time.Until(deadline) / 4
		if budget <= 0 {
			return fmt.Errorf("parent %q did not appear before deadline", parent)
		}
		sleepFor := 250 * time.Millisecond
		if sleepFor > budget {
			sleepFor = budget
		}
		time.Sleep(sleepFor)
	}
}
