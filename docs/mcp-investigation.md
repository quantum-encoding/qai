# MCP Investigation — Why `qai fleet mcp` Is Not Loaded By Default

A record of what we built, what we tested, and why the conductor MCP
server ships in the codebase but is **not** wired into Claude Code's
`mcpServers` config.

The short version: empirically, the protocol primitives the conductor
needs to deliver value over the existing tmux notifier daemon are not
implemented by the leading MCP client. The conductor remains in-tree
because the investigation taught us things worth keeping.

## What we built

`qai fleet mcp` (in `internal/fleet/mcp.go`) is a JSON-RPC 2.0 stdio
MCP server. It is architect-only — when launched from a worker pane
it exits at startup so that workers do not pay any MCP tax. When
launched from the architect's pane (matched against the active
fleet's recorded `architect-pane` file) it:

- Watches `~/.qai/fleet/<id>/inbox.jsonl` via fsnotify.
- Exposes two tools: `inbox_list` and `inbox_unread` (typed Report
  objects, structured JSONL content).
- Exposes one resource: `qai://fleet/<id>/inbox` (subscribable).
- On every inbox file change, attempts to push a wake-up notification
  to the client.

The intent was to give the architect (a Claude Code session in a tmux
pane) a typed-tool-result read path AND a server-push wake-up channel —
the one thing the existing tmux nudge can't structurally provide.

## What we tested

A 5-pane test fleet. Each worker read a Rust project, wrote
`SUMMARY.md`, committed if applicable, and posted a `done` report via
`qai report`. The architect (a fresh Claude Code session in tmux pane
%0) had `qai-conductor` configured under `mcpServers` in `~/.claude.json`
with `QAI_CONDUCTOR_DEBUG=1` so every byte going over the wire was
traced to `/tmp/qai-conductor.log`.

We tested four notification paths:

| Path | What it does | Result |
|---|---|---|
| A | `notifications/resources/updated` (subscribed) | Never fired — Claude Code never subscribed |
| B | `notifications/resources/updated` (unconditional) | Sent over the wire; Claude Code silently dropped |
| C | `notifications/tools/list_changed` | Claude Code re-fetched `tools/list` after each fire |
| D | `notifications/message` (logging) | Sent over the wire; Claude Code silently dropped |

## What we found

### Claude Code 2.1.121 does not advertise resource handling

The smoking-gun line from the handshake trace:

```
recv method=initialize id=0 params={"protocolVersion":"2025-11-25",
  "capabilities":{"roots":{},"elicitation":{}},
  "clientInfo":{"name":"claude-code","version":"2.1.121",...}}
```

`clientInfo.capabilities` is `{roots, elicitation}`. **There is no
`resources` key.** Per MCP spec semantics, clients only call
`resources/subscribe` when they advertise resource support. So the
protocol's headline feature — server-push notifications via
`notifications/resources/updated` — was never going to fire. The
server can advertise `resources.subscribe: true` all it wants; if the
client doesn't advertise the matching capability, nothing subscribes.

### Path C works, but Claude Code dedupes

`notifications/tools/list_changed` did trigger a refresh. The trace
shows Claude Code calling `tools/list` ~1ms after each fire. **On the
first event of the session**, when the conductor transitions to
architect mode and the inbox tools become available for the first
time, Claude Code surfaces a `<system-reminder>` to the agent
announcing the deferred tools.

After that, Claude Code dedupes. Subsequent `tools/list_changed`
notifications cause the client to re-fetch but the same tools being
"still available" doesn't re-fire any agent-visible signal. So path
C covered the first wake-up and went silent for every report after.

### Paths B and D were silently dropped

No follow-up RPC after either. Claude Code processed the messages and
either ignored them or applied them somewhere outside the agent's
context. From the agent's perspective, neither path produces an
observable signal.

## Why the conductor doesn't ship loaded

Putting it in a table:

| What conductor MCP gives us | Already covered by |
|---|---|
| First-contact wake-up | Tmux `[FLEET]` nudge fires on the same event |
| Typed read path | `qai fleet inbox --unread --json` returns the same data |
| No auto-classifier hazard | Solved by "you have permission" phrasing in the nudge |
| No tmux-quirk hazard | Solved by paste-buffer + double-Enter in `terminal.Send` |

There is no row in that table where MCP earns its keep. The notifier
daemon already fires on every event, debounces bursts cleanly, costs
no protocol overhead, and works because the architect is a tmux pane.

## Why the code stays

The conductor remains in-tree for three reasons:

1. **The code is correct.** Test suite (5 cases in `mcp_test.go`)
   covers the JSON-RPC handshake, tool listing under both architect
   and inert role states, resource subscription side effects, the
   `inbox_unread` cursor advance, and URI parsing. If a future Claude
   Code version starts implementing `resources/subscribe`, re-enabling
   is a one-line `~/.claude.json` change.

2. **The investigation produced empirical knowledge.** The capability
   advertisement check (`grep capabilities /tmp/qai-conductor.log`) is
   the test anyone making MCP claims should run before publishing.
   Future maintainers can re-run the same probe in five minutes and
   know whether the situation has changed.

3. **The git history tells the story.** Shipped → tested → unloaded
   is the embodied form of the principle the project documents. The
   conductor's existence and non-deployment is itself the evidence.

## How to re-test

```bash
# Enable debug logging and load the conductor
jq '.mcpServers["qai-conductor"] = {
      type:"stdio",
      command:"/Users/director/.local/bin/qai",
      args:["fleet","mcp"],
      env:{QAI_CONDUCTOR_DEBUG:"1"}
    }' ~/.claude.json > ~/.claude.json.tmp \
  && mv ~/.claude.json.tmp ~/.claude.json

# Restart Claude Code from a tmux pane so $TMUX_PANE is in env
# In another pane:
tail -f /tmp/qai-conductor.log

# Run a small fleet
qai fleet up internal/fleet/testdata/rust-ctf.yaml   # or a smaller one

# Look at the handshake. If clientInfo.capabilities now includes
# "resources":{"subscribe": true}, the situation has changed and the
# conductor's resources/updated path may now work.
```

If the resource-subscription primitive becomes implemented in a future
Claude Code release, re-enable the conductor and remove the experimental
fallback paths B/C/D from `notifyResourceUpdated` in `internal/fleet/mcp.go`.
The remaining path A (subscriber-gated) is the protocol-correct
implementation.
