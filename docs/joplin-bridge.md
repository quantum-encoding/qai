# qai joplin bridge + graph — Joplin as agent memory

A Joplin notebook is great for human note-taking; it's not a graph
database an agent can ask "what decisions did we make about
qai-cli?". The `joplin bridge` subsystem syncs your Joplin library
into SurrealDB as a typed graph (notes × notebooks × tags × wiki-
links), and `joplin graph context` is the read verb an agent uses at
session start to hydrate itself with relevant memory.

```bash
# write/maintain side — bridge
qai joplin bridge sync         # one-shot full pull, resumable
qai joplin bridge tail         # long-running event-stream consumer
qai joplin bridge status       # health + lag + backlog
qai joplin bridge schema       # print the embedded schema

# read side — graph
qai joplin graph context [query] [flags]   # the agent-memory verb
```

The bridge writes; the graph verb reads. They live in different
packages so the surfaces evolve independently.

---

## bridge sync — one-shot pull

```bash
qai joplin bridge sync                    # full library
qai joplin bridge sync --notebook qai     # only the qai/ subtree + descendants
```

Resumable via `bridge_state.bootstrap_progress`. Mid-walk crash leaves
the prior notebook's checkpoint intact; re-run picks up there. Each
notebook is one SurrealDB transaction.

On clean completion, captures the current `/events` cursor and writes
it to `bridge_state.cursor` — the precondition for `bridge tail` to
take over. `--notebook X` mode does NOT stamp `last_sync_completed`
because the library as a whole isn't synced.

Re-running a finished sync is a no-op. Every write is an UPSERT keyed
on the Joplin ID, with deterministic edge IDs.

---

## bridge tail — long-running event consumer

```bash
qai joplin bridge tail        # blocks; poll /events forever
qai joplin bridge tail --once # one poll then exit (cron-friendly)
```

