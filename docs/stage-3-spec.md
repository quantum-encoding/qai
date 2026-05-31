# Stage 3 — qai joplin bridge status polish + the Health API

You are picking up a multi-stage build. **Stages 0–2 are shipped.**

- Stage 0: `internal/projectid/` — `project:<resolved>` tag resolver.
- Stage 1: `internal/joplinbridge/` — `qai joplin bridge sync`
  (resumable, idempotent bootstrap).
- Stage 2: `qai joplin bridge tail` (foreground daemon, heartbeat-aware)
  + a minimal `qai joplin bridge status` (the observability floor).

**This document is Stage 3 only.** Stage 4 (the agent-memory read verb)
ships separately and will *consume* the Health API this stage produces.
Stage 5 (link parser + `links_to` edges) is independent.

**Read first, before writing a single line of Stage 3 code:**

- `internal/joplinbridge/status.go` — what Stage 2 ships. Stage 3
  *extends* this rather than replacing it; the existing
  `printStatus(w, s, now)` + `readStatus` / `readTagCounts` are the
  testable seams you build on.
- `internal/joplinbridge/tail.go` — to understand which fields are
  written and when (the heartbeat semantics live here).
- `internal/joplinbridge/sync.go` — `stmtCompletion` is the source of
  the carryover bug fixed below.
- `internal/joplinbridge/schema.surql` — the data model.

---

## Mission

Make `qai joplin bridge status` the trust layer Stage 4 depends on.
Three additions, one bug fix, one carryover signal carried into
documentation:

1. **A programmatic Health API** — a Go function Stage 4 calls
   directly (no shell-out, no JSON round-trip) to decide whether to
   serve agent memory or refuse with a freshness violation. This is
   the load-bearing piece; the rest of Stage 3 is polish.
2. **`--json` mode** — stable machine-readable schema for crons,
   dashboards, and the eventual `qai doctor` integration.
3. **`--watch` mode** — operator's live view.
4. **`--check` mode** — pure exit-code health probe for crons /
   monit / launchd / CI.
5. **Bug fix carried from Stage 2 production**: `stmtCompletion`
   stamps `last_event_at = now` on sync completion when it should
   leave the field untouched (sync didn't apply any *event*; it
   bootstrapped notebooks).
6. **Backlog estimation** — soft-failing comparison between
   `bridge_state.cursor` and Joplin's current `/events` head.

---

## THE load-bearing piece: the Health API

Stage 4's read verb will call something shaped roughly like:

```go
h, err := joplinbridge.Health(surrealClient, joplinClient, time.Now(), joplinbridge.Thresholds{
    MaxLag:         600 * time.Second, // tail freshness ceiling
    AllowBootstrap: false,             // bootstrap in flight → unhealthy for reads
})
if err != nil { /* invocation error — surface to user */ }
if !h.OK {
    return fmt.Errorf("graph not fresh enough to serve: %s", h.Reason)
}
if h.Reason != "" {
    // OK==true BUT a Reason is set → contract requires we surface it.
    fmt.Fprintf(os.Stderr, "warning: %s\n", h.Reason)
}
```

This API is the entire reason Stage 3 exists before Stage 4. Get its
shape right and Stage 4 is a thin layer on top of an existing query
language; get it wrong and Stage 4 either over-serves (gives agents
stale answers) or under-serves (refuses every quiet night).

### Required Go shape

