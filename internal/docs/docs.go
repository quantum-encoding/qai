// Package docs implements `qai docs` — a CSV-driven wrapper around the
// scrape-docs-site.mjs scraper. Two modes:
//
//	qai docs flat <csv> --notebook <name>
//	  Each CSV row is an INDIVIDUAL page. All rows land flat in ONE
//	  notebook. No link discovery, no BFS crawl. Use case: "I have a
//	  curated list of pages I want clipped to a research folder."
//
//	qai docs deep <csv>
//	  Each CSV row is a SEED for a separate full BFS crawl. Each row
//	  gets its own notebook (named in the CSV). Use case: "I want to
//	  scrape 10 different docs sites and have each in its own folder
//	  for browsing later."
//
// Why a wrapper rather than re-implementing the scraper in Go: the
// existing scrape-docs-site.mjs uses Playwright for full JS rendering,
// which docs sites (Mintlify, Docusaurus, Nextra) need. Re-implementing
// that in Go would mean either embedding a browser (chromedp) or
// settling for non-JS-rendered HTML (would break most docs sites).
// Same shellout pattern as `qai clip` → clip-to-joplin.mjs.
package docs

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// CmdDocs is the `qai docs` subcommand entry point. Routes to the
// sub-mode (flat / deep) and validates inputs before shelling out.
func CmdDocs(args []string) {
	if len(args) == 0 || hasHelp(args) {
		fmt.Print(helpDocs())
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}

	switch args[0] {
	case "flat":
		runFlat(args[1:])
	case "deep":
		runDeep(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "qai docs: unknown mode %q (want `flat` or `deep`)\n", args[0])
		fmt.Fprintln(os.Stderr, "  → fix: run `qai docs --help` for the full usage")
		os.Exit(1)
	}
}

func hasHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			return true
		}
	}
	return false
}

// scraperScript locates the Playwright-backed scraper. Same pattern as
// cmd/qai/main.go locates clipToJoplinJS — both shellouts assume the
// global ~/.claude/scripts/ install.
func scraperScript() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "scripts", "scrape-docs-site.mjs")
}

// ─── flat mode ───────────────────────────────────────────────────────────

func runFlat(args []string) {
	var (
		csvPath  string
		notebook string
		delay    int
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--notebook":
			if i+1 < len(args) {
				notebook = args[i+1]
				i++
			}
		case "--delay":
			if i+1 < len(args) {
				delay, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--help", "-h", "help":
			fmt.Print(helpDocs())
			return
		default:
			if csvPath == "" && !strings.HasPrefix(args[i], "-") {
				csvPath = args[i]
			}
		}
	}

	if csvPath == "" {
		fmt.Fprintln(os.Stderr, "qai docs flat: missing <csv> argument")
		fmt.Fprintln(os.Stderr, "  → fix: `qai docs flat <csv> --notebook <name>`")
		os.Exit(1)
	}
	if notebook == "" {
		fmt.Fprintln(os.Stderr, "qai docs flat: --notebook is required (where do these pages go?)")
		fmt.Fprintln(os.Stderr, "  → fix: `qai docs flat <csv> --notebook MyResearch`")
		os.Exit(1)
	}

	rows, err := readURLList(csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai docs flat: cannot read %s: %v\n", csvPath, err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "qai docs flat: %s has no usable rows\n", csvPath)
		os.Exit(1)
	}

	urls := make([]string, 0, len(rows))
	for _, r := range rows {
		urls = append(urls, r.URL)
	}

	fmt.Printf("📚 flat: %d page(s) → %s\n", len(urls), notebook)

	// Single shellout: pass all URLs as seeds with --flat, which tells
	// the scraper to render just the seeds and not follow links.
	cmdArgs := []string{scraperScript(), notebook}
	cmdArgs = append(cmdArgs, urls...)
	cmdArgs = append(cmdArgs, "--flat", "--limit", strconv.Itoa(len(urls)+10))
	if delay > 0 {
		cmdArgs = append(cmdArgs, "--delay", strconv.Itoa(delay))
	}
	runScraper(cmdArgs)
}

// ─── deep mode ───────────────────────────────────────────────────────────

