// ops.go — library API for tmux terminal operations.
//
// The CLI in terminal.go (CmdTerminal) is one consumer; qai fleet is another.
// Operations here return values + errors instead of printing or os.Exit-ing,
// so callers control formatting and lifecycle.

package terminal

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

// ─── types ────────────────────────────────────────────────────────────────

// Pane describes a single tmux pane in the qai-managed session.
type Pane struct {
	ID     string // tmux pane id like "%4"
	Name   string // pane title, possibly with leading spinner chars
	PID    int    // foreground process pid
	Cwd    string
	Cmd    string // current command (claude.exe, zsh, …)
	Width  int
	Height int
}

// PaneSnapshot pairs a pane title with a recent N-line capture of its output.
type PaneSnapshot struct {
	ID     string
	Title  string
	Output string // stripped of ANSI, last few lines
}

// SpawnOpts controls how a new pane is created.
//
//   - Name is set as the pane title. Used for later lookup by Send/Read/etc.
//   - Cwd, if non-empty, becomes the new pane's working directory.
//   - Cmd, if non-empty, is sent (with Enter) immediately after spawn.
//   - Mode selects a default command when Cmd is empty:
//     "interactive" (default) leaves the shell prompt as-is,
//     "background"  starts claude with a read-only tool whitelist,
//     "resume"      starts `claude --resume`.
type SpawnOpts struct {
	Name string
	Cwd  string
	Cmd  string
	Mode string
}

// ─── operations ───────────────────────────────────────────────────────────

// Spawn creates a new pane in the qai-managed tmux session and returns its
// pane id (e.g. "%4"). Idempotent on session creation.
func Spawn(opts SpawnOpts) (string, error) {
	if opts.Name == "" {
		return "", fmt.Errorf("spawn: name is required")
	}

	cwd := opts.Cwd
	if cwd != "" {
		cwd = strings.Replace(cwd, "~", config.Home, 1)
	}

	ensureSession()
	session := tmuxSession()

	args := []string{"split-window", "-t", session, "-P", "-F", "#{pane_id}"}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	paneID, err := tmuxRun(args...)
	if err != nil {
		return "", fmt.Errorf("split-window: %v", err)
	}
	paneID = strings.TrimSpace(paneID)

	tmuxRun("select-pane", "-t", paneID, "-T", opts.Name)
	tmuxRun("select-layout", "-t", session, "tiled")

	switch {
	case opts.Cmd != "":
		tmuxRun("send-keys", "-t", paneID, opts.Cmd, "Enter")
	case opts.Mode == "background":
		tmuxRun("send-keys", "-t", paneID,
			"claude --allowedTools 'Bash(readonly),Edit,Read,Write,Glob,Grep,Agent'", "Enter")
	case opts.Mode == "resume":
		tmuxRun("send-keys", "-t", paneID, "claude --resume", "Enter")
	}

	return paneID, nil
}

// Send writes input to a pane. The pane argument is matched in this order:
// exact title, cleaned title, substring (case-insensitive), or pane id ("%4").
// If withEnter is true, an Enter is appended.
//
// Long input or input containing shell-special chars is loaded into a tmux
// buffer and pasted, avoiding shell quoting issues.
func Send(pane, input string, withEnter bool) error {
	paneID, err := resolvePane(pane)
	if err != nil {
		return err
	}

	if len(input) > 100 || strings.ContainsAny(input, "'\"\\$`") {
		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("qai-term-%d", time.Now().UnixNano()))
		if err := os.WriteFile(tmpFile, []byte(input), 0600); err != nil {
			return fmt.Errorf("write temp file: %v", err)
		}
		defer os.Remove(tmpFile)
		if _, err := tmuxRun("load-buffer", tmpFile); err != nil {
			return fmt.Errorf("load-buffer: %v", err)
		}
		// -p enables bracketed-paste mode (wraps the bytes in
		// `\e[200~ ... \e[201~`). Without it, embedded newlines arrive
		// as keystrokes and Claude Code treats the input as a
		// multi-line compose; the follow-up Enter just appends a line
		// instead of submitting. With bracketed paste, Claude Code
		// recognises a paste block and the trailing Enter submits.
		if _, err := tmuxRun("paste-buffer", "-p", "-t", paneID); err != nil {
			return fmt.Errorf("paste-buffer: %v", err)
		}
		if withEnter {
			// Claude Code shows multi-line pastes as `[Pasted text +N
			// lines]` and only accepts Enter as "submit" once the
			// paste is fully rendered. The render tick varies (200ms
			// works sometimes, fails others under load), so we send
			// Enter twice with a gap. A second Enter on an already-
			// submitted (empty) prompt is a no-op in Claude Code.
			time.Sleep(500 * time.Millisecond)
			tmuxRun("send-keys", "-t", paneID, "Enter")
			time.Sleep(500 * time.Millisecond)
			tmuxRun("send-keys", "-t", paneID, "Enter")
		}
		return nil
	}

	// Short path: send the literal text first, then Enter separately
	// after a settle delay. Same reliability story as the paste path —
	// rapid back-to-back text+Enter via a single send-keys can be
	// interpreted by Claude Code as keystroke flood + held submit.
	if _, err := tmuxRun("send-keys", "-t", paneID, input); err != nil {
		return err
	}
	if withEnter {
		time.Sleep(300 * time.Millisecond)
		tmuxRun("send-keys", "-t", paneID, "Enter")
		time.Sleep(300 * time.Millisecond)
		tmuxRun("send-keys", "-t", paneID, "Enter")
	}
	return nil
}

