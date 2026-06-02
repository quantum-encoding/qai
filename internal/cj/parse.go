// Package cj parses CJ Dropshipping intelligence-dashboard pages that
// have been clipped to markdown via the Joplin Web Clipper.
//
// The clip workflow exists because CJ's intelligence dashboard is a
// React SPA behind a bot wall — neither qai scrape nor the CJ Open
// Platform API can surface the real-demand data the page shows.
// The human clips a category page in a real browser; this package
// reads the resulting markdown and emits structured JSON so an agent
// (in any session) can pipeline the data downstream without
// re-deriving the regex from scratch.
//
// Two distinct sections are extracted:
//
//  1. "CJ Recommended Top N" — products CJ can fulfil directly, with
//     pid + URL + listed-count + price-range. Boardable straight into
//     a research_products table.
//
//  2. Competitor sales table — Amazon listings indexed by CJ, with
//     ASIN + brand + sales volume + sales $ + reviews + listing-days.
//     Used as demand validation: high sales + many reviews + few
//     days-listed = a freshly proven winner.
//
// Pure parser — no I/O, no network. ParseFile is a one-line wrapper.
package cj

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Result is the extraction output. Counts is a convenience; consumers
// can recompute it from the slices but it makes --summary trivial.
type Result struct {
	SourceFile   string       `json:"source_file"`
	Category     string       `json:"category,omitempty"`
	CategoryID   string       `json:"category_id,omitempty"`
	CJProducts   []CJProduct  `json:"cj_products"`
	Competitors  []Competitor `json:"competitors"`
	Counts       Counts       `json:"counts"`
}

// CJProduct is one row from the "CJ Recommended Top N" section.
// PriceMin / PriceMax cover the "$11.02-33.53" range form; a single
// price ("$142.86") sets both to the same value.
type CJProduct struct {
	PID         string  `json:"pid"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	ListedCount int     `json:"listed_count"`
	PriceMin    float64 `json:"price_min"`
	PriceMax    float64 `json:"price_max"`
	PriceRaw    string  `json:"price_raw"` // verbatim "$11.02-33.53" — for display
}

// Competitor is one row from the competitor sales table. The numeric
// _Num fields are parsed from the K/M suffix notation so consumers
// can sort/filter; the raw strings ("$137.6K", "4.4K") stay alongside
// for display.
type Competitor struct {
	ASIN        string  `json:"asin"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Brand       string  `json:"brand"`
	Price       string  `json:"price"` // raw — can be a range like "$84.99 - $104.99"
	LQS         int     `json:"lqs"`
	SalesVolume string  `json:"sales_volume"`
	SalesVolNum int64   `json:"sales_volume_num"`
	SalesUSD    string  `json:"sales_usd"`
	SalesUSDNum int64   `json:"sales_usd_num"`
	Rating      float64 `json:"rating"`
	Reviews     string  `json:"reviews"`
	ReviewsNum  int64   `json:"reviews_num"`
	ListingDays int     `json:"listing_days"`
}

type Counts struct {
	CJProducts  int `json:"cj_products"`
	Competitors int `json:"competitors"`
}

// ParseFile reads the markdown clip from disk and runs Parse.
func ParseFile(path string) (*Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read clip: %w", err)
	}
	r := Parse(string(data))
	r.SourceFile = path
	return r, nil
}

// Parse extracts every CJ product and competitor row from the given
// markdown body. Sections with no matches yield empty slices, never
// nil — the JSON contract is "always emit []".
func Parse(md string) *Result {
	r := &Result{
		CJProducts:  []CJProduct{},
		Competitors: []Competitor{},
	}
	r.Category, r.CategoryID = parseCategory(md)

	// Two distinct CJ page shapes produce the same boardable output:
	//
	//   - Intelligence dashboard "CJ Recommended Top N" — the original
	//     parse target; explicit "Listed N\$PRICE" trailer.
	//   - Catalog search results (/search/<query>.html) — different
	//     card markup, same essential data (pid, title, list count,
	//     price range). Much wider discovery surface since the human
	//     can search by keyword.
	//
	// Both produce CJProduct rows; dedup by PID so a hybrid clip (rare
	// but possible if a recommended product also appears in search
	// results) doesn't double-count.
	r.CJProducts = dedupByPID(append(
		parseCJRecommended(md),
		parseSearchResults(md)...,
	))
	r.Competitors = parseCompetitorTable(md)
	r.Counts = Counts{
		CJProducts:  len(r.CJProducts),
		Competitors: len(r.Competitors),
	}
	return r
}