```go
package joplinbridge

// DefaultMaxLag is the freshness ceiling for tail's liveness signal
// (now - last_poll_at). Established by Stage 2's spec. CLI flags and
// Stage 4 callers both pick this up as their default.
const DefaultMaxLag = 600 * time.Second

// HealthCheck is the structured result of evaluating bridge_state
// against freshness thresholds. JSON tags are part of the stable
// schema --json emits; rename = breaking change (bump SchemaVersion).
type HealthCheck struct {
    OK            bool         `json:"ok"`
    Status        HealthStatus `json:"status"`           // see consts below
    Reason        string       `json:"reason,omitempty"` // see contract below
    SchemaVersion int          `json:"schema_version"`   // 1 for Stage 3

    // The raw bridge_state row, surfaced for callers that want to
    // inspect specific fields. Pointers preserve null distinction.
    State *StateSnapshot `json:"state,omitempty"`

    // Computed lag (now - last_poll_at) and data age (now -
    // last_event_at). Zero values when the source timestamps are
    // null — callers should check State.LastPollAt before reading.
    Lag     time.Duration `json:"lag_ns"`
    DataAge time.Duration `json:"data_age_ns"`

    // Backlog: how many events behind Joplin's current head. Soft-
    // failed when Joplin is unreachable — see BacklogState.
    Backlog      int64        `json:"backlog,omitempty"`
    BacklogState BacklogState `json:"backlog_state"`

    // Cold-start signals — carried forward as a sustained reminder
    // that tag-rich queries aren't useful yet. See Stage 2 commit.
    TagCount       int `json:"tag_count"`
    HasTagCount    int `json:"has_tag_count"`
    AmbiguousLinks int `json:"ambiguous_links"` // Stage 5 hook; always 0 until then
}

type HealthStatus string

const (
    HealthHealthy   HealthStatus = "healthy"    // OK=true, Reason==""
    HealthDegraded  HealthStatus = "degraded"   // OK=true, Reason populated — see contract
    HealthStale     HealthStatus = "stale"      // lag > MaxLag, OK=false
    HealthError     HealthStatus = "error"      // tail_state == 'error', OK=false
    HealthBootstrap HealthStatus = "bootstrap"  // sync in flight, OK=false (unless AllowBootstrap)
    HealthNoTail    HealthStatus = "no-tail"    // cursor set, last_poll_at null, OK=false
    HealthNeverRun  HealthStatus = "never-run"  // no bridge_state / null cursor, OK=false
)

type BacklogState string

const (
    BacklogOK      BacklogState = "ok"       // Joplin reached, delta computed
    BacklogUnknown BacklogState = "unknown"  // Joplin unreachable; soft-fail
)

type StateSnapshot struct {
    Cursor            *string    `json:"cursor"`
    LastPollAt        *time.Time `json:"last_poll_at"`
    LastEventAt       *time.Time `json:"last_event_at"`
    LastSyncCompleted *time.Time `json:"last_sync_completed"`
    TailState         *string    `json:"tail_state"`
    TailPID           *int       `json:"tail_pid"`
    EventsApplied    *int       `json:"events_applied"`
    LastError         *string    `json:"last_error"`
    BootstrapInFlight bool       `json:"bootstrap_in_flight"`
}

type Thresholds struct {
    MaxLag         time.Duration // healthy iff now - last_poll_at <= MaxLag
    AllowBootstrap bool          // if true, bootstrap-in-flight isn't an automatic NOT-OK
}

// Health is the entrypoint Stage 4 calls. Pure-ish: side effects are
// the SELECT queries against Surreal and the single /events?limit=1
// probe against Joplin for backlog. Passing j==nil reports
// BacklogUnknown without failing.
func Health(s surrealAPI, j joplinAPI, now time.Time, t Thresholds) (HealthCheck, error)
```

### THE CONTRACT — `OK` is necessary but not sufficient

This is the load-bearing semantic for the whole API:

- `Reason == ""` if and only if `Status == HealthHealthy`.
- Any other status carries a populated `Reason`.
- **A caller that only checks `OK` is buggy.** It must also surface
  `Reason` whenever populated, because:
  - `OK == true` AND `Reason == ""` → fully healthy, no caller
    obligation.
  - `OK == true` AND `Reason != ""` → degraded; data is servable
    *now*, but a real condition exists that the caller MUST surface
    to its user (warning, banner, log line). Stage 4 is contractually
    required to do this; refusing to serve is wrong, ignoring the
    Reason is also wrong.
  - `OK == false` → caller refuses to serve and surfaces `Reason`
    as the refusal explanation.

This contract is what resolves the silent-staleness time bomb a naive
"stopped tail with fresh data" classification would otherwise create.
The classifier labels it `HealthDegraded` with `OK=true` and `Reason`
populated; Stage 4 serves with a warning; the operator gets one
chance per read to see "tail daemon stopped" and act before the lag
threshold actually trips.

### Classification rules (exhaustive — first match wins)

| # | Condition | Status | OK | Reason example |
|---|-----------|--------|----|----|
| 1 | `bridge_state` row missing OR `cursor == nil` | `HealthNeverRun` | false | `"no cursor — run 'qai joplin bridge sync' first to bootstrap"` |
| 2 | `tail_state == "error"` | `HealthError` | false | `last_error` value verbatim |
| 3 | `bootstrap_in_flight == true` AND `!t.AllowBootstrap` | `HealthBootstrap` | false | `"bootstrap in flight — re-run 'qai joplin bridge sync' to resume"` |
| 4 | `cursor != nil` AND `last_poll_at == nil` | `HealthNoTail` | false | `"synced but tail daemon never started — run 'qai joplin bridge tail'"` |
| 5 | `now - last_poll_at > t.MaxLag` | `HealthStale` | false | `fmt.Sprintf("liveness lag %s exceeds %s", lag, maxLag)` |
| 6 | `tail_state == "stopped"` AND lag ≤ MaxLag | `HealthDegraded` | **true** | `"tail daemon stopped — data fresh now but staleness is climbing"` |
| 7 | Otherwise | `HealthHealthy` | true | `""` |

