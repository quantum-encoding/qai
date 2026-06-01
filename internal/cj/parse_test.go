package cj

import (
	"testing"
)

// fixtureClip is a hand-crafted subset of a real Joplin-clipped CJ
// Sales Trends page, covering every edge case the parser has to
// survive:
//
//   - A "CJ Recommended Top N" block with both a price range
//     ("$11.02-33.53") and a single-value price ("$142.86").
//   - A competitor row where the product title contains literal `|`
//     characters (the Advantage row) — must NOT shift the pipe-split.
//   - Non-breaking spaces in the "Listing" cell ("48 Days") —
//     Go's `\s` is ASCII-only, so the regex has to handle U+00A0
//     explicitly.
//   - A "Details" anchor in every row — must NOT win as the row's
//     extracted title.
const fixtureClip = "### CJ Recommended Top 5\n" +
	"\n" +
	"<img/>[High Quality Dog Bed](https://www.cjdropshipping.com/product/high-quality-dog-bed-p-1871839683610435586.html)Listed 494\\$11.02-33.53\n" +
	"\n" +
	"<img/>[Elegant Rectangular Pet Bed](https://www.cjdropshipping.com/product/elegant-rectangular-pet-bed-p-1864954624399527936.html)Listed 533\\$142.86\n" +
	"\n" +
	"### Competitor Table\n" +
	"\n" +
	"| Product | Brand | LQS | Sales Volume | Sales | Rating | Reviews | Listing | CJ Similar | Action |\n" +
	"| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n" +
	"| <img/>](https://www.cjdropshipping.com/intelligence/sales-trends/2619533011/B0GT6P8ZJP?categoryName=Pet%20Supplies)<br><br>[SmartBones Chicken-Wrapped Sticks, 7 Ounce (Pack of 2)](https://www.cjdropshipping.com/intelligence/sales-trends/2619533011/B0GT6P8ZJP?categoryName=Pet%20Supplies)SmartBones\\$13.98 | SmartBones | 85  | 9.8K | \\$137.6K | 4.7 | 4.4K | 48 Days |     | [Details](https://www.cjdropshipping.com/intelligence/sales-trends/2619533011/B0GT6P8ZJP?categoryName=Pet%20Supplies) |\n" +
	"| <img/>](https://www.cjdropshipping.com/intelligence/sales-trends/2619533011/B07RS578MJ?categoryName=Pet%20Supplies)<br><br>[Advantage II Ferret Vet-Recommended Flea Treatment & Prevention \\| Ferret 1+ lbs. \\| 2-Month Supply](https://www.cjdropshipping.com/intelligence/sales-trends/2619533011/B07RS578MJ?categoryName=Pet%20Supplies)Advantage\\$32.48 | Advantage | 84  | 669 | \\$21.7K | 4.5 | 2.4K | 2543 Days |     | [Details](https://www.cjdropshipping.com/intelligence/sales-trends/2619533011/B07RS578MJ?categoryName=Pet%20Supplies) |\n"

func TestParseCategory(t *testing.T) {
	r := Parse(fixtureClip)
	if r.Category != "Pet Supplies" {
		t.Errorf("Category = %q, want %q", r.Category, "Pet Supplies")
	}
	if r.CategoryID != "2619533011" {
		t.Errorf("CategoryID = %q, want %q", r.CategoryID, "2619533011")
	}
}

func TestParseCJRecommended(t *testing.T) {
	r := Parse(fixtureClip)
	if len(r.CJProducts) != 2 {
		t.Fatalf("CJProducts: got %d want 2", len(r.CJProducts))
	}

	// Range form: "$11.02-33.53"
	p0 := r.CJProducts[0]
	if p0.PID != "1871839683610435586" {
		t.Errorf("p0.PID = %q", p0.PID)
	}
	if p0.Title != "High Quality Dog Bed" {
		t.Errorf("p0.Title = %q", p0.Title)
	}
	if p0.ListedCount != 494 {
		t.Errorf("p0.ListedCount = %d, want 494", p0.ListedCount)
	}
	if p0.PriceMin != 11.02 || p0.PriceMax != 33.53 {
		t.Errorf("p0 price = (%v, %v), want (11.02, 33.53)", p0.PriceMin, p0.PriceMax)
	}

	// Single-value form: "$142.86"
	p1 := r.CJProducts[1]
	if p1.PriceMin != 142.86 || p1.PriceMax != 142.86 {
		t.Errorf("p1 price = (%v, %v), want (142.86, 142.86) — single value form",
			p1.PriceMin, p1.PriceMax)
	}
}

