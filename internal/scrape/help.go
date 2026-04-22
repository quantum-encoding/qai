package scrape

import "strings"

// helpScrape is a function (not a var) because it inlines the list of
// registered presets via PresetNames(), which isn't populated until
// each preset's init() has run — after package-level var initializers.
func helpScrape() string {
	return `qai scrape — pluggable web-scraping pipeline for affiliate / research

Runs a real-browser scrape (` + "`qai clip`" + ` → Joplin via Playwright), parses
the resulting note with a site-specific preset, picks a product hero
image, and emits a JSON brief. Works one URL at a time or as a batch
from a CSV. Built for building large affiliate-review libraries without
getting Amazon'd / Cloudflare'd / captcha'd.

Modes:
  qai scrape <url>                          Single URL (auto-detect preset)
  qai scrape --preset <name> <url>          Explicit preset
  qai scrape --amazon <url>                 Preset shorthand (--preset amazon)
  qai scrape --id <id> --preset amazon      Build URL from product ID
  qai scrape --csv targets.csv              Batch from CSV
  qai scrape --scout <search-url>           Extract product URLs → CSV

Flags:
  --preset <name>        Preset to use. Auto-detected from URL if omitted.
                         Registered: ` + strings.Join(PresetNames(), ", ") + `
  --amazon               Shorthand for --preset amazon.
  --id <id>              Build URL from ID (requires --preset).
  --notebook <name>      Joplin notebook (default: preset's default).
  --image-dir <path>     Where to save hero (default: public/product-images).
  --csv <path>           CSV of targets: ` + "`url[,preset[,notebook]]`" + `.
  --out, -o <path>       JSONL output. Without this flag, prints pretty JSON.
  --parallel <n>         Workers for --csv (default 1). Raise with care —
                         Joplin's search index can struggle with concurrent
                         writes.
  --resume               With --csv and --out, skip URLs already present.
  --scout                Treat the target as a search/listing URL. Emits
                         a CSV of discovered product URLs (one per row),
                         ready to feed back via --csv.
  --max <n>              With --scout, cap discovered URLs to the first N.

CSV format:
  Headerless (one URL per line):
    https://www.amazon.co.uk/dp/B0BT9R5XNN
    https://www.amazon.com/dp/B0GR1K7LHC

  Or with header row (extra columns are optional):
    url,preset,notebook
    https://www.amazon.co.uk/dp/B0BT9R5XNN,amazon,ProductResearch
    https://www.amazon.com/dp/B0GR1K7LHC,amazon,

Pipeline (per URL):
  1. ` + "`qai clip <url>`" + ` → Playwright → Joplin note
  2. Joplin Data API → find note body (retries for eventual consistency)
  3. Preset parser pulls structured fields (title, price, bullets, specs)
  4. Walk note's embedded images in body order, return first that
     passes the preset's filters (dims, aspect, format)
  5. Save hero as ` + "`<image-dir>/<ID>.jpg`" + `
  6. Emit JSON brief — one per URL, ready to hand to a review agent

Requirements:
  - Joplin Desktop running with Web Clipper enabled
    (Tools → Options → Web Clipper → Enable). Data API on :41184.
  - JOPLIN_TOKEN env var, or a configured Joplin Desktop install.

Examples:
  # Single Amazon URL, auto-detected preset.
  qai scrape https://www.amazon.co.uk/dp/B0BT9R5XNN

  # Explicit preset + custom image dir.
  qai scrape --amazon https://www.amazon.com/dp/B0GR1K7LHC \
             --image-dir ./site/public/product-images

  # Batch 50 products with 3 workers, JSONL output, resumable.
  qai scrape --csv products.csv --parallel 3 -o briefs.jsonl --resume

  # Two-stage: scout a search page, then batch-scrape the results.
  qai scrape --scout "https://www.amazon.co.uk/s?k=threadripper" \
             --max 30 -o threadrippers.csv
  qai scrape --csv threadrippers.csv --parallel 3 -o briefs.jsonl

Output:
  {
    "source_url": "...",
    "preset": "amazon",
    "id": "B0BT9R5XNN",
    "title": "MSI GeForce RTX 5080 ...",
    "image_path": "/product-images/B0BT9R5XNN.jpg",
    "joplin_note_id": "abc...",
    "data": {
      "price_new_gbp": 1149.99,
      "bullets": [...],
      "specs": {...},
      "related_asins": [...]
    }
  }

Adding a preset:
  Register a *scrape.Preset in a package init() — see internal/scrape/amazon.go
  for the reference implementation. Each preset owns its ExtractID,
  Parse, BuildURL, and ImageFilters; the clip→find→hero pipeline is shared.
`
}
