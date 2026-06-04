package terminal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"regexp"
	"strings"
	"time"
)

func CmdTerminal(args []string) {
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
	// Direct tmux pane id pass-through. `%N` is tmux's canonical id
	// syntax — when the caller already has one (e.g. from
	// `tmux list-panes`, panes.json, or a fleet recovery flow), there
	// is no point doing a list-and-match round-trip. Returns the id
	// verbatim so every `qai term <verb> <pane>` accepts ids.
	if strings.HasPrefix(name, "%") && len(name) > 1 {
		return name, nil
	}

	session := tmuxSession()
	out, err := tmuxRun("list-panes", "-s", "-t", session,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_current_command}\t#{pane_current_path}")
	if err != nil {
		return "", fmt.Errorf("no tmux session %q (probe with `tmux ls`; set TERMINAL_MCP_SESSION to override the session name)", session)
	}

	type pane struct{ id, title, cmd, path string }
	var panes []pane

	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) >= 2 {
			p := pane{id: parts[0], title: parts[1]}
			if len(parts) >= 3 {
				p.cmd = parts[2]
			}
			if len(parts) >= 4 {
				p.path = parts[3]
			}
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

	return "", fmt.Errorf("pane %q not found in tmux session %q (list active panes with `qai term list` or `tmux list-panes -s -t %s`)", name, session, session)
}

func capturePane(paneID string, lines int) string {
	out, _ := tmuxRun("capture-pane", "-p", "-t", paneID,
		"-S", fmt.Sprintf("-%d", lines))
	return stripAnsi(out)
}

// ── Actions ─────────────────────────────────────────────────────────────────

func termList() {
	ensureSession()
	PruneDead()        // drop registry records for panes that have gone away
	BackfillSessions() // fill in local session ids for panes whose transcripts now exist
	session := tmuxSession()
	out, err := tmuxRun("list-panes", "-s", "-t", session,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_pid}\t#{pane_current_path}\t#{pane_current_command}\t#{pane_width}x#{pane_height}")
	if err != nil {
		fmt.Println("No active terminals")
		return
	}

	byID := recordsByID()
	fmt.Printf("%-8s %-20s %-8s %-32s %-12s %-9s %s\n", "ID", "NAME", "PID", "CWD", "CMD", "SIZE", "SESSION")
	fmt.Println(strings.Repeat("-", 110))
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 6)
		if len(parts) < 6 {
			continue
		}
		cwd := parts[3]
		if strings.HasPrefix(cwd, config.Home) {
			cwd = "~" + cwd[len(config.Home):]
		}
		sess := ""
		if rec, ok := byID[parts[0]]; ok {
			sess = shortUUID(rec.SessionID)
		}
		fmt.Printf("%-8s %-20s %-8s %-32s %-12s %-9s %s\n",
			parts[0], parts[1], parts[2], cwd, parts[4], parts[5], sess)
		// Remote Control URL (only present when the pane's session has it
		// active) is long — print it on an indented continuation line.
		if rc := GetPaneRemoteControlURL(parts[0]); rc != "" {
			fmt.Printf("%-8s rc → %s\n", "", rc)
		}
	}
}

func termSpawn(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai term spawn \"name\" [--cwd /path] [--cmd \"command\"] [--mode interactive|background|resume]")
		os.Exit(1)
	}

	opts := SpawnOpts{Name: args[0], Mode: "interactive"}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--cwd":
			if i+1 < len(args) {
				opts.Cwd = args[i+1]
				i++
			}
		case "--cmd":
			if i+1 < len(args) {
				opts.Cmd = args[i+1]
				i++
			}
		case "--mode":
			if i+1 < len(args) {
				opts.Mode = args[i+1]
				i++
			}
		}
	}

	// Delegate to the library Spawn, which handles the split, title, retile,
	// command, and name→id registration in one place. The local session id is
	// backfilled lazily on the next list/snapshot, so spawn never blocks.
	paneID, err := Spawn(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: spawn %q failed: %v\n", opts.Name, err)
		fmt.Fprintln(os.Stderr, "  -> fix: is tmux running? probe with `tmux ls`. Start a server with `tmux new-session -d -s mcp-terminals` (or set TERMINAL_MCP_SESSION to an existing session).")
		os.Exit(1)
	}

	fmt.Printf("Spawned %q in %s\n", opts.Name, paneID)
	if rec, ok := AllRecords()[opts.Name]; ok && rec.SessionID != "" {
		fmt.Printf("  session %s\n", rec.SessionID)
	}
}