// TestParseCompetitorWithCleanRow — baseline. SmartBones row has no
// pipe in the title; expected to come out clean.
func TestParseCompetitorWithCleanRow(t *testing.T) {
	r := Parse(fixtureClip)
	if len(r.Competitors) < 1 {
		t.Fatalf("Competitors: got %d", len(r.Competitors))
	}
	c := r.Competitors[0]
	if c.ASIN != "B0GT6P8ZJP" {
		t.Errorf("ASIN = %q, want B0GT6P8ZJP", c.ASIN)
	}
	if c.Brand != "SmartBones" {
		t.Errorf("Brand = %q, want SmartBones", c.Brand)
	}
	if c.Title == "Details" {
		t.Errorf("Title = %q — the Details anchor leaked through", c.Title)
	}
	if c.Title != "SmartBones Chicken-Wrapped Sticks, 7 Ounce (Pack of 2)" {
		t.Errorf("Title = %q", c.Title)
	}
	if c.LQS != 85 {
		t.Errorf("LQS = %d, want 85", c.LQS)
	}
	if c.SalesVolNum != 9800 {
		t.Errorf("SalesVolNum = %d, want 9800 (parsed from 9.8K)", c.SalesVolNum)
	}
	if c.SalesUSDNum != 137600 {
		t.Errorf("SalesUSDNum = %d, want 137600 (parsed from $137.6K)", c.SalesUSDNum)
	}
	if c.ReviewsNum != 4400 {
		t.Errorf("ReviewsNum = %d, want 4400 (parsed from 4.4K)", c.ReviewsNum)
	}
	if c.ListingDays != 48 {
		t.Errorf("ListingDays = %d, want 48 — non-breaking space regression",
			c.ListingDays)
	}
	if c.Rating != 4.7 {
		t.Errorf("Rating = %v, want 4.7", c.Rating)
	}
}

// TestParseCompetitorWithPipeInTitle — the load-bearing edge case.
// The Advantage row's title contains literal `|` characters; a naive
// pipe-split would shift every subsequent column. Without the
// link-aware splitter, brand becomes a title fragment and listing
// days lands somewhere else.
func TestParseCompetitorWithPipeInTitle(t *testing.T) {
	r := Parse(fixtureClip)
	if len(r.Competitors) < 2 {
		t.Fatalf("Competitors: got %d, want at least 2", len(r.Competitors))
	}
	c := r.Competitors[1]
	if c.ASIN != "B07RS578MJ" {
		t.Errorf("ASIN = %q, want B07RS578MJ", c.ASIN)
	}
	if c.Brand != "Advantage" {
		t.Errorf("Brand = %q, want Advantage — pipe-in-title broke column count",
			c.Brand)
	}
	if c.LQS != 84 {
		t.Errorf("LQS = %d, want 84 — shifted column", c.LQS)
	}
	if c.SalesVolNum != 669 {
		t.Errorf("SalesVolNum = %d, want 669 — shifted column", c.SalesVolNum)
	}
	if c.SalesUSDNum != 21700 {
		t.Errorf("SalesUSDNum = %d, want 21700 (parsed from $21.7K)",
			c.SalesUSDNum)
	}
	if c.ListingDays != 2543 {
		t.Errorf("ListingDays = %d, want 2543", c.ListingDays)
	}
}

func TestParseKMNumber(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"137.6K", 137600},
		{"$137.6K", 137600},
		{"4.4K", 4400},
		{"2.4K", 2400},
		{"9.8K", 9800},
		{"1.4M", 1_400_000},
		{"669", 669},
		{"$32.48", 32}, // truncated to int — fine for sortable rank
		{"", 0},
		{"not-a-number", 0},
	}
	for _, c := range cases {
		got := parseKMNumber(c.in)
		if got != c.want {
			t.Errorf("parseKMNumber(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSplitPipeRowRespectsLinks proves the splitter ignores `|`
// inside `[...]` and `(...)`. Without this, every row with a pipe in
// the title would corrupt every downstream cell.
func TestSplitPipeRowRespectsLinks(t *testing.T) {
	line := `| pre [bracketed | with pipe] mid (paren | inside) end | second | third |`
	cells := splitPipeRow(line)
	want := []string{
		" pre [bracketed | with pipe] mid (paren | inside) end ",
		" second ",
		" third ",
	}
	if len(cells) != len(want) {
		t.Fatalf("got %d cells, want %d: %#v", len(cells), len(want), cells)
	}
	for i, w := range want {
		if cells[i] != w {
			t.Errorf("cell[%d] = %q, want %q", i, cells[i], w)
		}
	}
}

// TestEmptyInputDoesNotPanic — buildContext-style guard. An agent
// running --extract on a non-CJ markdown file should get an empty
// payload, not a nil-deref.
func TestEmptyInputDoesNotPanic(t *testing.T) {
	r := Parse("nothing to see here")
	if r == nil {
		t.Fatal("Parse returned nil")
	}
	if len(r.CJProducts) != 0 || len(r.Competitors) != 0 {
		t.Errorf("unexpected matches in non-CJ input: %+v", r)
	}
	if r.Counts.CJProducts != 0 || r.Counts.Competitors != 0 {
		t.Errorf("counts non-zero on empty: %+v", r.Counts)
	}
}
