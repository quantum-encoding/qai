package i18n

// exempt.go — load + match the exemption sidecar.
//
// Format on disk: `<i18n-dir>/.exempt.json`. Two shapes are accepted —
// the historical flat array (still emitted by older app configs) and
// the per-locale object that distinguishes "always identical across
// locales" from "identical in *this* locale because the word IS the
// same in that language."
//
// Legacy shape (backward compatible):
//
//   ["app.name", "settings.ai_usage.*", "**.brand_name"]
//
//   Every pattern applies to every locale. Right for brand names,
//   technical identifiers, abbreviation tokens — strings that should
//   be identical in en/ja/ar/zh/everywhere.
//
// New shape:
//
//   {
//     "global":     ["app.name", "settings.ai_usage.*"],
//     "per_locale": {
//       "fr": ["recipes.filters.meal_type_dessert", ...],
//       "nl": ["dashboard.title", ...]
//     },
//     "reasons":    { "per_locale.fr.dessert": "loanword" }
//   }
//
//   `global` patterns apply to every locale (same as the legacy
//   array). `per_locale` patterns apply only when checking that
//   specific locale — so "Dashboard" in Dutch can be exempt without
//   muting the scanner when Japanese accidentally ships "Dashboard"
//   in Latin script. `reasons` is documentation-only; the matcher
//   ignores it.
//
// Patterns are dot-separated globs:
//   "*"  matches exactly one segment
//   "**" matches zero or more segments
//
// Missing file = no exemptions. We do NOT default-exempt anything;
// silently exempting keys would manufacture the same "false coverage"
// problem the scanner is supposed to expose.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExemptList holds global + per-locale patterns. Construct via
// LoadExemptList; methods are safe to call on a nil receiver.
type ExemptList struct {
	Global    []string            // applies to every locale
	PerLocale map[string][]string // locale -> patterns specific to that locale
	Reasons   map[string]string   // optional; documentation only
}

// LoadExemptList reads `<i18nDir>/.exempt.json` if present. Returns
// an empty (matches-nothing) list when the file doesn't exist.
// Accepts either the legacy flat-array shape or the new
// per-locale object shape. Returns an error only when the file
// exists but is malformed — silent fallback would mask a typo.
func LoadExemptList(i18nDir string) (*ExemptList, error) {
	p := filepath.Join(i18nDir, ".exempt.json")
	body, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &ExemptList{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}

	// Sniff the first non-whitespace byte to pick a shape. `[` → legacy
	// array; `{` → new object. Anything else → reject so the user fixes
	// the file rather than silently treating it as empty.
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if len(trimmed) == 0 {
		return &ExemptList{}, nil
	}
	switch trimmed[0] {
	case '[':
		var patterns []string
		if err := json.Unmarshal(body, &patterns); err != nil {
			return nil, fmt.Errorf("parse %s (array shape): %w", p, err)
		}
		return &ExemptList{Global: patterns}, nil
	case '{':
		var raw struct {
			Global    []string            `json:"global"`
			PerLocale map[string][]string `json:"per_locale"`
			Reasons   map[string]string   `json:"reasons"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse %s (object shape): %w", p, err)
		}
		return &ExemptList{
			Global:    raw.Global,
			PerLocale: raw.PerLocale,
			Reasons:   raw.Reasons,
		}, nil
	default:
		return nil, fmt.Errorf("parse %s: expected JSON array or object, got %q", p, trimmed[:1])
	}
}

// Matches returns true when `path` is exempt for the given `locale`.
// Global patterns match unconditionally; per-locale patterns match
// only when their locale key matches the supplied locale. Passing
// an empty `locale` only considers global patterns.
func (e *ExemptList) Matches(path, locale string) bool {
	if e == nil {
		return false
	}
	for _, pat := range e.Global {
		if matchSegments(strings.Split(pat, "."), strings.Split(path, ".")) {
			return true
		}
	}
	if locale != "" {
		for _, pat := range e.PerLocale[locale] {
			if matchSegments(strings.Split(pat, "."), strings.Split(path, ".")) {
				return true
			}
		}
	}
	return false
}

// matchSegments — recursive glob over dot-segments.
//   "*"  matches exactly one segment
//   "**" matches zero or more segments
//   anything else matches literally
func matchSegments(pat, p []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Try to match zero segments and then the rest, then one segment,
			// etc. Standard glob backtrack.
			rest := pat[1:]
			if len(rest) == 0 {
				return true // ** at end matches everything remaining
			}
			for i := 0; i <= len(p); i++ {
				if matchSegments(rest, p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if pat[0] != "*" && pat[0] != p[0] {
			return false
		}
		pat = pat[1:]
		p = p[1:]
	}
	return len(p) == 0
}
