#!/bin/bash
# qai-recall-start.sh — Claude Code SessionStart-hook that injects a
# bounded briefing of recent session notes + open TODOs into the agent's
# context at session boot.
#
# Wire it up in ~/.claude/settings.json under hooks.SessionStart:
#
#   "SessionStart": [{
#     "hooks": [{
#       "type": "command",
#       "command": "bash /path/to/qai/scripts/qai-recall-start.sh",
#       "timeout": 10
#     }]
#   }]
#
# The hook is bounded by design: it inherits qai recall's defaults
# (5 sessions or 14 days, ~1500 token budget, project-scoped to the
# cwd basename). The "dump every prior note into the new session"
# anti-pattern is gated at the recall API, not here.
#
# Behaviour:
#   - If JOPLIN_TOKEN isn't set, exits silently (the hook is opt-in
#     for users with Joplin configured).
#   - If Joplin desktop isn't running, the hook fails silently rather
#     than blocking the SessionStart event with a noisy error.
#   - On success, emits a "## Recall" preamble + the markdown briefing
#     to stdout. Claude Code SessionStart hooks treat stdout as
#     context injected into the new session.

set -euo pipefail

if ! command -v qai >/dev/null 2>&1; then
  exit 0
fi

if [[ -z "${JOPLIN_TOKEN:-}" ]]; then
  exit 0
fi

# Run the recall, capturing stderr separately so a Joplin-down doesn't
# spew a connection-refused message into the agent's context.
output=$(qai recall 2>/dev/null || true)

# Skip empty / no-notes-found briefings — no point injecting a blank
# section.
if [[ -z "$output" ]] || echo "$output" | grep -q "(no session notes found"; then
  exit 0
fi

# Inject the briefing. The "## Recall" header signals the source so the
# agent knows this came from prior sessions rather than the current one.
echo "## Recall — prior session notes for this project"
echo
echo "$output"
echo
echo "(Briefing pulled via the qai-recall-start.sh SessionStart hook."
echo " Source: Joplin qai/sessions + qai/todos filtered by cwd basename."
echo " To skip: remove the hook from ~/.claude/settings.json under hooks.SessionStart.)"
