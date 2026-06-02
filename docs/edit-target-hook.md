# Edit-target hook — cross-repo project tagging

`qai project name` resolves a project tag from cwd via a layered chain
(`.qai/project` override → manifest walk → git-root basename → bare
basename). That's right when the agent's actual work matches its
launch directory. It breaks when:

> Claude is launched from `/Users/director/work/quantum-encoding/peterborough-hottubs`
> but every edit lands in `/Users/director/work/websites/dropship-accelerator`.

Without intervention, every `qai note` tags `peterborough-hottubs`,
polluting graph context + project-scoped recall for the actual work.

## Fix

A new highest-precedence resolution rule (`SourceEditTarget`) reads
`~/.qai/target-edit-path` and resolves from `dirname(<path>)` instead
of cwd. The Claude Code `PostToolUse` hook (`Edit|Write|MultiEdit`)
writes the last-edited file's absolute path there. Last-edit-wins so
the tag follows whatever the agent is actually touching.

**Failure-redirecting, not failure-inducing:** missing file, empty
file, or a target whose dirname resolves to noise (`/tmp`, `src`,
etc.) all fall through cleanly to the cwd-based chain. Enabling the
hook never breaks single-repo sessions; it just fixes cross-repo ones.

## Wiring

The hook script lives in your `~/.claude/hooks/`:

```bash
# ~/.claude/hooks/qai-target-from-edits.sh
INPUT=$(cat)
FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // empty')
[ -z "$FILE_PATH" ] && exit 0
case "$FILE_PATH" in /*) ABS="$FILE_PATH" ;;
  *) ABS="$(cd "$(dirname "$FILE_PATH")" 2>/dev/null && pwd)/$(basename "$FILE_PATH")" ;;
esac
[ -z "$ABS" ] || [ "$ABS" = "/" ] && exit 0
case "$ABS" in "$HOME/.qai/target-edit-path"|/tmp/*|"$HOME/.claude/"*) exit 0 ;; esac
mkdir -p "$HOME/.qai"
TMP="$HOME/.qai/target-edit-path.tmp.$$"
printf '%s\n' "$ABS" > "$TMP" && mv -f "$TMP" "$HOME/.qai/target-edit-path"
```

Register in `~/.claude/settings.json`:

```json
"PostToolUse": [
  {
    "matcher": "Edit|Write|MultiEdit",
    "hooks": [
      { "type": "command", "command": "bash ~/.claude/hooks/qai-target-from-edits.sh", "timeout": 5 }
    ]
  }
]
```

The `SessionStart` hook (`scripts/qai-recall-start.sh`) clears the
pointer file at boot so the previous session's last edit doesn't
drive a fresh session.

## Resolution chain

| Priority | Source | Trigger |
|----------|--------|---------|
| -1 | `SourceEditTarget` | `~/.qai/target-edit-path` exists + dirname resolves |
| 0 | `SourceOverride` | `.qai/project` file (walks up from cwd) |
| 1 | `SourceManifest` | `package.json` / `Cargo.toml` / `pyproject.toml` / `go.mod` |
| 2 | `SourceGitRoot` | basename of dir containing `.git` |
| 3 | `SourceCwdBasename` | bare cwd basename, last-ditch |

Consumers (`qai note`, `qai joplin graph context`) pick up the edit-
target automatically since they all go through `projectid.Resolve`.

## Diagnostics

```bash
qai project name --explain
# dropship-accelerator
#   source:   edit-target
#   via:      /Users/director/work/websites/dropship-accelerator/package.json
```

Source `edit-target` means the hook fired and the anchor was the
target dir's manifest. The `via:` line gives the file the resolver
walked to.

## Why explicit > clever

Auto-detecting "what project is this session about" by inspecting
recently-edited files sounds smarter but is fragile (which file edit
counts? when does it kick in? what about read-only sessions?).
Explicit beats clever for project-tag, since wrong tags pollute the
graph forever and `qai recall --project` silently misses notes. The
hook gives you the explicit-with-zero-effort variant: the agent's
edits ARE the signal, no extra typing required.
