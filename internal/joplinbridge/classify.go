package joplinbridge

import "strings"

// Tag kinds — the closed enum the schema's ASSERT pins.
const (
	KindProject  = "project"
	KindKind     = "kind"
	KindConcept  = "concept"
	KindFreeform = "freeform"
)

// reservedKindTags is the closed-set of tag titles that classify as
// 'kind' regardless of prefix. Keep this list in sync with the schema's
// ASSERT — adding here without updating the schema breaks the UPSERT.
// The match is case-insensitive at classify time; storage is lowercase.
var reservedKindTags = map[string]struct{}{
	"decision":   {},
	"bug":        {},
	"scratch":    {},
	"postmortem": {},
	"handoff":    {},
	"todo":       {},
}

// ClassifyTag derives the stored title and the kind axis from a raw
// Joplin tag title. The stored title strips the project:/concept:
// prefix so a query for title='qai-cli' matches whether the user typed
// 'project:qai-cli' or 'qai-cli' (the kind axis disambiguates).
//
// Returns (storedTitle, kind). Both are lowercased — Joplin tags are
// case-insensitive at search time anyway, and uniform casing keeps the
// schema index tight.
//
// Rules (in priority order):
//
//   - project:<name> prefix → ("name", "project")
//   - concept:<name> prefix → ("name", "concept")
//   - reservedKindTags hit  → (input, "kind")
//   - everything else       → (input, "freeform")
//
// project: beats concept: beats reserved, because tag namespaces are
// authored by the caller and prefix presence is a stronger signal than
// a coincidental match against a reserved word.
func ClassifyTag(raw string) (string, string) {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t == "" {
		return "", KindFreeform
	}
	if rest, ok := strings.CutPrefix(t, "project:"); ok {
		return strings.TrimSpace(rest), KindProject
	}
	if rest, ok := strings.CutPrefix(t, "concept:"); ok {
		return strings.TrimSpace(rest), KindConcept
	}
	if _, ok := reservedKindTags[t]; ok {
		return t, KindKind
	}
	return t, KindFreeform
}
