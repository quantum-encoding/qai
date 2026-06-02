# qai browser — CDP control of your real browser

Drives Chrome/Brave/Edge via the DevTools Protocol debug port. No
headless browser, no Playwright, no Node.js — every command attaches
to the browser you already have open, so your cookies, auth, sessions,
fingerprints, and extensions are all live.

```bash
qai browser launch            # auto-detect Brave/Chrome/Edge, start with debug port
qai browser list              # show open tabs + slot pinning
```

The rest of this doc walks each subcommand, the security perimeter,
and the multi-tab management surface.

---

## Tab management

You can hold several work streams across tabs without disturbing
whatever the human is doing in the foreground. Tabs are addressable
by 32-char hex ID (`qai browser list` shows them) OR by named slot.

```bash
qai browser new [url] [--slot <name>]   # spawn a fresh tab in the background
qai browser close <slot-or-id>           # close by slot or hex (prefix ok)
qai browser tab <id>                     # activate a tab (bring to foreground)
```

### Slots

Slot names are arbitrary strings (`TAB1`, `main`, `cj-research`,
`agent2`) that can't collide with a 32-char hex tab ID. Pins persist
in `~/.qai/browser-slots.json`. Stale entries (tabs the human closed
manually) are pruned automatically on every slot-aware command.

```bash
qai browser new https://cjdropshipping.com --slot cj-main
qai browser new --slot agent1                                 # blank tab
qai browser new --slot agent2

# every command that takes --tab accepts slot names
qai browser open https://other.com --tab agent1
qai browser screenshot --selector header --tab main
qai browser clip "research/clips" "page snapshot" --tab cj-main
```

`qai browser list` grew a slot column + summary footer:

```
71BD79205800 TAB1     Example Domain      https://example.com/
4E285C89112B cj-main  CJdropshipping      https://www.cjdropshipping.com/
9F3694EF4BFA -        Sale — Lutuno       https://lutuno.com/sale/
slots: TAB1=71BD7920  cj-main=4E285C89
```

### Why slots matter for parallel work

Multiple tabs share the browser session — cookies, localStorage, auth
are common across tabs. That's perfect for running parallel agents on
the same site without re-logging-in, or for keeping one stable "warm"
tab while a batch hammers another.

```bash
# parallel cj batches against the same login, isolated tab state
qai cj batch urls-A.csv --tab agent1 &
qai cj batch urls-B.csv --tab agent2 &
wait
```

For full session isolation (separate cookies), you'd need
`Target.createBrowserContext`, which qai doesn't yet expose — slots
are tab-level, not context-level.

---

## Navigation + page interaction

```bash
qai browser open <url>                     # navigate the targeted tab
qai browser extract [--html]               # get page text (or HTML)
qai browser screenshot [-o file.png]       # capture PNG
qai browser source                         # full document.outerHTML
qai browser pdf [-o file.pdf]              # print to PDF
qai browser click <selector>               # CSS-selector click
qai browser click <x> <y>                  # coordinate click
qai browser type "text"                    # keyboard input
qai browser eval "js expression"           # evaluate (gated; see below)
qai browser wait <selector> [timeout]      # poll until element appears
```

### `--selector`, `--wait`, `--theme`

Available on `open`, `screenshot`, `emulate`:

| Flag | What it does |
|------|--------------|
| `--selector <css>` | Element-scoped capture via `Page.captureScreenshot` clip from the box model. Smaller, sharper PNGs than full-viewport. |
| `--wait <css>` | Block until the element appears (or `--timeout` elapses) before capturing. Replaces sprinkled `sleep()`. |
| `--theme light\|dark` | Force `prefers-color-scheme` via `Emulation.setEmulatedMedia`. Applied PRE-navigate so first-paint sees the right media query. |
| `--timeout <dur>` | Cap for `--wait`. Accepts `5`, `5s`, `1m`. Default 10s. |

Theme determinism: after `setEmulatedMedia`, a `Runtime.evaluate` of
`matchMedia(...)` forces a renderer round-trip — the override is
guaranteed to be applied before any subsequent navigation. Without
this barrier, browser-process ACK ≠ renderer-process applied, and the
new document can render against the OS default scheme.

### Mobile emulation

```bash
qai browser emulate list                                # available devices
qai browser emulate iphone15 https://x.com -o ios.png
qai browser emulate pixel7   https://x.com --theme dark
qai browser emulate galaxy-s23                          # current tab
```

Applies CDP device-mode overrides (viewport, DPR, touch, user-agent)
on a single CDP connection so the screenshot captures the emulated
render. Blink rendering a mobile viewport — not WebKit. Reproduces
layout, viewport sizing, touch events, and UA-sniffing branches, but
NOT genuine iOS Safari quirks.

---

## Clip to Joplin

```bash
qai browser clip [notebook] [title]   # clip current tab to Joplin
```

