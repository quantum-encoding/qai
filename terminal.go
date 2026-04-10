package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func cmdTerminal(args []string) {
	if len(args) == 0 {
		termUsage()
		os.Exit(1)
	}

	action := args[0]
	rest := args[1:]

	switch action {
	case "help", "--help", "-h":
		termUsage()
		return
	case "list", "ls":
		termList()
	case "spawn", "new":
		termSpawn(rest)
	case "send", "write":
		termSend(rest)
	case "read":
		termRead(rest)
	case "close", "kill":
		termClose(rest)
	case "signal", "sig":
		termSignal(rest)
	case "resize":
		termResize(rest)
	case "snapshot", "snap":
		termSnapshot()
	case "wait":
		termWait(rest)
	default:
		fmt.Fprintf(os.Stderr, "qai term: unknown action %q\n", action)
		termUsage()
		os.Exit(1)
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-9;]*[a-zA-Z]|\].*?(?:\x1b\\|\x07)|\([B0])`)

func tmuxSession() string {
	if s := os.Getenv("TERMINAL_MCP_SESSION"); s != "" {
		return s
	}
	// Try to detect current tmux session
	if out, err := exec.Command("tmux", "display-message", "-p", "#S").CombinedOutput(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return "mcp-terminals"
}

func tmuxRun(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func ensureSession() {
	session := tmuxSession()
	if _, err := tmuxRun("has-session", "-t", session); err != nil {
		tmuxRun("new-session", "-d", "-s", session, "-x", "200", "-y", "50")
	}
}

func stripAnsi(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

func cleanName(s string) string {
	// Strip leading spinner chars
	s = strings.TrimLeft(s, "✳⏺✽ ")
	return strings.TrimSpace(s)
}

func findPane(name string) (string, error) {
	session := tmuxSession()
	out, err := tmuxRun("list-panes", "-s", "-t", session,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_current_command}\t#{pane_current_path}")
	if err != nil {
		return "", fmt.Errorf("no tmux session %q", session)
	}

	type pane struct{ id, title, cmd, path string }
	var panes []pane

	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) >= 2 {
			p := pane{id: parts[0], title: parts[1]}
			if len(parts) >= 3 { p.cmd = parts[2] }
			if len(parts) >= 4 { p.path = parts[3] }
			panes = append(panes, p)
		}
	}

	// Exact match on title
	for _, p := range panes {
		if p.title == name {
			return p.id, nil
		}
	}
	// Cleaned exact match
	for _, p := range panes {
		if cleanName(p.title) == name {
			return p.id, nil
		}
	}
	// Substring match
	lower := strings.ToLower(name)
	for _, p := range panes {
		if strings.Contains(strings.ToLower(p.title), lower) {
			return p.id, nil
		}
	}
	// Cleaned substring
	for _, p := range panes {
		if strings.Contains(strings.ToLower(cleanName(p.title)), lower) {
			return p.id, nil
		}
	}

	return "", fmt.Errorf("pane %q not found", name)
}

func capturePane(paneID string, lines int) string {
	out, _ := tmuxRun("capture-pane", "-p", "-t", paneID,
		"-S", fmt.Sprintf("-%d", lines))
	return stripAnsi(out)
}

// ── Actions ─────────────────────────────────────────────────────────────────

func termList() {
	ensureSession()
	session := tmuxSession()
	out, err := tmuxRun("list-panes", "-s", "-t", session,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_pid}\t#{pane_current_path}\t#{pane_current_command}\t#{pane_width}x#{pane_height}")
	if err != nil {
		fmt.Println("No active terminals")
		return
	}

	fmt.Printf("%-8s %-20s %-8s %-40s %-15s %s\n", "ID", "NAME", "PID", "CWD", "CMD", "SIZE")
	fmt.Println(strings.Repeat("-", 100))
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) >= 6 {
			cwd := parts[3]
			if strings.HasPrefix(cwd, home) {
				cwd = "~" + cwd[len(home):]
			}
			fmt.Printf("%-8s %-20s %-8s %-40s %-15s %s\n",
				parts[0], parts[1], parts[2], cwd, parts[4], parts[5])
		}
	}
}

func termSpawn(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai term spawn \"name\" [--cwd /path] [--cmd \"command\"] [--mode interactive|background|resume]")
		os.Exit(1)
	}

	name := args[0]
	cwd := ""
	command := ""
	mode := "interactive"
	shell := os.Getenv("SHELL")
	if shell == "" {
		// Platform-appropriate fallback.
		if _, err := exec.LookPath("bash"); err == nil {
			shell = "bash"
		} else if _, err := exec.LookPath("sh"); err == nil {
			shell = "sh"
		} else {
			shell = "bash" // last resort, let it fail with a clear error
		}
	}

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--cwd":
			if i+1 < len(args) { cwd = args[i+1]; i++ }
		case "--cmd":
			if i+1 < len(args) { command = args[i+1]; i++ }
		case "--mode":
			if i+1 < len(args) { mode = args[i+1]; i++ }
		}
	}

	if cwd != "" {
		cwd = strings.Replace(cwd, "~", home, 1)
	}

	ensureSession()
	session := tmuxSession()

	// Create new pane
	splitArgs := []string{"split-window", "-t", session, "-P", "-F", "#{pane_id}"}
	if cwd != "" {
		splitArgs = append(splitArgs, "-c", cwd)
	}
	paneID, err := tmuxRun(splitArgs...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: failed to create pane: %v\n", err)
		os.Exit(1)
	}
	paneID = strings.TrimSpace(paneID)

	// Set title
	tmuxRun("select-pane", "-t", paneID, "-T", name)
	// Tile layout
	tmuxRun("select-layout", "-t", session, "tiled")

	// Run command based on mode
	if command != "" {
		tmuxRun("send-keys", "-t", paneID, command, "Enter")
	} else if mode == "background" {
		cmd := "claude --allowedTools 'Bash(readonly),Edit,Read,Write,Glob,Grep,Agent'"
		tmuxRun("send-keys", "-t", paneID, cmd, "Enter")
	} else if mode == "resume" {
		tmuxRun("send-keys", "-t", paneID, "claude --resume", "Enter")
	}

	fmt.Printf("Spawned %q in %s\n", name, paneID)
}

func termSend(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai term send \"name\" \"input\" [--no-enter]")
		os.Exit(1)
	}

	paneID, err := findPane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	input := args[1]
	enter := true
	for _, a := range args[2:] {
		if a == "--no-enter" { enter = false }
	}

	// Use load-buffer for complex/long input
	if len(input) > 100 || strings.ContainsAny(input, "'\"\\$`") {
		tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("qai-term-%d", time.Now().UnixNano()))
		if err := os.WriteFile(tmpFile, []byte(input), 0600); err != nil {
			fmt.Fprintf(os.Stderr, "qai term: write temp file: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(tmpFile)
		tmuxRun("load-buffer", tmpFile)
		tmuxRun("paste-buffer", "-t", paneID)
		if enter {
			tmuxRun("send-keys", "-t", paneID, "Enter")
		}
	} else {
		sendArgs := []string{"send-keys", "-t", paneID, input}
		if enter {
			sendArgs = append(sendArgs, "Enter")
		}
		tmuxRun(sendArgs...)
	}

	// Brief pause then capture output
	time.Sleep(300 * time.Millisecond)
	output := capturePane(paneID, 30)
	if output != "" {
		fmt.Println(output)
	}
}

