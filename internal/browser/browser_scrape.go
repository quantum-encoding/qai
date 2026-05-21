// browser_scrape.go — Batch scrape URLs from a CSV file.
//
// Reads a CSV with URLs (and optional labels), navigates to each,
// and extracts text, HTML, or screenshots depending on mode.
//
// CSV format: first column is the URL. Optional second column is a label/title.
// Header row is auto-detected (skipped if first column looks like a URL).
//
// Usage:
//
//	qai browser scrape urls.csv                 # extract text from each URL
//	qai browser scrape urls.csv --screenshot    # screenshot each URL
//	qai browser scrape urls.csv --html          # extract HTML from each URL
//	qai browser scrape urls.csv --delay 2000    # wait 2s between pages
//	qai browser scrape urls.csv -o results/     # output directory

package browser

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type scrapeMode int

const (
	scrapeModeText scrapeMode = iota
	scrapeModeHTML
	scrapeModeScreenshot
)

type scrapeEntry struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

type scrapeResult struct {
	URL     string `json:"url"`
	Label   string `json:"label,omitempty"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	File    string `json:"file,omitempty"`
	Error   string `json:"error,omitempty"`
}

func browserScrape(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `usage: qai browser scrape <urls.csv> [options]

Batch process URLs from a CSV file. First column = URL, optional second column = label.

Modes:
  (default)       Extract page text
  --screenshot    Capture screenshot of each page
  --html          Extract full HTML

Options:
  --delay <ms>    Delay between pages (default: 1000)
  -o <dir>        Output directory (default: scrape-{timestamp})
  --json          Write results manifest as JSON`)
		os.Exit(1)
	}

	// Parse args
	csvPath := ""
	mode := scrapeModeText
	delay := 1000
	outputDir := ""
	jsonManifest := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--screenshot", "--ss":
			mode = scrapeModeScreenshot
		case "--html":
			mode = scrapeModeHTML
		case "--delay":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &delay)
				i++
			}
		case "-o", "--output":
			if i+1 < len(args) {
				outputDir = args[i+1]
				i++
			}
		case "--json":
			jsonManifest = true
		case "--port", "--tab":
			i++ // skip value (handled by connectToTab)
		default:
			if csvPath == "" {
				csvPath = args[i]
			}
		}
	}

	if csvPath == "" {
		fmt.Fprintln(os.Stderr, "qai browser scrape: CSV file path required")
		fmt.Fprintln(os.Stderr, "  → fix: qai browser scrape <urls.csv>   (first column = URL, optional second column = label)")
		os.Exit(1)
	}

	// Read CSV
	entries, err := readScrapeCSV(csvPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser scrape: read %s: %v\n", csvPath, err)
		fmt.Fprintln(os.Stderr, "  → fix: ensure the file exists and is a valid CSV (first column = URL)")
		os.Exit(1)
	}
	if len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "qai browser scrape: no URLs found in %s\n", csvPath)
		fmt.Fprintln(os.Stderr, "  → fix: first column must contain http:// or https:// URLs (header row 'url' is auto-skipped)")
		os.Exit(1)
	}

	// Pre-flight security check — runs BEFORE connecting to the browser
	// so a poisoned CSV (denied domain at row N) refuses the whole batch
	// rather than discovering it mid-run after dozens of navigations.
	if err := preflightScrape(entries); err != nil {
		fmt.Fprintf(os.Stderr, "qai browser scrape: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: edit ~/.qai/browser-policy.yaml to adjust denied_domains, or remove offending rows from the CSV")
		os.Exit(1)
	}

	// Output directory
	if outputDir == "" {
		modeStr := "text"
		if mode == scrapeModeScreenshot {
			modeStr = "screenshots"
		} else if mode == scrapeModeHTML {
			modeStr = "html"
		}
		outputDir = fmt.Sprintf("scrape-%s-%s", modeStr, time.Now().Format("20060102-150405"))
	}
	os.MkdirAll(outputDir, 0755)

	// Connect to browser
	client, tab, err := connectToTab(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai browser scrape: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	modeLabel := "text"
	if mode == scrapeModeScreenshot {
		modeLabel = "screenshot"
	} else if mode == scrapeModeHTML {
		modeLabel = "html"
	}

	fmt.Fprintf(os.Stderr, "scraping %d URLs (mode: %s, delay: %dms)\n", len(entries), modeLabel, delay)
	fmt.Fprintf(os.Stderr, "output: %s/\n\n", outputDir)

	start := time.Now()
	results := make([]scrapeResult, 0, len(entries))
	var succeeded, failed int

	for i, entry := range entries {
		result := scrapeResult{URL: entry.URL, Label: entry.Label}

		// Security gate on target URL
		targetTab := &cdpTab{URL: entry.URL, ID: tab.ID}
		if err := securityGate("scrape", targetTab, entry.URL); err != nil {
			result.Error = err.Error()
			results = append(results, result)
			failed++
			fmt.Fprintf(os.Stderr, "  [%d/%d] BLOCKED %s\n", i+1, len(entries), entry.URL)
			continue
		}

		// Navigate
		_, err := client.Call("Page.navigate", map[string]any{"url": entry.URL}, 10*time.Second)
		if err != nil {
			result.Error = fmt.Sprintf("navigate: %v", err)
			results = append(results, result)
			failed++
			fmt.Fprintf(os.Stderr, "  [%d/%d] FAIL %s — %v\n", i+1, len(entries), entry.URL, err)
			continue
		}

		// Wait for load
		client.WaitEvent("Page.loadEventFired", 15*time.Second)

		// Extra delay for JS rendering
		if delay > 0 {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}

		// Get page title
		if titleResult, err := client.Call("Runtime.evaluate", map[string]any{
			"expression": "document.title", "returnByValue": true,
		}, 5*time.Second); err == nil {
			var tv struct {
				Result struct{ Value string `json:"value"` } `json:"result"`
			}
			json.Unmarshal(titleResult, &tv)
			result.Title = tv.Result.Value
		}

		// Process based on mode
		switch mode {
		case scrapeModeText, scrapeModeHTML:
			expr := "document.body.innerText"
			ext := ".txt"
			if mode == scrapeModeHTML {
				expr = "document.body.innerHTML"
				ext = ".html"
			}

			evalResult, err := client.Call("Runtime.evaluate", map[string]any{
				"expression": expr, "returnByValue": true,
			}, 10*time.Second)
			if err != nil {
				result.Error = fmt.Sprintf("extract: %v", err)
				failed++
			} else {
				var ev struct {
					Result struct{ Value string `json:"value"` } `json:"result"`
				}
				json.Unmarshal(evalResult, &ev)
				result.Content = ev.Result.Value

				// Write to file
				filename := sanitizeFilename(entry.Label, entry.URL, i) + ext
				outPath := filepath.Join(outputDir, filename)
				if wErr := os.WriteFile(outPath, []byte(ev.Result.Value), 0644); wErr != nil {
					result.Error = fmt.Sprintf("write: %v", wErr)
					failed++
					continue
				}
				result.File = filename
				succeeded++
			}

		case scrapeModeScreenshot:
			ssResult, err := client.Call("Page.captureScreenshot", map[string]any{
				"format": "png", "quality": 80,
			}, 10*time.Second)
			if err != nil {
				result.Error = fmt.Sprintf("screenshot: %v", err)
				failed++
			} else {
				var ss struct{ Data string `json:"data"` }
				json.Unmarshal(ssResult, &ss)
				if ss.Data != "" {
					pngData, _ := base64.StdEncoding.DecodeString(ss.Data)
					filename := sanitizeFilename(entry.Label, entry.URL, i) + ".png"
					outPath := filepath.Join(outputDir, filename)
					if wErr := os.WriteFile(outPath, pngData, 0644); wErr != nil {
						result.Error = fmt.Sprintf("write: %v", wErr)
						failed++
					} else {
						result.File = filename
						succeeded++
					}
				} else {
					result.Error = "empty screenshot"
					failed++
				}
			}
		}

		if result.Error != "" {
			fmt.Fprintf(os.Stderr, "  [%d/%d] FAIL %s — %s\n", i+1, len(entries), entry.URL, result.Error)
		} else {
			fmt.Fprintf(os.Stderr, "  [%d/%d] OK   %s → %s\n", i+1, len(entries), entry.URL, result.File)
		}

		results = append(results, result)
	}

	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "\ndone in %.1fs — %d succeeded, %d failed\n", elapsed.Seconds(), succeeded, failed)
	fmt.Fprintf(os.Stderr, "output: %s/\n", outputDir)

	// Write manifest
	if jsonManifest {
		manifest := map[string]any{
			"mode":      modeLabel,
			"urls":      len(entries),
			"succeeded": succeeded,
			"failed":    failed,
			"duration":  elapsed.Seconds(),
			"results":   results,
		}
		manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
		manifestPath := filepath.Join(outputDir, "manifest.json")
		if err := os.WriteFile(manifestPath, manifestJSON, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: write manifest: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "manifest: %s\n", manifestPath)
	}
}

// readScrapeCSV reads URLs from a CSV file. First column = URL, second = label.
// Auto-detects and skips header rows.
func readScrapeCSV(path string) ([]scrapeEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1 // variable columns
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse CSV: %w", err)
	}

	var entries []scrapeEntry
	for i, row := range records {
		if len(row) == 0 {
			continue
		}
		url := strings.TrimSpace(row[0])
		if url == "" {
			continue
		}
		// Skip header row
		if i == 0 && isHeaderRow(url) {
			continue
		}
		// Must look like a URL
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			continue
		}
		label := ""
		if len(row) > 1 {
			label = strings.TrimSpace(row[1])
		}
		entries = append(entries, scrapeEntry{URL: url, Label: label})
	}
	return entries, nil
}

func isHeaderRow(first string) bool {
	lower := strings.ToLower(first)
	return lower == "url" || lower == "link" || lower == "address" || lower == "website" || lower == "href"
}

// sanitizeFilename creates a safe filename from a label or URL.
func sanitizeFilename(label, rawURL string, index int) string {
	name := label
	if name == "" {
		// Extract from URL: strip scheme, replace special chars
		name = rawURL
		name = strings.TrimPrefix(name, "https://")
		name = strings.TrimPrefix(name, "http://")
		if len(name) > 80 {
			name = name[:80]
		}
	}
	// Replace unsafe chars
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "?", "_", "&", "_",
		"=", "_", "#", "_", " ", "_", ".", "_",
	)
	name = replacer.Replace(name)
	// Prefix with index for ordering
	return fmt.Sprintf("%03d_%s", index+1, name)
}

