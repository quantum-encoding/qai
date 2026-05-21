// browser_network.go — `qai browser network` captures Network.* events
// over a window of time and emits a JSON request list (or a brief
// human-readable summary).
//
// The intended workflow: open the page you want to analyse in your
// real browser, then run 'qai browser network --duration 10s' and
// reload the tab. The capture window picks up every request the page
// fires — XHR, fetch, document, image, font, script, stylesheet —
// with status, MIME type, encoded bytes, and timing.
//
// Output suits both human reading (--summary) and downstream tooling
// (JSON, default). Pipe the JSON into jq for filtering, or write to
// a file with -o for later analysis. Same data DevTools' Network panel
// shows, no UI required.

package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// browserNetwork is the entry point for `qai browser network`.
func browserNetwork(args []string) {
	opts, err := parseNetworkArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser network: %v\n", err)
		os.Exit(1)
	}
	if opts.help {
		fmt.Println(helpNetwork)
		return
	}

	c, _, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser network: connect: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()

	if _, err := c.Call("Network.enable", nil, 5*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser network: Network.enable: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "▶ capturing network for %s …\n", opts.duration)
	events := c.Capture([]string{"Network."}, opts.duration)
	requests := requestsFromEvents(events)
	if len(opts.filterTypes) > 0 {
		requests = filterByType(requests, opts.filterTypes)
	}
	fmt.Fprintf(os.Stderr, "  captured %d request(s)\n", len(requests))

	var out []byte
	if opts.summary {
		out = []byte(summarizeRequests(requests))
	} else {
		b, err := json.MarshalIndent(requests, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai browser network: marshal: %v\n", err)
			os.Exit(1)
		}
		out = append(b, '\n')
	}

	if opts.outFile != "" {
		if err := os.WriteFile(opts.outFile, out, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "qai browser network: write %s: %v\n", opts.outFile, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "  → %s\n", opts.outFile)
		return
	}
	os.Stdout.Write(out)
}

// ── arg parsing ───────────────────────────────────────────────────────────

type networkOpts struct {
	duration    time.Duration
	outFile     string
	summary     bool
	filterTypes []string // empty = no filter
	help        bool
}

func parseNetworkArgs(args []string) (networkOpts, error) {
	o := networkOpts{duration: 5 * time.Second}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--help", "-h":
			o.help = true
		case "--duration":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--duration requires a value (e.g. 10s, 1m)")
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
		case "--summary":
			o.summary = true
		case "--filter":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--filter requires a comma-separated list of types")
			}
			i++
			o.filterTypes = strings.Split(args[i], ",")
		case "--json":
			// default — kept for clarity
		case "--port", "--tab":
			// consumed by connectToTab via the original args slice
			if i+1 < len(args) {
				i++
			}
		}
	}
	return o, nil
}

// ── network request reducer ───────────────────────────────────────────────
//
// Network.* events arrive as a sequence keyed by requestId. We pair
// requestWillBeSent / responseReceived / loadingFinished / loadingFailed
// into one record per requestId. The shape mirrors DevTools' Network
// panel row.

