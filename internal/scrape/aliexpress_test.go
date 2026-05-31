package scrape

import (
	"os"
	"strings"
	"testing"
)

// TestAliExtractID pins the URL-to-ID extractor against the URL shapes
// AliExpress actually serves: desktop /item/<id>.html, mobile subdomain,
// regional TLDs, and rewrites that bury the canonical path inside a
// querystring-noise prefix.
func TestAliExtractID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://www.aliexpress.com/item/1005006734834567.html", "1005006734834567"},
		{"https://www.aliexpress.us/item/3256805123456789.html", "3256805123456789"},
		{"https://m.aliexpress.com/item/1005006734834567.html?spm=foo", "1005006734834567"},
		{"https://www.aliexpress.co.uk/item/1005003456789012.html#review", "1005003456789012"},
		{"not a product url", ""},
		{"https://www.aliexpress.com/category/123/foo.html", ""},
	}
	for _, c := range cases {
		got := aliExpressExtractID(c.in)
		if got != c.want {
			t.Errorf("aliExpressExtractID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAliPriceRangeHighestWins is the load-bearing financial-math test.
// AliExpress lists variant-spread prices as ranges; the cheapest end is
// bait. landed_cost MUST be the HIGH end, never the low, and never the
// raw range string. This test would have caught the lowest-bait
// regression at the spec-review stage.
func TestAliPriceRangeHighestWins(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantLow     float64
		wantHigh    float64
		wantLanded  float64
		wantCurr    string
	}{
		{
			name:       "GBP hyphen range",
			body:       "# LED Diffuser Strip\n\nPrice £1.99 - £14.99 ",
			wantLow:    1.99,
			wantHigh:   14.99,
			wantLanded: 14.99,
			wantCurr:   "GBP",
		},
		{
			name:       "USD en-dash range",
			body:       "# LED Diffuser Strip\n\nPrice $3.50 – $42.00",
			wantLow:    3.5,
			wantHigh:   42.0,
			wantLanded: 42.0,
			wantCurr:   "USD",
		},
		{
			name:       "EUR em-dash range",
			body:       "# Item Title Long Enough\n\n€2.49 — €19.99 incl shipping",
			wantLow:    2.49,
			wantHigh:   19.99,
			wantLanded: 19.99,
			wantCurr:   "EUR",
		},
		{
			name:       "thousands separator",
			body:       "# Bulk Listing Title\n\n£1,299.00 - £2,499.50",
			wantLow:    1299.00,
			wantHigh:   2499.50,
			wantLanded: 2499.50,
			wantCurr:   "GBP",
		},
		{
			name:       "EUR suffix-currency with comma decimal (es Web Clipper format)",
			body:       "# Some Product Title Long Enough\n\n70,72€\n\n153,58€",
			wantLow:    70.72,
			wantHigh:   153.58,
			wantLanded: 153.58,
			wantCurr:   "EUR",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := aliExpressParse(c.body, "1005006734834567")
			gotLow, _ := out["price_low_"+strings.ToLower(c.wantCurr)].(float64)
			gotHigh, _ := out["price_high_"+strings.ToLower(c.wantCurr)].(float64)
			gotLanded, _ := out["landed_cost"].(float64)
			gotCurr, _ := out["currency"].(string)

			if gotLow != c.wantLow {
				t.Errorf("low = %v, want %v", gotLow, c.wantLow)
			}
			if gotHigh != c.wantHigh {
				t.Errorf("high = %v, want %v", gotHigh, c.wantHigh)
			}
			if gotLanded != c.wantLanded {
				t.Errorf("landed_cost = %v, want %v", gotLanded, c.wantLanded)
			}
			if gotCurr != c.wantCurr {
				t.Errorf("currency = %q, want %q", gotCurr, c.wantCurr)
			}
		})
	}
}

// TestAliPriceSingleFallback confirms that when no range is in the body
// the single-price fallback runs and treats the lone value as both
// bounds of a degenerate range, so downstream consumers don't have to
// special-case the missing-high state.
func TestAliPriceSingleFallback(t *testing.T) {
	body := "# Some Product Title Here Long Enough\n\nPrice: £24.99\n"
	out := aliExpressParse(body, "1005006734834567")
	low, _ := out["price_low_gbp"].(float64)
	high, _ := out["price_high_gbp"].(float64)
	landed, _ := out["landed_cost"].(float64)
	if low != 24.99 || high != 24.99 || landed != 24.99 {
		t.Fatalf("single-price fallback wrong: low=%v high=%v landed=%v", low, high, landed)
	}
}

// TestAliPriceNoRawRangeString guards the "never emit the raw range"
// half of the mandate. The brief must not contain a "1.99 - 14.99"
// string value anywhere — any downstream consumer doing arithmetic on
// data["price"] should see numeric floats, never need string parsing.
func TestAliPriceNoRawRangeString(t *testing.T) {
	body := "# Title Here Long Enough\n\nPrice £1.99 - £14.99"
	out := aliExpressParse(body, "1005006734834567")
	for k, v := range out {
		if s, ok := v.(string); ok {
			if strings.Contains(s, "1.99 - 14.99") || strings.Contains(s, "1.99-14.99") {
				t.Errorf("key %q leaked raw range string: %q", k, s)
			}
		}
	}
}

// TestAliSoldCount checks the demand-signal normaliser against the
// shapes AE actually emits: "10,000+ sold", "1.5K sold", "500 sold".
func TestAliSoldCount(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{"# Title Long\n\n10,000+ sold this week", 10000},
		{"# Title Long\n\n1.5K+ sold", 1500},
		{"# Title Long\n\nSold 500", 500},
		{"# Title Long\n\n2M sold", 2_000_000},
		{"# Title Long\n\n750 sold", 750},
		// Spanish "4.000+ vendido" — period is thousands sep, not decimal.
		// Regression case for the locale-agnostic parser delegation.
		{"# Title Long\n\n4.000+ vendido", 4000},
		{"# Title Long\n\n1.000+ vendidos", 1000},
	}
	for _, c := range cases {
		out := aliExpressParse(c.body, "1005006734834567")
		got, _ := out["sold_count"].(int)
		if got != c.want {
			t.Errorf("body %q sold_count = %d, want %d", c.body, got, c.want)
		}
	}
}

