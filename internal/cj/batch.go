// batch.go — `qai cj batch <urls.csv>`. Walks a list of CJ URLs,
// driving the full per-URL chain (navigate → wait → clip → extract)
// via subprocess calls to `qai browser` and the in-process Joplin
// client + Parse(). Aggregates a deduplicated CJProduct[] across all
// URLs and emits one JSON document the agent can pipe to jq / SQL /
// a SurrealDB writer.
//
// CSV format mirrors `qai browser scrape`:
//
//	url[,label]
//
// Header row is auto-skipped. The label, when provided, becomes the
// Joplin note title; otherwise the title is derived from the URL.

package cj

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// BatchRun is the per-URL outcome. Counts come from cj.Parse; NoteID
// is what `qai browser clip` returned; Error is set on any failure
// (subprocess non-zero, Joplin fetch fail, etc.) and the run is
// counted as failed.
type BatchRun struct {
	URL    string `json:"url"`
	Label  string `json:"label,omitempty"`
	NoteID string `json:"note_id,omitempty"`
	Counts Counts `json:"counts"`
	Error  string `json:"error,omitempty"`
}

// BatchResult is the aggregate emitted as JSON. CJProducts is the
// PID-deduplicated union across every URL — agents can sort/filter
// without seeing the same product twice.
type BatchResult struct {
	SourceCSV  string      `json:"source_csv"`
	Notebook   string      `json:"notebook"`
	StartedAt  string      `json:"started_at"`
	FinishedAt string      `json:"finished_at"`
	Runs       []BatchRun  `json:"runs"`
	CJProducts []CJProduct `json:"cj_products"` // deduped union
	Counts     BatchCounts `json:"counts"`
}

