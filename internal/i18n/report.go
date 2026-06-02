package i18n

// report.go — coverage report types + computation.
//
// Data flow:
//   1. Caller provides ExtractResult for each locale.
//   2. ComputeReport flattens the baseline (en) into a key set, then for
//      every other locale classifies each English key into one of
//      { present, missing, untranslated, exempt }.
//   3. The Report is the canonical structure consumed by every output
//      format (table, JSON, conductor UI).

import (
	"fmt"
	"sort"
	"time"
)

type CellStatus string

const (
	StatusPresent      CellStatus = "present"
	StatusMissing      CellStatus = "missing"
	StatusUntranslated CellStatus = "untranslated"
	StatusExempt       CellStatus = "exempt"
)

// KeyRow is one English key + per-locale status + values.
// Values are kept so the UI can display "en says X, es says X" diffs.
type KeyRow struct {
	Path       string                       `json:"path"`
	EnglishVal string                       `json:"english"`
	Locales    map[string]LocaleCell        `json:"locales"`     // locale -> cell
}

type LocaleCell struct {
	Status CellStatus `json:"status"`
	Value  string     `json:"value,omitempty"`
}

// LocaleStats summarizes one locale's coverage.
type LocaleStats struct {
	Locale       string  `json:"locale"`
	Total        int     `json:"total"`         // total English keys (excluding exempts)
	Present      int     `json:"present"`
	Missing      int     `json:"missing"`
	Untranslated int     `json:"untranslated"`
	Exempt       int     `json:"exempt"`
	Coverage     float64 `json:"coverage"`      // present / total
}

type Report struct {
	Repo        string         `json:"repo"`
	I18nDir     string         `json:"i18n_dir"`
	Mode        string         `json:"mode"`             // "per-locale" | "nested"
	Baseline    string         `json:"baseline"`         // e.g. "en"
	Locales     []string       `json:"locales"`          // sorted, includes baseline
	CommitSHA   string         `json:"commit_sha"`       // best-effort; empty if not a git repo
	ScannedAt   time.Time      `json:"scanned_at"`
	Stats       []LocaleStats  `json:"stats"`            // per-locale, baseline excluded
	Keys        []KeyRow       `json:"keys"`             // alphabetical by path
}

// ComputeReport cross-tabulates baseline + other locales.
//
// Inputs:
//   • extracts:   keyed by locale ("en", "es", ...)
//   • baseline:   the locale whose key set is "truth" (typically "en")
//   • exempt:     loaded from .exempt.json; nil = no exemptions
func ComputeReport(extracts map[string]*ExtractResult, baseline string, exempt *ExemptList) (*Report, error) {
	bl, ok := extracts[baseline]
	if !ok {
		return nil, fmt.Errorf("baseline locale %q not found in extracts", baseline)
	}

	// Build baseline path -> english value map (preserves first-seen order).
	enByPath := make(map[string]string, len(bl.Entries))
	enPaths := make([]string, 0, len(bl.Entries))
	for _, e := range bl.Entries {
		if _, dup := enByPath[e.Path]; dup {
			// Last write wins; flag would be nice but baseline shouldn't
			// have dups in practice (TS object literal would error first).
			continue
		}
		enByPath[e.Path] = e.Value
		enPaths = append(enPaths, e.Path)
	}
	sort.Strings(enPaths)

	// Per-locale extracts indexed by path for O(1) lookup.
	type localeMap map[string]string
	other := make(map[string]localeMap, len(extracts))
	otherLocales := make([]string, 0, len(extracts))
	for loc, ex := range extracts {
		if loc == baseline {
			continue
		}
		m := make(localeMap, len(ex.Entries))
		for _, e := range ex.Entries {
			m[e.Path] = e.Value
		}
		other[loc] = m
		otherLocales = append(otherLocales, loc)
	}
	sort.Strings(otherLocales)

	// Build KeyRow per English path.
	rows := make([]KeyRow, 0, len(enPaths))
	statsByLocale := make(map[string]*LocaleStats, len(otherLocales))
	for _, loc := range otherLocales {
		statsByLocale[loc] = &LocaleStats{Locale: loc}
	}

	for _, path := range enPaths {
		enVal := enByPath[path]

		row := KeyRow{
			Path:       path,
			EnglishVal: enVal,
			Locales:    make(map[string]LocaleCell, len(otherLocales)),
		}

		for _, loc := range otherLocales {
			m := other[loc]
			val, present := m[path]
			st := statsByLocale[loc]
			// Per-locale exempt: passing `loc` lets the matcher honor
			// per_locale entries for cognates/loanwords that are
			// legitimately identical to English in *this* language
			// without muting the same key in other locales where the
			// translation should differ.
			isExempt := exempt.Matches(path, loc)

			switch {
			case isExempt:
				row.Locales[loc] = LocaleCell{Status: StatusExempt, Value: val}
				st.Exempt++
			case !present:
				row.Locales[loc] = LocaleCell{Status: StatusMissing}
				st.Missing++
			case val == enVal:
				row.Locales[loc] = LocaleCell{Status: StatusUntranslated, Value: val}
				st.Untranslated++
			default:
				row.Locales[loc] = LocaleCell{Status: StatusPresent, Value: val}
				st.Present++
			}
		}

		rows = append(rows, row)
	}

	// Finalize stats: Total = present + missing + untranslated (exempts don't count toward coverage).
	stats := make([]LocaleStats, 0, len(otherLocales))
	for _, loc := range otherLocales {
		s := statsByLocale[loc]
		s.Total = s.Present + s.Missing + s.Untranslated
		if s.Total > 0 {
			s.Coverage = float64(s.Present) / float64(s.Total)
		}
		stats = append(stats, *s)
	}

	all := append([]string{baseline}, otherLocales...)
	sort.Strings(all)

	return &Report{
		Baseline:  baseline,
		Locales:   all,
		ScannedAt: time.Now().UTC(),
		Stats:     stats,
		Keys:      rows,
	}, nil
}

// MissingPairs returns a flat list suitable for the "work queue" view —
// every (path, locale) where status is missing OR untranslated.
type MissingPair struct {
	Path       string     `json:"path"`
	Locale     string     `json:"locale"`
	Status     CellStatus `json:"status"`
	EnglishVal string     `json:"english"`
}

func (r *Report) MissingPairs() []MissingPair {
	var out []MissingPair
	for _, row := range r.Keys {
		for loc, cell := range row.Locales {
			if cell.Status == StatusMissing || cell.Status == StatusUntranslated {
				out = append(out, MissingPair{
					Path:       row.Path,
					Locale:     loc,
					Status:     cell.Status,
					EnglishVal: row.EnglishVal,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Locale != out[j].Locale {
			return out[i].Locale < out[j].Locale
		}
		return out[i].Path < out[j].Path
	})
	return out
}
