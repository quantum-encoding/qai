package fleet

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

// withTempHome redirects config.Home to a per-test scratch dir so the
// MCP server's role-detection reads from that, not the developer's
// real ~/.qai. Returns a cleanup func the caller defers.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := config.Home
	config.Home = dir
	t.Cleanup(func() { config.Home = prev })
	return dir
}

// writeActiveFleet sets up the active-fleet pointer + architect-pane
// file so detectArchitectRole returns (fleetID, true) when $TMUX_PANE
// matches the given pane id.
func writeActiveFleet(t *testing.T, home, fleetID, archPane string) {
	t.Helper()
	dir := filepath.Join(home, ".qai", "fleet", fleetID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(home, ".qai", "fleet", "active")
	if err := os.WriteFile(active, []byte(fleetID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "architect-pane"), []byte(archPane+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// driveServer starts a server with synthetic stdin/stdout pipes so a
// test can write JSON-RPC messages and read responses + notifications.
type driver struct {
	t       *testing.T
	stdin   *io.PipeWriter
	stdout  *bufio.Reader
	server  *mcpServer
	closed  chan struct{}
	mu      sync.Mutex
	closedB bool
}

func startDriver(t *testing.T) *driver {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := newMCPServer(outW)

	closed := make(chan struct{})
	d := &driver{
		t:      t,
		stdin:  inW,
		stdout: bufio.NewReader(outR),
		server: srv,
		closed: closed,
	}

	go func() {
		_ = srv.serve(inR)
		_ = outW.Close()
		close(closed)
	}()
	t.Cleanup(d.close)
	return d
}

func (d *driver) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closedB {
		return
	}
	d.closedB = true
	_ = d.stdin.Close()
	select {
	case <-d.closed:
	case <-time.After(2 * time.Second):
		d.t.Error("driver: server did not exit within 2s of stdin close")
	}
}

func (d *driver) send(msg map[string]any) {
	d.t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.stdin.Write(append(b, '\n')); err != nil {
		d.t.Fatal(err)
	}
}

// readMessage blocks until one JSON-RPC line arrives. fail-on-timeout
// because tests should never hang silently.
func (d *driver) readMessage(timeout time.Duration) map[string]any {
	d.t.Helper()
	type result struct {
		msg map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := d.stdout.ReadBytes('\n')
		if err != nil {
			ch <- result{err: err}
			return
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			ch <- result{err: fmt.Errorf("decode %q: %w", line, err)}
			return
		}
		ch <- result{msg: m}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			d.t.Fatalf("readMessage: %v", r.err)
		}
		return r.msg
	case <-time.After(timeout):
		d.t.Fatalf("readMessage: timeout after %s", timeout)
		return nil
	}
}

// readUntil drains messages until one matches the predicate or the
// timeout fires. Useful when notifications and responses interleave.
func (d *driver) readUntil(timeout time.Duration, pred func(map[string]any) bool) map[string]any {
	d.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m := d.readMessage(time.Until(deadline))
		if pred(m) {
			return m
		}
	}
	d.t.Fatalf("readUntil: predicate did not match within %s", timeout)
	return nil
}

// ─── tests ────────────────────────────────────────────────────────────────

func TestMCPInitialize_NoFleet_ReturnsEmptyToolsAndResources(t *testing.T) {
	withTempHome(t)
	t.Setenv("TMUX_PANE", "%99")

	d := startDriver(t)
	d.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	resp := d.readMessage(2 * time.Second)
	if got := resp["error"]; got != nil {
		t.Fatalf("initialize returned error: %v", got)
	}

	d.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	resp = d.readMessage(2 * time.Second)
	if tools := mustList(t, resp, "tools"); len(tools) != 0 {
		t.Errorf("tools/list = %v, want empty (no active fleet)", tools)
	}
	d.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/list"})
	resp = d.readMessage(2 * time.Second)
	if resources := mustList(t, resp, "resources"); len(resources) != 0 {
		t.Errorf("resources/list = %v, want empty", resources)
	}
}

func TestMCPArchitect_ListsToolsAndResource(t *testing.T) {
	home := withTempHome(t)
	writeActiveFleet(t, home, "test-fleet-1", "%5")
	t.Setenv("TMUX_PANE", "%5") // we are the architect

	d := startDriver(t)
	d.server.evalRole() // force immediate role pickup, skip the 2s ticker

	d.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	_ = d.readMessage(2 * time.Second)

	d.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	resp := d.readMessage(2 * time.Second)
	tools := mustList(t, resp, "tools")
	if len(tools) != 2 {
		t.Errorf("tools/list count = %d, want 2 (inbox_list + inbox_unread); got %v", len(tools), tools)
	}

	d.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "resources/list"})
	resp = d.readMessage(2 * time.Second)
	resources := mustList(t, resp, "resources")
	if len(resources) != 1 {
		t.Fatalf("resources/list count = %d, want 1", len(resources))
	}
	uri, _ := resources[0].(map[string]any)["uri"].(string)
	if uri != "qai://fleet/test-fleet-1/inbox" {
		t.Errorf("resource uri = %q, want qai://fleet/test-fleet-1/inbox", uri)
	}
}

