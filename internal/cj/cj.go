// cj.go — `qai cj` command surface. Today: `qai cj extract <md>`.
// Future room (kept in mind for the dispatcher shape): `qai cj enrich
// <pid>` would call the CJ Open Platform API to fetch full product
// details, and `qai cj stage <md> --surreal` could push extract
// results to a SurrealDB research_products table. Both downstream of
// the parser this commit ships.

package cj

import (
	"encoding/json"
	"fmt"
	"os"
)

// CmdCJ dispatches `qai cj <sub>`. Wired from cmd/qai/main.go.
func CmdCJ(args []string) {
	if len(args) == 0 || isHelp(args[0]) {
		fmt.Println(helpCJ)
		return
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "extract", "parse":
		cmdExtract(rest)
	default:
		fmt.Fprintf(os.Stderr, "qai cj: unknown subcommand %q\n", sub)
		fmt.Fprintln(os.Stderr, "Run `qai cj --help` for the list.")
		os.Exit(1)
	}
}

func isHelp(s string) bool { return s == "--help" || s == "-h" || s == "help" }

func cmdExtract(args []string) {
	if len(args) == 0 || isHelp(args[0]) {
		fmt.Println(helpExtract)
		return
	}
	summary := false
	var file string
	for _, a := range args {
		switch a {
		case "--summary":
			summary = true
		case "--json":
			// implied; accept for symmetry
		default:
			if file == "" {
				file = a
			}
		}
	}
	if file == "" {
		fmt.Fprintln(os.Stderr, "qai cj extract: missing markdown file path")
		fmt.Fprintln(os.Stderr, "  → fix: qai cj extract <path/to/clip.md>")
		os.Exit(1)
	}
	r, err := ParseFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai cj extract: %v\n", err)
		os.Exit(1)
	}
	if summary {
		printSummary(r)
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

// printSummary is the human-skim view for sanity-checking a clip
// before piping the JSON downstream. Two short lists; the high-signal
// fields per row, no truncation that would hide useful demand info.
func printSummary(r *Result) {
	fmt.Printf("source:   %s\n", r.SourceFile)
	if r.Category != "" {
		fmt.Printf("category: %s (id=%s)\n", r.Category, r.CategoryID)
	}
	fmt.Println()
	fmt.Printf("CJ Recommended (%d):\n", r.Counts.CJProducts)
	for _, p := range r.CJProducts {
		fmt.Printf("  %s  %s  %s\n", p.PriceRaw, p.PID, truncate(p.Title, 80))
	}
	fmt.Println()
	fmt.Printf("Competitors (%d):\n", r.Counts.Competitors)
	for _, c := range r.Competitors {
		fmt.Printf("  %-9s %4d reviews %5d days  %s — %s\n",
			c.SalesUSD, c.ReviewsNum, c.ListingDays, c.Brand, truncate(c.Title, 70))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

const helpCJ = `qai cj — parse CJ Dropshipping intelligence-dashboard clips

The CJ intelligence dashboard is a React SPA behind a bot wall, so
discovery happens via the human web-clipper rather than scraping. This
verb reads the resulting markdown and emits structured product +
competitor-demand JSON so a downstream consumer (Surreal, sqlite,
spreadsheet, prompt) can avoid re-deriving the regex per session.

USAGE
  qai cj extract <clip.md>            Emit JSON of products + competitors
  qai cj extract <clip.md> --summary  Human-skim view

WORKFLOW
  1. Clip a category page from cjdropshipping.com/intelligence/sales-trends
     using the Joplin Web Clipper (renders the React page like a human,
     bypasses the bot wall, preserves all the data).
  2. Run 'qai cj extract <clip.md>' to get JSON.
  3. Pipeline JSON into your boarding script (project-specific SQL,
     SurrealDB insert, whatever).

OUTPUT (schema)
  {
    "source_file":  "...",
    "category":     "Pet Supplies",
    "category_id":  "2619533011",
    "cj_products":  [{pid, url, title, listed_count, price_min, price_max, price_raw}],
    "competitors":  [{asin, url, title, brand, price, lqs, sales_volume,
                      sales_volume_num, sales_usd, sales_usd_num, rating,
                      reviews, reviews_num, listing_days}],
    "counts":       {cj_products, competitors}
  }

  Numeric *_num fields parse "$137.6K" / "4.4K" / "1.4K" so downstream
  consumers can sort/filter without re-parsing the raw strings.

EXAMPLES
  qai cj extract ~/Documents/dropship/clip.md
  qai cj extract clip.md | jq '.cj_products[] | {pid, title, price_min}'
  qai cj extract clip.md | jq '.competitors | sort_by(.sales_usd_num) | reverse | .[0:5]'
  qai cj extract clip.md --summary`

const helpExtract = helpCJ
