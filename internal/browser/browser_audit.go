// browser_audit.go — `qai browser audit <url>` is the one-shot for
// SEO + page-debug runs. It compresses what was previously four-to-six
// separate commands (launch → open → network → console → screenshot →
// extract) into a single invocation that hands back a structured
// report.
//
// What it does, in order:
//
//   1. Connect to the running browser on the debug port.
//   2. Enable Network + Runtime + Log domains.
//   3. Navigate the target tab to the URL.
//   4. Capture events for `--duration` (default 10s). Page load fires
//      somewhere in that window; subsequent XHR / fetch traffic during
//      the rest of the window gets recorded too.
//   5. Pull Performance.getMetrics for navigation timing.
//   6. Take a screenshot.
//   7. Render a bundled JSON report (default) or a human summary.
//
// Output JSON shape (stable; tooling can rely on it):
//
//   { "url": ..., "captured_at": ..., "duration_ms": ...,
//     "page_title": ..., "screenshot_path": ...,
//     "summary": { request_count, total_bytes, failed_requests,
//                  console_errors, console_warnings, load_time_ms,
//                  dom_content_loaded_ms, dom_node_count },
//     "requests": [NetRequest...],
//     "console":  [ConsoleMessage...],
//     "metrics":  { name: value, ... } }

package browser

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ── command entry ────────────────────────────────────────────────────────

func browserAudit(args []string) {
	opts, err := parseAuditArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser audit: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: run 'qai browser audit --help' for flag syntax")
		os.Exit(1)
	}
	if opts.help {
		fmt.Print(helpAudit)
		return
	}
	if opts.url == "" {
		fmt.Fprintln(os.Stderr, "qai browser audit: missing URL")
		fmt.Fprintln(os.Stderr, "  → fix: qai browser audit https://example.com [--duration 10s] [-o report.json]")
		os.Exit(1)
	}

	c, _, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser audit: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	// Enable every event domain we're going to harvest BEFORE navigating
	// so we don't miss requestWillBeSent on the document itself.
	for _, dom := range []string{"Network.enable", "Runtime.enable", "Log.enable", "Page.enable"} {
		if _, err := c.Call(dom, nil, 5*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "qai browser audit: %s failed: %v\n", dom, err)
			os.Exit(1)
		}
	}

	startedAt := time.Now()
	fmt.Fprintf(os.Stderr, "▶ navigating to %s\n", opts.url)
	if _, err := c.Call("Page.navigate", map[string]any{"url": opts.url}, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser audit: Page.navigate: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: verify the URL is reachable; the browser tab may be sandboxed against navigation")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "  capturing for %s …\n", opts.duration)
	events := c.Capture([]string{"Network.", "Runtime.consoleAPICalled", "Log."}, opts.duration)

	requests := requestsFromEvents(events)
	console := consoleFromEvents(events)
	metrics := fetchMetrics(c)
	title := fetchTitle(c)

	report := AuditReport{
		URL:        opts.url,
		CapturedAt: startedAt.UTC().Format(time.RFC3339),
		DurationMs: opts.duration.Milliseconds(),
		PageTitle:  title,
		Requests:   requests,
		Console:    console,
		Metrics:    metrics,
		Summary:    summarizeAudit(requests, console, metrics),
	}

	// Screenshot.
	if !opts.noScreenshot {
		report.ScreenshotPath = takeScreenshot(c, opts.screenshotPath, opts.url)
	}

	// Output.
	switch {
	case opts.summary:
		fmt.Print(renderAuditSummary(report))
	default:
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai browser audit: marshal: %v\n", err)
			os.Exit(1)
		}
		if opts.outFile != "" {
			if err := os.WriteFile(opts.outFile, append(b, '\n'), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "qai browser audit: write %s: %v\n", opts.outFile, err)
				fmt.Fprintln(os.Stderr, "  → fix: ensure the parent directory exists + is writable, or pick a different -o path")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "  → %s\n", opts.outFile)
		} else {
			os.Stdout.Write(b)
			fmt.Println()
		}
	}
}

// ── args ─────────────────────────────────────────────────────────────────

type auditOpts struct {
	url             string
	duration        time.Duration
	outFile         string
	screenshotPath  string
	noScreenshot    bool
	summary         bool
	help            bool
}

