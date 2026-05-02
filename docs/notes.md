# Session Notes & User TODOs

`qai note` writes to Joplin. Two destinations, one command, two
flavours of usage — automatic via a Claude Code Stop-hook, or manual
called by the agent in lieu of long closing messages.

## The two notebooks

```
qai/sessions   What you (the agent) did. Long work logs, build
               summaries, debug recaps, anything that would otherwise
               get stuffed into a closing message the human has to skim.

qai/todos      What only the human can do. Key rotations, manual
               reviews, decisions, things the agent surfaced but
               cannot complete autonomously.
```

Both default to nesting under the `qai/` parent so Joplin's sidebar
stays organised.

## Manual usage

```bash
# Session summary
qai note "Built feature X. Fixed bug Y. Pushed as commit abc123."

# User TODO
qai note --todo "Rotate CLAUDE_ADMIN_API_KEY — leaked into a
                 transcript on 2026-04-28."

# Custom title
qai note --title "MCP investigation" "Three notification paths
                                       tested; only path C works..."

# Custom destination
qai note --to "qai/notebook/custom/path" "..."

# Read body from stdin (for hooks + scripts)
echo "..." | qai note --stdin --title "session summary"
```

Auto-titles are `"Session YYYY-MM-DD HH:MM — <cwd-basename>"` (or
`TODO ...` for `--todo`), so a list view scans cleanly.

## The agent-shorter-message pattern

The intended use of `qai note` from inside an agent loop is:

> Instead of writing a long summary in your closing message that the
> human has to scroll through, call `qai note "<details>"`. The note
> lives in Joplin where it's searchable. Your closing reply can be one
> line: "Done. Notes in Joplin."

This frees the conversation surface for the next request and gives
the human a searchable record without scrollback hunting.

For TODOs the same pattern applies: instead of telling the user "you
should rotate that key" and hoping they remember, call `qai note
--todo "..."`. They can drain `qai/todos` when they have time.

## The Claude Code Stop-hook (auto-save)

`scripts/qai-session-hook.sh` reads a Claude Code Stop-hook envelope
from stdin, finds the transcript file, extracts the last assistant
message, and pipes it into `qai note --stdin`.

Wire it into `~/.claude/settings.json`:

```json
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "bash /path/to/qai/scripts/qai-session-hook.sh",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

Replace `/path/to/qai` with wherever you cloned the repo (e.g.
`/Users/you/work/qai`).

The script fails silently if Joplin is down or `jq` is missing — it
never blocks the Stop event.

## Manual vs hook — pick one

You probably don't want both. The hook auto-saves *every* assistant
message including small replies and acknowledgements; the result is a
noisy session notebook. The manual path saves *only* the messages the
agent decides are worth saving — quieter and more deliberate, but
relies on the agent to remember (and on you, when adopting this for a
new project, to teach the agent the pattern via CLAUDE.md or a project
prompt).

Heuristics:

- **Hook**: choose this if you want a complete archive of every
  Claude Code session, regardless of triviality, and don't mind the
  noise.
- **Manual**: choose this if you want curated session notes that
  reflect decisions worth remembering. Good for long-running projects
  where the noise matters.

You can switch later by removing or re-adding the hook in
`settings.json`.

## Where the data lives

- Joplin desktop must be running with Web Clipper enabled for any of
  this to work.
- `JOPLIN_TOKEN` must be set in the env (or in `~/.qai/config.yaml`).
- See `docs/getting-started.md` for the full Joplin setup.
