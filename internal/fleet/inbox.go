// inbox.go — append-only JSONL inbox for worker→architect reports.
//
// The store is `~/.qai/fleet/<id>/inbox.jsonl`. Workers call qai report
// to append; the architect calls qai fleet inbox to read. A cursor file
// next to the inbox tracks bytes-consumed so --unread is stateless on
// the reader side.
//
// Concurrency model: O_APPEND on POSIX is atomic up to PIPE_BUF (>=4KB),
// which is larger than any single Report we'll ever write. Multiple
// workers appending in parallel never interleave their bytes; the worst
// case is interleaved *records*, which is fine — order isn't load-bearing.

package fleet

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

// Report is one structured status line a worker can post to the inbox.
//
// Status values: "done" | "blocked" | "progress" | "info". Other values
// are accepted but the notifier's progress-suppression logic only knows
// about these four.
type Report struct {
	TS        time.Time `json:"ts"`
	FleetID   string    `json:"fleet_id"`
	Pane      string    `json:"pane"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	Important bool      `json:"important,omitempty"`
}

// FleetDir returns the per-fleet state directory, creating it if missing.
// Layout:
//
//	~/.qai/fleet/<id>/inbox.jsonl
//	~/.qai/fleet/<id>/cursor
//	~/.qai/fleet/<id>/architect-pane
//	~/.qai/fleet/<id>/notifier.pid
func FleetDir(fleetID string) string {
	return filepath.Join(config.Home, ".qai", "fleet", fleetID)
}

// EnsureFleetDir creates the per-fleet state directory.
func EnsureFleetDir(fleetID string) (string, error) {
	dir := FleetDir(fleetID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %v", dir, err)
	}
	return dir, nil
}

// AppendReport writes a single report record to inbox.jsonl. Atomic via
// O_APPEND — multiple processes can call this concurrently without locks.
func AppendReport(r Report) error {
	if r.FleetID == "" {
		return fmt.Errorf("AppendReport: fleet_id required")
	}
	dir, err := EnsureFleetDir(r.FleetID)
	if err != nil {
		return err
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal report: %v", err)
	}
	line = append(line, '\n')

	path := filepath.Join(dir, "inbox.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open inbox: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write inbox: %v", err)
	}
	return nil
}

// ReadAll returns every report ever written to the inbox.
func ReadAll(fleetID string) ([]Report, error) {
	return readFrom(fleetID, 0)
}

// ReadUnread returns reports written since the last call, advances the
// cursor, and returns the records. If the inbox is empty or has no new
// data, returns an empty slice and a nil error.
func ReadUnread(fleetID string) ([]Report, error) {
	cursor, err := readCursor(fleetID)
	if err != nil {
		return nil, err
	}
	reports, newCursor, err := readFromTracked(fleetID, cursor)
	if err != nil {
		return nil, err
	}
	if newCursor != cursor {
		if err := writeCursor(fleetID, newCursor); err != nil {
			return nil, fmt.Errorf("advance cursor: %v", err)
		}
	}
	return reports, nil
}

// PeekUnread returns reports past the cursor *without* advancing it.
// Useful for the notifier, which wants to see what's pending without
// consuming the records on the architect's behalf.
func PeekUnread(fleetID string) ([]Report, error) {
	cursor, err := readCursor(fleetID)
	if err != nil {
		return nil, err
	}
	reports, _, err := readFromTracked(fleetID, cursor)
	return reports, err
}

// AdvanceCursor pushes the cursor to the current end-of-inbox without
// returning records. The architect uses this when it has decided to
// ignore everything pending.
func AdvanceCursor(fleetID string) error {
	path := filepath.Join(FleetDir(fleetID), "inbox.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return writeCursor(fleetID, 0)
		}
		return err
	}
	return writeCursor(fleetID, info.Size())
}

// readFrom returns reports from a given byte offset to the end of the
// inbox. Used by ReadAll (offset=0) and the unread paths.
func readFrom(fleetID string, offset int64) ([]Report, error) {
	reports, _, err := readFromTracked(fleetID, offset)
	return reports, err
}

func readFromTracked(fleetID string, offset int64) ([]Report, int64, error) {
	path := filepath.Join(FleetDir(fleetID), "inbox.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open inbox: %v", err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		// cursor may be ahead of file size if the inbox was truncated /
		// rotated; reset to start of file and read everything.
		if _, err := f.Seek(0, 0); err != nil {
			return nil, 0, fmt.Errorf("seek inbox: %v", err)
		}
		offset = 0
	}

	var reports []Report
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	pos := offset
	for scanner.Scan() {
		line := scanner.Bytes()
		// scanner consumed one trailing \n we have to credit back.
		pos += int64(len(line)) + 1
		if len(line) == 0 {
			continue
		}
		var r Report
		if err := json.Unmarshal(line, &r); err != nil {
			// Skip corrupt lines but keep advancing — robustness over
			// strictness; an architect that can't read the inbox is
			// worse than one that ignores a bad record.
			continue
		}
		reports = append(reports, r)
	}
	if err := scanner.Err(); err != nil {
		return reports, pos, fmt.Errorf("scan inbox: %v", err)
	}
	return reports, pos, nil
}

func readCursor(fleetID string) (int64, error) {
	path := filepath.Join(FleetDir(fleetID), "cursor")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cursor: %v", err)
	}
	if n < 0 {
		return 0, nil
	}
	return n, nil
}

func writeCursor(fleetID string, pos int64) error {
	if _, err := EnsureFleetDir(fleetID); err != nil {
		return err
	}
	path := filepath.Join(FleetDir(fleetID), "cursor")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(pos, 10)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