func parseAuditArgs(args []string) (auditOpts, error) {
	o := auditOpts{duration: 10 * time.Second}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--help", "-h":
			o.help = true
		case "--duration":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--duration requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return o, fmt.Errorf("bad --duration %q: %v", args[i], err)
			}
			o.duration = d
		case "-o", "--output":
			if i+1 >= len(args) {
				return o, fmt.Errorf("-o requires a path")
			}
			i++
			o.outFile = args[i]
		case "--screenshot":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--screenshot requires a path")
			}
			i++
			o.screenshotPath = args[i]
		case "--no-screenshot":
			o.noScreenshot = true
		case "--summary":
			o.summary = true
		case "--port", "--tab":
			if i+1 < len(args) {
				i++
			}
		default:
			if !strings.HasPrefix(args[i], "-") && o.url == "" {
				o.url = args[i]
			}
		}
	}
	return o, nil
}

// ── report shape ─────────────────────────────────────────────────────────

type AuditReport struct {
	URL            string             `json:"url"`
	CapturedAt     string             `json:"captured_at"`
	DurationMs     int64              `json:"duration_ms"`
	PageTitle      string             `json:"page_title,omitempty"`
	ScreenshotPath string             `json:"screenshot_path,omitempty"`
	Summary        AuditSummary       `json:"summary"`
	Requests       []NetRequest       `json:"requests"`
	Console        []ConsoleMessage   `json:"console"`
	Metrics        map[string]float64 `json:"metrics,omitempty"`
}

type AuditSummary struct {
	RequestCount        int     `json:"request_count"`
	TotalBytes          int64   `json:"total_bytes"`
	FailedRequests      int     `json:"failed_requests"`
	ConsoleErrors       int     `json:"console_errors"`
	ConsoleWarnings     int     `json:"console_warnings"`
	LoadTimeMs          float64 `json:"load_time_ms,omitempty"`
	DOMContentLoadedMs  float64 `json:"dom_content_loaded_ms,omitempty"`
	DOMNodeCount        float64 `json:"dom_node_count,omitempty"`
}

// ── console event reducer ────────────────────────────────────────────────

type ConsoleMessage struct {
	Level  string `json:"level"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
	URL    string `json:"url,omitempty"`
	Line   int    `json:"line,omitempty"`
}

func consoleFromEvents(events []cdpEvent) []ConsoleMessage {
	var out []ConsoleMessage
	for _, ev := range events {
		switch ev.Method {
		case "Runtime.consoleAPICalled":
			var p struct {
				Type string `json:"type"`
				Args []struct {
					Value       any    `json:"value"`
					Description string `json:"description"`
				} `json:"args"`
				StackTrace struct {
					CallFrames []struct {
						URL          string `json:"url"`
						LineNumber   int    `json:"lineNumber"`
					} `json:"callFrames"`
				} `json:"stackTrace"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				continue
			}
			texts := make([]string, 0, len(p.Args))
			for _, a := range p.Args {
				if a.Value != nil {
					texts = append(texts, fmt.Sprintf("%v", a.Value))
				} else if a.Description != "" {
					texts = append(texts, a.Description)
				}
			}
			msg := ConsoleMessage{Level: p.Type, Text: strings.Join(texts, " ")}
			if len(p.StackTrace.CallFrames) > 0 {
				msg.URL = p.StackTrace.CallFrames[0].URL
				msg.Line = p.StackTrace.CallFrames[0].LineNumber
			}
			out = append(out, msg)
		case "Log.entryAdded":
			var p struct {
				Entry struct {
					Source     string `json:"source"`
					Level      string `json:"level"`
					Text       string `json:"text"`
					URL        string `json:"url"`
					LineNumber int    `json:"lineNumber"`
				} `json:"entry"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				continue
			}
			out = append(out, ConsoleMessage{
				Level:  p.Entry.Level,
				Text:   p.Entry.Text,
				Source: p.Entry.Source,
				URL:    p.Entry.URL,
				Line:   p.Entry.LineNumber,
			})
		}
	}
	return out
}

// ── perf metrics ─────────────────────────────────────────────────────────

func fetchMetrics(c *cdpClient) map[string]float64 {
	// Performance.enable is implicit when Performance.getMetrics is
	// called, but be explicit so the CDP layer doesn't complain.
	_, _ = c.Call("Performance.enable", nil, 3*time.Second)
	raw, err := c.Call("Performance.getMetrics", nil, 3*time.Second)
	if err != nil {
		return nil
	}
	var parsed struct {
		Metrics []struct {
			Name  string  `json:"name"`
			Value float64 `json:"value"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	out := make(map[string]float64, len(parsed.Metrics))
	for _, m := range parsed.Metrics {
		out[m.Name] = m.Value
	}
	return out
}

