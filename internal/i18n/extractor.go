package i18n

// extractor.go — wraps the codebase_deity `i18n_extract` binary.
//
// We shell out one process per locale file. Tree-sitter parsing inside
// Go would require CGo bindings and a parallel TS toolchain; reusing
// the Rust binary is much cheaper to maintain. The binary is expected
// on PATH or via $QAI_I18N_EXTRACTOR.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type ExtractEntry struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

type ExtractResult struct {
	File    string         `json:"file"`
	Locale  string         `json:"locale"`
	Entries []ExtractEntry `json:"entries"`
}

// NestedResult is the --nested output: one file whose top-level consts are
// keyed by locale (the single-file Astro layout). Each LocaleBlock holds
// that locale's flattened entries, paths prefixed by their source const.
type NestedResult struct {
	File    string         `json:"file"`
	Mode    string         `json:"mode"` // "nested"
	Locales []LocaleBlock  `json:"locales"`
}

type LocaleBlock struct {
	Locale  string         `json:"locale"`
	Entries []ExtractEntry `json:"entries"`
}

// ResolveExtractor returns the path to the i18n_extract binary. Priority:
//   1. $QAI_I18N_EXTRACTOR  (explicit override; useful in CI)
//   2. `i18n_extract` on $PATH
//   3. error with install hint
//
// The hint mentions `cargo install --path` because that's the simplest
// way to land the binary in ~/.cargo/bin (already on most PATHs).
func ResolveExtractor() (string, error) {
	if p := strings.TrimSpace(os.Getenv("QAI_I18N_EXTRACTOR")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("QAI_I18N_EXTRACTOR=%s not found", p)
	}
	if p, err := exec.LookPath("i18n_extract"); err == nil {
		return p, nil
	}
	return "", errors.New("i18n_extract not found on PATH.\n" +
		"  Install with:\n" +
		"    cargo install --path /path/to/codebase_deity --bin i18n_extract\n" +
		"  Or set QAI_I18N_EXTRACTOR=/path/to/i18n_extract.")
}

// Extract runs the binary against one TS dictionary file and returns
// the parsed JSON. Bubbles up any stderr from the binary so failures
// surface in the qai output verbatim instead of being swallowed.
func Extract(extractor, file string) (*ExtractResult, error) {
	return extractSingle(extractor, file)
}

// ExtractConst extracts the object literal of the const named `name`
// (exported or not) and labels the result with that name as the locale.
// Used to pull a baseline that lives as `const en = {...}` inside a larger
// file (e.g. dropship src/i18n/ui.ts) rather than as its own en.ts.
func ExtractConst(extractor, file, name string) (*ExtractResult, error) {
	return extractSingle(extractor, file, "--const", name)
}

func extractSingle(extractor, file string, extra ...string) (*ExtractResult, error) {
	args := append([]string{}, extra...)
	args = append(args, file)
	cmd := exec.Command(extractor, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("i18n_extract %s: %w", file, err)
	}
	var r ExtractResult
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("decode extractor output for %s: %w", file, err)
	}
	return &r, nil
}

// ExtractNested runs the binary in --nested mode against a single-file Astro
// dictionary (one or more `Record<Locale, T>` consts) and returns one
// ExtractResult per locale. Returns an error if the file contains no
// locale-keyed object (i.e. it isn't a nested-layout file).
func ExtractNested(extractor, file string) ([]*ExtractResult, error) {
	cmd := exec.Command(extractor, "--nested", file)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("i18n_extract --nested %s: %w", file, err)
	}
	var nr NestedResult
	if err := json.Unmarshal(out, &nr); err != nil {
		return nil, fmt.Errorf("decode --nested output for %s: %w", file, err)
	}
	results := make([]*ExtractResult, 0, len(nr.Locales))
	for _, b := range nr.Locales {
		results = append(results, &ExtractResult{
			File:    nr.File,
			Locale:  b.Locale,
			Entries: b.Entries,
		})
	}
	return results, nil
}

// probeNested reports whether `file` is a nested-layout dictionary
// (contains ≥1 locale-keyed object). Used for convention detection; a
// failure here is a normal "not nested" signal, so stderr is discarded
// rather than surfaced to the user.
func probeNested(extractor, file string) ([]*ExtractResult, bool) {
	cmd := exec.Command(extractor, "--nested", file)
	cmd.Stderr = nil // swallow the "not nested" diagnostic during probing
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var nr NestedResult
	if err := json.Unmarshal(out, &nr); err != nil || len(nr.Locales) == 0 {
		return nil, false
	}
	results := make([]*ExtractResult, 0, len(nr.Locales))
	for _, b := range nr.Locales {
		results = append(results, &ExtractResult{File: nr.File, Locale: b.Locale, Entries: b.Entries})
	}
	return results, true
}