// Read returns the last N lines of a pane's output (ANSI stripped).
// Lines is clamped to 500.
func Read(pane string, lines int) (string, error) {
	if lines <= 0 {
		lines = 50
	}
	if lines > 500 {
		lines = 500
	}
	paneID, err := resolvePane(pane)
	if err != nil {
		return "", err
	}
	return capturePane(paneID, lines), nil
}

// Close terminates a pane. If force is true, kills immediately.
// Otherwise tries Ctrl-C → Ctrl-D → kill if the pane is still alive.
func Close(pane string, force bool) error {
	paneID, err := resolvePane(pane)
	if err != nil {
		return err
	}
	if force {
		_, err := tmuxRun("kill-pane", "-t", paneID)
		return err
	}
	tmuxRun("send-keys", "-t", paneID, "C-c")
	time.Sleep(200 * time.Millisecond)
	tmuxRun("send-keys", "-t", paneID, "C-d")
	time.Sleep(500 * time.Millisecond)
	if _, err := tmuxRun("list-panes", "-t", paneID); err == nil {
		tmuxRun("kill-pane", "-t", paneID)
	}
	return nil
}

// Signal sends a control sequence to a pane.
// Valid: ctrl-c, ctrl-d, ctrl-z, ctrl-l, ctrl-\.
func Signal(pane, sig string) error {
	sigMap := map[string]string{
		"ctrl-c":  "C-c",
		"ctrl-d":  "C-d",
		"ctrl-z":  "C-z",
		"ctrl-l":  "C-l",
		"ctrl-\\": "C-\\",
	}
	key, ok := sigMap[sig]
	if !ok {
		return fmt.Errorf("unknown signal %q (ctrl-c|ctrl-d|ctrl-z|ctrl-l|ctrl-\\)", sig)
	}
	paneID, err := resolvePane(pane)
	if err != nil {
		return err
	}
	_, err = tmuxRun("send-keys", "-t", paneID, key)
	return err
}

// Resize sets pane dimensions. Pass 0 to leave a dimension unchanged.
func Resize(pane string, width, height int) error {
	paneID, err := resolvePane(pane)
	if err != nil {
		return err
	}
	if width > 0 {
		if _, err := tmuxRun("resize-pane", "-t", paneID, "-x", strconv.Itoa(width)); err != nil {
			return err
		}
	}
	if height > 0 {
		if _, err := tmuxRun("resize-pane", "-t", paneID, "-y", strconv.Itoa(height)); err != nil {
			return err
		}
	}
	return nil
}

// List enumerates all panes in the qai-managed session.
func List() ([]Pane, error) {
	ensureSession()
	session := tmuxSession()
	out, err := tmuxRun("list-panes", "-s", "-t", session,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_pid}\t#{pane_current_path}\t#{pane_current_command}\t#{pane_width}\t#{pane_height}")
	if err != nil {
		return nil, fmt.Errorf("list-panes: %v", err)
	}
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 7)
		if len(parts) < 7 {
			continue
		}
		pid, _ := strconv.Atoi(parts[2])
		w, _ := strconv.Atoi(parts[5])
		h, _ := strconv.Atoi(parts[6])
		panes = append(panes, Pane{
			ID:     parts[0],
			Name:   parts[1],
			PID:    pid,
			Cwd:    parts[3],
			Cmd:    parts[4],
			Width:  w,
			Height: h,
		})
	}
	return panes, nil
}

// Snapshot returns a short tail of every pane's recent output.
func Snapshot() ([]PaneSnapshot, error) {
	ensureSession()
	session := tmuxSession()
	out, err := tmuxRun("list-panes", "-s", "-t", session, "-F", "#{pane_id}\t#{pane_title}")
	if err != nil {
		return nil, fmt.Errorf("list-panes: %v", err)
	}
	var snaps []PaneSnapshot
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) < 2 {
			continue
		}
		snaps = append(snaps, PaneSnapshot{
			ID:     parts[0],
			Title:  parts[1],
			Output: capturePane(parts[0], 5),
		})
	}
	return snaps, nil
}

// Wait polls a pane's output until pattern appears or timeout elapses.
// Returns the captured output that contained the pattern.
func Wait(pane, pattern string, timeout time.Duration) (string, error) {
	paneID, err := resolvePane(pane)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output := capturePane(paneID, 50)
		if strings.Contains(output, pattern) {
			return output, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("timeout waiting for %q in pane %q", pattern, pane)
}

// resolvePane accepts either a tmux pane id ("%4") or a name and returns
// the canonical pane id.
func resolvePane(pane string) (string, error) {
	if strings.HasPrefix(pane, "%") {
		return pane, nil
	}
	return findPane(pane)
}