func fetchTitle(c *cdpClient) string {
	raw, err := c.Call("Runtime.evaluate", map[string]any{
		"expression":    "document.title",
		"returnByValue": true,
	}, 3*time.Second)
	if err != nil {
		return ""
	}
	var v struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v.Result.Value
}

// ── screenshot ───────────────────────────────────────────────────────────

func takeScreenshot(c *cdpClient, targetPath, url string) string {
	if targetPath == "" {
		// Default to ~/Pictures/generated/audit-<hostslug>-<ts>.png
		host := safeHost(url)
		ts := time.Now().Format("20060102-150405")
		home, _ := os.UserHomeDir()
		dir := filepath.Join(home, "Pictures", "generated")
		_ = os.MkdirAll(dir, 0o755)
		targetPath = filepath.Join(dir, fmt.Sprintf("audit-%s-%s.png", host, ts))
	}
	raw, err := c.Call("Page.captureScreenshot", map[string]any{
		"format":      "png",
		"fromSurface": true,
	}, 15*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warning: screenshot failed: %v\n", err)
		return ""
	}
	var v struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if v.Data == "" {
		return ""
	}
	// Decode + write.
	data, err := decodeBase64(v.Data)
	if err != nil {
		return ""
	}
	if err := os.WriteFile(targetPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: write screenshot %s: %v\n", targetPath, err)
		return ""
	}
	return targetPath
}

func safeHost(rawURL string) string {
	s := rawURL
	for _, prefix := range []string{"https://", "http://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		s = "page"
	}
	return strings.ReplaceAll(s, ":", "-")
}

// ── summary + render ─────────────────────────────────────────────────────

func summarizeAudit(reqs []NetRequest, cons []ConsoleMessage, metrics map[string]float64) AuditSummary {
	s := AuditSummary{RequestCount: len(reqs)}
	for _, r := range reqs {
		s.TotalBytes += r.Size
		if r.Failed != "" || (r.Status >= 400 && r.Status < 600) {
			s.FailedRequests++
		}
	}
	for _, m := range cons {
		switch strings.ToLower(m.Level) {
		case "error":
			s.ConsoleErrors++
		case "warning", "warn":
			s.ConsoleWarnings++
		}
	}
	if metrics != nil {
		if v, ok := metrics["DomContentLoaded"]; ok {
			s.DOMContentLoadedMs = v * 1000
		}
		if v, ok := metrics["Load"]; ok {
			s.LoadTimeMs = v * 1000
		}
		if v, ok := metrics["Nodes"]; ok {
			s.DOMNodeCount = v
		}
	}
	return s
}