Drains Joplin's `/events?cursor=...` stream, applies create/update/
delete events to the graph, advances the cursor in `bridge_state`.
Heartbeats `last_poll_at` every successful poll regardless of whether
new events came back — that's the LIVENESS signal `status`/`graph
context` read to decide freshness.

Notes-only event scope: Joplin's `/events` reports note changes but
not folder/tag changes. Tail materialises new notebooks + new tags
LAZILY when a note carrying them arrives. The nightly cron is the
structural backstop for catching renames that don't move through
`/events`.

---

## bridge status — Health API + ergonomics

```bash
qai joplin bridge status                # human-readable default
qai joplin bridge status --json         # full HealthCheck as JSON
qai joplin bridge status --watch [N]    # redraw every N seconds (default 5s)
qai joplin bridge status --check        # exit code only — cron-friendly
qai joplin bridge status --max-lag 30m  # override the 10m default threshold
qai joplin bridge status --allow-bootstrap  # treat in-flight bootstrap as OK
```

The load-bearing piece is the `Health()` function (the same one
`graph context` calls). It classifies into one of seven states:

| Status | OK? | Meaning |
|--------|-----|---------|
| `healthy` | true | All systems normal. Reason is empty. |
| `degraded` | **true** | Data fresh, but tail stopped. Reason populated; caller MUST surface it. |
| `stale` | false | `now - last_poll_at > MaxLag`. |
| `error` | false | Tail reported a terminal error. |
| `bootstrap` | false | Sync in-flight (unless `--allow-bootstrap`). |
| `no-tail` | false | Synced but tail daemon never started. |
| `never-run` | false | No cursor; bootstrap hasn't been run. |

**The contract:** `Reason == "" iff Status == HealthHealthy`. `OK=true`
with populated `Reason` (the degraded case) is NOT silently servable —
the caller must surface the reason to the user (warning banner /
stderr / log entry).

`--check` exit codes for cron consumers:

| Exit | Meaning |
|------|---------|
| 0 | healthy OR degraded — data is servable right now |
| 1 | stale / error / no-tail / never-run — operator action needed |
| 2 | bootstrap — sync in flight; retry later rather than alert |
| 3 | invocation error (bad flags, Surreal unreachable, etc.) |

---

## graph context — the agent-memory read verb

```bash
qai joplin graph context                       # default: --project resolved from cwd
qai joplin graph context "wasm"                # full-text over title + excerpt
qai joplin graph context --project qai-cli     # notes tagged project:qai-cli
qai joplin graph context --tag decision        # notes tagged 'decision'
qai joplin graph context --tag A --with B      # notes carrying BOTH A and B
```

Returns one JSON object with relevant notes + their graph
neighbourhood. The verb the whole bridge was built to enable — feeds
an agent its memory at session start.

### The freshness gate

Asymmetric with `status --check` (which alerts on stale). This verb
is for an agent consumer that values labelled-degraded over nothing,
so the default behaviour is **serve-stale-with-label** + a `--strict`
opt-in:

| Health.OK | `--strict`? | Behaviour |
|-----------|-------------|-----------|
| true (healthy) | n/a | Serve. `stale: false`, empty `freshness_warning`. |
| true (degraded) | n/a | Serve. `stale: false`, `freshness_warning` populated, stderr line. |
| false | absent | Serve. `stale: true`, warning populated, stderr line. Exit 0. |
| false | present | Refuse. `unhealthy:` stderr. Exit 1 (or 2 for bootstrap). |

The same `Health()` call drives the gate AND populates the payload's
`health` summary — no drift possible.

### Output

```json
{
  "schema_version": 1,
  "query":    { "kind": "fulltext|project|tag", "value": "...", "hops": 1 },
  "stale":    false,
  "freshness_warning": "",
  "health":   { "status": "healthy", "lag_ns": 0, "data_age_ns": 0, "backlog": 0 },
  "notes": [
    {
      "id":       "...",
      "title":    "...",
      "excerpt":  "...",
      "body":     null,
      "notebook": { "id": "...", "title": "...", "path": "qai/sessions" },
      "tags":     [{"title":"qai-cli","kind":"project"}],
      "links_out":[{"id":"...","title":"..."}],
      "links_in": [{"id":"...","title":"..."}],
      "score":    0.0,
      "match":    "primary|neighbour"
    }
  ],
  "counts": { "primary": 12, "neighbours": 4, "total": 16 }
}
```

`match: primary|neighbour` is load-bearing — without it a 1-hop
expansion of 12 hits looks like 40 equally-relevant notes. `score` is
populated only on the full-text path (BM25 sum across title + excerpt
indexes). `notebook.path` is the slash-joined ancestor chain via
`nested_in` (cached across rows so 20 notes in `qai/sessions` walk
the chain once).

### Flags

| Flag | Notes |
|------|-------|
| `--hops N` | Neighbourhood depth (0\|1\|2, default 1). Cold-graph reality: 2 mostly returns notebook siblings, which is noise. |
| `--limit N` | Primary hits cap. Default 20, hard cap 200 (exit 3 on overage). |
| `--include-bodies` | Fetch full bodies from Joplin (off by default; excerpts only). 8-worker pool; per-note failure → `null` body + stderr line. |
| `--max-lag <dur>` | Freshness threshold (default 10m). Same as `bridge status --max-lag`. |
| `--strict` | Refuse on !OK instead of serving-with-label. |
| `--explain` | Print the emitted SurrealQL to stderr and exit 0 without executing. |
| `--json` | Implied — `graph context` is machine-first. |

### Cold-graph reality

At session start there will be **6,800+ notes, 2-3 tags, ~10
`has_tag` edges, and zero `links_to`** (Stage 5 isn't shipped yet).
The richest neighbourhood is `contains` (every note belongs to a
notebook), then full-text on title+excerpt, then `project:` tags
(sparse but high-signal).

That's why `context` is **search-first, graph-second**: match by
query, then attach whatever neighbourhood exists. The graph enriches
the search hits; it's not the primary lookup. `links_to` is queried
no-op-safe — it returns zero rows today and lights up automatically
when Stage 5 populates the edge.

---

## SurrealDB schema

```bash
qai joplin bridge schema | less
```

Idempotent — every `DEFINE` uses `IF NOT EXISTS`, every `UPSERT` is
keyed on the Joplin ID, every edge ID is deterministic
(`<table>:<in>_<out>`). Re-applying is a no-op.

Namespace `quantumencoding`, database `notes_graph` (overridable via
`QAI_SURREAL_NS` / `QAI_SURREAL_DB`).

Full-text indexes (Stage 4) — `note.title` and `note.excerpt` both
carry a `FULLTEXT ANALYZER note_search BM25 HIGHLIGHTS` index. The
analyzer chain is `lowercase, ascii, snowball(english)` so queries
like `wasm` match `WASM`, `Wasm`, and `wäsm`. First sync after Stage
4 builds these in ~4.5s on a 6,800-note library.

---

## Credentials

No compiled-in defaults — pass via env or flags so a misconfigured
target can't silently leak dev creds to prod.

| Var | Notes |
|-----|-------|
| `QAI_SURREAL_USER` | Required |
| `QAI_SURREAL_PASS` | Required |
| `QAI_SURREAL_URL` | Default `http://127.0.0.1:8000` |
| `QAI_SURREAL_NS` | Default `quantumencoding` |
| `QAI_SURREAL_DB` | Default `notes_graph` for bridge/graph verbs |
| `JOPLIN_TOKEN` | Or read from `~/.config/joplin-desktop/settings.json` |
| `JOPLIN_URL` | Default `http://127.0.0.1:41184` |
| `JOPLIN_TIMEOUT` | Default `60s` (was 10s; large clip POSTs need more). |
