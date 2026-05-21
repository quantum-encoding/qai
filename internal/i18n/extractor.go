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
	cmd := exec.Command(extractor, file)
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