type BatchCounts struct {
	URLs       int `json:"urls"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	CJProducts int `json:"cj_products_total"`
}

type batchEntry struct {
	URL   string
	Label string
}

func cmdBatch(args []string) {
	if len(args) == 0 || isHelp(args[0]) {
		os.Stdout.WriteString(helpBatch + "\n")
		return
	}

	var (
		csvPath  string
		notebook = "dropship-accelerator/clips"
		delayMS  int
		// CJ pages render a header (logo, nav icons) immediately, then
		// hydrate the React product grid asynchronously. A bare `img`
		// selector fires on the logo — too early. `a[href*="/product/"]`
		// only appears once at least one product card is in the DOM,
		// so the clip captures real data. Override with --wait for
		// non-CJ pages or grid layouts that use different selectors.
		waitSel = `a[href*="/product/"]`
		timeout = "30s"
		outFile    string
		tab        string
		summary    bool
		strict     bool
		softWait   bool
		maxRetries = 3
		retryBase  = 30 * time.Second
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--notebook":
			notebook = nextArg(args, i, "--notebook")
			i++
		case "--delay":
			n, _ := strconv.Atoi(nextArg(args, i, "--delay"))
			delayMS = n
			i++
		case "--wait":
			waitSel = nextArg(args, i, "--wait")
			i++
		case "--timeout":
			timeout = nextArg(args, i, "--timeout")
			i++
		case "-o", "--output":
			outFile = nextArg(args, i, "-o")
			i++
		case "--tab":
			tab = nextArg(args, i, "--tab")
			i++
		case "--summary":
			summary = true
		case "--strict":
			strict = true
		case "--soft-wait":
			softWait = true
		case "--max-retries":
			n, err := strconv.Atoi(nextArg(args, i, "--max-retries"))
			if err != nil || n < 0 {
				fmt.Fprintf(os.Stderr, "qai cj batch: --max-retries must be >= 0\n")
				os.Exit(1)
			}
			maxRetries = n
			i++
		case "--retry-base":
			d, err := time.ParseDuration(nextArg(args, i, "--retry-base"))
			if err != nil || d <= 0 {
				fmt.Fprintf(os.Stderr, "qai cj batch: --retry-base invalid duration: %q\n", args[i+1])
				os.Exit(1)
			}
			retryBase = d
			i++
		case "--json":
			// implied
		default:
			if csvPath == "" && !looksLikeFlag(a) {
				csvPath = a
			}
		}
	}

	if csvPath == "" {
		fmt.Fprintln(os.Stderr, "qai cj batch: missing CSV file path")
		fmt.Fprintln(os.Stderr, "  → fix: qai cj batch <urls.csv>   (col1 = URL, col2 = optional label)")
		os.Exit(1)
	}

	entries, err := readBatchCSV(csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai cj batch: %v\n", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "qai cj batch: no URLs in %s\n", csvPath)
		fmt.Fprintln(os.Stderr, "  → fix: first column must be http(s) URLs; header row is auto-skipped")
		os.Exit(1)
	}

	// Pin the target tab once for the whole batch so multiple-tab
	// browsers don't race. If --tab wasn't given, discover the first
	// page tab. Without pinning, every `qai browser open` would
	// re-discover and potentially pick a different tab if the user
	// flips one mid-batch.
	if tab == "" {
		tab = discoverFirstPageTab()
	}
	if tab == "" {
		fmt.Fprintln(os.Stderr, "qai cj batch: no page tabs on the browser")
		fmt.Fprintln(os.Stderr, "  → fix: open at least one tab in your browser, then retry")
		os.Exit(1)
	}

	joplinClient, err := newJoplinClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai cj batch: joplin: %v\n", err)
		os.Exit(1)
	}

	result := &BatchResult{
		SourceCSV:  csvPath,
		Notebook:   notebook,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		Runs:       []BatchRun{},
		CJProducts: []CJProduct{},
	}
	result.Counts.URLs = len(entries)

	seen := map[string]struct{}{}
	fmt.Fprintf(os.Stderr, "batch: %d URLs → notebook %q (tab %s)\n",
		len(entries), notebook, tab[:8])

	for i, entry := range entries {
		if i > 0 && delayMS > 0 {
			time.Sleep(time.Duration(delayMS) * time.Millisecond)
		}
		run := processOne(entry, notebook, tab, waitSel, timeout, softWait,
			maxRetries, retryBase, joplinClient)
		result.Runs = append(result.Runs, run)
		if run.Error != "" {
			result.Counts.Failed++
			fmt.Fprintf(os.Stderr, "  [%d/%d] FAIL %s — %s\n",
				i+1, len(entries), entry.URL, run.Error)
			continue
		}
		result.Counts.Succeeded++
		// Dedup against the global PID set.
		// (run.Counts already reflects the per-URL parse, but we re-walk
		// the products to apply the dedup; cheap.)
		for _, p := range processProducts[run.NoteID] {
			if _, dup := seen[p.PID]; dup {
				continue
			}
			seen[p.PID] = struct{}{}
			result.CJProducts = append(result.CJProducts, p)
		}
		delete(processProducts, run.NoteID)
		fmt.Fprintf(os.Stderr, "  [%d/%d] OK   %s — %d products → note %s\n",
			i+1, len(entries), entry.URL, run.Counts.CJProducts, run.NoteID[:8])
	}

	result.Counts.CJProducts = len(result.CJProducts)
	result.FinishedAt = time.Now().UTC().Format(time.RFC3339)

	if strict && (result.Counts.Failed > 0 || result.Counts.CJProducts == 0) {
		fmt.Fprintf(os.Stderr,
			"qai cj batch --strict: %d failed runs, %d total products — exit 3\n",
			result.Counts.Failed, result.Counts.CJProducts)
		emitResult(result, outFile, summary)
		os.Exit(3)
	}
	emitResult(result, outFile, summary)
}

// processProducts is a per-batch scratch map keyed by note ID so the
// main loop can pull the parsed CJProduct slice without needing to
// hold it on the BatchRun struct (which we want to keep minimal for
// JSON output).
var processProducts = map[string][]CJProduct{}

// processOne runs the four-step chain for one URL. Returns a BatchRun
// with Error set on any failure; the caller decides whether to count
// it as a hard fail (--strict) or just include it in the manifest.
//
// softWait=true means a wait timeout doesn't abort — we proceed to
// clip whatever's on the page. Useful when CJ's anti-bot is throttling
// the session and you'd rather get a skeleton-state clip you can
// inspect than a clean failure.
//
// Retry policy: when `qai browser open --wait` times out (the CJ-
// throttle signature: stderr contains "timeout after … waiting for
// selector"), we sleep `backoff(attempt, retryBase)` and re-navigate.
// Up to maxRetries times. Non-throttle errors (security gate, bad URL,
// browser unreachable) fail immediately — backoff won't help those.
func processOne(entry batchEntry, notebook, tab, waitSel, timeout string,
	softWait bool, maxRetries int, retryBase time.Duration,
	jc *joplin.Client) BatchRun {
	run := BatchRun{URL: entry.URL, Label: entry.Label}

	// 1. Navigate + wait, with throttle-aware retry.
	openArgs := []string{"open", entry.URL, "--wait", waitSel,
		"--timeout", timeout, "--tab", tab}
	var lastErr error
	var lastStderr string
	for attempt := 0; attempt <= maxRetries; attempt++ {
		stderr, err := runQaiBrowserCaptureStderr(openArgs...)
		if err == nil {
			lastErr = nil
			lastStderr = ""
			break
		}
		lastErr = err
		lastStderr = stderr
		if !looksLikeThrottle(stderr) || attempt == maxRetries {
			break
		}
		delay := backoff(attempt, retryBase)
		fmt.Fprintf(os.Stderr,
			"    throttle on attempt %d/%d — backing off %s before retry\n",
			attempt+1, maxRetries+1, delay)
		time.Sleep(delay)
	}
	if lastErr != nil {
		// Surface the navigate stderr to the user — they don't see it
		// otherwise since we captured it. Cuts trailing newlines so the
		// batch log stays clean.
		stderrTrim := strings.TrimRight(lastStderr, "\n ")
		if stderrTrim != "" {
			fmt.Fprintln(os.Stderr, "    "+stderrTrim)
		}
		if !softWait {
			run.Error = "navigate: " + lastErr.Error()
			return run
		}
		// Soft-wait: record the timeout but keep going. The downstream
		// counts (0 products) will already flag that the page wasn't
		// ready; the human can inspect the resulting note manually.
		fmt.Fprintf(os.Stderr, "    soft-wait: clipping skeleton state anyway\n")
	}

	// 2. Clip. Stdout is the note ID alone.
	title := entry.Label
	if title == "" {
		title = deriveTitle(entry.URL)
	}
	noteID, err := runQaiBrowserCapture("clip", notebook, title, "--tab", tab)
	if err != nil {
		run.Error = "clip: " + err.Error()
		return run
	}
	run.NoteID = noteID

	// 3. Fetch + parse.
	note, err := jc.GetNote(noteID, "id", "title", "body")
	if err != nil || note == nil {
		run.Error = "fetch note: " + safeErr(err)
		return run
	}
	parsed := Parse(note.Body)
	run.Counts = parsed.Counts
	processProducts[noteID] = parsed.CJProducts
	return run
}

func deriveTitle(url string) string {
	// "Stress Ball search page 2 - 2026-06-02" style, derived from
	// URL path so each clip is human-distinguishable in Joplin.
	short := strings.TrimPrefix(url, "https://")
	short = strings.TrimPrefix(short, "http://")
	if len(short) > 80 {
		short = short[:80]
	}
	return fmt.Sprintf("cj-clip %s — %s",
		time.Now().UTC().Format("2006-01-02 15:04"),
		short)
}

func safeErr(err error) string {
	if err == nil {
		return "(nil)"
	}
	return err.Error()
}

// runQaiBrowserCaptureStderr runs `qai browser <subArgs>` and returns
// the captured stderr so the navigate retry loop can scan it for the
// throttle signature without leaking partial timeout messages to the
// user when we're about to retry anyway.
func runQaiBrowserCaptureStderr(subArgs ...string) (string, error) {
	args := append([]string{"browser"}, subArgs...)
	cmd := exec.Command(qaiBinary(), args...)
	var sb strings.Builder
	cmd.Stderr = &sb
	err := cmd.Run()
	return sb.String(), err
}

// looksLikeThrottle returns true when a `qai browser open --wait`
// stderr matches the CJ-throttle signature: a clean wait-selector
// timeout with no other error. This is the signal we should back off
// and retry; security-gate denials, network errors, etc. produce
// different stderr and aren't retried (backoff won't help).
func looksLikeThrottle(stderr string) bool {
	return strings.Contains(stderr, "timeout after") &&
		strings.Contains(stderr, "waiting for selector")
}

// backoff returns the delay before retry attempt N (0-indexed).
// Exponential 2^attempt × base, plus uniform jitter in
// [-base/4, +base/4) to desynchronise concurrent batches, capped at
// 5 minutes so a long tail of retries doesn't stall the whole batch.
//
// The jitter offset is centered on zero so the average delay matches
// the pure-exponential schedule; the cap stops it growing forever.
func backoff(attempt int, base time.Duration) time.Duration {
	if base <= 0 {
		base = 30 * time.Second
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 10 {
		attempt = 10 // 2^10 × 30s ≈ 8.5 hours — far past the cap anyway
	}
	delay := base * (1 << attempt)
	jitter := time.Duration(rand.Int63n(int64(base)/2)) - base/4
	delay += jitter
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	if delay < base/2 {
		delay = base / 2
	}
	return delay
}

func runQaiBrowserCapture(subArgs ...string) (string, error) {
	args := append([]string{"browser"}, subArgs...)
	cmd := exec.Command(qaiBinary(), args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// qaiBinary resolves the qai executable to call subprocesses with.
// In normal install both ~/.local/bin/qai and ~/go/bin/qai exist;
// PATH resolves the right one. exec.LookPath would be more correct
// here, but $PATH-relative "qai" works on every shell qai runs in.
func qaiBinary() string {
	if v := os.Getenv("QAI_BINARY"); v != "" {
		return v
	}
	return "qai"
}

func discoverFirstPageTab() string {
	cmd := exec.Command(qaiBinary(), "browser", "list")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// `qai browser list` format: "<ID> <Title>... <URL>"
		fields := strings.Fields(line)
		if len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

func newJoplinClient() (*joplin.Client, error) {
	token, err := joplin.LoadDefaultToken()
	if err != nil {
		return nil, err
	}
	base := os.Getenv("JOPLIN_URL")
	if base == "" {
		base = "http://127.0.0.1:41184"
	}
	return joplin.New(joplin.Config{BaseURL: base, Token: token}), nil
}

// readBatchCSV reads URLs from CSV. First column = URL, optional
// second column = label. Header rows (where first col is "url" or
// similar) are auto-skipped. Lines whose first column doesn't look
// like an http(s) URL are silently dropped — defensive against
// stray empties / comment rows.
func readBatchCSV(path string) ([]batchEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	rdr := csv.NewReader(f)
	rdr.FieldsPerRecord = -1
	rdr.TrimLeadingSpace = true
	rows, err := rdr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse CSV: %w", err)
	}
	out := make([]batchEntry, 0, len(rows))
	for i, row := range rows {
		if len(row) == 0 {
			continue
		}
		url := strings.TrimSpace(row[0])
		if url == "" {
			continue
		}
		if i == 0 && isHeaderRow(url) {
			continue
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			continue
		}
		var label string
		if len(row) > 1 {
			label = strings.TrimSpace(row[1])
		}
		out = append(out, batchEntry{URL: url, Label: label})
	}
	return out, nil
}

func isHeaderRow(first string) bool {
	switch strings.ToLower(first) {
	case "url", "link", "address", "website", "href":
		return true
	}
	return false
}

func nextArg(args []string, i int, name string) string {
	if i+1 >= len(args) {
		fmt.Fprintf(os.Stderr, "qai cj batch: missing value for %s\n", name)
		os.Exit(1)
	}
	return args[i+1]
}

// emitResult writes the JSON to outFile (or stdout if "-" or empty
// without --summary). When --summary is also set, the human view is
// printed to stdout regardless.
func emitResult(r *BatchResult, outFile string, summary bool) {
	var w io.Writer = os.Stdout
	closeAfter := false
	if outFile != "" && outFile != "-" {
		f, err := os.Create(outFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai cj batch: write %s: %v\n", outFile, err)
			os.Exit(1)
		}
		w = f
		closeAfter = true
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		fmt.Fprintf(os.Stderr, "qai cj batch: encode: %v\n", err)
		os.Exit(1)
	}
	if closeAfter {
		_ = w.(*os.File).Close()
		fmt.Fprintf(os.Stderr, "\ndone — %d/%d succeeded, %d products → %s\n",
			r.Counts.Succeeded, r.Counts.URLs, r.Counts.CJProducts, outFile)
	}
	if summary {
		printBatchSummary(r)
	}
}

func printBatchSummary(r *BatchResult) {
	fmt.Printf("\nBatch summary — %s\n", r.SourceCSV)
	fmt.Printf("  notebook:  %s\n", r.Notebook)
	fmt.Printf("  urls:      %d  (ok: %d, fail: %d)\n",
		r.Counts.URLs, r.Counts.Succeeded, r.Counts.Failed)
	fmt.Printf("  products:  %d  (dedup'd across all URLs by PID)\n",
		r.Counts.CJProducts)
	fmt.Println()
	for _, run := range r.Runs {
		status := "OK"
		if run.Error != "" {
			status = "FAIL"
		}
		fmt.Printf("  %-4s %s\n", status, run.URL)
		if run.Error != "" {
			fmt.Printf("       %s\n", run.Error)
		} else {
			fmt.Printf("       %d products → note %s\n",
				run.Counts.CJProducts, run.NoteID[:8])
		}
	}
}

const helpBatch = `qai cj batch — run navigate→clip→extract for many URLs at once

USAGE
  qai cj batch <urls.csv> [flags]

CSV FORMAT
  col1 = URL  (http or https)
  col2 = optional label (becomes the Joplin note title)
  Header row 'url' / 'link' / etc. is auto-skipped.

FLAGS
  --notebook <path>   Joplin notebook for clips (default: dropship-accelerator/clips)
  --wait <selector>   Selector to wait for after navigate
                      (default: a[href*="/product/"] — only fires once
                      the React product grid has hydrated; a bare 'img'
                      matches the page-header logo before products load)
  --timeout <dur>     Wait timeout per page (default: 30s)
  --delay <ms>        Pause between pages (default: 0)
  --tab <id>          Pin to one tab ID (default: first page tab; required if you have several)
  -o <file>           Write JSON to file (default: stdout). Use "-" for explicit stdout.
  --summary           Also print a human summary after writing JSON
  --strict            Exit 3 if any URL failed OR product total is zero
  --soft-wait         A wait timeout doesn't abort; clip whatever's on the page.
                      Use when CJ's anti-bot throttling leaves the page stuck
                      in skeleton state — you get a clip you can inspect.
  --max-retries N     Retry navigate on a CJ-throttle timeout up to N times.
                      Default: 3. Set 0 to disable retries entirely.
  --retry-base <dur>  Base backoff for the first retry. Default: 30s.
                      Schedule is exponential: base, 2×base, 4×base, … with
                      ±25% jitter, capped at 5 minutes per retry. Only the
                      throttle signature ("timeout after … waiting for
                      selector") triggers a retry; real errors (security
                      gate, network, bad URL) fail immediately.

ANTI-BOT NOTES
  CJ rate-limits programmatic navigation. After 5-10 fast 'qai browser open'
  calls in a row to /search or /intelligence pages, the React grid stays in
  skeleton state indefinitely for the session — the page loads, the imgs
  download, but the product anchors never materialize. Symptoms:
    - .skeleton-card-wrap count stays at 60
    - 0 elements match a[href*=/product/]
    - --wait times out cleanly

  Workarounds (in increasing intervention):
    1. Pace yourself — pass --delay 5000 or higher.
    2. Run small batches (5-10 URLs), wait minutes between.
    3. Manually navigate one CJ page in your browser to "warm" the session
       before running a programmatic batch.
    4. For high-volume discovery, use CJ's Open Platform API directly.

OUTPUT (JSON)
  {
    "source_csv": "...",
    "notebook":   "...",
    "started_at": "ISO8601",
    "finished_at":"ISO8601",
    "runs":       [{url, label, note_id, counts, error}],
    "cj_products":[{pid, url, title, listed_count, price_min, price_max, price_raw}],
                  // deduplicated across all URLs by PID
    "counts":     {urls, succeeded, failed, cj_products_total}
  }

CHAIN PER URL
  1. qai browser open <url> --wait <sel> --timeout <dur> --tab <id>
  2. qai browser clip <notebook> "<derived-title>" --tab <id>
  3. Joplin GET /notes/<id>?fields=body
  4. cj.Parse(body) → CJProduct[] + Counts

The chain is the same as the single-URL flow, just driven from a CSV.
The browser tab is reused for every page, so cookies/auth are
preserved end-to-end — log into CJ once, batch as many URLs as needed.

EXAMPLES
  qai cj batch urls.csv
  qai cj batch urls.csv --strict -o results.json
  qai cj batch urls.csv --notebook "cj/research" --delay 2000
  qai cj batch urls.csv --summary | jq '.cj_products | sort_by(.listed_count) | reverse | .[0:10]'`
