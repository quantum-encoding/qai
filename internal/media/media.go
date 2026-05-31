// Package media implements `qai media` — multimodal chat with cached
// uploads. Designed for the "watch a 1 GB course video and ask
// questions over multiple turns" pattern without re-uploading or
// re-billing the file each turn.
//
// Surface (see CmdMedia for full dispatch):
//
//	qai media <file> <prompt>             — one-shot question over <file>
//	qai media chat <file> <prompt>        — start cached multi-turn session
//	qai media chat [--session ID] <prompt>— resume an active or named session
//	qai media sessions                    — list user's sessions
//	qai media sessions --rm <id>          — delete a session
//
// The whole flow:
//
//  1. Auto-compress (compress.go) if the file is over the compression
//     threshold (defaults to 20 MB) — slow upload links + Gemini's
//     1-frame-per-second sampling mean re-encoding at 480p/1fps/64kbps
//     mono cuts a 150 MB lecture to ~5 MB with no loss of intelligibility
//     for narration-heavy content.
//  2. Upload to POST /qai/v1/files (multipart) → file_uri.
//  3. EITHER create a single-use cache + chat once (one-shot mode) OR
//     create a server-side session (multi-turn mode). Session creation
//     also creates the underlying Vertex cache; subsequent turns hit
//     the cached read rate.
//
// Server-side state — the session lives in a SEPARATE Firestore database
// from the rest of the platform (broker side; see backend
// FIRESTORE_MEDIA_DB_ID env var). Client only keeps a pointer to the
// most recently used session in ~/.qai/media-sessions/active so the
// no-arg `qai media chat "..."` form resumes the right one.
package media

import (
	"fmt"
	"os"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

// Cfg is wired by main before any command runs.
var Cfg *config.Config

// CmdMedia is the entry point dispatched from cmd/qai/main.go.
func CmdMedia(args []string) {
	// Top-level help only fires when the first arg is help-flavoured
	// or when there's nothing at all. Subcommand-level --help is
	// handled by each subcommand so `qai media batch --help` reaches
	// the batch help instead of the parent.
	if len(args) == 0 {
		fmt.Println(helpMedia)
		os.Exit(1)
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Println(helpMedia)
		return
	}

	switch args[0] {
	case "chat":
		cmdChat(args[1:])
	case "sessions":
		cmdSessions(args[1:])
	case "batch":
		cmdBatch(args[1:])
	default:
		// One-shot path. First positional = file, rest = prompt.
		cmdOneShot(args)
	}
}

// diefatal is the package-local "die with helpful stderr" used at every
// validation failure. Mirrors the pattern used elsewhere in qai — every
// non-zero exit names what failed and how to fix it.
func diefatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "qai media: "+format+"\n", a...)
	os.Exit(1)
}

// stripModelFlag returns args with any `--model X` pair removed and the
// extracted model name (or "" if not present). Centralised so each
// subcommand parses it the same way without duplicating switch cases.
func stripModelFlag(args []string) (out []string, model string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--model" || args[i] == "-m" {
			if i+1 < len(args) {
				model = args[i+1]
			}
			out = append(out, args[:i]...)
			if i+2 <= len(args) {
				out = append(out, args[i+2:]...)
			}
			return
		}
	}
	return args, ""
}

// stripFlag pulls a single named flag with its value from args; same
// shape as stripModelFlag but generic. Used for --session / --mime /
// --no-compress etc.
func stripFlag(args []string, name string) (out []string, value string, present bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			present = true
			if i+1 < len(args) {
				value = args[i+1]
				out = append(out, args[:i]...)
				if i+2 <= len(args) {
					out = append(out, args[i+2:]...)
				}
				return
			}
			out = append(out, args[:i]...)
			return
		}
	}
	return args, "", false
}

// stripBoolFlag handles boolean flags with no value (--no-compress).
func stripBoolFlag(args []string, name string) (out []string, present bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			present = true
			out = append(out, args[:i]...)
			if i+1 <= len(args) {
				out = append(out, args[i+1:]...)
			}
			return
		}
	}
	return args, false
}

// joinPrompt collects positional non-flag args into a single prompt
// string. Spaces preserved between tokens — the user can quote
// multi-word prompts OR pass them as bare argv (everything after the
// file path joins).
func joinPrompt(args []string) string {
	return strings.TrimSpace(strings.Join(args, " "))
}

