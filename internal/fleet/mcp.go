// mcp.go — `qai fleet mcp` is the conductor MCP server.
//
// This is the *only* MCP server qai ships. Every other qai surface is a
// CLI invocation. The justification for this one is narrow and load-
// bearing: the architect (the lead Claude Code session orchestrating a
// fleet) needs to be told when a worker report lands without having to
// poll. CLIs can't push; MCP can. So this server exists, and only this
// server exists.
//
// Lifecycle:
//
//   1. Claude Code launches `qai fleet mcp` as a stdio subprocess at
//      session start (configured under `mcpServers` in ~/.claude.json).
//   2. The server checks $TMUX_PANE against the active fleet's recorded
//      architect-pane. If a fleet is active and we are NOT the architect,
//      we exit immediately — workers should not pay an MCP tax. If no
//      fleet is active, or we ARE the architect, the server stays alive.
//   3. A 2s ticker re-evaluates role so a fleet brought up *after* this
//      Claude Code session opened still wires up correctly.
//   4. When acting as architect, the server fsnotify-watches the inbox
//      file and pushes `notifications/resources/updated` to the client
//      whenever a worker appends a new report.
//
// The protocol is JSON-RPC 2.0 over newline-delimited stdio per the MCP
// stdio transport. We hand-roll the wire format rather than pull in an
// MCP SDK — the surface we expose (initialize, tools/list, tools/call,
// resources/{list,read,subscribe,unsubscribe}, ping) is small enough
// that a typed dispatcher beats an SDK dependency.
//
// Stderr is for human/operator logs and is surfaced by Claude Code in
// its MCP-server log panel. Stdout is reserved for protocol messages
// only — anything written to stdout that isn't valid JSON-RPC will
// desync the client.

package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/quantum-encoding/qai-cli/internal/config"
)

const (
	mcpProtocolVersion = "2024-11-05"
	mcpServerName      = "qai-conductor"
	mcpServerVersion   = "1.0.0"
)

// RunMCPServer is the entrypoint for `qai fleet mcp`. It blocks until
// stdin closes (Claude Code shuts the server down by closing the pipe).
func RunMCPServer() error {
	// Fast-path exit for confirmed workers: if there's an active fleet
	// AND we're inside a tmux pane AND that pane isn't the architect,
	// we are unambiguously a worker. Exit cleanly so the worker session
	// pays no long-running memory cost. (~50ms total in the worker's
	// startup path; Claude Code tolerates fast-exit MCP servers.)
	if isDefiniteWorker() {
		fmt.Fprintln(os.Stderr, "qai-conductor: worker pane, exiting (architect-only server)")
		return nil
	}

	s := newMCPServer(os.Stdout)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.roleLoop(ctx)
	return s.serve(os.Stdin)
}

// ─── server state ──────────────────────────────────────────────────────────

type mcpServer struct {
	out   io.Writer
	outMu sync.Mutex

	stateMu     sync.Mutex
	fleetID     string // active fleet id, or "" when no fleet is active
	isArchitect bool
	watcher     *fsnotify.Watcher
	watcherDone chan struct{}

	subMu       sync.Mutex
	subscribers map[string]bool // resource URI → currently subscribed
}

func newMCPServer(out io.Writer) *mcpServer {
	return &mcpServer{
		out:         out,
		subscribers: map[string]bool{},
	}
}

// ─── stdio loop ────────────────────────────────────────────────────────────

func (s *mcpServer) serve(r io.Reader) error {
	sc := bufio.NewScanner(r)
	// MCP messages can be large (resources/read returning a 100KB inbox).
	// 8MB cap is well over anything qai will produce.
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			fmt.Fprintf(os.Stderr, "qai-conductor: malformed message: %v\n", err)
			continue
		}
		s.dispatch(msg)
	}
	return sc.Err()
}

type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *mcpServer) dispatch(msg jsonrpcMessage) {
	isNotification := len(msg.ID) == 0
	switch msg.Method {
	case "initialize":
		s.handleInitialize(msg.ID)
	case "notifications/initialized", "initialized":
		// fire-and-forget from the client, no response
	case "ping":
		s.respond(msg.ID, map[string]any{})
	case "tools/list":
		s.handleToolsList(msg.ID)
	case "tools/call":
		s.handleToolsCall(msg.ID, msg.Params)
	case "resources/list":
		s.handleResourcesList(msg.ID)
	case "resources/read":
		s.handleResourcesRead(msg.ID, msg.Params)
	case "resources/subscribe":
		s.handleResourcesSubscribe(msg.ID, msg.Params)
	case "resources/unsubscribe":
		s.handleResourcesUnsubscribe(msg.ID, msg.Params)
	default:
		if !isNotification {
			s.respondError(msg.ID, -32601, "method not found: "+msg.Method)
		}
	}
}

