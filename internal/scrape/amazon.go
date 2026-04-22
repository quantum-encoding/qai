package scrape

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Amazon preset — tuned against the UK marketplace but works on any
// Amazon TLD because the markup is consistent across regions.
//
// Image filters (empirically tuned across 9 real Amazon UK product pages):
//   - JPG only. PNGs in Amazon dumps are logos, Prime badges, and
//     certification marks — never the product hero.
//   - Both dims ≥ 400 px. Rejects avatars, thumbnails, category icons.
//   - Longest side ≤ 1500 px. Amazon caps hero at _AC_SL1500_ here;
//     images above that are customer-review photos.
//   - Aspect ratio ≤ 1.8. Promo banners are 2:1+ (728×90, 650×350,
//     1464×600); product heroes are 1:1 to 1.75:1.

func init() {
	RegisterPreset(&Preset{
		Name:            "amazon",
		Hosts:           []string{"amazon.co.uk", "amazon.com", "amazon.de", "amazon.fr", "amazon.it", "amazon.es", "amazon.ca"},
		DefaultNotebook: "ProductResearch",
		ExtractID:       amazonExtractASIN,
		BuildURL:        amazonBuildURL,
		Parse:           amazonParse,
		ImageFilters: ImageFilters{
			MinW:           400,
			MinH:           400,
			MaxLongSide:    1500,
			MaxAspectRatio: 1.8,
			JPEGOnly:       true,
		},
	})
}

var (
	asinDPRe = regexp.MustCompile(`/dp/([A-Z0-9]{10})`)
	asinGPRe = regexp.MustCompile(`/gp/product/([A-Z0-9]{10})`)
)

func amazonExtractASIN(productURL string) string {
	if m := asinDPRe.FindStringSubmatch(productURL); len(m) == 2 {
		return m[1]
	}
	if m := asinGPRe.FindStringSubmatch(productURL); len(m) == 2 {
		return m[1]
	}
	return ""
}

func amazonBuildURL(asin string) string {
	return fmt.Sprintf("https://www.amazon.co.uk/dp/%s", asin)
}

// ─── parser ──────────────────────────────────────────────────────────────

var (
	priceDupRe     = regexp.MustCompile(`£([0-9,]+\.\d{2})£([0-9,]+\.\d{2})`)
	priceSingleRe  = regexp.MustCompile(`£([0-9,]+\.\d{2})`)
	usedPriceRe    = regexp.MustCompile(`Used[^£]{0,60}£([0-9,]+\.\d{2})`)
	titleHeadingRe = regexp.MustCompile(`(?m)^#\s+([^\n]{30,220})`)
	bulletRe       = regexp.MustCompile(`-\s+([A-Z][A-Z][A-Z][^\n]{20,300})`)
	specRowRe      = regexp.MustCompile(`\|\s*([A-Z][A-Za-z ]+)\s*\|\s*([^|\n]{2,200})\s*\|`)
	dpRefRe        = regexp.MustCompile(`/dp/([A-Z0-9]{10})`)
)

var titleKeywords = []string{
	"Laptop", "Graphics", "GPU", "MacBook", "Card", "Monitor",
	"Gaming", "Keyboard", "Mouse", "Headset",
}

func amazonParse(body, asin string) map[string]any {
	out := map[string]any{}

	// Main price — Amazon renders the canonical price as a duplicated
	// adjacent pair `£X,XXX.XX£X,XXX.XX` in the accessibility markup.
	// This pattern is unique to the real price — add-ons, carousel items
	// and related-product prices never duplicate that way.
	if m := priceDupRe.FindStringSubmatch(body); len(m) == 3 && m[1] == m[2] {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil {
			out["price_new_gbp"] = f
		}
	} else if idx := strings.Index(body, "Buy New"); idx >= 0 {
		start := idx - 20
		if start < 0 {
			start = 0
		}
		end := idx + 300
		if end > len(body) {
			end = len(body)
		}
		if m := priceSingleRe.FindStringSubmatch(body[start:end]); len(m) == 2 {
			if f, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil {
				out["price_new_gbp"] = f
			}
		}
	}

	if m := usedPriceRe.FindStringSubmatch(body); len(m) == 2 {
		if f, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil {
			out["price_used_gbp"] = f
		}
	}

	// Title — first H1 whose text contains a product-category keyword.
	for _, m := range titleHeadingRe.FindAllStringSubmatch(body, -1) {
		t := strings.TrimSpace(m[1])
		for _, kw := range titleKeywords {
			if strings.Contains(t, kw) {
				out["title"] = t
				break
			}
		}
		if _, ok := out["title"]; ok {
			break
		}
	}

	// Bullets — "About this item" block.
	if idx := strings.Index(body, "About this item"); idx >= 0 {
		end := idx + 4000
		if end > len(body) {
			end = len(body)
		}
		var bullets []string
		for _, m := range bulletRe.FindAllStringSubmatch(body[idx:end], -1) {
			if len(bullets) >= 8 {
				break
			}
			bullets = append(bullets, strings.TrimSpace(m[1]))
		}
		if len(bullets) > 0 {
			out["bullets"] = bullets
		}
	}

	// Spec table — Product Details / Product Information section.
	pdIdx := strings.Index(body, "Product Details")
	if pdIdx < 0 {
		pdIdx = strings.Index(body, "Product Information")
	}
	if pdIdx >= 0 {
		end := pdIdx + 4000
		if end > len(body) {
			end = len(body)
		}
		specs := map[string]string{}
		for _, m := range specRowRe.FindAllStringSubmatch(body[pdIdx:end], -1) {
			key := strings.TrimSpace(m[1])
			val := strings.TrimSpace(m[2])
			keyLower := strings.ToLower(key)
			if (keyLower == "---" || keyLower == "colour") && (val == "---" || val == "") {
				continue
			}
			if strings.HasPrefix(key, "-") || strings.HasPrefix(val, "-") {
				continue
			}
			specs[key] = val
		}
		if len(specs) > 0 {
			out["specs"] = specs
		}
	}

	// Related ASINs — useful for expanding the review pipeline (feed
	// these back in as the next batch).
	seen := map[string]bool{asin: true}
	var related []string
	for _, m := range dpRefRe.FindAllStringSubmatch(body, -1) {
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
		out["related_asins"] = related
	}

	return out
}