// resolveModel applies the same alias map qai chat uses, plus the
// media-specific default (gemini-3.1-flash-lite — the cheapest 3.x
// flash variant). Centralised here so every media subcommand picks the
// same default and accepts the same shorthand.
func resolveModel(raw string) string {
	if raw == "" {
		return defaultModel
	}
	if id, ok := modelAliases[strings.ToLower(raw)]; ok {
		return id
	}
	return raw
}

const defaultModel = "gemini-3.1-flash-lite"

// modelAliases is the small media-specific lookup table. We deliberately
// only expose the Gemini family — the server-side caches endpoint
// refuses non-Gemini models, so accepting "claude" here would just send
// the user into a 400.
var modelAliases = map[string]string{
	"gemini":           defaultModel,
	"flash":            defaultModel,
	"flash-lite":       defaultModel,
	"gemini-lite":      defaultModel,
	"gemini-flash":     "gemini-3.5-flash",
	"gemini-flash-pro": "gemini-3-flash-preview",
	"gemini-pro":       "gemini-3.1-pro-preview",
}

const helpMedia = `qai media — multimodal chat with cached uploads

One-shot:
  qai media <file> "prompt"
    Uploads <file>, asks one question, prints the answer. The cache
    is released afterward — for follow-ups, use chat mode instead.

Multi-turn (cached):
  qai media chat <file> "first prompt"
    Uploads <file>, creates a server-side session with a Vertex cache
    of the file + system instruction, and runs the first turn. Saves
    the session id as the active session so follow-ups don't need it.

  qai media chat "next prompt"
    Resumes the active session — no upload, hits the cached read
    rate (~10× cheaper than re-sending the file).

  qai media chat --session <id> "prompt"
    Explicit session pick (use after qai media sessions to find ids).

Auto-walk + output:
  qai media chat <file> --auto "prompt"
    Forces a structured plan on turn 1 ({total_chunks, outline, chunk-1
    content}), then auto-loops continuation turns up to --max-turns.
    Streams each chunk to stdout as it arrives.

  qai media chat ... -o out.md
    Save the full response (single-turn) or assembled chunks (--auto)
    to a markdown file with frontmatter. Use --append to add to an
    existing file.

  qai media chat ... --max-turns 12
    Cap auto chunks (default 8). The plan may estimate more; capped
    runs end with a "resume from chunk N+1" hint to stderr.

Batch (auto-auto mode — every video in a folder/CSV):
  qai media batch --folder <dir> [--output-dir <dir>] [--parallel N]
  qai media batch --csv <path>   [--output-dir <dir>]
  qai media batch <file>...
    See 'qai media batch --help' for full flag list.

Sessions:
  qai media sessions               List all your sessions.
  qai media sessions --rm <id>     Delete a session and release its cache.

Flags (all subcommands):
  --model, -m <id>      Override default model. Default: gemini-3.1-flash-lite
                        (cheapest 3.x flash variant — ~$0.32/M in,
                        ~$1.88/M out, with cached reads at ~$0.03/M).
                        Aliases: gemini, flash, flash-lite, gemini-pro.
  --template, -t <name> Named audit-profile system prompt — same
                        registry as 'qai chat --template'. Default
                        when neither --system nor --template is set:
                        media-narrate (verbose-faithful narration that
                        chunks when content won't fit in one reply).
                        Edit ~/.qai/profiles/media-narrate.yaml to
                        customise without rebuilding.
  --system <prompt>     Explicit system prompt (wins over --template).
                        Session-create only; ignored on follow-up turns
                        because the cache carries the original.
  --mime <type>         Override the auto-detected MIME type (e.g.
                        video/mp4). Auto-detection works from the
                        extension; pass this if your file has an
                        unusual extension.
  --no-compress         Skip the auto-compress step for files >20 MB
                        (default behaviour: re-encode video to
                        480p/1fps/64kbps mono before upload — turns
                        a 150 MB lecture into ~5 MB with no loss for
                        narration content).
  --ttl <seconds>       Cache TTL. Default: 3600 (1h). Min 60, max 86400.
  --max-tokens <n>      Cap output tokens per turn.

Examples:
  qai media ~/Videos/lecture.mp4 "summarise the three core points"

  qai media chat ~/Videos/big-course.mp4 "explain part 1-3"
  qai media chat "now part 4-6"
  qai media chat "and part 7-end"

  qai media chat --system "You are a study coach. Be terse." \
    ~/Videos/lecture.mp4 "give me three questions to test myself"

  qai media sessions
  qai media sessions --rm 8c1e9a02-...`