func termSend(args []string) {
	// Batch mode: `qai term send --json [file]` reads one JSON payload
	// (stdin by default, or a file path / "-") and fans a shared body +
	// per-pane messages out to many panes in a single invocation. See
	// termSendBatch for the schema.
	// A leading flag (`--json`, `--fail-fast`) selects batch mode. Plain
	// send takes a pane name first, which never starts with "--".
	if len(args) >= 1 && strings.HasPrefix(args[0], "--") {
		termSendBatch(args)
		return
	}

	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai term send \"name\" \"input\" [--no-enter]")
		fmt.Fprintln(os.Stderr, "       qai term send --json [--fail-fast] [file]   (batch: shared body + per-pane messages)")
		os.Exit(1)
	}

	paneID, err := resolvePane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	input := args[1]
	enter := true
	for _, a := range args[2:] {
		if a == "--no-enter" {
			enter = false
		}
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

// batchPayload is the JSON schema for `qai term send --json`.
//
//	{
//	  "shared": "common preamble ...",   // prepended to every message,
//	                                       // OR, if it contains {{message}},
//	                                       // the per-pane message is substituted there
//	  "shared_suffix": "... trailer",     // optional, appended to every message
//	  "enter": true,                       // default submit behaviour (default true)
//	  "messages": [
//	    { "pane": "worker-1", "message": "do X" },
//	    { "pane": "%7", "message": "do Y", "enter": false }
//	  ]
//	}
type batchPayload struct {
	Shared       string         `json:"shared"`
	SharedSuffix string         `json:"shared_suffix"`
	Enter        *bool          `json:"enter"`
	Messages     []batchMessage `json:"messages"`
}

type batchMessage struct {
	Pane    string `json:"pane"`
	Message string `json:"message"`
	Enter   *bool  `json:"enter"` // per-pane override of the top-level default
}

// mergeMessage combines the shared body with a per-pane message and
// substitutes template tokens. If shared contains {{message}} the per-pane
// message is substituted there; otherwise shared is a prefix. shared_suffix,
// if set, is always appended. {{pane}} (anywhere in the result) becomes the
// pane identifier. Empty parts are dropped so we never emit stray blank lines.
func mergeMessage(shared, suffix, msg, pane string) string {
	var body string
	if strings.Contains(shared, "{{message}}") {
		body = strings.ReplaceAll(shared, "{{message}}", msg)
	} else {
		body = joinNonEmpty("\n\n", shared, msg)
	}
	body = joinNonEmpty("\n\n", body, suffix)
	body = strings.ReplaceAll(body, "{{pane}}", pane)
	return body
}

// joinNonEmpty joins only the non-empty parts with sep, so an absent shared
// body or suffix doesn't leave a double newline in the merged message.
func joinNonEmpty(sep string, parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func termSendBatch(args []string) {
	// Parse flags. --json is the mode selector; --fail-fast aborts on the
	// first send error; any remaining positional is the payload file
	// ("-" or none means stdin). Flags may appear in any order.
	failFast := false
	hasJSON := false
	file := ""
	for _, a := range args {
		switch {
		case a == "--json":
			hasJSON = true
		case a == "--fail-fast":
			failFast = true
		case a == "-":
			file = "" // explicit stdin
		default:
			file = a // payload file path
		}
	}
	if !hasJSON {
		fmt.Fprintln(os.Stderr, "qai term: batch mode requires --json")
		fmt.Fprintln(os.Stderr, "usage: qai term send --json [--fail-fast] [file]")
		os.Exit(1)
	}

	// Resolve the input source.
	var raw []byte
	var err error
	if file != "" {
		raw, err = os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai term: read %q: %v\n", file, err)
			os.Exit(1)
		}
	} else {
		raw, err = io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai term: read stdin: %v\n", err)
			os.Exit(1)
		}
	}

	var payload batchPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		fmt.Fprintf(os.Stderr, "qai term: parse JSON payload: %v\n", err)
		fmt.Fprintln(os.Stderr, "  -> expected: {\"shared\": \"...\", \"messages\": [{\"pane\": \"name\", \"message\": \"...\"}]}")
		os.Exit(1)
	}
	if len(payload.Messages) == 0 {
		fmt.Fprintln(os.Stderr, "qai term: payload has no messages")
		os.Exit(1)
	}

	defaultEnter := true
	if payload.Enter != nil {
		defaultEnter = *payload.Enter
	}

	// Pre-flight: resolve every pane before touching any. The batch is
	// atomic — if a single pane can't be resolved we report all the
	// failures and exit without sending anything, so a typo in the last
	// message doesn't leave the first N panes half-driven.
	paneIDs := make([]string, len(payload.Messages))
	var preflight []string
	for i, m := range payload.Messages {
		if m.Pane == "" {
			preflight = append(preflight, fmt.Sprintf("message %d: empty \"pane\"", i))
			continue
		}
		id, err := resolvePane(m.Pane)
		if err != nil {
			preflight = append(preflight, fmt.Sprintf("%s: %v", m.Pane, err))
			continue
		}
		paneIDs[i] = id
	}
	if len(preflight) > 0 {
		fmt.Fprintln(os.Stderr, "qai term: pre-flight failed, nothing sent:")
		for _, e := range preflight {
			fmt.Fprintf(os.Stderr, "  ✗ %s\n", e)
		}
		os.Exit(1)
	}

	// Serial fan-out. tmux's paste buffer is a single global clipboard, so
	// concurrent sends would stomp each other — we go one pane at a time.
	// (Send uses a per-pane named buffer to avoid clobbering the user's own
	// clipboard mid-run.) Panes are pre-resolved above, so the only error
	// here is a send/paste failure.
	failures := 0
	for i, m := range payload.Messages {
		enter := defaultEnter
		if m.Enter != nil {
			enter = *m.Enter
		}
		body := mergeMessage(payload.Shared, payload.SharedSuffix, m.Message, m.Pane)
		if err := Send(paneIDs[i], body, enter); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s (%s): %v\n", m.Pane, paneIDs[i], err)
			failures++
			if failFast {
				fmt.Fprintf(os.Stderr, "qai term: --fail-fast — aborting after %d/%d sent\n", i, len(payload.Messages))
				os.Exit(1)
			}
			continue
		}
		fmt.Printf("✓ %s (%s)\n", m.Pane, paneIDs[i])
	}

	fmt.Printf("sent %d/%d\n", len(payload.Messages)-failures, len(payload.Messages))
	if failures > 0 {
		os.Exit(1)
	}
}