// dedupByPID keeps the first occurrence per pid. Intelligence-page
// matches run first so their richer "listed_count" wins if the same
// pid also shows up in a search-results block (search rows have a
// "Lists: N" count that means roughly the same thing).
func dedupByPID(in []CJProduct) []CJProduct {
	seen := make(map[string]struct{}, len(in))
	out := make([]CJProduct, 0, len(in))
	for _, p := range in {
		if _, dup := seen[p.PID]; dup {
			continue
		}
		seen[p.PID] = struct{}{}
		out = append(out, p)
	}
	return out
}

// ────────────────────────────────────────────────────────────────────────
// CJ Recommended Top N
// ────────────────────────────────────────────────────────────────────────

// cjRecommendedRow captures one boardable product:
//
//	[TITLE](https://www.cjdropshipping.com/product/SLUG-p-PID.html)Listed N\$PRICE
//
// The trailing "Listed N\$PRICE" is one run-on string in the clip
// (no whitespace between segments) because Joplin renders the React
// card as a single flow.
var cjRecommendedRow = regexp.MustCompile(
	`\[(?P<title>[^\]]+)\]\((?P<url>https://www\.cjdropshipping\.com/product/[^)]*-p-(?P<pid>\d+)\.html)\)Listed\s+(?P<listed>\d+)\\\$(?P<price>[\d.\-]+)`,
)

func parseCJRecommended(md string) []CJProduct {
	out := []CJProduct{}
	matches := cjRecommendedRow.FindAllStringSubmatch(md, -1)
	for _, m := range matches {
		title := strings.TrimSpace(m[cjRecommendedRow.SubexpIndex("title")])
		url := m[cjRecommendedRow.SubexpIndex("url")]
		pid := m[cjRecommendedRow.SubexpIndex("pid")]
		listed, _ := strconv.Atoi(m[cjRecommendedRow.SubexpIndex("listed")])
		priceRaw := m[cjRecommendedRow.SubexpIndex("price")]
		pMin, pMax := parsePriceRange(priceRaw)
		out = append(out, CJProduct{
			PID:         pid,
			URL:         url,
			Title:       title,
			ListedCount: listed,
			PriceMin:    pMin,
			PriceMax:    pMax,
			PriceRaw:    "$" + priceRaw,
		})
	}
	return out
}

// ────────────────────────────────────────────────────────────────────────
// Catalog search results (/search/<query>.html)
// ────────────────────────────────────────────────────────────────────────

// CJ catalog search renders each card as one long markdown link with
// HTML <br> separators inside the anchor:
//
//   [![](:/HASH)<br>List<br>Added Products<br>TITLE<br>Lists: N<br>\$PRICE<br>QTY](/product/SLUG-p-PID.html)
//
// The URL is RELATIVE here (no scheme/host) — Joplin's clipper drops
// the host prefix because the page is same-origin. We prepend the
// canonical https://www.cjdropshipping.com so downstream consumers
// don't have to.
var searchResultRow = regexp.MustCompile(
	`Added Products<br>(?P<title>[^<]+?)<br>Lists:\s*(?P<lists>\d+)<br>\\?\$(?P<price>[\d.\-]+)[^]]*?\]\((?P<url>/product/[^)]*-p-(?P<pid>\d+)\.html)\)`,
)

func parseSearchResults(md string) []CJProduct {
	out := []CJProduct{}
	matches := searchResultRow.FindAllStringSubmatch(md, -1)
	for _, m := range matches {
		title := strings.TrimSpace(m[searchResultRow.SubexpIndex("title")])
		listed, _ := strconv.Atoi(m[searchResultRow.SubexpIndex("lists")])
		priceRaw := m[searchResultRow.SubexpIndex("price")]
		url := "https://www.cjdropshipping.com" + m[searchResultRow.SubexpIndex("url")]
		pid := m[searchResultRow.SubexpIndex("pid")]
		pMin, pMax := parsePriceRange(priceRaw)
		out = append(out, CJProduct{
			PID:         pid,
			URL:         url,
			Title:       title,
			ListedCount: listed,
			PriceMin:    pMin,
			PriceMax:    pMax,
			PriceRaw:    "$" + priceRaw,
		})
	}
	return out
}

// parsePriceRange handles both "11.02-33.53" and "142.86" forms.
// Returns (min, max); for a single value, min == max.
func parsePriceRange(raw string) (float64, float64) {
	if i := strings.Index(raw, "-"); i >= 0 {
		lo, _ := strconv.ParseFloat(raw[:i], 64)
		hi, _ := strconv.ParseFloat(raw[i+1:], 64)
		return lo, hi
	}
	v, _ := strconv.ParseFloat(raw, 64)
	return v, v
}

// ────────────────────────────────────────────────────────────────────────
// Competitor sales table
// ────────────────────────────────────────────────────────────────────────

