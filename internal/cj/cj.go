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
	"io"
	"os"

	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// CmdCJ dispatches `qai cj <sub>`. Wired from cmd/qai/main.go.
func CmdCJ(args []string) {
	if len(args) == 0 || isHelp(args[0]) {
		os.Stdout.WriteString(helpCJ + "\n")
		return
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "extract", "parse":
		cmdExtract(rest)
	case "batch":
		cmdBatch(rest)
	default:
		fmt.Fprintf(os.Stderr, "qai cj: unknown subcommand %q\n", sub)
		fmt.Fprintln(os.Stderr, "Run `qai cj --help` for the list.")
		os.Exit(1)
	}
}

func isHelp(s string) bool { return s == "--help" || s == "-h" || s == "help" }

func cmdExtract(args []string) {
	if len(args) == 0 || isHelp(args[0]) {
		os.Stdout.WriteString(helpExtract + "\n")
		return
	}
	var (
		summary    bool
		strict     bool
		joplinRef  string // ID or title pattern
		file       string
		readStdin  bool
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--summary":
			summary = true
		case "--strict":
			strict = true
		case "--json":
			// implied; accept for symmetry
		case "--joplin":
			if i+1 >= len(args) {
				dieMissing("--joplin <note-id | title-pattern>")
			}
			joplinRef = args[i+1]
			i++
		case "-":
			readStdin = true
		default:
			if file == "" && !looksLikeFlag(a) {
				file = a
			}
		}
	}

	// Source resolution — exactly one of {file, stdin, joplin} must
	// produce the markdown body. Order matters: explicit --joplin
	// wins over file/stdin if both are provided, since it's the more
	// specific intent.
	source, body, err := loadSource(joplinRef, file, readStdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai cj extract: %v\n", err)
		os.Exit(1)
	}

	r := Parse(body)
	r.SourceFile = source

	if strict && r.Counts.CJProducts == 0 && r.Counts.Competitors == 0 {
		// Catch the "clip didn't land on a CJ page" failure mode.
		// In a pipeline this is the difference between "extracted
		// nothing useful" and "the clip silently failed". Exit 3
		// matches the other invocation-error exits across qai.
		fmt.Fprintln(os.Stderr,
			"qai cj extract --strict: 0 CJ products and 0 competitors — likely not a CJ intelligence clip")
		fmt.Fprintln(os.Stderr,
			"  → fix: verify the source page, or omit --strict to accept empty results")
		os.Exit(3)
	}

	if summary {
		printSummary(r)
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
}

// loadSource resolves the markdown body from one of three inputs:
//   - --joplin <ref> → fetch via Joplin REST API (ref is ID or title)
//   - "-"            → read stdin
//   - file path      → read file
//
// Returns (source-label, body, err). The source-label is used as the
// payload's source_file field for traceability; for stdin it's "-",
// for Joplin it's the note ID.
func loadSource(joplinRef, file string, readStdin bool) (string, string, error) {
	switch {
	case joplinRef != "":
		body, id, err := fetchFromJoplin(joplinRef)
		if err != nil {
			return "", "", err
		}
		return "joplin:" + id, body, nil
	case readStdin:
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "", fmt.Errorf("read stdin: %w", err)
		}
		return "-", string(data), nil
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", file, err)
		}
		return file, string(data), nil
	default:
		return "", "", fmt.Errorf("missing markdown source — pass a file path, '-' for stdin, or --joplin <id-or-title>")
	}
}

// fetchFromJoplin loads a note's body via the Joplin REST API. The
// ref disambiguates by shape:
//
//   - 32-char hex → treat as note ID, GET /notes/<id>?fields=body
//   - anything else → treat as title pattern, /search?type=note&query=,
//     take the first result (Joplin search orders by updated_at DESC,
//     so this is the most-recently-modified matching note).
//
// Returns (body, note-id, err) so the source label carries the
// resolved ID even when the caller passed a title.
func fetchFromJoplin(ref string) (string, string, error) {
	token, err := joplin.LoadDefaultToken()
	if err != nil {
		return "", "", err
	}
	base := os.Getenv("JOPLIN_URL")
	if base == "" {
		base = "http://127.0.0.1:41184"
	}
	c := joplin.New(joplin.Config{BaseURL: base, Token: token})

	if looksLikeNoteID(ref) {
		note, err := c.GetNote(ref, "id", "title", "body")
		if err != nil {
			return "", "", fmt.Errorf("get note %s: %w", ref, err)
		}
		if note == nil {
			return "", "", fmt.Errorf("note %s not found", ref)
		}
		return note.Body, note.ID, nil
	}

	// Title pattern. SearchNotes returns user_updated_time DESC, so
	// item[0] is the freshest. Request body explicitly — the default
	// search projection doesn't include it.
	results, err := c.SearchNotes(ref, 5, "id", "title", "body")
	if err != nil {
		return "", "", fmt.Errorf("search %q: %w", ref, err)
	}
	if len(results) == 0 {
		return "", "", fmt.Errorf("no Joplin note matches %q", ref)
	}
	hit := results[0]
	if hit.Body == "" {
		// Fall back to a direct GetNote — some Joplin builds drop
		// body in search results regardless of the fields param.
		full, gerr := c.GetNote(hit.ID, "id", "title", "body")
		if gerr != nil {
			return "", "", fmt.Errorf("search %q matched %s but body fetch failed: %w", ref, hit.ID, gerr)
		}
		return full.Body, full.ID, nil
	}
	return hit.Body, hit.ID, nil
}

// looksLikeNoteID matches Joplin's 32-char hex ID shape. Anything else
// is treated as a title pattern.
func looksLikeNoteID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func looksLikeFlag(s string) bool {
	return len(s) > 1 && s[0] == '-'
}

func dieMissing(what string) {
	fmt.Fprintf(os.Stderr, "qai cj extract: missing value for %s\n", what)
	os.Exit(1)
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
  qai cj extract <clip.md>             Read markdown from a file
  qai cj extract -                     Read markdown from stdin
  qai cj extract --joplin <id>         Fetch a Joplin note by 32-char ID
  qai cj extract --joplin "<title>"    Find latest matching note by title
  qai cj extract ... --summary         Human-skim view
  qai cj extract ... --strict          Exit 3 if nothing extracted

  qai cj batch <urls.csv> [flags]      Navigate→clip→extract for many URLs at once
                                       See 'qai cj batch --help' for the full surface.

WORKFLOW (fully scripted)
  qai browser open "<cj-intelligence-url>"
  qai browser wait "table" --timeout 30s         # React done rendering
  NOTE_ID=$(qai browser clip "cj/clips" "Pet Supplies $(date +%F)")
  qai cj extract --joplin "$NOTE_ID" | jq '.competitors[0:5]'

  Or for a fresh session that already clipped earlier:
  qai cj extract --joplin "Sales Trends" --strict | …  # most recent match

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
  qai cj extract clip.md --summary
  curl -s https://example.com/clip.md | qai cj extract -
  qai cj extract --joplin a1b2c3d4e5f67890a1b2c3d4e5f67890
  qai cj extract --joplin "Sales Trends" --strict | jq '.counts'`

const helpExtract = helpCJ