func termRead(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai term read \"name\" [--lines 50]")
		os.Exit(1)
	}

	paneID, err := findPane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	lines := 50
	for i := 1; i < len(args); i++ {
		if args[i] == "--lines" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &lines)
			i++
		}
	}
	if lines > 500 { lines = 500 }

	output := capturePane(paneID, lines)
	fmt.Println(output)
}

func termClose(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai term close \"name\" [--force]")
		os.Exit(1)
	}

	paneID, err := findPane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	force := false
	for _, a := range args[1:] {
		if a == "--force" { force = true }
	}

	if force {
		tmuxRun("kill-pane", "-t", paneID)
	} else {
		// Graceful: Ctrl-C, Ctrl-D, then kill if still alive
		tmuxRun("send-keys", "-t", paneID, "C-c")
		time.Sleep(200 * time.Millisecond)
		tmuxRun("send-keys", "-t", paneID, "C-d")
		time.Sleep(500 * time.Millisecond)
		// Check if still alive
		if _, err := tmuxRun("list-panes", "-t", paneID); err == nil {
			tmuxRun("kill-pane", "-t", paneID)
		}
	}

	fmt.Printf("Closed %q\n", args[0])
}

func termSignal(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai term signal \"name\" <ctrl-c|ctrl-d|ctrl-z|ctrl-l|ctrl-\\>")
		os.Exit(1)
	}

	paneID, err := findPane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	sigMap := map[string]string{
		"ctrl-c":  "C-c",
		"ctrl-d":  "C-d",
		"ctrl-z":  "C-z",
		"ctrl-l":  "C-l",
		"ctrl-\\": "C-\\",
	}

	key, ok := sigMap[args[1]]
	if !ok {
		fmt.Fprintf(os.Stderr, "qai term: unknown signal %q (use ctrl-c, ctrl-d, ctrl-z, ctrl-l, ctrl-\\)\n", args[1])
		os.Exit(1)
	}

	tmuxRun("send-keys", "-t", paneID, key)
}

