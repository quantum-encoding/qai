package fleet

import (
	"os"
	"testing"
)

// TestClaudeMatcherFiresOnRealFixture asserts the Claude matcher matches the
// captured output of a real `claude` startup once the input box renders.
// Fixture is a verbatim `tmux capture-pane -p` from claude 2.1.119.
func TestClaudeMatcherFiresOnRealFixture(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("testdata/ready-fixtures/claude-2.1.119-default.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m := matcherFor(AgentClaude)
	if !m.Matches(string(data)) {
		t.Fatalf("claude matcher did not fire on real ready-state fixture")
	}
}

// TestClaudeMatcherDoesNotFireOnTrustPrompt asserts we don't false-positive
// on the "Do you trust this folder?" pre-REPL screen, which appears before
// the input box has rendered.
func TestClaudeMatcherDoesNotFireOnTrustPrompt(t *testing.T) {
	t.Parallel()

	const trustScreen = `╭──────────────────────────────────╮
│ Do you trust the files in this folder? │
│                                  │
│ ❯ 1. Yes, I trust this folder    │
│   2. No, exit                    │
│                                  │
│ Enter to confirm · Esc to cancel │
╰──────────────────────────────────╯`
	m := matcherFor(AgentClaude)
	if m.Matches(trustScreen) {
		t.Fatalf("claude matcher false-positived on trust prompt")
	}
}

// TestClaudeMatcherDoesNotFireOnEmpty makes sure the empty buffer doesn't
// match.
func TestClaudeMatcherDoesNotFireOnEmpty(t *testing.T) {
	t.Parallel()
	m := matcherFor(AgentClaude)
	if m.Matches("") {
		t.Fatalf("matched empty buffer")
	}
}

// TestShellMatcherIsAlwaysFalse — AgentShell never matches via Matches();
// the WaitForPrompt path uses the quiet-period heuristic instead.
func TestShellMatcherIsAlwaysFalse(t *testing.T) {
	t.Parallel()
	m := matcherFor(AgentShell)
	if m.Matches("director@host ~/dir\n❯ ") {
		t.Fatalf("shell matcher should never return true via Matches()")
	}
}
