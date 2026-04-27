// browser.go — Browser automation via Chrome DevTools Protocol.
//
// Connects to the user's existing Chrome/Brave debug port — no headless
// browser, no Playwright, no Node. Uses their real session with all
// cookies, auth, and fingerprints intact.
//
// Usage:
//
//	qai browser list                        # list open tabs
//	qai browser open <url>                  # navigate to URL
//	qai browser extract [--html]            # get page text or HTML
//	qai browser screenshot [-o file.png]    # capture screenshot
//	qai browser click <selector|x y>        # click element
//	qai browser eval "js"                   # evaluate JavaScript

package browser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ─── port config ──────────────────────────────────────────────────────────

const defaultCDPPort = 9222

func browserPort(args []string) int {
	// Check args for --port
	for i, a := range args {
		if a == "--port" && i+1 < len(args) {
			if p, err := strconv.Atoi(args[i+1]); err == nil {
				return p
			}
		}
	}
	// Check env
	if v := os.Getenv("QAI_BROWSER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			return p
		}
	}
	return defaultCDPPort
}

// stripFlags removes --port and --tab and their values from args.
func stripFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "--tab":
			i++ // skip value
		default:
			out = append(out, args[i])
		}
	}
	return out
}

// tabIDFromArgs extracts --tab value.
func tabIDFromArgs(args []string) string {
	for i, a := range args {
		if a == "--tab" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// ─── connect helper ───────────────────────────────────────────────────────

// connectToTab discovers tabs, selects one, connects via WebSocket, and
// injects stealth. Most commands call this as their first step.
func connectToTab(args []string) (*cdpClient, *cdpTab, error) {
	port := browserPort(args)
	tabs, err := cdpListTabs(port)
	if err != nil {
		return nil, nil, err
	}

	// Filter to page-type tabs
	var pages []cdpTab
	for _, t := range tabs {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			pages = append(pages, t)
		}
	}
	if len(pages) == 0 {
		return nil, nil, fmt.Errorf("no open browser tabs found")
	}

	// Select tab
	target := &pages[0]
	if wantID := tabIDFromArgs(args); wantID != "" {
		found := false
		for i, t := range pages {
			if t.ID == wantID || strings.HasPrefix(t.ID, wantID) {
				target = &pages[i]
				found = true
				break
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("tab %q not found", wantID)
		}
	}

	client, err := cdpConnect(target.WebSocketDebuggerURL)
	if err != nil {
		return nil, nil, err
	}

	// Enable page events
	client.Call("Page.enable", nil, 5*time.Second)
	client.Call("DOM.enable", nil, 5*time.Second)

	// Inject stealth
	injectStealth(client)

	return client, target, nil
}

// injectStealth checks if navigator.webdriver is already patched, and if
// not, injects the stealth script for the current page and all future
// navigations in this tab session.
func injectStealth(c *cdpClient) {
	// Check current state
	result, err := c.Call("Runtime.evaluate", map[string]any{
		"expression":   "typeof navigator.webdriver",
		"returnByValue": true,
	}, 3*time.Second)
	if err == nil {
		var val struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if json.Unmarshal(result, &val) == nil && val.Result.Value == "undefined" {
			return // already patched
		}
	}

	// Inject for future navigations
	c.Call("Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": stealthJS,
	}, 3*time.Second)

	// Patch current page
	c.Call("Runtime.evaluate", map[string]any{
		"expression": stealthJS,
	}, 3*time.Second)
}

// ─── selector → coordinates ───────────────────────────────────────────────

func resolveSelector(c *cdpClient, sel string) (float64, float64, error) {
	// Scroll into view first
	c.Call("Runtime.evaluate", map[string]any{
		"expression": fmt.Sprintf(`document.querySelector(%q)?.scrollIntoView({block:"center"})`, sel),
	}, 5*time.Second)

	// Small delay for scroll to settle
	time.Sleep(100 * time.Millisecond)

	// Get document root
	docResult, err := c.Call("DOM.getDocument", nil, 5*time.Second)
	if err != nil {
		return 0, 0, fmt.Errorf("DOM.getDocument: %w", err)
	}
	var doc struct {
		Root struct {
			NodeID int `json:"nodeId"`
		} `json:"root"`
	}
	if err := json.Unmarshal(docResult, &doc); err != nil {
		return 0, 0, fmt.Errorf("parse document: %w", err)
	}

	// querySelector
	qResult, err := c.Call("DOM.querySelector", map[string]any{
		"nodeId":   doc.Root.NodeID,
		"selector": sel,
	}, 5*time.Second)
	if err != nil {
		return 0, 0, fmt.Errorf("querySelector %q: %w", sel, err)
	}
	var qr struct {
		NodeID int `json:"nodeId"`
	}
	if err := json.Unmarshal(qResult, &qr); err != nil || qr.NodeID == 0 {
		return 0, 0, fmt.Errorf("element not found: %s", sel)
	}

	// getBoxModel
	boxResult, err := c.Call("DOM.getBoxModel", map[string]any{
		"nodeId": qr.NodeID,
	}, 5*time.Second)
	if err != nil {
		return 0, 0, fmt.Errorf("getBoxModel: %w", err)
	}
	var box struct {
		Model struct {
			Content []float64 `json:"content"` // [x1,y1, x2,y2, x3,y3, x4,y4]
		} `json:"model"`
	}
	if err := json.Unmarshal(boxResult, &box); err != nil || len(box.Model.Content) < 8 {
		return 0, 0, fmt.Errorf("parse box model for %s", sel)
	}

	// Center of content quad
	cx := (box.Model.Content[0] + box.Model.Content[4]) / 2
	cy := (box.Model.Content[1] + box.Model.Content[5]) / 2
	return cx, cy, nil
}

// ─── dispatcher ───────────────────────────────────────────────────────────

func CmdBrowser(args []string) {
	if len(args) == 0 {
		browserUsage()
		os.Exit(1)
	}

	// Real-flag parse for --yes/-y. Matters because the previous
	// os.Args substring scan tripped on quoted prompts containing those
	// characters. Strip the flag from args before dispatch so individual
	// command handlers don't see it.
	cleaned := args[:0:len(args)]
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			SetYesFlag(true)
			continue
		}
		cleaned = append(cleaned, a)
	}
	args = cleaned

	if len(args) == 0 {
		browserUsage()
		os.Exit(1)
	}

	action := args[0]
	rest := args[1:]

	switch action {
	case "help", "--help", "-h":
		browserUsage()
	case "launch", "start":
		browserLaunch(rest)
	case "list", "ls":
		browserList(rest)
	case "open", "navigate", "go":
		browserOpen(rest)
	case "tab":
		browserTab(rest)
	case "extract", "text":
		browserExtract(rest)
	case "screenshot", "ss":
		browserScreenshot(rest)
	case "click":
		browserClick(rest)
	case "type":
		browserType(rest)
	case "eval", "js":
		browserEval(rest)
	case "scrape":
		browserScrape(rest)
	case "clip":
		browserClip(rest)
	case "wait":
		browserWait(rest)
	case "source":
		browserSource(rest)
	case "pdf":
		browserPDF(rest)
	default:
		fmt.Fprintf(os.Stderr, "qai browser: unknown action %q\n", action)
		browserUsage()
		os.Exit(1)
	}
}

