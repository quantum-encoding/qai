package scrape

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// AliExpress preset — tuned against the global storefront but works on
// any AE TLD (aliexpress.us, aliexpress.co.uk, etc.) since the markup
// is consistent across regions.
//
// Critical price-extraction mandate: AliExpress lists variant-spread
// prices as ranges in the DOM (e.g. "£1.99 - £14.99"). The lowest
// number is bait — it's the cheapest variant (smallest size / fewest
// LEDs / etc.) that no realistic dropship listing will be ordering.
// landed_cost MUST select the HIGHEST value in any range so margin
// math and supplier-cost screens aren't tricked into a false positive.
//
// Image filters for alicdn-hosted heroes:
//   - JPG only. AE PNGs in clipped dumps are payment-method badges,
//     seller-rating ribbons, and currency-flag widgets — not heroes.
//   - Both dims ≥ 350 px. AE's gallery thumbs render at 240px;
//     anything smaller is a variant swatch or UI chrome.
//   - Longest side ≤ 1500 px. AE caps gallery hero at 960x960; bigger
//     payloads are reviewer-uploaded photos in the comments section.
//   - Aspect ratio ≤ 1.8. AE product heroes are 1:1; banners are wider.

func init() {
	RegisterPreset(&Preset{
		Name: "aliexpress",
		// Hosts are matched as substrings of the URL host, so this
		// list captures aliexpress.com, .us, .ru, .co.uk, fr.aliexpress.com,
		// m.aliexpress.com, etc. without enumerating every TLD.
		Hosts:           []string{"aliexpress.com", "aliexpress.us", "aliexpress.ru", "aliexpress.co.uk"},
		DefaultNotebook: "ProductResearch",
		ExtractID:       aliExpressExtractID,
		BuildURL:        aliExpressBuildURL,
		Parse:           aliExpressParse,
		Scout:           aliExpressScout,
		ScoutTitle:      aliExpressScoutTitle,
		ImageFilters: ImageFilters{
			// AE serves product heroes as AVIF (large, modern codec)
			// and occasionally PNG with transparent backgrounds.
			// JPEGs in AE clips are favicons / store-rating ribbons,
			// the opposite of Amazon's pattern. Filter inverts:
			// AVIF + PNG allowed, dim check applied when decodable,
			// otherwise a byte-size floor catches heroes vs thumbs.
			//
			// MaxLongSide 2000: AE's gallery hero is 1600×1600 (one
			// step above Amazon's 1500 cap). Anything above 2000 is
			// either a reviewer-uploaded photo deep in the comments
			// section or a zoom-view artifact, both undesirable.
			MinW:           350,
			MinH:           350,
			MaxLongSide:    2000,
			MaxAspectRatio: 1.8,
			JPEGOnly:       false,
			MinSizeBytes:   80_000, // accepts AVIF heroes when dim-read fails
		},
	})
}

// AliExpress item URLs come in a few shapes:
//
//	https://www.aliexpress.com/item/1005006734834567.html
//	https://www.aliexpress.us/item/3256805123456789.html
//	https://m.aliexpress.com/item/1005006734834567.html
//	https://www.aliexpress.com/item/_pvid=...&_t=...&item/1005...html (rewrites)
//
// All carry the canonical 12-16 digit numeric ID in /item/<digits>.html.
var aliItemRe = regexp.MustCompile(`/item/(\d{10,18})\.html`)

func aliExpressExtractID(productURL string) string {
	if m := aliItemRe.FindStringSubmatch(productURL); len(m) == 2 {
		return m[1]
	}
	return ""
}

func aliExpressBuildURL(id string) string {
	return fmt.Sprintf("https://www.aliexpress.com/item/%s.html", id)
}

// ─── parser ──────────────────────────────────────────────────────────────

