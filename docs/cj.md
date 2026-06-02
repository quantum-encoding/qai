# qai cj — CJ Dropshipping research workflow

CJ's intelligence dashboard + catalog search are React SPAs behind a
bot wall. Neither `qai scrape` nor their Open Platform API can surface
the demand data the dashboard shows. So discovery happens via the
human web clipper; `qai cj` codifies the extraction so no future
agent session has to re-derive the regex from scratch.

```bash
qai cj extract <markdown-file>          # parse a clipped CJ page → JSON
qai cj extract -                        # same, from stdin
qai cj extract --joplin <id-or-title>   # fetch + parse a Joplin note
qai cj batch <urls.csv>                 # full chain × N URLs
```

---

## The workflow

```
human clips a CJ page             →   Joplin Web Clipper
qai parses the clipped markdown   →   qai cj extract (this verb)
agent boards results downstream   →   project-specific SQL / SurrealDB
```

Two CJ page shapes the parser handles, same `CJProduct[]` output:

1. **Intelligence dashboard** (`/intelligence/sales-trends/<id>`) —
   has "CJ Recommended Top N" boardable products + a competitor sales
   table (Amazon listings with sales $, reviews, listing-days).
2. **Catalog search** (`/search/<query>.html`) — wider discovery
   surface; one wrapper per product card with title + price + list
   count + pid.

Hybrid clips dedupe by PID; the intelligence-page row wins (its
`listed_count` is more meaningful).

---

## Extract — single page

```bash
qai cj extract clip.md                       # JSON to stdout
qai cj extract clip.md --summary             # human skim
qai cj extract clip.md --strict              # exit 3 if 0/0
qai cj extract -                             # markdown from stdin
qai cj extract --joplin <32-hex-id>          # fetch by exact note ID
qai cj extract --joplin "Sales Trends"       # find latest note by title
```

`--strict` catches the "clip didn't land on a CJ page" failure mode
that's easy to miss in a pipeline. Without it, a wrongly-clipped page
silently succeeds with an empty payload.

`source_file` in the output is stamped with the actual source: `-`
for stdin, `joplin:<resolved-id>` for either Joplin path, the
filesystem path otherwise. Title-based Joplin lookup resolves to the
freshest matching note (Joplin sorts search by `user_updated_time`
desc).

### Output

```json
{
  "source_file": "joplin:8f8396ba2bd7402e9c0a4c0199b0ec9a",
  "category":    "Pet Supplies",
  "category_id": "2619533011",
  "cj_products": [
    {
      "pid":          "1871839683610435586",
      "url":          "https://www.cjdropshipping.com/product/.../-p-1871...html",
      "title":        "High Quality Dog Bed Sofa Mats...",
      "listed_count": 494,
      "price_min":    11.02,
      "price_max":    33.53,
      "price_raw":    "$11.02-33.53"
    }
  ],
  "competitors": [
    {
      "asin":             "B0GT6P8ZJP",
      "url":              "https://www.cjdropshipping.com/...",
      "title":            "SmartBones Chicken-Wrapped Sticks...",
      "brand":            "SmartBones",
      "price":            "$13.98",
      "lqs":              85,
      "sales_volume":     "9.8K",
      "sales_volume_num": 9800,
      "sales_usd":        "$137.6K",
      "sales_usd_num":    137600,
      "rating":           4.7,
      "reviews":          "4.4K",
      "reviews_num":      4400,
      "listing_days":     48
    }
  ],
  "counts": { "cj_products": 5, "competitors": 10 }
}
```

Numeric `*_num` fields parse the `$137.6K` / `4.4K` / `1.4M`
notation so consumers can `sort_by` / `select(.x > N)` without
re-parsing the display strings.

### Three edge cases the parser survives

Locked by tests:

1. **Literal `|` inside product titles** (e.g. "Flea Treatment |
   Ferret 1+ lbs. | 2-Month Supply"). A naive markdown-table-split
   shifts every subsequent column → wrong brand, wrong sales, wrong
   everything. The splitter walks the line tracking `[]` and `()`
   bracket depth and only splits at depth-0 pipes.
2. **Non-breaking space** (U+00A0) between the number and "Days" in
   the listing-days cell. Go's RE2 `\s` is ASCII-only, so the regex
   explicitly includes `\x{00a0}`.
3. **`[Details](url)` button** as the trailing anchor on every
   competitor row. Picking the last anchor blindly returns "Details"
   as the title. The parser skips `Details` + image anchors, then
   takes the longest remaining string.

### Useful jq one-liners

```bash
# top 5 boardable products by listed_count (proxy for "other CJ sellers chose this")
qai cj extract clip.md \
  | jq '.cj_products | sort_by(.listed_count) | reverse | .[0:5]'

# sub-$1 products only
qai cj extract clip.md \
  | jq '.cj_products | map(select(.price_min < 1))'

# freshly proven competitor winners: high sales + low listing-days
qai cj extract clip.md \
  | jq '.competitors | map(select(.sales_usd_num > 50000 and .listing_days < 365))'

# top 3 competitor brands by sales
qai cj extract clip.md \
  | jq '.competitors | sort_by(.sales_usd_num) | reverse | .[0:3] | map({brand, sales_usd, listing_days})'
```

---

## Batch — many URLs at once

```bash
qai cj batch urls.csv                     # navigate → wait → clip → extract per row
qai cj batch urls.csv --tab cj-research --notebook "research/clips"
qai cj batch urls.csv -o results.json --summary
qai cj batch urls.csv --strict | jq '.cj_products | length'
```

CSV format mirrors `qai browser scrape`: column 1 = URL, optional
column 2 = label (becomes the Joplin note title). Header row
auto-skipped.

The chain per URL:
1. `qai browser open <url> --wait <sel> --timeout <dur> --tab <id>`
2. `qai browser clip <notebook> "<title>" --tab <id>` → captures note ID
3. Joplin `GET /notes/<id>?fields=body`
4. `cj.Parse(body)` → `CJProduct[]` + counts

Pinned to ONE browser tab for the whole batch so multiple-tab users
don't race. Output is one JSON document with per-URL `runs[]`
(traceability: url, label, note_id, counts, error) + a PID-deduped
`cj_products[]` union across every URL.

### Flags

| Flag | Notes |
|------|-------|
| `--notebook <path>` | Joplin notebook for clips. Default `dropship-accelerator/clips`. |
| `--wait <css>` | Hydration selector. Default `a[href*="/product/"]` — only appears once the React grid renders, not a bare `img` (which matches the page-header logo). |
| `--timeout <dur>` | Per-page wait cap. Default 30s. |
| `--delay <ms>` | Pause between pages. Default 0; recommend higher for anti-bot. |
| `--tab <id\|slot>` | Pin to one browser tab. Default: first page tab. |
| `--soft-wait` | A wait timeout doesn't abort; clip whatever's on the page. |
| `--max-retries N` | Retry navigate on a CJ-throttle timeout up to N times. Default 3. |
| `--retry-base <dur>` | Initial backoff. Default 30s. Schedule is exponential ± 25% jitter, floored at base/2, capped at 5min. |
| `--strict` | Exit 3 if any URL failed OR total products = 0. |
| `-o <file>` | Write JSON to file (default stdout; `-` explicit). |
| `--summary` | Human view after JSON. |

### Retry policy

Only the CJ throttle signature triggers retry — stderr containing
both `"timeout after"` and `"waiting for selector"`. Real errors
(security gate, network, bad URL) fail immediately because backoff
won't help them and retrying would mask bugs.

Backoff schedule (default base 30s, 3 retries):

| Attempt | Delay before retry |
|---------|--------------------|
| 1 | ~30s (jittered) |
| 2 | ~1m |
| 3 | ~2m |

`base * 2^attempt + uniform_jitter(-base/4, +base/4)`, floored at
`base/2`, capped at 5 minutes per retry.

---

## Anti-bot reality

CJ rate-limits programmatic navigation. After ~5–10 quick
`qai browser open` calls to `/search` or `/intelligence` pages, the
React grid stays in skeleton state **indefinitely** for the session
— the page loads, imgs download, but product anchors never
materialize. Symptoms:

- `.skeleton-card-wrap` count stays at 60
- 0 elements match `a[href*="/product/"]`
- `--wait` times out cleanly

The retry helps on transient throttles (a burst that triggers anti-
bot for a few minutes). It cannot break a session-wide rate limit —
that needs a fresh tab/login or CJ-side reset.

Workarounds (increasing intervention):

1. Pace yourself — `--delay 5000` or higher.
2. Run small batches (5–10 URLs), wait minutes between.
3. Manually navigate one CJ page in your browser to warm the session
   before running a programmatic batch.
4. For high-volume discovery, use CJ's Open Platform API directly
   (separate auth surface, separate rate limits).

`--soft-wait` lets you clip the skeleton state anyway when you'd
rather inspect what came back than fail-and-skip.

---

## Joplin client timeout

Bumped from 10s to 60s default because clip-style POSTs of multi-MB
`body_html` regularly take 30s+ for Joplin Desktop to convert HTML
→ markdown + download every referenced image. Override with
`JOPLIN_TIMEOUT=30s` (Go duration syntax) if you want fail-fast on
other paths.