// ─── commands ─────────────────────────────────────────────────────────────

func browserLaunch(args []string) {
	port := browserPort(args)

	// Already running?
	if _, err := cdpListTabs(port); err == nil {
		fmt.Fprintf(os.Stderr, "browser already available on localhost:%d\n", port)
		browserList(args)
		return
	}

	// Find browser binary — platform-aware detection.
	type candidate struct {
		name string
		path string
	}
	var browsers []candidate

	switch runtime.GOOS {
	case "darwin":
		browsers = []candidate{
			{"Brave", "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"},
			{"Chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
			{"Edge", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
			{"Chromium", "/Applications/Chromium.app/Contents/MacOS/Chromium"},
		}
	case "linux":
		browsers = []candidate{
			{"Brave", "brave-browser"},
			{"Chrome", "google-chrome"},
			{"Chrome", "google-chrome-stable"},
			{"Chromium", "chromium-browser"},
			{"Chromium", "chromium"},
			{"Edge", "microsoft-edge"},
		}
	case "windows":
		browsers = []candidate{
			{"Chrome", `C:\Program Files\Google\Chrome\Application\chrome.exe`},
			{"Chrome", `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`},
			{"Brave", `C:\Program Files\BraveSoftware\Brave-Browser\Application\brave.exe`},
			{"Edge", `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`},
		}
	}

	var chosen candidate
	for _, b := range browsers {
		// On Linux, check PATH first; on macOS/Windows, check absolute path.
		if runtime.GOOS == "linux" {
			if p, err := exec.LookPath(b.path); err == nil {
				chosen = candidate{name: b.name, path: p}
				break
			}
		} else {
			if _, err := os.Stat(b.path); err == nil {
				chosen = b
				break
			}
		}
	}
	if chosen.path == "" {
		fmt.Fprintln(os.Stderr, "qai browser: no Chrome/Brave/Edge/Chromium found")
		fmt.Fprintln(os.Stderr, "Install a Chromium-based browser, or pass --port to connect to an existing debug session")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "launching %s with --remote-debugging-port=%d...\n", chosen.name, port)

	cmd := exec.Command(chosen.path, fmt.Sprintf("--remote-debugging-port=%d", port))
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: launch failed: %v\n", err)
		os.Exit(1)
	}

	// Detach — don't wait for the browser process
	cmd.Process.Release()

	// Poll until debug port is ready (up to 10 seconds)
	fmt.Fprintf(os.Stderr, "waiting for debug port...")
	for i := range 50 {
		_ = i
		time.Sleep(200 * time.Millisecond)
		if tabs, err := cdpListTabs(port); err == nil {
			fmt.Fprintf(os.Stderr, " ready!\n")
			pageCount := 0
			for _, t := range tabs {
				if t.Type == "page" {
					pageCount++
				}
			}
			fmt.Fprintf(os.Stderr, "%s running — %d tab(s) on port %d\n", chosen.name, pageCount, port)
			return
		}
	}

	fmt.Fprintf(os.Stderr, "\nqai browser: timeout waiting for debug port\n")
	fmt.Fprintf(os.Stderr, "If %s was already running, quit it first and retry.\n", chosen.name)
	if runtime.GOOS == "darwin" {
		fmt.Fprintf(os.Stderr, "  osascript -e 'quit app \"%s\"' && sleep 1 && qai browser launch\n", chosen.name)
	} else {
		fmt.Fprintf(os.Stderr, "  Close %s, then run: qai browser launch\n", chosen.name)
	}
	os.Exit(1)
}

func browserList(args []string) {
	port := browserPort(args)
	tabs, err := cdpListTabs(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}

	jsonMode := false
	for _, a := range args {
		if a == "--json" {
			jsonMode = true
		}
	}

	// Privacy filter: redact denied tabs entirely; redact sensitive tabs'
	// title+URL unless --yes is set (caller has explicitly accepted the
	// disclosure for this invocation). Audit-log the redaction counts.
	denied, sensitive := 0, 0
	for i := range tabs {
		if d, _ := checkDomainDenied(tabs[i].URL); d {
			tabs[i].Title = "[denied]"
			tabs[i].URL = "[denied]"
			denied++
			continue
		}
		if checkDomainSensitivity(tabs[i].URL) && !yesFlag {
			tabs[i].Title = "[sensitive]"
			tabs[i].URL = "[sensitive]"
			sensitive++
		}
	}

	reason := "audit_only"
	if denied > 0 || sensitive > 0 {
		reason = fmt.Sprintf("redacted denied=%d sensitive=%d", denied, sensitive)
	}
	auditLog(auditEntry{Command: "list", Allowed: true, Reason: reason})

	if jsonMode {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(tabs)
		return
	}

	for _, t := range tabs {
		if t.Type != "page" {
			continue
		}
		title := t.Title
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Printf("%-12s %-60s %s\n", t.ID[:12], title, t.URL)
	}
	if sensitive > 0 || denied > 0 {
		fmt.Fprintf(os.Stderr, "(%d denied, %d sensitive entries redacted — re-run with --yes to reveal sensitive)\n", denied, sensitive)
	}
}

func browserOpen(args []string) {
	clean := stripFlags(args)
	if len(clean) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai browser open <url>")
		os.Exit(1)
	}
	url := clean[0]

	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	// Security gate on target URL (not current tab)
	targetTab := &cdpTab{URL: url, ID: tab.ID}
	if err := securityGate("open", targetTab, url); err != nil {
		os.Exit(1)
	}

	_, err = client.Call("Page.navigate", map[string]any{"url": url}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: navigate: %v\n", err)
		os.Exit(1)
	}

	// Wait for page load (best-effort, don't fail on timeout)
	loadErr := client.WaitEvent("Page.loadEventFired", 15*time.Second)

	// Get final URL and title
	result, err := client.Call("Runtime.evaluate", map[string]any{
		"expression":    "JSON.stringify({url: location.href, title: document.title})",
		"returnByValue": true,
	}, 5*time.Second)
	if err == nil {
		var val struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if json.Unmarshal(result, &val) == nil {
			var page struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			}
			if json.Unmarshal([]byte(val.Result.Value), &page) == nil {
				fmt.Printf("%s\n%s\n", page.Title, page.URL)
				return
			}
		}
	}

	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "warning: page load timeout, page may still be loading\n")
	}
	fmt.Println(url)
}

