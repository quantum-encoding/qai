package fleet

import (
	"strings"
	"testing"
)

// architectInputBusyOnString lets us test the predicate against synthetic
// pane buffers without standing up a real tmux pane. It mirrors what
// architectInputBusy does after terminal.Read returns.
func architectInputBusyOnString(buf string) (bool, string) {
	lines := strings.Split(buf, "\n")
	footerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "shift+tab") {
			footerIdx = i
			break
		}
	}
	if footerIdx < 0 {
		return true, "footer marker not found"
	}
	for i := footerIdx - 1; i >= 0; i-- {
		line := lines[i]
		idx := strings.Index(line, "❯")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(line[idx+len("❯"):])
		if rest != "" {
			return true, "human typing"
		}
		return false, "idle"
	}
	return true, "no input line"
}

func TestSafetyPredicate_IdleArchitect(t *testing.T) {
	t.Parallel()
	const idle = `─────────────────────────────────
❯
─────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)`
	busy, reason := architectInputBusyOnString(idle)
	if busy {
		t.Fatalf("idle architect flagged busy (%s)", reason)
	}
}

func TestSafetyPredicate_HumanMidTyping(t *testing.T) {
	t.Parallel()
	const typing = `─────────────────────────────────
❯ actually pivot to fixing the auth bug
─────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)`
	busy, reason := architectInputBusyOnString(typing)
	if !busy {
		t.Fatalf("human-typing wasn't flagged busy")
	}
	if reason != "human typing" {
		t.Errorf("wrong reason: %q", reason)
	}
}

func TestSafetyPredicate_FooterMissing_MidResponse(t *testing.T) {
	t.Parallel()
	// Architect is mid-response. The pane bottom shows tool output, not
	// the input box. We hold (which is fine — Anthropic queues the nudge
	// behind the response anyway, but holding doesn't hurt).
	const midResponse = `Reading file…
  ⎿  /path/to/file.go
✻ Bloviating… (12s · thinking)`
	busy, reason := architectInputBusyOnString(midResponse)
	if !busy {
		t.Fatalf("mid-response wasn't flagged busy")
	}
	if !strings.Contains(reason, "footer") {
		t.Errorf("wrong reason: %q", reason)
	}
}

func TestSafetyPredicate_HumanTypingSingleChar(t *testing.T) {
	t.Parallel()
	// Even one character past the prompt should hold.
	const typing = `─────────────────────────────────
❯ a
─────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to cycle)`
	busy, _ := architectInputBusyOnString(typing)
	if !busy {
		t.Fatalf("single-char typing wasn't flagged busy")
	}
}

func TestSafetyPredicate_FooterWrappedAcrossLines(t *testing.T) {
	t.Parallel()
	// Real fixture: narrow pane wraps "(shift+tab to cycle)" across lines.
	// Anchor is just "shift+tab", which still appears.
	const idleNarrow = `─────────────────────────────────
❯
─────────────────────────────────
  ⏵⏵ auto mode on (shift+tab to
     1 MCP server failed · /mcp`
	busy, reason := architectInputBusyOnString(idleNarrow)
	if busy {
		t.Fatalf("narrow-pane idle wasn't flagged idle (%s)", reason)
	}
}

// TestFilterReportable verifies the progress-suppression invariant.
func TestFilterReportable(t *testing.T) {
	t.Parallel()
	in := []Report{
		{Status: "done", Message: "a"},
		{Status: "progress", Message: "b"},
		{Status: "blocked", Message: "c"},
		{Status: "progress", Message: "d", Important: true},
		{Status: "info", Message: "e"},
	}
	out := filterReportable(append([]Report{}, in...))
	if len(out) != 4 {
		t.Fatalf("filter kept %d, want 4: %+v", len(out), out)
	}
	for _, r := range out {
		if r.Status == "progress" && !r.Important {
			t.Errorf("progress without --important slipped through: %+v", r)
		}
	}
}