var (
	// Two price shapes — AliExpress emits BOTH depending on the source
	// of the clip:
	//
	//   PREFIX  (Anglo / API):    ￡92.95   $24.99   £14.99
	//                             — period as decimal separator
	//                             — thousands optionally comma
	//
	//   SUFFIX  (European / .es / .fr / .it Web Clipper):
	//                             153,58€   70,72€   1.299,50€
	//                             — comma as decimal separator
	//                             — thousands optionally period
	//
	// The numeric body of each regex is permissive enough to capture
	// either decimal style; locale-aware parsing happens in
	// parseLocalePrice based on which currency symbol matched.
	aliPriceSinglePrefixRe = regexp.MustCompile(`([£￡$€])\s*([0-9][0-9.,]*)`)
	aliPriceSingleSuffixRe = regexp.MustCompile(`([0-9][0-9.,]*)\s*([£￡$€])`)

	// "10,000+ sold", "1.5K+ sold", "5000 sold", "10K sold" — number-first.
	// Also "21 vendido(s)" / "1.000+ vendidos" (Spanish), "1000 vendus"
	// (French), "1000 verkauft" (German). Localised vocabulary in the
	// regional Web Clipper captures.
	aliSoldRe = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?[KkMm]?)\+?\s*(?:sold|vendido|vendid|vendu|verkauf|venduto)`)

	// "Sold 1,500", "Sold 500+" — word-first variant AE uses in some
	// regional themes. Tried after the number-first pattern misses.
	aliSoldRePrefix = regexp.MustCompile(`(?i)sold\s+(\d+(?:[.,]\d+)?[KkMm]?)\+?`)

	// Star rating — DECIMAL form only (3.8, 4.5) anchored by a rating
	// vocabulary word within 40 non-letter characters. Without the
	// decimal requirement + vocab anchor, the regex would happily
	// match single digits inside product IDs (the "1" in
	// "1005012198843418" followed by "0" would parse as rating 1.0).
	// Multilingual vocab: review/rating/stars (en), valoracion (es),
	// valutazion (it), note/avis (fr), wertung/bewertung (de),
	// оценка (ru), ★ glyph.
	aliRatingRe = regexp.MustCompile(`(?i)\b([1-5]\.\d{1,2})\b[^a-z]{0,40}(?:★|out of 5|stars?|valoracion|valutazion|review|rating|note|avis|wertung|bewertung|оценк)`)

	// Review count near a star block: "(1,234)" or "1,234 Reviews" or
	// "1234 ratings". Loose; the rating regex picks up adjacent stars.
	aliReviewsRe = regexp.MustCompile(`(?i)([0-9][0-9,]*)\s*(?:reviews?|ratings?)`)

	// Title — first H1 with at least 20 chars. AE titles are stuffed
	// with category keywords ("LED Diffuser Strip Light Cover…") so the
	// length check alone is usually enough to skip nav text.
	aliTitleRe = regexp.MustCompile(`(?m)^#\s+([^\n]{20,300})`)

	// Specs table — Joplin renders the DOM spec table as markdown rows.
	aliSpecRowRe = regexp.MustCompile(`\|\s*([A-Z][A-Za-z0-9 /\-]+?)\s*\|\s*([^|\n]{1,300})\s*\|`)

	// Other AE item links on the page (related products / "more from
	// this store" / "people also viewed"). Bounded path match.
	aliItemRefRe = regexp.MustCompile(`/item/(\d{10,18})\.html`)

	// Free-shipping detection. AE renders this many different ways; we
	// match on the common shapes Joplin's markdown conversion produces.
	aliFreeShippingRe = regexp.MustCompile(`(?i)free\s*shipping|free\s*delivery`)
)

// currencyCode maps the symbol AE renders into the ISO-ish suffix we
// stamp on the emitted keys. Falls back to "gbp" when we can't tell —
// the user's account is UK-locale by default.
//
// Note: AE's markdown captures often use U+FFE1 (￡ FULLWIDTH POUND
// SIGN) rather than U+00A3 (£) — we accept both as GBP.
func currencyCode(sym string) string {
	switch sym {
	case "£", "￡":
		return "gbp"
	case "$":
		return "usd"
	case "€":
		return "eur"
	default:
		return "gbp"
	}
}

// isDiscountContext returns true when the left-context of a price
// suggests it's a discount/saving amount rather than a list price.
// Filters out things like:
//
//	"New shoppers save ￡54.57"
//	"￡3.00 off on ￡15.00"          (the "off" tags ￡3.00 as the saving)
//	"Save up to ￡10"
//	"Nuevo comprador · -82,86€"     (Spanish: New buyer)
//	"Économisez 50€"                (French: save)
//
// Multilingual because AE Web Clipper captures use the user's display
// locale. Without these filters, AE's strike-through markup could
// give the highest number to a discount tag and ship the wrong
// landed_cost downstream.
func isDiscountContext(ctxLower string) bool {
	for _, kw := range []string{
		"save ", "saving", " off", "discount", "coupon",
		"comprador", "nuevo compr", // Spanish
		"économisez", "économie", "réduction", // French
		"sparen", "rabatt",      // German
		"risparmia", "sconto",   // Italian
	} {
		if strings.Contains(ctxLower, kw) {
			return true
		}
	}
	return false
}