// NetRequest is one row's worth of data — keep the JSON field names
// stable; downstream tooling (jq, dashboards) reads them.
type NetRequest struct {
	ID          string            `json:"id"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Type        string            `json:"type"`
	Status      int               `json:"status,omitempty"`
	StatusText  string            `json:"status_text,omitempty"`
	MimeType    string            `json:"mime_type,omitempty"`
	Size        int64             `json:"size,omitempty"`
	DurationMs  float64           `json:"duration_ms,omitempty"`
	FromCache   bool              `json:"from_cache,omitempty"`
	Failed      string            `json:"failed,omitempty"`
	Protocol    string            `json:"protocol,omitempty"`
	RemoteIP    string            `json:"remote_ip,omitempty"`
	Initiator   string            `json:"initiator,omitempty"`
	Headers     map[string]string `json:"request_headers,omitempty"`
	ResHeaders  map[string]string `json:"response_headers,omitempty"`
	startMonoMs float64
}

func requestsFromEvents(events []cdpEvent) []NetRequest {
	byID := map[string]*NetRequest{}

	for _, ev := range events {
		var p map[string]any
		if err := json.Unmarshal(ev.Params, &p); err != nil {
			continue
		}
		rid, _ := p["requestId"].(string)
		if rid == "" {
			continue
		}

		switch ev.Method {
		case "Network.requestWillBeSent":
			r := NetRequest{ID: rid}
			if req, ok := p["request"].(map[string]any); ok {
				r.URL, _ = req["url"].(string)
				r.Method, _ = req["method"].(string)
				if hdrs, ok := req["headers"].(map[string]any); ok {
					r.Headers = stringMap(hdrs)
				}
			}
			if t, ok := p["type"].(string); ok {
				r.Type = t
			}
			if ts, ok := p["timestamp"].(float64); ok {
				r.startMonoMs = ts * 1000
			}
			if init, ok := p["initiator"].(map[string]any); ok {
				if t, _ := init["type"].(string); t != "" {
					r.Initiator = t
				}
			}
			byID[rid] = &r
		case "Network.responseReceived":
			r := byID[rid]
			if r == nil {
				continue
			}
			if resp, ok := p["response"].(map[string]any); ok {
				if status, ok := resp["status"].(float64); ok {
					r.Status = int(status)
				}
				if st, _ := resp["statusText"].(string); st != "" {
					r.StatusText = st
				}
				if mt, _ := resp["mimeType"].(string); mt != "" {
					r.MimeType = mt
				}
				if proto, _ := resp["protocol"].(string); proto != "" {
					r.Protocol = proto
				}
				if rip, _ := resp["remoteIPAddress"].(string); rip != "" {
					r.RemoteIP = rip
				}
				if fromCache, _ := resp["fromDiskCache"].(bool); fromCache {
					r.FromCache = true
				}
				if hdrs, ok := resp["headers"].(map[string]any); ok {
					r.ResHeaders = stringMap(hdrs)
				}
			}
		case "Network.loadingFinished":
			r := byID[rid]
			if r == nil {
				continue
			}
			if size, ok := p["encodedDataLength"].(float64); ok {
				r.Size = int64(size)
			}
			if ts, ok := p["timestamp"].(float64); ok && r.startMonoMs > 0 {
				r.DurationMs = ts*1000 - r.startMonoMs
			}
		case "Network.loadingFailed":
			r := byID[rid]
			if r == nil {
				continue
			}
			if reason, _ := p["errorText"].(string); reason != "" {
				r.Failed = reason
			}
		}
	}

	out := make([]NetRequest, 0, len(byID))
	for _, r := range byID {
		if r != nil {
			out = append(out, *r)
		}
	}
	// Stable ordering: by start time so the JSON matches the chronology
	// the user saw in their tab.
	sort.Slice(out, func(i, j int) bool { return out[i].startMonoMs < out[j].startMonoMs })
	return out
}

// stringMap reduces an MEditor-style header map (string|number values)
// down to plain string→string. CDP sends header values as strings, but
// any field could in principle be non-string; guard with a type assertion.
func stringMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func filterByType(rs []NetRequest, types []string) []NetRequest {
	allow := map[string]bool{}
	for _, t := range types {
		allow[strings.ToLower(strings.TrimSpace(t))] = true
	}
	out := rs[:0]
	for _, r := range rs {
		if allow[strings.ToLower(r.Type)] {
			out = append(out, r)
		}
	}
	return out
}

// ── summary mode ──────────────────────────────────────────────────────────

func summarizeRequests(rs []NetRequest) string {
	var sb strings.Builder
	if len(rs) == 0 {
		return "no requests captured\n"
	}
	byType := map[string]int{}
	byStatus := map[int]int{}
	var totalSize int64
	var slowest []NetRequest
	var failed []NetRequest
	for _, r := range rs {
		byType[r.Type]++
		byStatus[r.Status]++
		totalSize += r.Size
		if r.Failed != "" {
			failed = append(failed, r)
		}
	}
	// Top 5 slowest.
	sorted := append([]NetRequest(nil), rs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DurationMs > sorted[j].DurationMs })
	if len(sorted) > 5 {
		slowest = sorted[:5]
	} else {
		slowest = sorted
	}

	fmt.Fprintf(&sb, "%d requests · %.1f KB transferred\n\n", len(rs), float64(totalSize)/1024)

	fmt.Fprintln(&sb, "By type:")
	for _, kv := range sortedCountPairs(byType) {
		fmt.Fprintf(&sb, "  %-12s %d\n", kv.k, kv.v)
	}

	fmt.Fprintln(&sb, "\nBy status:")
	for _, kv := range sortedIntCountPairs(byStatus) {
		fmt.Fprintf(&sb, "  %-4d %d\n", kv.k, kv.v)
	}

	if len(failed) > 0 {
		fmt.Fprintf(&sb, "\nFailed (%d):\n", len(failed))
		for _, r := range failed {
			fmt.Fprintf(&sb, "  %s  %s  %s\n", r.Method, truncURL(r.URL, 60), r.Failed)
		}
	}

	fmt.Fprintln(&sb, "\nSlowest:")
	for _, r := range slowest {
		fmt.Fprintf(&sb, "  %6.0f ms  %s  %s\n", r.DurationMs, r.Method, truncURL(r.URL, 70))
	}
	return sb.String()
}

type kvPair struct {
	k string
	v int
}

func sortedCountPairs(m map[string]int) []kvPair {
	out := make([]kvPair, 0, len(m))
	for k, v := range m {
		out = append(out, kvPair{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].v > out[j].v })
	return out
}

type intKvPair struct {
	k int
	v int
}

func sortedIntCountPairs(m map[int]int) []intKvPair {
	out := make([]intKvPair, 0, len(m))
	for k, v := range m {
		out = append(out, intKvPair{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].k < out[j].k })
	return out
}

func truncURL(u string, n int) string {
	if len(u) <= n {
		return u
	}
	return u[:n-1] + "…"
}

// ── help ─────────────────────────────────────────────────────────────────

const helpNetwork = `qai browser network — capture Network.* events

Records every request a tab makes during a capture window. Same data
DevTools' Network panel shows, dumped as JSON (default) or summary.

USAGE
  qai browser network [--duration 5s] [--filter <types>] [--summary]
                      [-o <file>] [--tab <id>] [--port <n>]

WORKFLOW
  1. Open the page you want to analyse in your existing browser tab.
  2. Run 'qai browser network --duration 10s' — capture window starts.
  3. Reload the tab (or click whatever triggers the requests you care
     about). Every request in the window is recorded.
  4. JSON output goes to stdout, or to a file with -o.

FLAGS
  --duration <d>     Capture window (default 5s). Accepts Go duration
                     syntax: 5s, 30s, 2m, etc.
  --filter <types>   Comma-separated list of types to keep (e.g.
                     "xhr,fetch,script"). Default: keep all.
  --summary          Human-readable summary instead of JSON: counts by
                     type/status, top 5 slowest requests, failures.
  -o, --output <f>   Write to file instead of stdout.

OUTPUT (JSON)
  An array of request records, sorted by start time. Per record:
    id, method, url, type, status, status_text, mime_type,
    size (encoded bytes), duration_ms, from_cache, failed,
    protocol, remote_ip, initiator,
    request_headers, response_headers

EXAMPLES
  qai browser network                         # 5s window, JSON to stdout
  qai browser network --duration 30s -o reqs.json
  qai browser network --filter xhr,fetch      # only AJAX traffic
  qai browser network --summary               # human view of the window

SEE ALSO
  qai browser eval "JS"   # run anything DevTools Console can
  qai browser pdf         # save current page to PDF`