// ─── handlers ──────────────────────────────────────────────────────────────

func (s *mcpServer) handleInitialize(id json.RawMessage) {
	s.respond(id, map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
			"resources": map[string]any{
				"subscribe":   true,
				"listChanged": false,
			},
		},
		"serverInfo": map[string]any{
			"name":    mcpServerName,
			"version": mcpServerVersion,
		},
	})
}

func (s *mcpServer) handleToolsList(id json.RawMessage) {
	tools := []any{}
	if _, ok := s.activeFleet(); ok {
		tools = append(tools,
			map[string]any{
				"name":        "inbox_list",
				"description": "List every worker report from the active fleet's inbox, in chronological order. Read-only — does not advance the architect cursor.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
			map[string]any{
				"name":        "inbox_unread",
				"description": "Return reports the architect hasn't seen yet AND advance the architect cursor. Use this after a notifications/resources/updated event fires.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		)
	}
	s.respond(id, map[string]any{"tools": tools})
}

func (s *mcpServer) handleToolsCall(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		s.respondError(id, -32602, "invalid params: "+err.Error())
		return
	}
	fleetID, ok := s.activeFleet()
	if !ok {
		s.respondError(id, -32603, "no active fleet — bring one up with `qai fleet up <manifest>`")
		return
	}
	switch p.Name {
	case "inbox_list":
		reports, err := ReadAll(fleetID)
		if err != nil {
			s.respondError(id, -32603, err.Error())
			return
		}
		s.respondToolText(id, formatReports(reports))
	case "inbox_unread":
		reports, err := ReadUnread(fleetID, CursorArchitect)
		if err != nil {
			s.respondError(id, -32603, err.Error())
			return
		}
		s.respondToolText(id, formatReports(reports))
	default:
		s.respondError(id, -32601, "unknown tool: "+p.Name)
	}
}

func (s *mcpServer) handleResourcesList(id json.RawMessage) {
	resources := []any{}
	if fleetID, ok := s.activeFleet(); ok {
		resources = append(resources, map[string]any{
			"uri":         inboxURI(fleetID),
			"name":        "Fleet inbox",
			"description": fmt.Sprintf("Live worker reports for fleet %s. Subscribe for resources/updated notifications on each new report.", fleetID),
			"mimeType":    "application/x-ndjson",
		})
	}
	s.respond(id, map[string]any{"resources": resources})
}

func (s *mcpServer) handleResourcesRead(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		s.respondError(id, -32602, "invalid params: "+err.Error())
		return
	}
	fleetID, ok := parseInboxURI(p.URI)
	if !ok {
		s.respondError(id, -32602, "unknown resource: "+p.URI)
		return
	}
	reports, err := ReadAll(fleetID)
	if err != nil {
		s.respondError(id, -32603, err.Error())
		return
	}
	s.respond(id, map[string]any{
		"contents": []any{
			map[string]any{
				"uri":      p.URI,
				"mimeType": "application/x-ndjson",
				"text":     formatReports(reports),
			},
		},
	})
}

func (s *mcpServer) handleResourcesSubscribe(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		s.respondError(id, -32602, "invalid params: "+err.Error())
		return
	}
	s.subMu.Lock()
	s.subscribers[p.URI] = true
	s.subMu.Unlock()
	s.respond(id, map[string]any{})
}

func (s *mcpServer) handleResourcesUnsubscribe(id json.RawMessage, raw json.RawMessage) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		s.respondError(id, -32602, "invalid params: "+err.Error())
		return
	}
	s.subMu.Lock()
	delete(s.subscribers, p.URI)
	s.subMu.Unlock()
	s.respond(id, map[string]any{})
}

// ─── role + watcher control loop ───────────────────────────────────────────

func (s *mcpServer) roleLoop(ctx context.Context) {
	s.evalRole()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.tearDownWatcher()
			return
		case <-t.C:
			s.evalRole()
		}
	}
}

// evalRole reads the active-fleet pointer + architect-pane file and
// updates the watcher state if the role changed.
func (s *mcpServer) evalRole() {
	fleetID, isArch := detectArchitectRole()
	s.stateMu.Lock()
	roleChanged := fleetID != s.fleetID || isArch != s.isArchitect
	if roleChanged {
		s.tearDownWatcherLocked()
		s.fleetID = fleetID
		s.isArchitect = isArch
		if isArch && fleetID != "" {
			s.startWatcherLocked(fleetID)
		}
	}
	s.stateMu.Unlock()
	if roleChanged && isArch && fleetID != "" {
		// Tell any subscribers that the resource set might have shifted.
		s.notifyResourceUpdated(inboxURI(fleetID))
	}
}

