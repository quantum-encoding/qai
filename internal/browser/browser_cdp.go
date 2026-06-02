// browser_cdp.go — Low-level Chrome DevTools Protocol client over WebSocket.
//
// Connects to a running Chrome/Brave instance via its debug port,
// sends CDP commands as JSON, and receives responses with ID matching.
// No external browser launch — uses the user's existing session.

package browser

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ─── tab discovery ────────────────────────────────────────────────────────

// cdpTab represents a browser tab from the /json endpoint.
type cdpTab struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpListTabs fetches the list of open tabs from the browser's debug port.
func cdpListTabs(port int) ([]cdpTab, error) {
	url := fmt.Sprintf("http://localhost:%d/json", port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to browser on localhost:%d (no debug port listening)\n"+
			"  → fix: launch your browser with remote debugging, then retry:\n"+
			"      Brave:   open -a \"Brave Browser\" --args --remote-debugging-port=%d\n"+
			"      Chrome:  open -a \"Google Chrome\" --args --remote-debugging-port=%d\n"+
			"      or:      qai browser launch\n"+
			"    custom port: pass --port N, or set QAI_BROWSER_PORT=N",
			port, port, port)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read /json: %w", err)
	}

	var tabs []cdpTab
	if err := json.Unmarshal(data, &tabs); err != nil {
		return nil, fmt.Errorf("parse /json: %w", err)
	}
	return tabs, nil
}

// cdpActivateTab brings a tab to the foreground.
func cdpActivateTab(port int, tabID string) error {
	url := fmt.Sprintf("http://localhost:%d/json/activate/%s", port, tabID)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("activate tab: %w", err)
	}
	resp.Body.Close()
	return nil
}

// cdpNewTab opens a fresh tab via PUT /json/new. targetURL may be ""
// for a blank tab. Returns the created tab's metadata so the caller
// can pin it to a slot or attach for further work.
//
// The new tab is created in the background — the browser does not
// switch focus to it, so calling this in a loop from an agent doesn't
// interrupt whatever the human is doing in the foreground tab.
func cdpNewTab(port int, targetURL string) (*cdpTab, error) {
	u := fmt.Sprintf("http://localhost:%d/json/new", port)
	if targetURL != "" {
		u += "?" + targetURL
	}
	req, err := http.NewRequest(http.MethodPut, u, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create tab: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("create tab HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var tab cdpTab
	if err := json.Unmarshal(data, &tab); err != nil {
		return nil, fmt.Errorf("parse new-tab response: %w (body: %s)", err, string(data))
	}
	return &tab, nil
}

// cdpCloseTab closes a tab via PUT /json/close/<id>. Idempotent — a
// non-200 from a stale ID is treated as already-closed.
func cdpCloseTab(port int, tabID string) error {
	u := fmt.Sprintf("http://localhost:%d/json/close/%s", port, tabID)
	req, err := http.NewRequest(http.MethodPut, u, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("close tab: %w", err)
	}
	resp.Body.Close()
	return nil
}

// ─── websocket client ─────────────────────────────────────────────────────

// cdpClient holds an active WebSocket connection to a single browser tab.
type cdpClient struct {
	conn   *websocket.Conn
	nextID atomic.Int64
	mu     sync.Mutex // protects reads (single reader)
	events []cdpEvent // buffered events seen while waiting for responses
}

type cdpEvent struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type cdpMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// cdpConnect dials the WebSocket debugger URL and returns a ready client.
func cdpConnect(wsURL string) (*cdpClient, error) {
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket connect: %w", err)
	}
	c := &cdpClient{conn: conn}
	c.nextID.Store(1)
	return c, nil
}

// Close cleanly shuts down the WebSocket connection.
func (c *cdpClient) Close() {
	c.conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	c.conn.Close()
}

// Call sends a CDP command and waits for the matching response.
func (c *cdpClient) Call(method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	id := c.nextID.Add(1) - 1

	msg := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		msg["params"] = params
	} else {
		msg["params"] = map[string]any{}
	}

	data, _ := json.Marshal(msg)

	c.mu.Lock()
	err := c.conn.WriteMessage(websocket.TextMessage, data)
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("CDP send: %w", err)
	}

	return c.recv(id, timeout)
}

// CallNoWait sends a CDP command without waiting for a response.
func (c *cdpClient) CallNoWait(method string, params map[string]any) error {
	id := c.nextID.Add(1) - 1

	msg := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		msg["params"] = params
	} else {
		msg["params"] = map[string]any{}
	}

	data, _ := json.Marshal(msg)

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// recv reads messages until one with the matching id arrives.
// Events (messages without id) are buffered for WaitEvent.
func (c *cdpClient) recv(id int64, timeout time.Duration) (json.RawMessage, error) {
	deadline := time.Now().Add(timeout)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("CDP timeout (%v) waiting for response id=%d", timeout, id)
		}
		c.conn.SetReadDeadline(deadline)

		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				return nil, fmt.Errorf("connection closed")
			}
			return nil, fmt.Errorf("CDP recv: %w", err)
		}

		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue // skip unparseable messages
		}

		// Event (no id) — buffer it
		if msg.ID == nil {
			if msg.Method != "" {
				c.events = append(c.events, cdpEvent{Method: msg.Method, Params: msg.Params})
			}
			continue
		}

		// Response for a different command — skip
		if *msg.ID != id {
			continue
		}

		// Our response
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
}