func termRead(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai term read \"name\" [--lines 50]")
		os.Exit(1)
	}

	paneID, err := resolvePane(args[0])
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
	if lines > 500 {
		lines = 500
	}

	output := capturePane(paneID, lines)
	fmt.Println(output)
}

func termClose(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai term close \"name\" [--force]")
		os.Exit(1)
	}

	paneID, err := resolvePane(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai term: %v\n", err)
		os.Exit(1)
	}

	force := false
	for _, a := range args[1:] {
		if a == "--force" {
			force = true
		}
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

	// Drop the registry record (by the ref the user gave, or by the resolved id).
	_ = UnregisterPaneRef(args[0])
	_ = UnregisterPaneRef(paneID)

	fmt.Printf("Closed %q\n", args[0])
}

func termSignal(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai term signal \"name\" <ctrl-c|ctrl-d|ctrl-z|ctrl-l|ctrl-\\>")
		os.Exit(1)
	}

	paneID, err := resolvePane(args[0])
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

	paneID, err := resolvePane(args[0])
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

	BackfillSessions()
	byID := recordsByID()
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) < 2 {
			continue
		}
		paneID, title := parts[0], parts[1]
		output := capturePane(paneID, 5)

		header := fmt.Sprintf("━━━ %s [%s]", title, paneID)
		if rec, ok := byID[paneID]; ok && rec.SessionID != "" {
			header += fmt.Sprintf(" session=%s", shortUUID(rec.SessionID))
		}
		if rc := GetPaneRemoteControlURL(paneID); rc != "" {
			header += fmt.Sprintf(" rc=%s", rc)
		}
		fmt.Printf("%s ━━━\n", header)
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

	paneID, err := resolvePane(args[0])
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
	if timeout > 120 {
		timeout = 120
	}

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

	fmt.Fprintf(os.Stderr, "qai term: timed out after %ds waiting for %q in pane %q\n", timeout, pattern, args[0])
	fmt.Fprintln(os.Stderr, "  -> fix: raise --timeout, broaden the pattern, or read the pane with `qai term read \"<name>\"` to see what's there")
	os.Exit(1)
}

// ── Usage ───────────────────────────────────────────────────────────────────

func termUsage() {
	fmt.Fprint(os.Stderr, `qai term — terminal management via tmux (fire-and-forget, no MCP server)

  qai term list                               List all active terminals
  qai term spawn "name" [--cwd /path] [--cmd "command"] [--mode M]
  qai term send "name" "input" [--no-enter]   Send input to terminal
  qai term send --json [--fail-fast] [file]   Batch: shared body + per-pane messages (stdin or file)
  qai term read "name" [--lines 50]           Read terminal output
  qai term close "name" [--force]             Close terminal
  qai term signal "name" <ctrl-c|ctrl-d|...>  Send signal
  qai term resize "name" [--width N] [--height N]
  qai term snapshot                           Overview of all terminals
  qai term wait "name" "pattern" [--timeout 30]

Modes: interactive (default), background (auto-approve safe tools), resume

Pane names survive Claude Code's title rewrites: every spawn records a
name→pane-id map at ~/.qai/term-panes.json, consulted before title matching, so
send/read/close "name" keep working after a pane becomes "✳ Claude Code".
list and snapshot show each pane's local Claude session id (and a Remote
Control URL if one is active), backfilled lazily once the session exists.

Batch JSON (qai term send --json [--fail-fast] [file]):
  {
    "shared": "common preamble (use {{message}} to place each pane's text inline)",
    "shared_suffix": "optional trailer appended to every message",
    "enter": true,
    "messages": [
      { "pane": "worker-1", "message": "individual task" },
      { "pane": "%7", "message": "another task", "enter": false }
    ]
  }
  Each pane gets shared + message (or {{message}} substituted) + shared_suffix.
  Tokens: {{message}} = the pane's message, {{pane}} = the pane name/id.
  All panes are resolved up front — if any is unknown, nothing is sent.
  --fail-fast aborts on the first send error (default: send all, report ✓/✗).
  Panes are sent serially via a per-pane named buffer (clipboard-safe).
`)
}