func (s *mcpServer) tearDownWatcher() {
	s.stateMu.Lock()
	s.tearDownWatcherLocked()
	s.stateMu.Unlock()
}

func (s *mcpServer) tearDownWatcherLocked() {
	if s.watcher != nil {
		_ = s.watcher.Close()
		s.watcher = nil
	}
	if s.watcherDone != nil {
		<-s.watcherDone
		s.watcherDone = nil
	}
}

func (s *mcpServer) startWatcherLocked(fleetID string) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai-conductor: fsnotify init: %v\n", err)
		return
	}
	inbox := filepath.Join(FleetDir(fleetID), "inbox.jsonl")
	// Watch the file directly when present; otherwise the parent dir,
	// which fires on inbox.jsonl creation.
	if _, err := os.Stat(inbox); err == nil {
		_ = w.Add(inbox)
	} else {
		_ = w.Add(filepath.Dir(inbox))
	}
	s.watcher = w
	done := make(chan struct{})
	s.watcherDone = done
	go s.watchLoop(w, fleetID, done)
}

func (s *mcpServer) watchLoop(w *fsnotify.Watcher, fleetID string, done chan struct{}) {
	defer close(done)
	uri := inboxURI(fleetID)
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				s.notifyResourceUpdated(uri)
			}
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}

// ─── notifications ─────────────────────────────────────────────────────────

func (s *mcpServer) notifyResourceUpdated(uri string) {
	s.subMu.Lock()
	subbed := s.subscribers[uri]
	s.subMu.Unlock()
	if !subbed {
		return
	}
	s.notify("notifications/resources/updated", map[string]any{
		"uri": uri,
	})
}

func (s *mcpServer) notify(method string, params any) {
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (s *mcpServer) respond(id json.RawMessage, result any) {
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}

func (s *mcpServer) respondToolText(id json.RawMessage, text string) {
	s.respond(id, map[string]any{
		"content": []any{
			map[string]any{
				"type": "text",
				"text": text,
			},
		},
	})
}

func (s *mcpServer) respondError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func (s *mcpServer) write(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai-conductor: marshal: %v\n", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(b); err == nil {
		_, _ = s.out.Write([]byte{'\n'})
	}
}

// ─── role detection ────────────────────────────────────────────────────────

// detectArchitectRole reads the active fleet pointer and architect-pane
// file, then matches against $TMUX_PANE. Returns ("", false) when no
// fleet is active.
func detectArchitectRole() (fleetID string, isArchitect bool) {
	if config.Home == "" {
		return "", false
	}
	activeFile := filepath.Join(config.Home, ".qai", "fleet", "active")
	data, err := os.ReadFile(activeFile)
	if err != nil {
		return "", false
	}
	fleetID = strings.TrimSpace(string(data))
	if fleetID == "" {
		return "", false
	}
	archFile := filepath.Join(config.Home, ".qai", "fleet", fleetID, "architect-pane")
	archData, err := os.ReadFile(archFile)
	if err != nil {
		return fleetID, false
	}
	archPane := strings.TrimSpace(string(archData))
	myPane := os.Getenv("TMUX_PANE")
	if myPane == "" {
		return fleetID, false
	}
	return fleetID, myPane == archPane
}

// isDefiniteWorker returns true only when we can confidently identify
// this process as a worker pane: there's an active fleet, we're inside
// a tmux pane, and that pane isn't the architect. Anything ambiguous
// (no fleet, no tmux env, etc.) returns false so we stay alive in case
// we become the architect later in the session.
func isDefiniteWorker() bool {
	if os.Getenv("TMUX_PANE") == "" {
		return false
	}
	fleetID, isArch := detectArchitectRole()
	if fleetID == "" {
		return false
	}
	return !isArch
}

func (s *mcpServer) activeFleet() (string, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.isArchitect || s.fleetID == "" {
		return "", false
	}
	return s.fleetID, true
}

// ─── helpers ───────────────────────────────────────────────────────────────

func inboxURI(fleetID string) string {
	return "qai://fleet/" + fleetID + "/inbox"
}

func parseInboxURI(uri string) (string, bool) {
	const prefix = "qai://fleet/"
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(uri, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "inbox" || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// formatReports turns a slice of Reports into newline-delimited JSON,
// one per line. Empty input yields an explicit "(no reports)" string
// so a tool result is never silently empty — the caller can tell the
// agent there was nothing new.
func formatReports(rs []Report) string {
	if len(rs) == 0 {
		return "(no reports)"
	}
	var sb strings.Builder
	for _, r := range rs {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return sb.String()
}