**Rules 4 and 6 are the two corrections from spec review** — read
them carefully:

- **Rule 4 (`HealthNoTail`)** distinguishes "sync ran, tail not
  started" from "nothing has ever run" (Rule 1). The user shouldn't
  be told to re-bootstrap a graph that's sitting fully populated;
  they should be told to start the daemon. This is the state the
  system was in between the Stage 1 and Stage 2 live runs.
- **Rule 6 (`HealthDegraded`)** is `OK=true` *with a populated
  Reason*. The contract above is what makes this safe — Stage 4
  serves but the warning reaches the user.

### Thresholds default

```go
const DefaultMaxLag = 600 * time.Second // 10 minutes
```

Stage 2's spec established this as the freshness floor; carry it
forward as an exported constant so CLI flags and Stage 4 can both
pick it up.

---

## CLI surface

`status` keeps its current default (human-readable) and gains:

```
qai joplin bridge status                Human-readable (Stage 2 baseline + backlog row)
qai joplin bridge status --json         Single JSON object: the HealthCheck struct
qai joplin bridge status --watch [Ns]   Refresh in place every N seconds (default 5s)
qai joplin bridge status --check        Exit code only; one-line stderr on unhealthy

Modifiers:
  --max-lag <dur>      Override the freshness threshold (default 10m).
  --allow-bootstrap    Treat bootstrap-in-flight as OK (operator mode).
```

### Default human output

Identical to Stage 2 plus the backlog row:

```
backlog:             3 events behind (Joplin at cursor 8661)   ← BacklogOK
backlog:             unknown (Joplin unreachable)              ← BacklogUnknown
backlog:             0 events behind (caught up)               ← BacklogOK, delta 0
```

When the status is `HealthDegraded` (Rule 6), add a final line:

```
warning:             tail daemon stopped — data fresh now but staleness is climbing
```

This is the human form of the contract obligation: the warning is
visible to anyone reading the default output, not just programmatic
consumers.

### `--json` discipline

Single JSON object per invocation. Schema tracked via
`schema_version` (start at 1; bump on any field rename or removal).
Pretty-printed to stdout, two-space indent (matches the existing
`emitJSON` helper style in `internal/joplinops/joplinops.go`).

Optional fields use `omitempty` so consumers can rely on field
presence:

```json
{
  "ok": true,
  "status": "degraded",
  "reason": "tail daemon stopped — data fresh now but staleness is climbing",
  "schema_version": 1,
  "state": {
    "cursor": "8658",
    "last_poll_at": "2026-05-31T21:00:53.436Z",
    "last_event_at": "2026-05-31T21:00:39.573Z",
    "last_sync_completed": "2026-05-31T20:41:28.830Z",
    "tail_state": "stopped",
    "tail_pid": 31035,
    "events_applied": 11,
    "last_error": null,
    "bootstrap_in_flight": false
  },
  "lag_ns": 14000000000,
  "data_age_ns": 28000000000,
  "backlog": 3,
  "backlog_state": "ok",
  "tag_count": 3,
  "has_tag_count": 10,
  "ambiguous_links": 0
}
```

### `--watch`

Clear screen + redraw in place every N seconds (default 5s). Use
`fmt.Print("\033[2J\033[H")` for the clear; don't add a curses
dependency. Trap SIGINT for clean exit (don't leave the terminal in
a weird state). Display the wall-clock timestamp at the top so the
operator can see the refresh happen even when nothing changes:

```
qai joplin bridge status — 2026-05-31T22:13:04Z
─────────────────────────────────────────────────
state:               running   (pid 31203)
last_poll_at:        ...
...
```

Append-mode (`--watch --append`) is **not** shipped in Stage 3. Add
it later if scrollback-history users actually miss it.

### `--check`

Pure exit-code health probe. Prints **nothing** on healthy or
degraded; one line to stderr on the other branches. Exit codes:

| Code | When |
|------|------|
| 0 | `HealthHealthy` OR `HealthDegraded` — data is servable right now |
| 1 | `HealthStale` / `HealthError` / `HealthNoTail` / `HealthNeverRun` — not servable, operator action needed |
| 2 | `HealthBootstrap` — sync in flight; caller may want to retry later rather than alert |
| 3 | Invocation error (bad flags, Surreal unreachable, etc.) |