func runDeep(args []string) {
	var (
		csvPath string
		limit   int = 300
		delay   int = 750
		cdpPort int // 0 means headless Chromium (default)
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit":
			if i+1 < len(args) {
				limit, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--delay":
			if i+1 < len(args) {
				delay, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--cdp":
			// --cdp <port> tells the scraper to attach to an
			// already-running Brave/Chrome instead of launching
			// headless Chromium. Use when the target site
			// challenges headless requests. Bare --cdp (no
			// arg) defaults to 9222 to match qai browser launch.
			cdpPort = 9222
			if i+1 < len(args) {
				if v, err := strconv.Atoi(args[i+1]); err == nil {
					cdpPort = v
					i++
				}
			}
		case "--help", "-h", "help":
			fmt.Print(helpDocs())
			return
		default:
			if csvPath == "" && !strings.HasPrefix(args[i], "-") {
				csvPath = args[i]
			}
		}
	}

	if csvPath == "" {
		fmt.Fprintln(os.Stderr, "qai docs deep: missing <csv> argument")
		fmt.Fprintln(os.Stderr, "  → fix: `qai docs deep <csv>` (CSV must have url,notebook columns)")
		os.Exit(1)
	}

	rows, err := readSeedList(csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai docs deep: cannot read %s: %v\n", csvPath, err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "qai docs deep: %s has no usable rows\n", csvPath)
		os.Exit(1)
	}

	// Each row is its own job. We could group by notebook to batch
	// multi-seed crawls into one invocation — but keeping them
	// separate matches the user-facing model ("each line = one job")
	// and the scraper's resume logic handles intra-notebook dedup
	// cleanly across sequential runs.
	fmt.Printf("📚 deep: %d job(s) — each row is one BFS crawl into its own notebook\n\n", len(rows))

	var okCount, failCount int
	for i, row := range rows {
		fmt.Printf("━━━ [%d/%d] %s → %s ━━━\n", i+1, len(rows), row.URL, row.Notebook)

		rowLimit := limit
		if row.Limit > 0 {
			rowLimit = row.Limit
		}

		cmdArgs := []string{scraperScript(), row.Notebook, row.URL,
			"--limit", strconv.Itoa(rowLimit),
			"--delay", strconv.Itoa(delay),
		}
		if row.Domain != "" {
			cmdArgs = append(cmdArgs, "--domain", row.Domain)
		}
		if row.PathPrefix != "" {
			cmdArgs = append(cmdArgs, "--path-prefix", row.PathPrefix)
		}
		if cdpPort > 0 {
			cmdArgs = append(cmdArgs, "--cdp", strconv.Itoa(cdpPort))
		}

		if err := runScraper(cmdArgs); err != nil {
			failCount++
			fmt.Fprintf(os.Stderr, "  ✗ job failed: %v\n", err)
		} else {
			okCount++
		}
		fmt.Println()
	}

	fmt.Printf("🏁 deep: %d ok, %d failed (of %d total)\n", okCount, failCount, len(rows))
	if failCount > 0 {
		os.Exit(1)
	}
}

// ─── shellout helper ─────────────────────────────────────────────────────

func runScraper(args []string) error {
	cmd := exec.Command("node", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// ─── CSV readers ─────────────────────────────────────────────────────────

// flatRow is a single page for flat mode. Title is read from CSV if
// present but not currently passed to the scraper (the scraper derives
// the note title from the rendered page's <title>).
type flatRow struct {
	URL   string
	Title string
}

// seedRow is one BFS-crawl job for deep mode.
type seedRow struct {
	URL        string
	Notebook   string
	Domain     string // optional: defaults to URL host on the scraper side
	PathPrefix string // optional: defaults to "/" on the scraper side
	Limit      int    // optional: overrides the global --limit
}

// readURLList parses a CSV for flat mode.
// Accepts headerless (just URLs) or header `url[,title]`. Blank lines
// and lines starting with `#` are skipped.
func readURLList(path string) ([]flatRow, error) {
	rawRows, err := readCSVRaw(path)
	if err != nil {
		return nil, err
	}
	if len(rawRows) == 0 {
		return nil, nil
	}

	colURL, colTitle, start := detectHeader(rawRows[0], map[string]struct{}{
		"url":   {},
		"title": {},
	})
	colURLIdx := colURL["url"]
	colTitleIdx := colURL["title"]
	if colTitleIdx == 0 && start == 0 {
		colTitleIdx = -1
	}
	_ = colTitle

	out := make([]flatRow, 0, len(rawRows)-start)
	for _, cells := range rawRows[start:] {
		if len(cells) == 0 {
			continue
		}
		u := strings.TrimSpace(cells[colURLIdx])
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		row := flatRow{URL: u}
		if colTitleIdx >= 0 && colTitleIdx < len(cells) {
			row.Title = strings.TrimSpace(cells[colTitleIdx])
		}
		out = append(out, row)
	}
	return out, nil
}

// readSeedList parses a CSV for deep mode.
// Requires a header row with at minimum `url,notebook`. Optional
// columns: domain, path_prefix, limit.
func readSeedList(path string) ([]seedRow, error) {
	rawRows, err := readCSVRaw(path)
	if err != nil {
		return nil, err
	}
	if len(rawRows) < 2 {
		return nil, fmt.Errorf("deep mode requires a header row + at least one data row")
	}

	cols, _, start := detectHeader(rawRows[0], map[string]struct{}{
		"url":         {},
		"notebook":    {},
		"domain":      {},
		"path_prefix": {},
		"limit":       {},
	})
	if start == 0 {
		return nil, fmt.Errorf("deep mode requires a header row — first row must be `url,notebook[,...]`")
	}
	if _, ok := cols["url"]; !ok {
		return nil, fmt.Errorf("deep mode CSV header missing `url` column")
	}
	if _, ok := cols["notebook"]; !ok {
		return nil, fmt.Errorf("deep mode CSV header missing `notebook` column")
	}

	out := make([]seedRow, 0, len(rawRows)-1)
	for _, cells := range rawRows[start:] {
		if len(cells) == 0 {
			continue
		}
		urlIdx := cols["url"]
		nbIdx := cols["notebook"]
		if urlIdx >= len(cells) || nbIdx >= len(cells) {
			continue
		}
		u := strings.TrimSpace(cells[urlIdx])
		nb := strings.TrimSpace(cells[nbIdx])
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		if nb == "" {
			return nil, fmt.Errorf("row with URL %q has empty notebook column — every deep-mode row needs a notebook name", u)
		}
		row := seedRow{URL: u, Notebook: nb}
		if idx, ok := cols["domain"]; ok && idx < len(cells) {
			row.Domain = strings.TrimSpace(cells[idx])
		}
		if idx, ok := cols["path_prefix"]; ok && idx < len(cells) {
			row.PathPrefix = strings.TrimSpace(cells[idx])
		}
		if idx, ok := cols["limit"]; ok && idx < len(cells) {
			if n, err := strconv.Atoi(strings.TrimSpace(cells[idx])); err == nil {
				row.Limit = n
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// readCSVRaw loads a CSV with variable columns into a [][]string.
func readCSVRaw(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	rows, err := r.ReadAll()
	if err != nil && err != io.EOF {
		return nil, err
	}
	return rows, nil
}

// detectHeader looks at the first row and checks if it's a header by
// matching known column names. Returns (column-name → index map,
// title-column-shortcut, start-row).
//
// Heuristic: it's a header iff at least one cell is a known column
// name AND the first cell doesn't look like a URL (no dots / slashes).
// Avoids the "first row is data" footgun when the user writes a
// headerless CSV.
func detectHeader(firstRow []string, known map[string]struct{}) (map[string]int, map[string]int, int) {
	cols := map[string]int{}
	if len(firstRow) == 0 {
		return cols, nil, 0
	}
	first := strings.TrimSpace(firstRow[0])
	looksLikeURL := strings.ContainsAny(first, "/.")
	hasKnown := false
	for i, h := range firstRow {
		name := strings.ToLower(strings.TrimSpace(h))
		if _, ok := known[name]; ok {
			cols[name] = i
			hasKnown = true
		}
	}
	if hasKnown && !looksLikeURL {
		return cols, nil, 1
	}
	// Headerless — assume URL is column 0.
	return map[string]int{"url": 0}, nil, 0
}

// ─── help ────────────────────────────────────────────────────────────────

func helpDocs() string {
	return `qai docs — clip documentation sites to Joplin (CSV-driven)

Two modes:

  qai docs flat <csv> --notebook <name>
    Clip each URL in the CSV as a single page. All to one notebook.
    No link discovery, no BFS crawl. Use when you have a curated list
    of pages and want them in one research folder.

  qai docs deep <csv>
    For each CSV row, BFS-crawl the seed URL + follow same-domain links.
    Each row gets its own notebook. Use when you want to scrape multiple
    docs sites and have each in its own browsable folder.

Flat-mode CSV format (headerless OR with header):
  Headerless:
    https://docs.example.com/install
    https://docs.example.com/quickstart

  With header (only the url column is honoured today):
    url,title
    https://docs.example.com/install,Installation Guide

Deep-mode CSV format (header REQUIRED, url + notebook minimum):
    url,notebook[,domain[,path_prefix[,limit]]]
    https://docs.pimlico.io/references/platform,Pimlico_Docs,docs.pimlico.io,/,300
    https://docs.tauri.app/,Tauri_Docs
    https://svelte.dev/docs/svelte/overview,Svelte_Docs,svelte.dev,/docs/svelte/,200

  Columns:
    url          required — the seed for the BFS crawl
    notebook     required — Joplin notebook for this row's results
    domain       optional — defaults to the URL's host (restricts crawl scope)
    path_prefix  optional — defaults to "/" (only follow links under this path)
    limit        optional — defaults to --limit flag (default 300)

Flat flags:
  --notebook <name>    required — Joplin notebook (under qai/ root by default)
  --delay <ms>         per-page politeness delay (default 750)

Deep flags:
  --limit <n>          default --limit per row, overridden by row's own
                       limit column if present (default 300)
  --delay <ms>         per-page politeness delay (default 750)

Both modes shell out to ~/.claude/scripts/scrape-docs-site.mjs which uses
Playwright (full JS rendering — handles Mintlify, Docusaurus, Nextra etc.)
and posts to Joplin via the Web Clipper REST API. Requires JOPLIN_TOKEN
in the environment and Joplin Desktop running with Web Clipper enabled.

Examples:

  # Flat: 10 curated pages into one research folder
  qai docs flat ./research/pimlico-spend-cap.csv --notebook PimlicoSpendCap

  # Deep: 5 different docs sites, each in its own folder
  qai docs deep ./tools/scrape-jobs.csv

Notebooks are auto-prefixed with "qai/" unless you pass an already-rooted
path. To opt out of the qai/ root prefix, set JOPLIN_ROOT_NOTEBOOK="" in
the environment (matches clip-to-joplin.mjs's behaviour).
`
}