Reads the rendered DOM via CDP (so all React/JS-driven content is
captured), POSTs `body_html` to Joplin Desktop's `/notes` endpoint —
the same endpoint the Joplin Web Clipper extension uses. Joplin's
converter does the HTML→markdown + image-download pass. Output is a
Joplin note bit-compatible with what the extension would produce.

Stdout is the new note's ID alone, so you can pipe:

```bash
NOTE=$(qai browser clip "research/clips" "page snapshot")
qai joplin note "$NOTE" --full > saved.md
qai cj extract --joplin "$NOTE" --strict | jq '.cj_products | length'
```

Default Joplin client timeout is 60s (was 10s); a 1.3 MB body_html
with 60 image fetches routinely takes 30s+ for Joplin to process.
Override with `JOPLIN_TIMEOUT` env var.

---

## Batch scrape

```bash
qai browser scrape urls.csv                       # extract text from each URL
qai browser scrape urls.csv --screenshot          # screenshot each
qai browser scrape urls.csv --html                # HTML extraction
qai browser scrape urls.csv --delay 2000          # 2s between pages
qai browser scrape urls.csv -o results/           # output directory
qai browser scrape urls.csv --json                # write manifest.json
```

CSV format: column 1 = URL, optional column 2 = label. Header row
auto-detected. Pre-flight security check walks every URL through
deny/sensitive before the CDP connection opens, so a poisoned CSV
fails at row 0, not row 50.

---

## Security perimeter

Layered defence against prompt-injection attacks that try to drive an
authenticated browser session against the user's interests:

| Layer | Protection | Example |
|-------|-----------|---------|
| **Pattern block** | Hard-deny dangerous JS before it reaches the browser. Eval-only. No override. | `document.cookie`, `localStorage`, `fetch(`, `eval(`, `XMLHttpRequest`, `sendBeacon`, `chrome.runtime`, `ServiceWorker` |
| **Hard deny** | Refuse the action outright — no TTY prompt, no `--yes` override. | Builtin: `chrome://`, `chrome-extension://`, `edge://`, `about:`, `devtools://`, `file://`, `view-source:`. Plus user `denied_domains` list. |
| **Domain sensitivity** | Flag sensitive domains for confirmation. | ~30 builtins: AWS/GCP/Azure consoles, `*.github.com`, banks, `*.stripe.com`, `*.1password.com`, `*.okta.com`, `mail.google.com` |
| **TTY confirmation** | Require human `[y/N]` approval on sensitive domains; non-interactive runs are denied unless `--yes` is passed. | Piped/automated input without `--yes` is denied by default |
| **List redaction** | `qai browser list` hides URL+title of denied/sensitive tabs unless `--yes` is passed. | Stops an agent enumerating "what banking site is the user on" |
| **Batch pre-flight** | `qai browser scrape <csv>` walks every URL through deny/sensitive checks **before** the CDP connection opens. | A poisoned CSV with `https://vault.internal.mycompany.com/...` at row 50 fails at row 0 |
| **Audit log** | JSONL trail of every command at `~/.qai/browser-audit.log`. | Logged regardless of allow/deny, with reason code |

Gated actions: `open`, `extract`, `screenshot`, `click`, `type`,
`eval`, `wait`, `source`, `pdf`, `clip`, `tab`, `scrape`, `new`. `list`
always runs (with redaction). `launch` is local-only and not gated.

The `--yes` flag is parsed as a real flag (not a substring of
`os.Args`), so a quoted prompt that happens to contain the literal
characters `--yes` cannot bypass confirmation.

User-configurable via `~/.qai/browser-policy.yaml` (template ships at
`internal/browser/browser-policy.example.yaml`):

```yaml
# Hard deny — no TTY prompt, no --yes override.
denied_domains:
  - "vault.internal.mycompany.com"
  - "admin.mycompany.com"

# Sensitive — TTY confirm or --yes.
sensitive_domains:
  - "*.internal.mycompany.com"
  - "grafana.mycompany.com"

# Bypass sensitivity (does NOT bypass denied_domains).
trusted_domains:
  - "localhost"

# Extra eval deny patterns (regex, case-insensitive).
blocked_patterns:
  - "internalAPI\\.secret"

# When true, everything not trusted is sensitive.
strict_mode: false
```

Stealth injection removes `navigator.webdriver` and spoofs browser
fingerprints (plugins, WebGL, permissions API) to avoid bot detection
on legitimate automation tasks. Skipped on `emulate` (it would clobber
the mobile fingerprint).

---

## Global flags

| Flag | Notes |
|------|-------|
| `--port <n>` | CDP port. Default 9222, or `QAI_BROWSER_PORT` env var. |
| `--tab <id\|slot\|prefix>` | Target a specific tab. Accepts a 32-char hex ID, a unique prefix, or a pinned slot name. |
| `--slot <name>` | On `new`: pin the created tab to this slot. |
| `--json` | Machine-readable JSON output (where supported). |
| `--yes`, `-y` | Skip the TTY confirmation for sensitive domains. Must be a real flag, not a substring. |