func TestMCPSubscribe_PushesNotificationOnInboxAppend(t *testing.T) {
	home := withTempHome(t)
	writeActiveFleet(t, home, "test-fleet-2", "%7")
	t.Setenv("TMUX_PANE", "%7")

	d := startDriver(t)
	d.server.evalRole() // start the watcher

	d.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	_ = d.readMessage(2 * time.Second)

	uri := "qai://fleet/test-fleet-2/inbox"
	d.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "resources/subscribe",
		"params":  map[string]any{"uri": uri},
	})
	_ = d.readMessage(2 * time.Second)

	// Append a report so fsnotify fires.
	if err := AppendReport(Report{
		TS:      time.Now(),
		FleetID: "test-fleet-2",
		Pane:    "test-worker",
		Status:  "done",
		Message: "synthetic report",
	}); err != nil {
		t.Fatalf("AppendReport: %v", err)
	}

	notif := d.readUntil(3*time.Second, func(m map[string]any) bool {
		return m["method"] == "notifications/resources/updated"
	})
	params, ok := notif["params"].(map[string]any)
	if !ok {
		t.Fatalf("notification params not an object: %v", notif)
	}
	if params["uri"] != uri {
		t.Errorf("notification uri = %v, want %s", params["uri"], uri)
	}
}

func TestMCPInboxUnread_ReturnsAndAdvancesCursor(t *testing.T) {
	home := withTempHome(t)
	writeActiveFleet(t, home, "test-fleet-3", "%9")
	t.Setenv("TMUX_PANE", "%9")

	// Pre-append a couple of reports before the driver starts.
	for i := 0; i < 2; i++ {
		_ = AppendReport(Report{
			TS:      time.Now(),
			FleetID: "test-fleet-3",
			Pane:    fmt.Sprintf("worker-%d", i),
			Status:  "progress",
			Message: fmt.Sprintf("report %d", i),
		})
	}

	d := startDriver(t)
	d.server.evalRole()

	d.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	_ = d.readMessage(2 * time.Second)

	d.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "inbox_unread",
			"arguments": map[string]any{},
		},
	})
	resp := d.readMessage(2 * time.Second)
	text := mustToolText(t, resp)
	if strings.Count(text, "\n") != 2 {
		t.Errorf("inbox_unread first call = %q, want 2 newline-delimited reports", text)
	}

	// Second call should be empty — cursor advanced.
	d.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "inbox_unread",
			"arguments": map[string]any{},
		},
	})
	resp = d.readMessage(2 * time.Second)
	if text := mustToolText(t, resp); !strings.Contains(text, "no reports") {
		t.Errorf("second inbox_unread = %q, want \"no reports\"", text)
	}
}

func TestIsDefiniteWorker(t *testing.T) {
	home := withTempHome(t)
	writeActiveFleet(t, home, "fleet-x", "%1")

	tests := []struct {
		name    string
		pane    string
		wantOut bool
	}{
		{"no tmux env → not definite worker", "", false},
		{"matches architect → not worker", "%1", false},
		{"different pane in active fleet → definite worker", "%2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TMUX_PANE", tt.pane)
			if got := isDefiniteWorker(); got != tt.wantOut {
				t.Errorf("isDefiniteWorker(TMUX_PANE=%q) = %v, want %v", tt.pane, got, tt.wantOut)
			}
		})
	}
}

func TestParseInboxURI(t *testing.T) {
	tests := []struct {
		uri      string
		fleetID  string
		valid    bool
	}{
		{"qai://fleet/abc-123/inbox", "abc-123", true},
		{"qai://fleet/abc/inbox/extra", "", false},
		{"qai://fleet//inbox", "", false},
		{"http://other/inbox", "", false},
		{"qai://fleet/abc/notes", "", false},
	}
	for _, tt := range tests {
		got, ok := parseInboxURI(tt.uri)
		if ok != tt.valid || got != tt.fleetID {
			t.Errorf("parseInboxURI(%q) = (%q, %v), want (%q, %v)", tt.uri, got, ok, tt.fleetID, tt.valid)
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────

func mustList(t *testing.T, msg map[string]any, key string) []any {
	t.Helper()
	r, ok := msg["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %v", msg)
	}
	list, ok := r[key].([]any)
	if !ok {
		t.Fatalf("result missing %q: %v", key, r)
	}
	return list
}

func mustToolText(t *testing.T, msg map[string]any) string {
	t.Helper()
	r, ok := msg["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %v", msg)
	}
	content, ok := r["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("result missing content: %v", r)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] not object: %v", content)
	}
	text, _ := first["text"].(string)
	return text
}