func renderAuditSummary(r AuditReport) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", r.URL)
	if r.PageTitle != "" {
		fmt.Fprintf(&sb, "  %q\n", r.PageTitle)
	}
	fmt.Fprintf(&sb, "  captured %s over %dms\n\n", r.CapturedAt, r.DurationMs)

	fmt.Fprintln(&sb, "Network:")
	fmt.Fprintf(&sb, "  %d requests, %.1f KB transferred", r.Summary.RequestCount, float64(r.Summary.TotalBytes)/1024)
	if r.Summary.FailedRequests > 0 {
		fmt.Fprintf(&sb, ", %d failed/4xx/5xx", r.Summary.FailedRequests)
	}
	fmt.Fprintln(&sb)

	if r.Summary.LoadTimeMs > 0 || r.Summary.DOMContentLoadedMs > 0 {
		fmt.Fprintln(&sb, "\nTiming:")
		if r.Summary.DOMContentLoadedMs > 0 {
			fmt.Fprintf(&sb, "  DOMContentLoaded  %.0f ms\n", r.Summary.DOMContentLoadedMs)
		}
		if r.Summary.LoadTimeMs > 0 {
			fmt.Fprintf(&sb, "  load              %.0f ms\n", r.Summary.LoadTimeMs)
		}
		if r.Summary.DOMNodeCount > 0 {
			fmt.Fprintf(&sb, "  DOM nodes         %.0f\n", r.Summary.DOMNodeCount)
		}
	}

	if r.Summary.ConsoleErrors+r.Summary.ConsoleWarnings > 0 {
		fmt.Fprintf(&sb, "\nConsole: %d error(s), %d warning(s)\n", r.Summary.ConsoleErrors, r.Summary.ConsoleWarnings)
		for _, m := range r.Console {
			if strings.EqualFold(m.Level, "error") || strings.EqualFold(m.Level, "warning") || strings.EqualFold(m.Level, "warn") {
				fmt.Fprintf(&sb, "  [%s] %s\n", m.Level, truncURL(m.Text, 100))
			}
		}
	}

	if r.Summary.FailedRequests > 0 {
		fmt.Fprintln(&sb, "\nFailed requests:")
		for _, req := range r.Requests {
			if req.Failed != "" || (req.Status >= 400 && req.Status < 600) {
				label := req.Failed
				if label == "" {
					label = fmt.Sprintf("HTTP %d %s", req.Status, req.StatusText)
				}
				fmt.Fprintf(&sb, "  %s  %s  %s\n", req.Method, truncURL(req.URL, 60), label)
			}
		}
	}

	if r.ScreenshotPath != "" {
		fmt.Fprintf(&sb, "\nScreenshot: %s\n", r.ScreenshotPath)
	}

	// Top-5 slowest. Cheap insight that often surfaces the bottleneck.
	if len(r.Requests) > 0 {
		sorted := append([]NetRequest(nil), r.Requests...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].DurationMs > sorted[j].DurationMs })
		fmt.Fprintln(&sb, "\nSlowest requests:")
		n := 5
		if len(sorted) < n {
			n = len(sorted)
		}
		for _, req := range sorted[:n] {
			fmt.Fprintf(&sb, "  %6.0f ms  %s  %s\n", req.DurationMs, req.Method, truncURL(req.URL, 70))
		}
	}

	return sb.String()
}

// ── base64 ────────────────────────────────────────────────────────────────

func decodeBase64(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// ── help ─────────────────────────────────────────────────────────────────

const helpAudit = `qai browser audit — one-shot SEO / page-debug capture

Navigates a browser tab to a URL, records the page load (network
requests, console messages, perf metrics), takes a screenshot, and
returns a bundled JSON report — same data DevTools would show, all in
one command.

USAGE
  qai browser audit <url> [--duration 10s] [--summary] [-o report.json]
                          [--screenshot file.png] [--no-screenshot]
                          [--tab <id>] [--port <n>]

WORKFLOW
  1. Launch your browser with the debug port if not already running:
       open -a "Brave Browser" --args --remote-debugging-port=9222
  2. Run:  qai browser audit https://example.com --summary
  3. Read the network + console + timing summary, or feed the JSON
     into another tool.

FLAGS
  --duration <d>     Capture window after navigation (default 10s).
  -o, --output <f>   Write JSON report to <f> (default: stdout).
  --screenshot <f>   Screenshot path (default: ~/Pictures/generated/audit-<host>-<ts>.png).
  --no-screenshot    Skip the screenshot.
  --summary          Human-readable summary instead of JSON.

OUTPUT (JSON, default)
  { url, captured_at, duration_ms, page_title, screenshot_path,
    summary: { request_count, total_bytes, failed_requests,
               console_errors, console_warnings,
               load_time_ms, dom_content_loaded_ms, dom_node_count },
    requests: [...NetRequest...],
    console:  [...ConsoleMessage...],
    metrics:  { name: value, ... } }

SEE ALSO
  qai browser network    — network capture only (smaller, focused)
  qai browser eval "JS"  — DevTools Console expressions
  qai browser pdf        — save current page to PDF
`
