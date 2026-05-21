package scrape

import (
	"encoding/csv"
	"fmt"
	"os"
)

// runScout fetches a search / listing page, extracts product URLs via
// the preset's Scout function, and emits a CSV that the caller can
// feed straight back in via `qai scrape --csv`.
//
// Scout uses a direct HTTP fetch, not the clip pipeline. Marketplace
// listings are server-rendered and don't need a real browser — that
// machinery is reserved for product detail pages where the clip
// pipeline's anti-bot bypass actually earns its keep. Scout pages
// also tend to kill Playwright's networkidle wait (continuous
// analytics pings), so the direct fetch is both faster and more
// reliable.
//
// Typical two-stage workflow:
//
//	qai scrape --scout "https://www.amazon.co.uk/s?k=threadripper" -o t.csv
//	qai scrape --csv t.csv -o briefs.jsonl --parallel 3
func runScout(searchURL string, preset *Preset, opts *flags) {
	if preset.Scout == nil {
		fmt.Fprintf(os.Stderr, "qai scrape --scout: preset %q has no Scout extractor\n", preset.Name)
		fmt.Fprintln(os.Stderr, "  → fix: use a preset whose site exposes listing pages (e.g. amazon), or scrape product URLs directly")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "▶ [%s] scouting %s\n", preset.Name, searchURL)
	body, err := fetchHTML(searchURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai scrape --scout: fetch %s failed: %v\n", searchURL, err)
		fmt.Fprintln(os.Stderr, "  → fix: check the URL opens in a browser; some sites geo-block or require a UA the scout fetcher can't fake")
		os.Exit(1)
	}

	urls := preset.Scout(body, searchURL)
	if opts.scoutMax > 0 && len(urls) > opts.scoutMax {
		urls = urls[:opts.scoutMax]
	}
	fmt.Fprintf(os.Stderr, "  ▶ found %d unique products\n", len(urls))

	// Emit CSV. Writes to --out if set, otherwise stdout.
	var w *csv.Writer
	if opts.outPath == "" || opts.outPath == "-" {
		w = csv.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(opts.outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai scrape --scout: cannot create output file %s: %v\n", opts.outPath, err)
			fmt.Fprintln(os.Stderr, "  → fix: pick a writable path; the parent directory must already exist")
			os.Exit(1)
		}
		defer f.Close()
		w = csv.NewWriter(f)
	}

	_ = w.Write([]string{"url", "preset"})
	for _, u := range urls {
		_ = w.Write([]string{u, preset.Name})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		fmt.Fprintf(os.Stderr, "qai scrape --scout: write CSV to %s: %v\n", firstNonEmpty(opts.outPath, "stdout"), err)
		fmt.Fprintln(os.Stderr, "  → fix: check disk space and that the output file is still writable")
		os.Exit(1)
	}

	if opts.outPath != "" && opts.outPath != "-" {
		fmt.Fprintf(os.Stderr, "  ▶ wrote %s — feed it back with: qai scrape --csv %s\n",
			opts.outPath, opts.outPath)
	}
}
