package scrape

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ─── CSV batch runner ────────────────────────────────────────────────────

// csvRow is a single scrape target. Columns are: url (required),
// preset (optional — auto-detected from URL host otherwise), notebook
// (optional — overrides preset default).
type csvRow struct {
	URL      string
	Preset   string
	Notebook string
}

// runBatch reads a CSV of targets and runs the scrape pipeline per row.
// Results are written to --out (JSONL) if set, otherwise streamed to
// stdout as pretty JSON separated by blank lines.
//
// --parallel N runs N workers. Each worker owns its own `qai clip`
// shellout; they share the Joplin Data API but HTTP calls are cheap.
// Default is 1 because Joplin's search index can get confused by
// rapid-fire writes.
//
// --resume skips URLs whose brief already exists in the --out file
// (by matching source_url). Useful when a long batch bails partway.
func runBatch(opts *flags) {
	rows, err := readCSV(opts.csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai scrape: read %s: %v\n", opts.csvPath, err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "qai scrape: %s has no rows\n", opts.csvPath)
		os.Exit(1)
	}

	skip := map[string]bool{}
	if opts.resume && opts.outPath != "" && opts.outPath != "-" {
		skip = previouslyDone(opts.outPath)
	}

	fmt.Fprintf(os.Stderr, "▶ batch: %d rows, parallel=%d, out=%s\n",
		len(rows), opts.parallel, firstNonEmpty(opts.outPath, "stdout"))

	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, opts.parallel)
		outMu    sync.Mutex
		okCount  int
		errCount int
	)

	for i, r := range rows {
		if skip[r.URL] {
			fmt.Fprintf(os.Stderr, "  [%d/%d] skip (already done): %s\n", i+1, len(rows), r.URL)
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int, row csvRow) {
			defer wg.Done()
			defer func() { <-sem }()

			subOpts := *opts // copy so per-row overrides don't leak
			subOpts.csvPath = ""
			subOpts.target = row.URL
			subOpts.idOnly = false
			if row.Preset != "" {
				subOpts.preset = row.Preset
			}
			if row.Notebook != "" {
				subOpts.notebook = row.Notebook
			}

			productURL, preset, err := resolveTarget(&subOpts)
			if err != nil {
				outMu.Lock()
				errCount++
				outMu.Unlock()
				fmt.Fprintf(os.Stderr, "  [%d/%d] ✘ %s: %v\n", idx+1, len(rows), row.URL, err)
				return
			}
			brief, err := runOne(productURL, preset, &subOpts)
			if err != nil {
				outMu.Lock()
				errCount++
				outMu.Unlock()
				fmt.Fprintf(os.Stderr, "  [%d/%d] ✘ %s: %v\n", idx+1, len(rows), row.URL, err)
				// Still emit a brief stub so --resume can skip on retry.
				stub := &Brief{URL: row.URL, Preset: preset.Name, Error: err.Error()}
				outMu.Lock()
				emit(stub, &subOpts)
				outMu.Unlock()
				return
			}
			outMu.Lock()
			okCount++
			emit(brief, &subOpts)
			outMu.Unlock()
			fmt.Fprintf(os.Stderr, "  [%d/%d] ✓ %s (%s)\n", idx+1, len(rows), brief.ID, brief.Preset)
		}(i, r)
	}
	wg.Wait()

	fmt.Fprintf(os.Stderr, "▶ batch done: %d ok, %d errors\n", okCount, errCount)
}

// readCSV parses a CSV file. Accepts either headerless (single column
// of URLs) or header-driven (`url[,preset[,notebook]]`).
func readCSV(path string) ([]csvRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // allow variable columns
	r.TrimLeadingSpace = true

	rawRows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rawRows) == 0 {
		return nil, nil
	}

	// Detect header: first row contains "url" (case-insensitive) and
	// no dots/slashes (so it's not itself a URL).
	header := rawRows[0]
	hasHeader := false
	colURL, colPreset, colNotebook := 0, -1, -1
	if len(header) > 0 && strings.EqualFold(strings.TrimSpace(header[0]), "url") &&
		!strings.ContainsAny(header[0], "/.") {
		hasHeader = true
		for i, h := range header {
			switch strings.ToLower(strings.TrimSpace(h)) {
			case "url":
				colURL = i
			case "preset":
				colPreset = i
			case "notebook":
				colNotebook = i
			}
		}
	}

	start := 0
	if hasHeader {
		start = 1
	}

	rows := make([]csvRow, 0, len(rawRows)-start)
	for _, cells := range rawRows[start:] {
		if len(cells) == 0 {
			continue
		}
		u := strings.TrimSpace(cells[colURL])
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		row := csvRow{URL: u}
		if colPreset >= 0 && colPreset < len(cells) {
			row.Preset = strings.TrimSpace(cells[colPreset])
		}
		if colNotebook >= 0 && colNotebook < len(cells) {
			row.Notebook = strings.TrimSpace(cells[colNotebook])
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// previouslyDone scans an existing JSONL output file and returns the
// set of URLs that have already been processed successfully.
func previouslyDone(path string) map[string]bool {
	done := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return done
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for {
		var b Brief
		if err := dec.Decode(&b); err == io.EOF {
			break
		} else if err != nil {
			break
		}
		if b.URL != "" && b.Error == "" {
			done[b.URL] = true
		}
	}
	return done
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