// TestAliRating confirms the rating regex stays in the 1.0..5.0 band —
// can't accidentally grab a price like £4.99 as a rating, can't grab a
// review count like "4,800" without explicit context.
func TestAliRating(t *testing.T) {
	body := "# Some LED Strip Title Long Enough\n\n4.8 out of 5 stars (1,234 reviews)\n"
	out := aliExpressParse(body, "1005006734834567")
	rating, _ := out["rating"].(float64)
	if rating != 4.8 {
		t.Errorf("rating = %v, want 4.8", rating)
	}
	reviews, _ := out["reviews_count"].(int)
	if reviews != 1234 {
		t.Errorf("reviews_count = %v, want 1234", reviews)
	}
}

// TestAliRealWebClipperES runs the parser against a real Joplin Web
// Clipper export of an es.aliexpress.com product page. This is the
// canonical "Web Clipper format" — comma-decimal, suffix €, Spanish
// vocabulary ("vendido(s)", "Nuevo comprador"). Catches any regression
// where the locale-agnostic parser, the discount-context filter, or
// the multilingual sold-pattern detection drifts.
//
// Ground truth for this fixture:
//   Sale price:    70,72€
//   Discount tag:  -82,86€ (Nuevo comprador — must NOT be ingested as price)
//   Original:      153,58€ ← landed_cost per HIGHEST-wins mandate
//   Rating:        3.8
//   Sold:          21 vendido(s)
func TestAliRealWebClipperES(t *testing.T) {
	body, err := os.ReadFile("testdata/aliexpress-es-tile-machine.md")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out := aliExpressParse(string(body), "1005012198843418")

	// Landed cost — must be the ORIGINAL price, not the sale or the
	// "save" discount tag. This is the load-bearing financial-math
	// guarantee per the task brief.
	landed, _ := out["landed_cost"].(float64)
	if landed != 153.58 {
		t.Errorf("landed_cost = %v, want 153.58 (the strike-through original, not the sale or discount tag)", landed)
	}
	low, _ := out["price_low_eur"].(float64)
	if low != 70.72 {
		t.Errorf("price_low_eur = %v, want 70.72", low)
	}
	high, _ := out["price_high_eur"].(float64)
	if high != 153.58 {
		t.Errorf("price_high_eur = %v, want 153.58", high)
	}

	// Discount tag (82.86) must NOT appear as a price observation.
	// Same for the related-products prices further down (49.12, 108.79,
	// etc.) — those belong to OTHER items and the 8KB main-product
	// window should keep them out of the aggregate.
	for _, blocked := range []float64{82.86, 59.67, 38.75, 87.39, 49.12} {
		for _, key := range []string{"landed_cost", "price_low_eur", "price_high_eur"} {
			if v, _ := out[key].(float64); v == blocked {
				t.Errorf("%s = %v leaked a forbidden value (discount tag or related-product price)", key, blocked)
			}
		}
	}

	currency, _ := out["currency"].(string)
	if currency != "EUR" {
		t.Errorf("currency = %q, want EUR", currency)
	}

	// Spanish "vendido(s)" must parse to 21.
	sold, _ := out["sold_count"].(int)
	if sold != 21 {
		t.Errorf("sold_count = %v, want 21", sold)
	}

	rating, _ := out["rating"].(float64)
	if rating != 3.8 {
		t.Errorf("rating = %v, want 3.8", rating)
	}
}

// TestAliScoutIDExtraction walks a synthetic search-page body and
// confirms scout pulls every /item/<id>.html in body order, dedupes,
// and preserves the search URL's TLD on each rebuilt product URL.
func TestAliScoutIDExtraction(t *testing.T) {
	body := strings.Join([]string{
		"# Search Results",
		"[Item A](/item/1005006111111111.html)",
		"[Item B](/item/1005006222222222.html)",
		"[Item A repeat](/item/1005006111111111.html)",
		"[Item C](/item/3256805333333333.html)",
	}, "\n")
	got := aliExpressScout(body, "https://www.aliexpress.co.uk/wholesale?SearchText=led+diffuser")
	want := []string{
		"https://www.aliexpress.co.uk/item/1005006111111111.html",
		"https://www.aliexpress.co.uk/item/1005006222222222.html",
		"https://www.aliexpress.co.uk/item/3256805333333333.html",
	}
	if len(got) != len(want) {
		t.Fatalf("scout returned %d urls, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scout[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
