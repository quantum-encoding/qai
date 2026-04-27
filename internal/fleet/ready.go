// ready.go — agent readiness detection.
//
// The single most common failure mode is sending the prompt before the
// agent's REPL is listening: the prompt vanishes into stdin before the
// agent reads from it. Each agent has a different "I'm ready" signal —
// Claude prints the input box footer, Gemini does something else, a
// plain shell just shows PS1. The matcher fingerprint is per-agent.

package fleet

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/terminal"
)

// AgentKind identifies which readiness fingerprint to match against.
type AgentKind int

const (
	// AgentClaude — Claude Code CLI. Detects the input-box footer that
	// only renders once the REPL is listening. Stable across permission
	// modes and verified against 2.1.119.
	AgentClaude AgentKind = iota

	// AgentShell — bare interactive shell. We can't fingerprint PS1
	// reliably across user customisations (zsh, bash, starship, ohmyposh,
	// fish). Best-effort: wait a short quiet period after spawn.
	AgentShell

	// AgentGeneric — caller-supplied custom matcher. See ReadyMatcher.Pattern.
	AgentGeneric
)

// ReadyMatcher decides whether a captured pane buffer means "ready for input."
//
// Pattern is a regex (line-mode), Anchor is a substring fast-path that lets
// us skip the regex compile for the common case. If both are set, both must
// match.
type ReadyMatcher struct {
	Kind     AgentKind
	Anchor   string         // cheap substring pre-filter ("" to skip)
	Pattern  *regexp.Regexp // can be nil; matched against the whole buffer
	QuietMin time.Duration  // shell mode: how long the buffer has to be unchanging
}

// matcherFor returns the built-in matcher for an AgentKind.
func matcherFor(kind AgentKind) ReadyMatcher {
	switch kind {
	case AgentClaude:
		// "shift+tab" is the keyboard-shortcut hint in the input-box
		// footer. It only renders once Claude has finished init and is
		// reading stdin. The fuller phrase ("shift+tab to cycle") wraps
		// across lines on narrow panes, so we match the unwrappable
		// anchor and rely on the fact that this token doesn't appear in
		// tool output or messages — only in the footer.
		return ReadyMatcher{
			Kind:   AgentClaude,
			Anchor: "shift+tab",
		}
	case AgentShell:
		return ReadyMatcher{
			Kind:     AgentShell,
			QuietMin: 400 * time.Millisecond,
		}
	default:
		return ReadyMatcher{Kind: AgentGeneric}
	}
}

// Matches reports whether buf indicates the agent is ready.
// For AgentShell this is always false — use waitForPrompt's quiet-period path.
func (m ReadyMatcher) Matches(buf string) bool {
	if m.Kind == AgentShell {
		return false
	}
	if m.Anchor != "" && !strings.Contains(buf, m.Anchor) {
		return false
	}
	if m.Pattern == nil {
		return true
	}
	return m.Pattern.MatchString(buf)
}

// WaitForPrompt blocks until the agent in pane shows its readiness signal,
// or timeout elapses. Polls every pollInterval (default 200ms).
//
// For AgentShell, "ready" is defined as the captured tail being unchanged
// for QuietMin since spawn — there's no universal PS1 marker we can trust.
func WaitForPrompt(pane string, m ReadyMatcher, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	const pollInterval = 200 * time.Millisecond

	deadline := time.Now().Add(timeout)
	var lastBuf string
	var lastChange = time.Now()

	for {
		buf, err := terminal.Read(pane, 30)
		if err != nil {
			return fmt.Errorf("read pane %q: %v", pane, err)
		}

		switch m.Kind {
		case AgentShell:
			if buf != lastBuf {
				lastBuf = buf
				lastChange = time.Now()
			}
			if time.Since(lastChange) >= m.QuietMin {
				return nil
			}
		default:
			if m.Matches(buf) {
				return nil
			}
			lastBuf = buf // for shell-style fallback if Pattern is empty
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for %s ready signal in pane %q (last buffer tail: %q)",
				timeout, agentLabel(m.Kind), pane, tail(lastBuf, 200))
		}
		time.Sleep(pollInterval)
	}
}

func agentLabel(k AgentKind) string {
	switch k {
	case AgentClaude:
		return "claude"
	case AgentShell:
		return "shell"
	default:
		return "generic"
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