func termResize(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai term resize \"name\" [--width N] [--height N]")
		os.Exit(1)
	}

	paneID, err := findPane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--width":
			if i+1 < len(args) {
				tmuxRun("resize-pane", "-t", paneID, "-x", args[i+1])
				i++
			}
		case "--height":
			if i+1 < len(args) {
				tmuxRun("resize-pane", "-t", paneID, "-y", args[i+1])
				i++
			}
		}
	}
}

func termSnapshot() {
	ensureSession()
	session := tmuxSession()
	out, err := tmuxRun("list-panes", "-s", "-t", session,
		"-F", "#{pane_id}\t#{pane_title}")
	if err != nil {
		fmt.Println("No active terminals")
		return
	}

	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) < 2 { continue }
		paneID, title := parts[0], parts[1]
		output := capturePane(paneID, 5)

		fmt.Printf("━━━ %s ━━━\n", title)
		if output != "" {
			fmt.Println(output)
		} else {
			fmt.Println("(no output)")
		}
		fmt.Println()
	}
}

func termWait(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai term wait \"name\" \"pattern\" [--timeout 30]")
		os.Exit(1)
	}

	paneID, err := findPane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	pattern := args[1]
	timeout := 30

	for i := 2; i < len(args); i++ {
		if args[i] == "--timeout" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &timeout)
			i++
		}
	}
	if timeout > 120 { timeout = 120 }

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)

	for time.Now().Before(deadline) {
		output := capturePane(paneID, 50)
		if strings.Contains(output, pattern) {
			fmt.Println("found")
			fmt.Println(output)
			return
		}
		time.Sleep(time.Second)
	}

	fmt.Fprintln(os.Stderr, "timeout")
	os.Exit(1)
}

// ── Usage ───────────────────────────────────────────────────────────────────

func termUsage() {
	fmt.Fprint(os.Stderr, `qai term — terminal management via tmux (fire-and-forget, no MCP server)

  qai term list                               List all active terminals
  qai term spawn "name" [--cwd /path] [--cmd "command"] [--mode M]
  qai term send "name" "input" [--no-enter]   Send input to terminal
  qai term read "name" [--lines 50]           Read terminal output
  qai term close "name" [--force]             Close terminal
  qai term signal "name" <ctrl-c|ctrl-d|...>  Send signal
  qai term resize "name" [--width N] [--height N]
  qai term snapshot                           Overview of all terminals
  qai term wait "name" "pattern" [--timeout 30]

Modes: interactive (default), background (auto-approve safe tools), resume
`)
}
