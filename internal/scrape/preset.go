// Package scrape implements `qai scrape` — a generic web-scraping
// pipeline for affiliate / research workflows, with pluggable per-site
// presets. Amazon is preset #1; others plug in via RegisterPreset.
//
// The shared pipeline is always the same:
//
//   1. `qai clip <url>` → Playwright → Joplin note (bypasses bot detection).
//   2. Locate the clipped note via the Joplin Data API.
//   3. Preset-specific parser extracts structured fields from the note body.
//   4. Hero-image picker walks the note's embedded resources, applying
//      preset-tuned filters (JPG-only, min/max dims, aspect cap).
//   5. Emit a JSON brief — one object per URL, ready for an agent prompt.
//
// Batch mode reads a CSV and runs the pipeline per row with bounded
// parallelism. URLs not matching a registered preset use `--preset
// generic` (no structured parse, note body is captured raw).
package scrape

// Preset describes how to scrape a particular site.
type Preset struct {
	// Name is the short identifier used in --preset flag and CSV column.
	Name string

	// Hosts are hostname substrings this preset auto-matches. For
	// example, the amazon preset matches any URL host containing
	// "amazon." — amazon.co.uk, amazon.com, amazon.de, amazon.fr.
	Hosts []string

	// DefaultNotebook is the Joplin notebook to clip into.
	DefaultNotebook string

	// ExtractID pulls the site's canonical product ID from the URL.
	// Empty return means this URL isn't a product page we can handle.
	ExtractID func(productURL string) string

	// BuildURL reconstructs a full URL from an ID alone (for --id mode).
	// May be nil if the site doesn't support ID-only lookup.
	BuildURL func(id string) string

	// Parse extracts structured fields from the clipped note body.
	// The returned map is merged into the JSON brief under the preset's
	// own namespace. Use the "title", "image_path", and "price_*" keys
	// to feed canonical fields.
	Parse func(body, id string) map[string]any

	// ImageFilters tunes the hero-picker constraints for this site's
	// typical layout. Different marketplaces use different hero sizes.
	ImageFilters ImageFilters
}

// ImageFilters are the constraints the hero-picker applies when
// walking a note's embedded image resources. Presets tune these based
// on empirical observation of the target site.
//
// JPEGOnly rejects PNGs outright — on most marketplaces, PNGs in
// scraped dumps are logos, trust badges, or UI chrome, not hero shots.
// Set it false for sites whose product heroes are PNG (rare).
type ImageFilters struct {
	MinW           int
	MinH           int
	MaxLongSide    int
	MaxAspectRatio float64
	JPEGOnly       bool
}

// presets holds the registry, keyed by preset name.
var presets = map[string]*Preset{}

// RegisterPreset adds a preset to the registry. Call from a package
// init() so the preset is available before CmdScrape dispatches.
func RegisterPreset(p *Preset) {
	if p == nil || p.Name == "" {
		return
	}
	presets[p.Name] = p
}

// PresetByName returns the named preset, or nil if unknown.
func PresetByName(name string) *Preset {
	return presets[name]
}

// PresetForURL auto-detects a preset by matching the URL's host against
// each registered preset's Hosts list. Returns nil if no preset matches.
func PresetForURL(u string) *Preset {
	host := urlHost(u)
	if host == "" {
		return nil
	}
	for _, p := range presets {
		for _, h := range p.Hosts {
			if stringContains(host, h) {
				return p
			}
		}
	}
	return nil
}

// PresetNames returns all registered preset names, for help text.
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	return out
}