// Capture collects every event whose Method matches one of the given
// prefixes for the specified duration. Returns the collected events in
// arrival order. Anything that doesn't match the prefixes is left in
// the client's event buffer so subsequent WaitEvent calls can still
// see it.
//
// Caller is expected to have already issued the corresponding
// `<Domain>.enable` Calls — Capture is purely an event drain, not a
// domain initialiser. (Done this way so the caller can enable multiple
// domains and capture them together.)
//
// Usage:
//
//	c.Call("Network.enable", nil, 5*time.Second)
//	events := c.Capture([]string{"Network."}, 5*time.Second)
func (c *cdpClient) Capture(acceptPrefixes []string, duration time.Duration) []cdpEvent {
	matches := func(method string) bool {
		for _, p := range acceptPrefixes {
			if strings.HasPrefix(method, p) {
				return true
			}
		}
		return false
	}

	// First, drain anything already buffered that matches.
	var collected, remaining []cdpEvent
	for _, ev := range c.events {
		if matches(ev.Method) {
			collected = append(collected, ev)
		} else {
			remaining = append(remaining, ev)
		}
	}
	c.events = remaining

	// Then read from the wire for `duration`, collecting matches and
	// re-buffering everything else. ReadDeadline expiry is how we exit
	// the loop — it's expected, not an error.
	deadline := time.Now().Add(duration)
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			break
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(rem))
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			// Most common cause is the read deadline firing; that's
			// the loop's normal exit condition.
			break
		}
		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.ID != nil {
			// Late response to an earlier Call — nothing to do here.
			continue
		}
		if msg.Method == "" {
			continue
		}
		ev := cdpEvent{Method: msg.Method, Params: msg.Params}
		if matches(msg.Method) {
			collected = append(collected, ev)
		} else {
			c.events = append(c.events, ev)
		}
	}
	return collected
}

// WaitEvent waits for a CDP event by method name (e.g. "Page.loadEventFired").
// Checks buffered events first, then reads new messages.
func (c *cdpClient) WaitEvent(eventName string, timeout time.Duration) error {
	// Check buffer first
	for i, ev := range c.events {
		if ev.Method == eventName {
			c.events = append(c.events[:i], c.events[i+1:]...)
			return nil
		}
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timeout waiting for event %s", eventName)
		}
		c.conn.SetReadDeadline(deadline)

		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", eventName, err)
		}

		var msg cdpMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		if msg.ID == nil && msg.Method == eventName {
			return nil
		}

		// Buffer other events
		if msg.ID == nil && msg.Method != "" {
			c.events = append(c.events, cdpEvent{Method: msg.Method, Params: msg.Params})
		}
	}
}

// ─── stealth injection ────────────────────────────────────────────────────

const stealthJS = `(function(){
Object.defineProperty(navigator,'webdriver',{get:()=>undefined});
Object.defineProperty(navigator,'plugins',{get:()=>[
  {name:'Chrome PDF Plugin',filename:'internal-pdf-viewer'},
  {name:'Chrome PDF Viewer',filename:'mhjfbmdgcfjbbpaeojofohoefgiehjai'},
  {name:'Native Client',filename:'internal-nacl-plugin'}
]});
Object.defineProperty(navigator,'languages',{get:()=>['en-US','en']});
Object.defineProperty(navigator,'platform',{get:()=>'MacIntel'});
Object.defineProperty(navigator,'hardwareConcurrency',{get:()=>8});
Object.defineProperty(navigator,'deviceMemory',{get:()=>8});
Object.defineProperty(navigator,'maxTouchPoints',{get:()=>0});
if(!window.chrome)window.chrome={};
if(!window.chrome.runtime)window.chrome.runtime={};
window.chrome.csi=function(){};
window.chrome.loadTimes=function(){};
const oq=window.navigator.permissions.query;
window.navigator.permissions.query=(p)=>
  p.name==='notifications'?Promise.resolve({state:Notification.permission}):oq(p);
const gp=WebGLRenderingContext.prototype.getParameter;
WebGLRenderingContext.prototype.getParameter=function(p){
  if(p===37445)return'Intel Inc.';
  if(p===37446)return'Intel Iris OpenGL Engine';
  return gp.call(this,p);
};
const ip=HTMLIFrameElement.prototype;
const od=Object.getOwnPropertyDescriptor(ip,'contentWindow');
if(od){Object.defineProperty(ip,'contentWindow',{get:function(){
  const w=od.get.call(this);if(w){try{w.chrome=window.chrome}catch(e){}}return w;
}});}
})()`