func browserTab(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai browser tab <id>")
		os.Exit(1)
	}
	port := browserPort(args)
	tabID := stripFlags(args)[0]

	// Find full ID from prefix
	tabs, err := cdpListTabs(port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	for _, t := range tabs {
		if t.ID == tabID || strings.HasPrefix(t.ID, tabID) {
			// Gate on the target tab's URL — switching focus to a
			// banking/SSO/internal-page tab is itself a privacy event,
			// and lets the next command operate on a sensitive surface.
			target := t
			if err := securityGate("tab", &target, target.URL); err != nil {
				os.Exit(1)
			}
			if err := cdpActivateTab(port, t.ID); err != nil {
				fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("activated: %s — %s\n", t.Title, t.URL)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "qai browser: tab %q not found\n", tabID)
	os.Exit(1)
}

func browserExtract(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	if err := securityGate("extract", tab, ""); err != nil {
		os.Exit(1)
	}

	expr := "document.body.innerText"
	for _, a := range args {
		if a == "--html" {
			expr = "document.body.innerHTML"
		}
	}

	result, err := client.Call("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
	}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: extract: %v\n", err)
		os.Exit(1)
	}

	var val struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &val); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: parse result: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(val.Result.Value)
}

func browserScreenshot(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	if err := securityGate("screenshot", tab, ""); err != nil {
		os.Exit(1)
	}

	result, err := client.Call("Page.captureScreenshot", map[string]any{
		"format":  "png",
		"quality": 80,
	}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: screenshot: %v\n", err)
		os.Exit(1)
	}

	var ss struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(result, &ss); err != nil || ss.Data == "" {
		fmt.Fprintln(os.Stderr, "qai browser: no screenshot data")
		os.Exit(1)
	}

	pngData, err := base64.StdEncoding.DecodeString(ss.Data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: decode png: %v\n", err)
		os.Exit(1)
	}

	// Output file
	outFile := fmt.Sprintf("screenshot-%s.png", time.Now().Format("20060102-150405"))
	for _, a := range stripFlags(args) {
		if a == "-o" {
			continue
		}
		// Grab the value after -o
		for i, b := range args {
			if b == "-o" && i+1 < len(args) {
				outFile = args[i+1]
				break
			}
		}
		break
	}
	// Simpler: just parse -o directly
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			outFile = args[i+1]
		}
	}

	if err := os.WriteFile(outFile, pngData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: write %s: %v\n", outFile, err)
		os.Exit(1)
	}
	fmt.Printf("saved: %s (%d bytes)\n", outFile, len(pngData))
}