func aliExpressParse(body, id string) map[string]any {
	out := map[string]any{}

	// ── Price extraction — HIGHEST WINS ─────────────────────────────
	//
	// AliExpress renders price in several conflicting ways depending on
	// the page state — and crucially, the cheap variant is always the
	// bait. We have to defend against ALL of these patterns:
	//
	//   Range:           "￡1.99 - ￡14.99"                          → 14.99
	//   Strike-through:  "￡38.38 ... New shoppers save ￡54.57 ￡92.95" → 92.95
	//   Single:          "￡24.99"                                    → 24.99
	//
	// The naive "first match" / "adjacent range" regex from the first
	// cut fails the strike-through case because the prices are
	// separated by other markup. The robust approach is to scan ALL
	// currency-prefixed prices in a window of the body limited to the
	// main product block (first ~8KB — beyond that we're in
	// related-products / store-carousel territory and prices there
	// belong to OTHER items, not the one we're scraping), then take
	// MAX as landed_cost and MIN as price_low.
	//
	// "Save"/"off"/"Save up to" prefixes are filtered because those
	// label discount amounts, not list prices. Without that filter,
	// "New shoppers save ￡54.57" could legitimately be the largest
	// number if the discount tops the listing.
	// Bound the price-scan window to the MAIN product section. AE
	// clips render related products beneath an h3 heading (### ) or
	// after a localised "similar items" marker. Both signals catch
	// the boundary; we pick whichever appears first. Without this
	// guard the price aggregate would absorb related-products prices
	// and the highest-wins mandate would surface the wrong currency
	// amount entirely.
	scanWindow := mainProductWindow(body)

	type priceObs struct {
		value float64
		sym   string
	}
	var observations []priceObs

	// Prefix-currency matches (￡92.95, $24.99).
	for _, m := range aliPriceSinglePrefixRe.FindAllStringSubmatchIndex(scanWindow, -1) {
		if len(m) < 6 {
			continue
		}
		if shouldSkipPrice(scanWindow, m[0], m[1]) {
			continue
		}
		sym := scanWindow[m[2]:m[3]]
		numStr := scanWindow[m[4]:m[5]]
		if v, ok := parseLocalePrice(numStr, sym); ok && v > 0 {
			observations = append(observations, priceObs{value: v, sym: sym})
		}
	}
	// Suffix-currency matches (70,72€  153,58€). We have to also reject
	// matches that overlap a prefix one we already counted — easy to do
	// by tracking the byte ranges, but cleaner to just dedupe by value
	// at the aggregate step (genuine duplicates are common in clipped
	// markdown anyway — same price listed in multiple places).
	for _, m := range aliPriceSingleSuffixRe.FindAllStringSubmatchIndex(scanWindow, -1) {
		if len(m) < 6 {
			continue
		}
		if shouldSkipPrice(scanWindow, m[0], m[1]) {
			continue
		}
		numStr := scanWindow[m[2]:m[3]]
		sym := scanWindow[m[4]:m[5]]
		if v, ok := parseLocalePrice(numStr, sym); ok && v > 0 {
			observations = append(observations, priceObs{value: v, sym: sym})
		}
	}

	if len(observations) > 0 {
		// Pick the dominant currency by count — AE pages occasionally
		// show a single $-tagged price (a converted reference) in a
		// €-dominant page; we report the page's primary currency, not
		// the rogue conversion line.
		currCount := map[string]int{}
		for _, o := range observations {
			currCount[o.sym]++
		}
		var primarySym string
		for s, n := range currCount {
			if n > currCount[primarySym] {
				primarySym = s
			}
		}
		var prices []float64
		for _, o := range observations {
			if o.sym == primarySym {
				prices = append(prices, o.value)
			}
		}
		priceLow, priceHigh := prices[0], prices[0]
		for _, p := range prices[1:] {
			if p < priceLow {
				priceLow = p
			}
			if p > priceHigh {
				priceHigh = p
			}
		}
		currency := currencyCode(primarySym)
		out["price_low_"+currency] = round2(priceLow)
		out["price_high_"+currency] = round2(priceHigh)
		out["landed_cost"] = round2(priceHigh) // mandate: HIGHEST wins
		out["currency"] = strings.ToUpper(currency)
		out["price_observations"] = len(prices)
	}

	// ── Sold count — demand signal ──────────────────────────────────
	// Try number-first ("10,000+ sold") then word-first ("Sold 1,500").
	// We expose both the canonical integer and the matched raw text so
	// a reviewer can spot-check that we parsed the right phrase.
	if m := aliSoldRe.FindStringSubmatch(body); len(m) == 2 {
		out["sold_raw"] = strings.TrimSpace(m[0])
		if n, ok := parseSoldCount(m[1]); ok {
			out["sold_count"] = n
		}
	} else if m := aliSoldRePrefix.FindStringSubmatch(body); len(m) == 2 {
		out["sold_raw"] = strings.TrimSpace(m[0])
		if n, ok := parseSoldCount(m[1]); ok {
			out["sold_count"] = n
		}
	}

	// ── Star rating ─────────────────────────────────────────────────
	if m := aliRatingRe.FindStringSubmatch(body); len(m) == 2 {
		if r, err := strconv.ParseFloat(m[1], 64); err == nil && r >= 1.0 && r <= 5.0 {
			out["rating"] = r
		}
	}

	// ── Review count (separate signal; rating can be present without it) ─
	if m := aliReviewsRe.FindStringSubmatch(body); len(m) == 2 {
		clean := strings.ReplaceAll(m[1], ",", "")
		if n, err := strconv.Atoi(clean); err == nil && n > 0 {
			out["reviews_count"] = n
		}
	}

	// ── Free shipping flag ──────────────────────────────────────────
	if aliFreeShippingRe.MatchString(body) {
		out["free_shipping"] = true
	}

	// ── Title — first H1 with ≥ 20 chars ────────────────────────────
	if m := aliTitleRe.FindStringSubmatch(body); len(m) == 2 {
		out["title"] = strings.TrimSpace(m[1])
	}

	// ── Specs table — name/value pairs ──────────────────────────────
	// AE renders specifications as a 2-column markdown table when
	// Joplin's clipper processes the rendered DOM. The first column is
	// the attribute name, second is the value. We restrict to the
	// "Specifications" section to avoid grabbing the reviews table or
	// the shipping-options table further down the page.
	if idx := indexFold(body, "Specifications"); idx >= 0 {
		end := idx + 4000
		if end > len(body) {
			end = len(body)
		}
		specs := map[string]string{}
		for _, m := range aliSpecRowRe.FindAllStringSubmatch(body[idx:end], -1) {
			key := strings.TrimSpace(m[1])
			val := strings.TrimSpace(m[2])
			if key == "" || val == "" || val == "---" {
				continue
			}
			if strings.HasPrefix(key, "-") || strings.HasPrefix(val, "-") {
				continue
			}
			// Cap to a sensible number of attributes — AE's spec tables
			// occasionally include 50+ rows of duplicated translations.
			if len(specs) >= 20 {
				break
			}
			specs[key] = val
		}
		if len(specs) > 0 {
			out["specs"] = specs
		}
	}

	// ── Related item IDs (for follow-up scrape batches) ─────────────
	seen := map[string]bool{id: true}
	var related []string
	for _, m := range aliItemRefRe.FindAllStringSubmatch(body, -1) {
		if len(m) == 2 && !seen[m[1]] {
			seen[m[1]] = true
			related = append(related, m[1])
		}
	}
	sort.Strings(related)
	if len(related) > 20 {
		related = related[:20]
	}
	if len(related) > 0 {
		out["related_ids"] = related
	}

	return out
}