**`HealthDegraded` exits 0.** This is the decision the contract drives:
`--check`'s job is "is the data servable right now," and it is. The
fact that staleness is climbing reaches the user via the Reason in
`--json` / default output, not via the exit code. Don't double-encode
the same decision.

Example stderr line on unhealthy:

```
unhealthy: stale — liveness lag 23m exceeds 10m
unhealthy: no-tail — synced but tail daemon never started — run 'qai joplin bridge tail'
unhealthy: error — joplin GET: HTTP 400: cursor invalid
```

The format is `unhealthy: <status> — <reason>`. One line, ASCII em
dash, machine-grepable.

---

## Bug fix — `last_event_at` smearing on sync completion

`internal/joplinbridge/statements.go` currently has:

```go
func stmtCompletion(cursor string, now time.Time) string {
    return fmt.Sprintf(
        "UPSERT %s SET cursor = %s, last_event_at = %s, last_sync_completed = %s, ...",
        bridgeStateID, quote(cursor),
        fmt.Sprintf("<datetime>%q", now.UTC()...),  // ← writes "now" to last_event_at
        fmt.Sprintf("<datetime>%q", now.UTC()...),
    )
}
```

The `last_event_at = <now>` is wrong: sync didn't apply any events;
it bootstrapped the graph. Tail's per-event apply is the only legit
source of `last_event_at`. The current behaviour makes a fresh-sync
status read "data age = 17m" when there's been no event activity at
all — confusing operator UX, and would mislead a Stage 4 caller that
reads data age. Stage 4 *shouldn't* read data age (it reads lag),
but defence in depth says fix the source.

**Fix:** drop `last_event_at = …` from both `stmtCompletion` and
`stmtCompletionScoped`. Leave the field untouched on sync — tail
will populate it on the first event apply. The schema is
`option<datetime>` so null is fine.

Stage 1's `TestSyncBasicWriteThrough` asserts the completion writes
`last_sync_completed` — that assertion is unaffected. Add a sibling
assertion to confirm `last_event_at` is **not** part of the
completion statement.

---

## Decisions made during spec review

These started as open questions; recording the resolutions so any
future reader can audit the chain without re-litigating.

1. **Backlog estimation cost** — done on default output and `--json`,
   skipped on `--check`. Rationale: `--check`'s contract is "pure
   Surreal-cost probe"; coupling it to Joplin liveness would defeat
   the cron use case.
2. **`--watch` redraw vs append** — redraw only. Append variant
   deferred until someone actually misses it.
3. **Cold-start `tags / has_tag` row** — kept in default output.
   Two numbers cost nothing and the reminder is the whole point.
4. **`HealthDegraded` exit code in `--check`** — exit 0 (servable
   right now). The staleness-climbing fact propagates via the
   Reason, not the exit code. Same decision as Rule 6's `OK=true`;
   recorded once, applied in both places (CLI exit table + Go API
   classification).
5. **`HealthNoTail` distinct from `HealthNeverRun`** — yes,
   separate. They guide the user to different actions ("run tail"
   vs "run sync"); collapsing them would tell users to
   re-bootstrap a populated graph.

---

## Out of scope (do not build)

- Stage 4's read verb (`graph context`) — Stage 4. Stage 3 produces
  the Health API it'll consume.
- Stage 5 — `ambiguous_links` is exported as 0 always until Stage 5
  wires `links_to` rows; don't compute it speculatively.
- Multi-daemon coordination — the `tail_pid` row is informational
  only; status doesn't enforce single-instance.
- TUI / curses / interactive widgets — `--watch` is plain redraw.
- Modifying the daemon — status is read-only. Don't add a
  "restart tail" verb or similar.
- Backlog estimation that walks Joplin's full `/events` history —
  just `limit=1` to read the current head cursor is enough.
- Append-mode `--watch`.

---

## Acceptance

Done when:

1. `Health(s, j, now, thresholds)` returns a `HealthCheck` matching
   the classification rules above, with `SchemaVersion = 1`. All 7
   `HealthStatus` constants exercised by table tests against a fake
   `surrealAPI` + `joplinAPI`.
2. **The contract is testable**: a unit test asserts that
   `Reason == ""` iff `Status == HealthHealthy`. (Iterate the
   status constants; the empty-Reason case is exactly one.)
3. `status` default output matches Stage 2 plus the backlog row,
   plus the `warning:` line when `Status == HealthDegraded`.
4. `status --json` emits the `HealthCheck` struct with stable field
   names. A schema-stability test asserts the JSON shape via a
   golden string (any field rename = test fail). Include one golden
   per status branch the JSON could carry.