// Each competitor row is a single line beginning with `|` and
// containing the ASIN-style URL `/sales-trends/<categoryId>/<ASIN>?…`.
// Column layout (10 cells):
//
//	| Product | Brand | LQS | Sales Volume | Sales | Rating | Reviews | Listing | CJ Similar | Action |
//
// The Product cell has TWO embedded markdown links (image-anchor +
// title-text) plus a trailing "BRAND$PRICE" run-on. We extract:
//   - title from the second link
//   - URL + ASIN from either link (same target)
//   - price by trimming after the brand name
var competitorRowPrefix = regexp.MustCompile(`^\|.*?intelligence/sales-trends/\d+/[A-Z0-9]+`)
var asinURLRe = regexp.MustCompile(
	`https://www\.cjdropshipping\.com/intelligence/sales-trends/\d+/(?P<asin>[A-Z0-9]+)[^)\s]*`,
)
var titleLinkRe = regexp.MustCompile(`\[(?P<title>[^\]]+)\]\(https://www\.cjdropshipping\.com/intelligence/sales-trends/[^)]+\)`)

func parseCompetitorTable(md string) []Competitor {
	out := []Competitor{}
	for _, line := range strings.Split(md, "\n") {
		if !competitorRowPrefix.MatchString(line) {
			continue
		}
		c, ok := parseCompetitorRow(line)
		if ok {
			out = append(out, c)
		}
	}
	return out
}

func parseCompetitorRow(line string) (Competitor, bool) {
	// Pull URL + ASIN once.
	urlMatch := asinURLRe.FindStringSubmatch(line)
	if len(urlMatch) == 0 {
		return Competitor{}, false
	}
	asin := urlMatch[asinURLRe.SubexpIndex("asin")]
	url := urlMatch[0]

	// Title is the LONGEST embedded link's anchor text that ISN'T the
	// trailing "Details" button. The row has three matching links:
	// the image-wrapped link (anchor is HTML `<img …>` markup), the
	// real title link (the product name), and the Action column's
	// "Details" link. Skipping "Details" and taking the longest of
	// the remaining anchors reliably picks the title across the
	// table's row variants without needing to count link position.
	var title string
	for _, m := range titleLinkRe.FindAllStringSubmatch(line, -1) {
		t := strings.TrimSpace(m[titleLinkRe.SubexpIndex("title")])
		if t == "" || t == "Details" {
			continue
		}
		// Image anchors look like `<img width="54" …>` — skip them too.
		if strings.HasPrefix(t, "<img") {
			continue
		}
		if len(t) > len(title) {
			title = t
		}
	}

	// Pipe-split for the rest of the columns. The first cell (Product)
	// runs from start to the first ` | ` boundary, after which the
	// columns are clean.
	cells := splitPipeRow(line)
	if len(cells) < 10 {
		return Competitor{}, false
	}
	// cells layout:
	//   0 = Product (we already pulled title/URL/ASIN out of here)
	//   1 = Brand
	//   2 = LQS
	//   3 = Sales Volume (e.g. "9.8K")
	//   4 = Sales (e.g. "$137.6K")
	//   5 = Rating (e.g. "4.7")
	//   6 = Reviews (e.g. "4.4K")
	//   7 = Listing (e.g. "48 Days")
	//   8 = CJ Similar (often blank)
	//   9 = Action (Details link)
	brand := strings.TrimSpace(cells[1])
	price := extractTrailingPrice(cells[0], brand)
	lqs, _ := strconv.Atoi(strings.TrimSpace(cells[2]))
	salesVolume := cleanCell(cells[3])
	salesUSD := cleanCell(cells[4])
	rating, _ := strconv.ParseFloat(strings.TrimSpace(cells[5]), 64)
	reviews := cleanCell(cells[6])
	listingDays := parseListingDays(cells[7])

	return Competitor{
		ASIN:        asin,
		URL:         url,
		Title:       title,
		Brand:       brand,
		Price:       price,
		LQS:         lqs,
		SalesVolume: salesVolume,
		SalesVolNum: parseKMNumber(salesVolume),
		SalesUSD:    salesUSD,
		SalesUSDNum: parseKMNumber(strings.TrimPrefix(salesUSD, "$")),
		Rating:      rating,
		Reviews:     reviews,
		ReviewsNum:  parseKMNumber(reviews),
		ListingDays: listingDays,
	}, true
}