// parseLocalePrice interprets a raw price string in either of the two
// number formats AE clips emit. We DON'T trust the currency symbol
// alone — Web Clipper output sometimes mixes locales (a € symbol with
// "2.49" Anglo decimal because the merchant's source data was Anglo).
// Instead we read the separator pattern from the number itself:
//
//	Both . and , present: the LAST one is the decimal separator.
//	"1.299,99" → period=thousands, comma=decimal → 1299.99
//	"1,299.99" → comma=thousands,  period=decimal → 1299.99
//
//	Only one separator: 3 digits after = thousands; 2 digits after = decimal.
//	"1,500" → thousands → 1500
//	"70,72" → decimal   → 70.72
//	"2.49"  → decimal   → 2.49
//	"1.500" → thousands → 1500
//
// Returns (value, ok) — ok=false on parse failure so the caller can
// drop the observation rather than poison the max aggregate.
func parseLocalePrice(raw, currencySym string) (float64, bool) {
	_ = currencySym // reserved — kept on the signature for future tracing
	clean := strings.TrimSpace(raw)
	lastComma := strings.LastIndex(clean, ",")
	lastPeriod := strings.LastIndex(clean, ".")

	var decimal, thousands rune
	switch {
	case lastComma >= 0 && lastPeriod >= 0:
		// Both present — later one is decimal.
		if lastComma > lastPeriod {
			decimal, thousands = ',', '.'
		} else {
			decimal, thousands = '.', ','
		}
	case lastComma >= 0:
		afterDigits := len(clean) - lastComma - 1
		switch afterDigits {
		case 2:
			decimal = ','
		case 3:
			thousands = ','
		}
	case lastPeriod >= 0:
		afterDigits := len(clean) - lastPeriod - 1
		switch afterDigits {
		case 2:
			decimal = '.'
		case 3:
			thousands = '.'
		}
	}

	if thousands != 0 {
		clean = strings.ReplaceAll(clean, string(thousands), "")
	}
	if decimal == ',' {
		// Swap the last (and only remaining) comma to period.
		if i := strings.LastIndex(clean, ","); i >= 0 {
			clean = clean[:i] + "." + clean[i+1:]
		}
	}

	v, err := strconv.ParseFloat(clean, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// mainProductWindow returns the prefix of body that contains the main
// product section, stripped of related-products carousels. Detection
// markers, in priority order:
//
//  1. First localised "similar items" / "related items" heading.
//  2. First h3 heading (### ) — AE renders related product titles
//     at h3 inside the same page block.
//
// Whichever appears earliest wins. The result is clamped to
// [minPriceScanBytes, maxPriceScanBytes] so neither a missing
// boundary nor a pathologically early h3 produces a misleading scan.
func mainProductWindow(body string) string {
	const (
		minPriceScanBytes = 1000
		maxPriceScanBytes = 12000
	)
	boundary := len(body)
	for _, marker := range []string{
		"\n### ",                  // any h3 — used by AE for related products
		"Artículos similares",     // es
		"Related items",           // en
		"You may also like",       // en
		"More from this store",    // en
		"Articles similaires",     // fr
		"Ähnliche Artikel",        // de
		"Articoli simili",         // it
		"Похожие товары",          // ru
	} {
		if i := strings.Index(body, marker); i >= 0 && i < boundary {
			boundary = i
		}
	}
	if boundary < minPriceScanBytes && len(body) >= minPriceScanBytes {
		boundary = minPriceScanBytes
	}
	if boundary > maxPriceScanBytes {
		boundary = maxPriceScanBytes
	}
	if boundary > len(body) {
		boundary = len(body)
	}
	return body[:boundary]
}

// shouldSkipPrice decides whether a single price observation should be
// dropped before it enters the highest-wins aggregate. We filter:
//
//   - Discount-tag prefixes ("New shoppers save ￡54.57",
//     "-82,86€ Nuevo comprador")
//   - Promo-threshold prices ("X off on Y" / "X en Y" — Y is the
//     spend-threshold, not a list price)
//
// Critical scoping rule: context is bounded to the SAME PARAGRAPH as
// the price (we cut at the most recent blank line). AE clips render
// "Nuevo comprador" / "New shoppers save" as the tail of the
// previous price block; without paragraph scoping, that tag's
// keywords would falsely filter the next paragraph's strike-through
// price and the max-wins mandate would surface a sale price instead.
func shouldSkipPrice(body string, start, end int) bool {
	left := start - 80
	if left < 0 {
		left = 0
	}
	right := end + 30
	if right > len(body) {
		right = len(body)
	}
	ctxLeft := strings.ToLower(body[left:start])
	ctxRight := strings.ToLower(body[end:right])

	// Scope to the current paragraph — cut everything before the most
	// recent blank line (two consecutive newlines or end of headings).
	if i := strings.LastIndex(ctxLeft, "\n\n"); i >= 0 {
		ctxLeft = ctxLeft[i+2:]
	}
	if i := strings.Index(ctxRight, "\n\n"); i >= 0 {
		ctxRight = ctxRight[:i]
	}

	if isDiscountContext(ctxLeft) {
		return true
	}
	// Negative-prefix discount labels: "-82,86€" — dash glued directly
	// to the number, marking it as savings rather than a list price.
	// Distinction vs "£1.99 - £14.99" (space-separated → range, keep
	// both): we check the strict trailing rune of ctxLeft only.
	for _, dash := range []string{"-", "–", "—", "−"} {
		if strings.HasSuffix(ctxLeft, dash) {
			return true
		}
	}
	// Threshold prices in "X off on Y" / "X en Y" promo lines. The
	// price IMMEDIATELY following "on " / "en " / "auf " / "sur " is
	// the spend-threshold for the discount, not a product list price.
	// We check the same-paragraph left context for these prepositions.
	trimmedLeft := strings.TrimRight(ctxLeft, " ")
	for _, prep := range []string{" on", " en", " auf", " sur"} {
		if strings.HasSuffix(trimmedLeft, prep) {
			return true
		}
	}
	// Right-context "off" suggests this price is the OFF amount in an
	// "X off Y" promo line — same intent, different position.
	if strings.HasPrefix(strings.TrimLeft(ctxRight, " "), "off") {
		return true
	}
	return false
}

// round2 rounds to two decimal places. parseFloat on a stripped-comma
// string can introduce ULP noise (£1299.99 → 1299.9899999999996); we
// don't want that surfacing in downstream JSONL.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// parseSoldCount maps "10,000+", "4.000+" (es), "1.5K", "2M" → int.
// Used for the demand signal. Returns (0, false) on parse failure so
// the caller can skip emitting an unreliable number.
//
// We delegate the numeric normalisation to parseLocalePrice — it
// already knows that "4.000" with three trailing digits is a thousands
// separator (Spanish format), whereas "70,72" with two is a decimal
// (also Spanish). Without that delegation a regex-side fix would
// either break English ("4,000+" → 4) or Spanish ("4.000+" → 4); the
// locale-agnostic parser handles both.
func parseSoldCount(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "+")
	if s == "" {
		return 0, false
	}
	mult := 1.0
	last := s[len(s)-1]
	switch last {
	case 'K', 'k':
		mult = 1_000
		s = s[:len(s)-1]
	case 'M', 'm':
		mult = 1_000_000
		s = s[:len(s)-1]
	}
	v, ok := parseLocalePrice(s, "")
	if !ok {
		return 0, false
	}
	return int(v * mult), true
}

// indexFold is a case-insensitive substring search — used for the
// "Specifications" header lookup since AE inconsistently capitalises
// section names across regional themes.
func indexFold(haystack, needle string) int {
	return strings.Index(strings.ToLower(haystack), strings.ToLower(needle))
}

// ─── scout ───────────────────────────────────────────────────────────────

// aliExpressScout extracts product URLs from a search / wholesale page
// body. AE search results render as /item/<id>.html links in the
// clipped markdown. We dedupe by ID and rebuild canonical URLs using
// the same TLD as the search URL so an aliexpress.us search yields
// .us product links, aliexpress.co.uk yields .co.uk, etc.
//
// Note: AE search pages also contain promoted-tile links (interleaved
// with organic), "store recommendations" carousels, and "you may also
// like" strips. We return everything in body order; --max caps the
// caller, who can stop at the top N (usually the organic results).
func aliExpressScout(body, searchURL string) []string {
	host := "www.aliexpress.com"
	scheme := "https"
	if u, err := url.Parse(searchURL); err == nil && u.Host != "" {
		host = u.Host
		if u.Scheme != "" {
			scheme = u.Scheme
		}
	}

	seen := map[string]bool{}
	var urls []string
	for _, m := range aliItemRefRe.FindAllStringSubmatch(body, -1) {
		if len(m) != 2 {
			continue
		}
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		urls = append(urls, fmt.Sprintf("%s://%s/item/%s.html", scheme, host, id))
	}
	return urls
}

// aliExpressScoutTitle derives a unique Joplin note title from a search
// URL. AE supports two search-URL shapes:
//
//	https://www.aliexpress.com/wholesale?SearchText=led+diffuser
//	https://www.aliexpress.com/w/wholesale-led-diffuser.html
//
// We try SearchText first; fall back to the /w/wholesale-<slug>.html
// path; finally use the host if neither is present.
func aliExpressScoutTitle(searchURL string) string {
	u, err := url.Parse(searchURL)
	if err != nil {
		return "AliExpress Scout: " + searchURL
	}
	if q := u.Query().Get("SearchText"); q != "" {
		return "AliExpress Scout: " + q
	}
	path := strings.Trim(u.Path, "/")
	if strings.HasPrefix(path, "w/wholesale-") {
		slug := strings.TrimSuffix(strings.TrimPrefix(path, "w/wholesale-"), ".html")
		slug = strings.ReplaceAll(slug, "-", " ")
		if slug != "" {
			return "AliExpress Scout: " + slug
		}
	}
	if path == "" {
		return "AliExpress Scout: " + u.Host
	}
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		path = path[idx+1:]
	}
	return "AliExpress Scout: " + path
}