func browserClick(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	clean := stripFlags(args)
	if len(clean) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai browser click <selector> or <x> <y>")
		os.Exit(1)
	}

	if err := securityGate("click", tab, strings.Join(clean, " ")); err != nil {
		os.Exit(1)
	}

	var cx, cy float64

	// Two numeric args = coordinates
	if len(clean) >= 2 {
		x, errX := strconv.ParseFloat(clean[0], 64)
		y, errY := strconv.ParseFloat(clean[1], 64)
		if errX == nil && errY == nil {
			cx, cy = x, y
			goto doClick
		}
	}

	// Otherwise treat as CSS selector
	{
		sel := clean[0]
		var selErr error
		cx, cy, selErr = resolveSelector(client, sel)
		if selErr != nil {
			fmt.Fprintf(os.Stderr, "qai browser: %v\n", selErr)
			os.Exit(1)
		}
	}

doClick:
	// Move → press → release
	for _, ev := range []struct {
		typ   string
		extra map[string]any
	}{
		{"mouseMoved", nil},
		{"mousePressed", map[string]any{"button": "left", "clickCount": 1}},
		{"mouseReleased", map[string]any{"button": "left", "clickCount": 1}},
	} {
		params := map[string]any{"type": ev.typ, "x": cx, "y": cy}
		for k, v := range ev.extra {
			params[k] = v
		}
		client.Call("Input.dispatchMouseEvent", params, 5*time.Second)
	}

	time.Sleep(300 * time.Millisecond) // settle
	fmt.Printf("clicked at (%.0f, %.0f)\n", cx, cy)
}

