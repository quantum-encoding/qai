#!/bin/bash
# qai-session-hook.sh — Claude Code Stop-hook that saves the last
# assistant message to Joplin via `qai note`.
#
# Wire it up in ~/.claude/settings.json under hooks.Stop:
#
#   "Stop": [{
#     "hooks": [{
#       "type": "command",
#       "command": "bash /path/to/qai/scripts/qai-session-hook.sh",
#       "timeout": 10
#     }]
#   }]
#
# Behaviour:
#   - Reads the Stop-hook JSON envelope from stdin
#   - Locates the transcript_path
#   - Extracts the LAST assistant message's text content
#   - Pipes that text into `qai note --stdin` so it lands in qai/sessions
#
# Failure modes (all silent — never block the Stop event):
#   - No transcript path  → exit 0
#   - Empty last message  → exit 0
#   - Joplin unreachable  → exit 0 (the qai command will print to stderr,
#                                    but the hook itself shouldn't fail)
#   - jq missing          → exit 0 with a one-line stderr nag
#
# Title format: "<cwd-basename> — <session-id-prefix> — <YYYY-MM-DD HH:MM>"

set -euo pipefail

# ── Dependencies ─────────────────────────────────────────────────────────

if ! command -v jq >/dev/null 2>&1; then
  echo "qai-session-hook: jq not found on PATH; skipping" >&2
  exit 0
fi

if ! command -v qai >/dev/null 2>&1; then
  echo "qai-session-hook: qai not found on PATH; skipping" >&2
  exit 0
fi

# ── Parse the Stop-hook envelope ─────────────────────────────────────────

input="$(cat)"
transcript=$(echo "$input" | jq -r '.transcript_path // empty')
sid=$(echo "$input" | jq -r '.session_id // empty')

if [[ -z "$transcript" || ! -f "$transcript" ]]; then
  exit 0
fi

# ── Extract the last assistant message ────────────────────────────────────
#
# The transcript is JSONL, one event per line. We want the last entry
# with .type == "assistant" and pull its text-content blocks joined.

text=$(jq -s '
  map(select(.type == "assistant"))
  | last
  | (.message.content // [])
  | map(select(.type == "text") | .text)
  | join("\n\n")
' "$transcript" 2>/dev/null || true)

# Strip surrounding quotes from jq output and trim whitespace.
text=$(printf '%s' "$text" | sed -e 's/^"//' -e 's/"$//')

if [[ -z "$text" || "$text" == "null" ]]; then
  exit 0
fi

# ── Title + dispatch ─────────────────────────────────────────────────────

cwd_base="$(basename "$PWD")"
sid_short="${sid:0:8}"
ts="$(date +'%Y-%m-%d %H:%M')"

if [[ -n "$sid_short" ]]; then
  title="${cwd_base} — ${sid_short} — ${ts}"
else
  title="${cwd_base} — ${ts}"
fi

# Pipe through and don't fail the hook if Joplin is down.
printf '%s' "$text" | qai note --stdin --title "$title" >/dev/null 2>&1 || true
exit 0