5. `status --watch` clears + redraws every N seconds, exits cleanly
   on SIGINT. Tests assert the inner loop produces correct output
   per tick (don't test the terminal redraw itself; that's an
   integration concern).
6. `status --check` is silent on `HealthHealthy` and
   `HealthDegraded` (exit 0), prints one stderr line and exits
   1/2/3 on the documented branches. Table tests cover all four
   codes.
7. `--max-lag <dur>` parses Go durations (`time.ParseDuration`) and
   is plumbed through both human and `--check` modes.
8. `--allow-bootstrap` flips bootstrap to OK=true; verified by a
   test against a fixture with `bootstrap_progress` set.
9. Backlog estimation: with a fake Joplin returning a cursor head N
   positions ahead, `BacklogState = ok` and `Backlog = N`. With
   Joplin unreachable, `BacklogState = unknown` and the human
   output renders the soft-fail message; `--check` doesn't fail on
   unknown backlog alone.
10. **Bug fix:** `stmtCompletion` no longer writes `last_event_at`.
    Stage 1's `TestSyncBasicWriteThrough` still passes; a new
    assertion confirms the absence.
11. `--json` schema is documented in `helpStatus` text so a fresh
    operator can grep for field meanings.
12. `go build ./...`, `go vet ./...`,
    `go test ./internal/joplinbridge/...` all green; existing 20
    tests still pass; new tests added per criteria 1, 2, 4, 6, 9.
13. `TestNoEdgeUpsertSyntax` still passes (the Stage 2 grep test —
    no `UPSERT contains/has_tag/nested_in` regressions).

---

## Style match

- Reuse Stage 1/2 fakes (`fakeJoplin`, `tailSurreal`); extend them
  rather than introducing a third generation.
- `printStatus(w, s, now)` already exists as the testable seam.
  `Health(s, j, now, t)` is its sibling — pure-ish, takes
  interfaces, returns a struct. The CLI binds them together; tests
  drive them directly.
- No new dependencies. ANSI escape codes for `--watch` are inline
  string constants, not a library.
- All `bridge_state` reads go through one shared helper (extend
  `readStatus` to return a `*StateSnapshot` with `time.Time` fields
  parsed once, rather than parsing in three places — the existing
  `formatLag` already parses; promote that to the read layer).
- `--check` exit codes are constants at the top of the file:
  `exitHealthy = 0`, `exitUnhealthy = 1`, `exitBootstrap = 2`,
  `exitInvocation = 3`. The CLI maps `HealthStatus → exit code` via
  a small switch.
- Tests cover branches, not implementation. The Health classifier
  is the load-bearing piece — exhaustive table test with one row
  per `HealthStatus` constant.

---

## Carryover signals (information, not requirements)

Two known soft truths from Stage 2 production that Stage 3 should
preserve in the output rather than try to fix:

1. **`events_applied` undercount of ~50** from the first tail run
   (heartbeat write failed pre-schema-apply, the increment didn't
   land). The counter is correct going forward; backfilling
   historical events is out of scope. Don't try to detect or
   correct — show the current value verbatim.
2. **Tag graph is cold** (3 tags, 10 has_tag edges as of Stage 2's
   live-fire). Stage 3 keeps the `tags / has_tag` row to remind any
   Stage 4 designer reading the output that tag-based queries
   won't surface much until vocabulary accumulates via
   `qai note --tag` and manual tagging. The Health API does NOT
   factor tag count into OK-ness (a freshly-installed bridge with
   0 tags is healthy if tail is alive).

---

## Scale

Two distinct pieces of work: the Health Go API (load-bearing,
~150 LOC + tests) and the CLI polish (`--watch` / `--json` /
`--check` / backlog, ~250 LOC + tests). The bug fix is ~10 LOC.
Total likely ~500 LOC of code + tests, smaller than Stages 1 or 2.

Build order recommendation:

1. Bug fix (10 LOC, drops `last_event_at` from completion).
2. `StateSnapshot` extraction from the existing `readStatus`.
3. `Health` classifier — pure function over `StateSnapshot` +
   `Thresholds` + computed lag/backlog. This is the load-bearing
   piece; get the table test passing before any CLI work.
4. CLI: `--json` (trivially exercises the API once `Health` works).
5. CLI: `--check` (exit-code switch on `HealthStatus`).
6. CLI: backlog row + the `warning:` line for `HealthDegraded`.
7. CLI: `--watch` (loop + ANSI clear).
8. Documentation pass on `helpStatus`.