func browserType(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	clean := stripFlags(args)
	if len(clean) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai browser type \"text\"")
		os.Exit(1)
	}
	text := clean[0]

	if err := securityGate("type", tab, text); err != nil {
		os.Exit(1)
	}

	for _, ch := range text {
		s := string(ch)
		// keyDown
		client.Call("Input.dispatchKeyEvent", map[string]any{
			"type": "keyDown",
			"text": s,
			"key":  s,
		}, 3*time.Second)
		// keyUp
		client.Call("Input.dispatchKeyEvent", map[string]any{
			"type": "keyUp",
			"key":  s,
		}, 3*time.Second)
		time.Sleep(10 * time.Millisecond)
	}

	fmt.Printf("typed %d characters\n", len([]rune(text)))
}

func browserEval(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	clean := stripFlags(args)
	if len(clean) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai browser eval \"js expression\"")
		os.Exit(1)
	}
	expr := clean[0]

	// Security gate: Layer 1 (pattern block) + Layer 2-4 (domain check)
	if err := securityGate("eval", tab, expr); err != nil {
		os.Exit(1)
	}

	result, err := client.Call("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
	}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: eval: %v\n", err)
		os.Exit(1)
	}

	var val struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &val); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: parse: %v\n", err)
		os.Exit(1)
	}

	// Print the value
	switch val.Result.Type {
	case "string":
		var s string
		json.Unmarshal(val.Result.Value, &s)
		fmt.Println(s)
	case "undefined":
		fmt.Println("undefined")
	default:
		fmt.Println(string(val.Result.Value))
	}
}

func browserWait(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	clean := stripFlags(args)
	if len(clean) == 0 {
		fmt.Fprintln(os.Stderr, "usage: qai browser wait <selector> [timeout_seconds]")
		os.Exit(1)
	}
	sel := clean[0]

	if err := securityGate("wait", tab, sel); err != nil {
		os.Exit(1)
	}
	timeout := 10 * time.Second
	if len(clean) > 1 {
		if secs, err := strconv.Atoi(clean[1]); err == nil {
			timeout = time.Duration(secs) * time.Second
		}
	}

	deadline := time.Now().Add(timeout)
	expr := fmt.Sprintf("document.querySelector(%q) !== null", sel)

	for {
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "qai browser: timeout waiting for %s\n", sel)
			os.Exit(1)
		}

		result, err := client.Call("Runtime.evaluate", map[string]any{
			"expression":    expr,
			"returnByValue": true,
		}, 5*time.Second)
		if err == nil {
			var val struct {
				Result struct {
					Value bool `json:"value"`
				} `json:"result"`
			}
			if json.Unmarshal(result, &val) == nil && val.Result.Value {
				fmt.Printf("found: %s\n", sel)
				return
			}
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func browserSource(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	if err := securityGate("source", tab, ""); err != nil {
		os.Exit(1)
	}

	result, err := client.Call("Runtime.evaluate", map[string]any{
		"expression":    "document.documentElement.outerHTML",
		"returnByValue": true,
	}, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: source: %v\n", err)
		os.Exit(1)
	}

	var val struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &val); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: parse: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(val.Result.Value)
}