// splitPipeRow splits a markdown table row on `|`, ignoring pipes that
// fall inside `[...]` or `(...)` (markdown link anchors / URLs). Some
// CJ product titles contain literal pipes ("Ferret 1+ lbs. | 2-Month
// Supply" in an Advantage row), and a naive strings.Split would shift
// every subsequent column by N → wrong brand, wrong sales, wrong
// everything. Walks the line once tracking bracket depth and only
// splits at depth-0 pipes.
//
// Outer empty cells (the markdown's `| col1 | col2 |` bookend pipes)
// are stripped.
func splitPipeRow(line string) []string {
	var out []string
	var sb strings.Builder
	depthB, depthP := 0, 0 // [..]  (..)
	for _, r := range line {
		switch r {
		case '[':
			depthB++
			sb.WriteRune(r)
		case ']':
			if depthB > 0 {
				depthB--
			}
			sb.WriteRune(r)
		case '(':
			depthP++
			sb.WriteRune(r)
		case ')':
			if depthP > 0 {
				depthP--
			}
			sb.WriteRune(r)
		case '|':
			if depthB == 0 && depthP == 0 {
				out = append(out, sb.String())
				sb.Reset()
			} else {
				sb.WriteRune(r)
			}
		default:
			sb.WriteRune(r)
		}
	}
	out = append(out, sb.String())
	if len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		out = out[1:]
	}
	if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return out
}

// cleanCell strips the markdown escape backslashes Joplin sprinkles
// before `$` and trims whitespace.
func cleanCell(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, `\$`, "$")
	return s
}

// extractTrailingPrice pulls the "BRAND$PRICE" run-on from the
// Product cell. The format is reliable: the brand string immediately
// precedes the price; the price is everything after it up to the
// next pipe.
//
// Example:  "...](URL)SmartBones\$13.98"   → "$13.98"
//           "...](URL)Vivarium Electronics\$84.99 - \$104.99" → "$84.99 - $104.99"
func extractTrailingPrice(productCell, brand string) string {
	productCell = cleanCell(productCell)
	if brand == "" {
		return ""
	}
	idx := strings.LastIndex(productCell, brand)
	if idx < 0 {
		return ""
	}
	after := productCell[idx+len(brand):]
	// Brand string may have trailing whitespace, then the price. Trim
	// quotes and leading non-$ characters that aren't part of the price.
	after = strings.TrimSpace(after)
	if i := strings.Index(after, "$"); i >= 0 {
		return after[i:]
	}
	return ""
}

// listingDaysRe pulls "48" out of "48 Days" (also handles "5,523 Days").
//
// CJ renders these cells with a non-breaking space (U+00A0) between
// the number and "Days", and Go's RE2 `\s` is ASCII-only. The
// character class explicitly includes NBSP so the regex matches both
// the rare ASCII-space form and the wire format.
var listingDaysRe = regexp.MustCompile(`([\d,]+)[\s\x{00a0}]*Days?`)

func parseListingDays(cell string) int {
	m := listingDaysRe.FindStringSubmatch(cell)
	if len(m) < 2 {
		return 0
	}
	v, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	return v
}

// parseKMNumber turns "137.6K" → 137600, "1.4K" → 1400, "9.8K" → 9800,
// "2.4M" → 2_400_000, "$137.6K" → 137600 (strips $), "669" → 669.
// Returns 0 on unparseable input — caller treats it as "unknown".
func parseKMNumber(raw string) int64 {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "$")
	s = strings.ReplaceAll(s, ",", "")
	if s == "" {
		return 0
	}
	multiplier := int64(1)
	last := s[len(s)-1]
	switch last {
	case 'K', 'k':
		multiplier = 1_000
		s = s[:len(s)-1]
	case 'M', 'm':
		multiplier = 1_000_000
		s = s[:len(s)-1]
	case 'B', 'b':
		multiplier = 1_000_000_000
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(v * float64(multiplier))
}

// ────────────────────────────────────────────────────────────────────────
// Category detection
// ────────────────────────────────────────────────────────────────────────

// categoryURLRe pulls the category ID + name from any sales-trends URL.
// Example: /sales-trends/2619533011/B0GT6P8ZJP?categoryName=Pet%20Supplies
var categoryURLRe = regexp.MustCompile(
	`sales-trends/(?P<id>\d+)(?:/[A-Z0-9]+)?\?categoryName=(?P<name>[^)&\s]+)`,
)

func parseCategory(md string) (string, string) {
	m := categoryURLRe.FindStringSubmatch(md)
	if len(m) == 0 {
		return "", ""
	}
	id := m[categoryURLRe.SubexpIndex("id")]
	name := m[categoryURLRe.SubexpIndex("name")]
	// URL-decode "%20" → " ", "%2C" → ",". Light pass; full url-decoding
	// not needed since CJ only uses these two encodings in category names.
	name = strings.ReplaceAll(name, "%20", " ")
	name = strings.ReplaceAll(name, "%2C", ",")
	name = strings.ReplaceAll(name, "%2B", "+")
	return name, id
}
