package joplinbridge

import "testing"

func TestClassifyTag(t *testing.T) {
	cases := []struct {
		in        string
		wantTitle string
		wantKind  string
	}{
		// project: prefix → project kind, prefix stripped
		{"project:qai-cli", "qai-cli", KindProject},
		{"Project:QAI-CLI", "qai-cli", KindProject},
		{"PROJECT:qai-cli", "qai-cli", KindProject},
		{"project: spaced ", "spaced", KindProject},

		// concept: prefix → concept kind, prefix stripped
		{"concept:billing", "billing", KindConcept},
		{"Concept:Auth-Flow", "auth-flow", KindConcept},

		// reserved → kind axis, title preserved
		{"decision", "decision", KindKind},
		{"DECISION", "decision", KindKind},
		{"bug", "bug", KindKind},
		{"scratch", "scratch", KindKind},
		{"postmortem", "postmortem", KindKind},
		{"handoff", "handoff", KindKind},
		{"todo", "todo", KindKind},

		// project: takes priority over a coincidental reserved-word body
		{"project:decision", "decision", KindProject},
		// concept: same — namespace prefix wins
		{"concept:bug", "bug", KindConcept},

		// freeform — everything else
		{"work", "work", KindFreeform},
		{"research", "research", KindFreeform},
		{"  WhiteSpaced  ", "whitespaced", KindFreeform},
		{"weird-tag", "weird-tag", KindFreeform},

		// empty input → freeform with empty title (caller should skip)
		{"", "", KindFreeform},
		{"   ", "", KindFreeform},
	}
	for _, c := range cases {
		title, kind := ClassifyTag(c.in)
		if title != c.wantTitle || kind != c.wantKind {
			t.Errorf("ClassifyTag(%q) = (%q, %q), want (%q, %q)",
				c.in, title, kind, c.wantTitle, c.wantKind)
		}
	}
}
