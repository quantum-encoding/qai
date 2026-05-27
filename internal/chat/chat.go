// Package chat implements `qai chat` — a single-shot chat wrapper that
// reads a message from stdin (or argv), optionally prepends a system
// prompt loaded from the qai audit-profile registry, and POSTs to
// /qai/v1/chat via the conduct package.
//
// The intent is to make the pipe pattern
//
//	qai compile . | qai chat --model gemini --template review
//
// work first-shot: stdin becomes the user message, `--template <name>`
// resolves to the `system` field of the named audit profile (embedded
// or ~/.qai/profiles/), and the model defaults from config.
package chat

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/audit"
	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/conduct"
)

// Cfg is set by main before any command runs (matches the pattern used by
// other packages that need broker base URL / default model).
var Cfg *config.Config

const helpChat = `qai chat — single-shot chat with optional profile/template

Reads the user message from stdin OR positional args, optionally
prepends a system prompt loaded from a named audit profile, and POSTs
to /qai/v1/chat. Prints the assistant text to stdout.

Usage:
  qai chat [flags] "message"
  echo "message" | qai chat [flags]
  qai compile . | qai chat --template review

Flags:
  --model, -m <id>          Model id (default: from config, see qai models)
  --template, -t <name>     Audit profile whose system prompt to use
                            (see: qai audit -- profile names; e.g. review,
                            security-redteam, code-review, documentation)
  --system <prompt>         Explicit system prompt (overrides --template)
  --max-tokens <n>          Cap broker output tokens (0 = broker default)
  -h, --help                This help

Examples:
  qai chat --model gemini-3-pro "explain this error"
  qai compile ./src | qai chat --template review --model claude-opus-4-6
  cat diff.patch | qai chat -t review`

// CmdChat is the entry point dispatched from cmd/qai/main.go.
func CmdChat(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Println(helpChat)
			return
		}
	}

	modelRaw := defaultModel()
	template := ""
	system := ""
	maxTokens := 0
	var msgParts []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--model" || a == "-m":
			if i+1 >= len(args) {
				diefatal("flag %s requires a value", a)
			}
			modelRaw = args[i+1]
			i++
		case a == "--template" || a == "-t":
			if i+1 >= len(args) {
				diefatal("flag %s requires a value", a)
			}
			template = args[i+1]
			i++
		case a == "--system":
			if i+1 >= len(args) {
				diefatal("flag %s requires a value", a)
			}
			system = args[i+1]
			i++
		case a == "--max-tokens":
			if i+1 >= len(args) {
				diefatal("flag %s requires a value", a)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				diefatal("--max-tokens: %v", err)
			}
			maxTokens = n
			i++
		case strings.HasPrefix(a, "-"):
			diefatal("unknown flag %q", a)
		default:
			msgParts = append(msgParts, a)
		}
	}

	// Resolve system prompt: explicit --system wins; else look up template;
	// else no system prompt. Empty template name is a no-op so the user can
	// pass --template "" to opt out programmatically without removing the flag.
	if system == "" && template != "" {
		sys, _, ok := audit.LookupProfile(template)
		if !ok {
			names := audit.ProfileNames()
			fmt.Fprintf(os.Stderr, "qai chat: unknown template %q\n", template)
			fmt.Fprintf(os.Stderr, "  -> fix: pass --template <name> with one of: %s\n", strings.Join(names, ", "))
			fmt.Fprintf(os.Stderr, "          (or drop a YAML into ~/.qai/profiles/ — see qai audit --help)\n")
			os.Exit(1)
		}
		system = sys
	}

	// Resolve message: positional args concatenated, OR stdin if no args
	// were given. We deliberately do NOT mix the two — pick one source —
	// because silently concatenating stdin onto an argv-supplied message
	// would surprise callers piping accidentally.
	message := strings.Join(msgParts, " ")
	if message == "" {
		if isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, "qai chat: no message (give one as argv, or pipe via stdin)")
			fmt.Fprintln(os.Stderr, "  -> fix: `qai chat \"your message\"` or `echo msg | qai chat`")
			os.Exit(1)
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai chat: read stdin: %v\n", err)
			os.Exit(1)
		}
		message = strings.TrimSpace(string(data))
		if message == "" {
			fmt.Fprintln(os.Stderr, "qai chat: stdin was empty — nothing to send")
			os.Exit(1)
		}
	}

	model := resolveModel(modelRaw)
	text, err := conduct.Chat(model, system, message, maxTokens)
	if err != nil {
		conduct.DieAPI(err)
	}
	fmt.Println(text)
}

// defaultModel returns the fallback chat model id. APIConfig doesn't
// carry a default-chat-model field today (only BaseURL + APIKey), so we
// hardcode a known-good Gemini 3 Pro id. If/when a config knob lands,
// swap this for the cfg lookup — keeping it in one function makes that
// a one-line change.
func defaultModel() string {
	return "gemini-3.1-pro-preview"
}

// chatAliases maps human-friendly model shortnames to canonical broker
// ids. Lookup is case-insensitive. The set is small on purpose — one
// alias per major family — to keep the surface obvious without
// duplicating the full model registry. Use the exact id if you want
// something else (see `qai conduct models`).
var chatAliases = map[string]string{
	"gemini":       "gemini-3.1-pro-preview",
	"gemini-pro":   "gemini-3.1-pro-preview",
	"gemini-flash": "gemini-3.5-flash",
	"claude":       "claude-opus-4-6",
	"claude-opus":  "claude-opus-4-6",
	"grok":         "grok-4-1-fast-non-reasoning",
	"gpt":          "gpt-5.2",
	"gpt-5":        "gpt-5.2",
}

// resolveModel applies aliases (case-insensitive). Anything not in the
// alias table passes through verbatim so users can name exact ids.
func resolveModel(raw string) string {
	if id, ok := chatAliases[strings.ToLower(raw)]; ok {
		return id
	}
	return raw
}

func diefatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "qai chat: "+format+"\n", a...)
	os.Exit(1)
}

// isTerminal returns true if f is a TTY. We avoid pulling in
// golang.org/x/term to keep the dep surface flat — checking stat mode
// is enough for the "nothing piped in" decision.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