func browserPDF(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	if err := securityGate("pdf", tab, ""); err != nil {
		os.Exit(1)
	}

	result, err := client.Call("Page.printToPDF", map[string]any{
		"printBackground": true,
	}, 15*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: pdf: %v\n(Note: printToPDF may require headless mode)\n", err)
		os.Exit(1)
	}

	var pdf struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(result, &pdf); err != nil || pdf.Data == "" {
		fmt.Fprintln(os.Stderr, "qai browser: no PDF data")
		os.Exit(1)
	}

	pdfData, err := base64.StdEncoding.DecodeString(pdf.Data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: decode pdf: %v\n", err)
		os.Exit(1)
	}

	outFile := fmt.Sprintf("page-%s.pdf", time.Now().Format("20060102-150405"))
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			outFile = args[i+1]
		}
	}

	if err := os.WriteFile(outFile, pdfData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: write %s: %v\n", outFile, err)
		os.Exit(1)
	}
	fmt.Printf("saved: %s (%d bytes)\n", outFile, len(pdfData))
}

func browserClip(args []string) {
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	if err := securityGate("clip", tab, ""); err != nil {
		os.Exit(1)
	}

	// Get current page URL and title
	result, err := client.Call("Runtime.evaluate", map[string]any{
		"expression":    "JSON.stringify({url: location.href, title: document.title})",
		"returnByValue": true,
	}, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser: get page info: %v\n", err)
		os.Exit(1)
	}

	var evalResult struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	json.Unmarshal(result, &evalResult)

	var page struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	json.Unmarshal([]byte(evalResult.Result.Value), &page)

	if page.URL == "" {
		fmt.Fprintln(os.Stderr, "qai browser: could not get page URL")
		os.Exit(1)
	}

	// Use existing clip-to-joplin script with the live URL
	clean := stripFlags(args)
	notebook := "Clips"
	title := page.Title
	if len(clean) > 0 {
		notebook = clean[0]
	}
	if len(clean) > 1 {
		title = clean[1]
	}

	_ = notebook
	_ = title
	fmt.Fprintf(os.Stderr, "qai browser clip: use 'qai clip %s' instead\n", page.URL)
}

// ─── usage ────────────────────────────────────────────────────────────────

func browserUsage() {
	fmt.Fprint(os.Stderr, `qai browser — control your Chrome/Brave via CDP debug port

Connects to your existing browser session. No headless browser, no Playwright,
no Node.js. Uses your real cookies, auth, and fingerprints.

Commands:
  qai browser launch                      Start browser with debug port (auto-detects Brave/Chrome)
  qai browser list                        List open tabs
  qai browser open <url>                  Navigate to URL
  qai browser tab <id>                    Activate a specific tab
  qai browser extract [--html]            Get page text (or HTML)
  qai browser screenshot [-o file.png]    Capture screenshot as PNG
  qai browser click <selector>            Click element by CSS selector
  qai browser click <x> <y>              Click at coordinates
  qai browser type "text"                 Type text character by character
  qai browser eval "js expression"        Evaluate JavaScript
  qai browser clip [notebook] [title]     Extract page + save to Joplin
  qai browser wait <selector> [timeout]   Wait for element to appear
  qai browser source                      Get full page HTML source
  qai browser pdf [-o file.pdf]           Print page to PDF
  qai browser scrape <urls.csv>           Batch scrape — extract text from each URL
  qai browser scrape <urls.csv> --screenshot  Batch screenshot each URL
  qai browser scrape <urls.csv> --html    Batch extract HTML from each URL

Global flags:
  --port <n>            CDP port (default: 9222, or QAI_BROWSER_PORT env)
  --tab <id>            Target a specific tab by ID (prefix match)
  --json                Machine-readable JSON output (where supported)
`)
}
