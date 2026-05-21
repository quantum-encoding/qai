package i18n

// apply.go — write a translation back into a locale TS file.
//
// Pipeline:
//   1. Resolve target file path (--file OR <repo>/<i18n-dir>/<locale>.ts).
//   2. Shell out to codebase_deity's `i18n_patch` to locate the value
//      node's byte range. Returns {found, kind, quote, start, end, current}.
//   3. Sanity-check: if caller supplied --expect-current, verify the on-disk
//      value matches. Prevents stomping a manual edit you didn't know about.
//   4. Render the new value as a TS string literal using the same quote
//      style the source uses, escaping appropriately.
//   5. Splice bytes [start, end) → new literal, write back.
//
// We do NOT handle MISSING keys (insertion into nested objects) here —
// the locator returns found=false and qai surfaces a clear error so the
// caller knows to use the editor manually. Insertion is a v1.1 op.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type ApplyOptions struct {
	File          string // explicit file path; takes precedence over Repo/Locale
	Repo          string // repo root, when File is empty
	I18nDir       string // relative i18n dir under Repo; default "src/lib/i18n"
	Locale        string // locale code (e.g. "es"); locale file is <i18nDir>/<Locale>.ts
	Key           string // dot-path
	Value         string // new value, raw (will be escaped for TS)
	ExpectCurrent string // optional: fail if on-disk value differs
	DryRun        bool   // print planned change; don't write
}

type ApplyResult struct {
	File         string `json:"file"`
	Key          string `json:"key"`
	OldValue     string `json:"old_value"`
	NewValue     string `json:"new_value"`
	Quote        string `json:"quote"`
	BytesBefore  int    `json:"bytes_before"`
	BytesAfter   int    `json:"bytes_after"`
	Wrote        bool   `json:"wrote"`
}

type patchLocator struct {
	Found     string `json:"found"` // serialized as string "true"/"false" by the Rust binary
	Kind      string `json:"kind"`
	Quote     string `json:"quote"`
	StartByte int    `json:"start_byte"`
	EndByte   int    `json:"end_byte"`
	Current   string `json:"current"`
	Reason    string `json:"reason"`
}

// Apply performs the write described by opts.
func Apply(opts ApplyOptions) (*ApplyResult, error) {
	file, err := resolveFile(opts)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Key) == "" {
		return nil, errors.New("apply: --key is required")
	}

	// Resolve the locator binary the same way the extractor does — same
	// install, same env var.
	patchBin, err := resolvePatchBinary()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(patchBin, file, opts.Key)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("i18n_patch %s %s: %w", file, opts.Key, err)
	}
	var loc patchLocator
	if err := json.Unmarshal(out, &loc); err != nil {
		return nil, fmt.Errorf("decode locator output: %w (raw: %s)", err, string(out))
	}
	if loc.Found != "true" {
		return nil, fmt.Errorf("locator: %s", loc.Reason)
	}
	if loc.Kind == "template_string" {
		return nil, fmt.Errorf("apply: key %q is a template_string (backtick); refusing to edit — handle manually", opts.Key)
	}

	if opts.ExpectCurrent != "" && opts.ExpectCurrent != loc.Current {
		return nil, fmt.Errorf(
			"apply: --expect-current mismatch for %q\n  on-disk: %q\n  expected: %q\nFile may have been edited since the scan — rescan and retry.",
			opts.Key, loc.Current, opts.ExpectCurrent,
		)
	}

	source, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if loc.EndByte > len(source) || loc.StartByte < 0 || loc.StartByte >= loc.EndByte {
		return nil, fmt.Errorf("locator returned invalid byte range [%d, %d) for file of length %d",
			loc.StartByte, loc.EndByte, len(source))
	}

	// Sanity: the bytes we're about to replace should start AND end with
	// the reported quote character. Catches any drift between the locator
	// run and the file read.
	if string(source[loc.StartByte]) != loc.Quote ||
		string(source[loc.EndByte-1]) != loc.Quote {
		return nil, fmt.Errorf(
			"apply: byte range doesn't bracket %q quotes — file may have changed between locate and write",
			loc.Quote)
	}

	newLiteral := encodeStringLiteral(opts.Value, loc.Quote)
	result := &ApplyResult{
		File:        file,
		Key:         opts.Key,
		OldValue:    loc.Current,
		NewValue:    opts.Value,
		Quote:       loc.Quote,
		BytesBefore: loc.EndByte - loc.StartByte,
		BytesAfter:  len(newLiteral),
	}

	if opts.DryRun {
		return result, nil
	}

	// Splice [start, end) → newLiteral. Build a fresh buffer; in-place
	// mutation is too easy to get wrong with offset shifts.
	var buf []byte
	buf = append(buf, source[:loc.StartByte]...)
	buf = append(buf, newLiteral...)
	buf = append(buf, source[loc.EndByte:]...)

	if err := os.WriteFile(file, buf, 0644); err != nil {
		return nil, fmt.Errorf("write %s: %w", file, err)
	}
	result.Wrote = true
	return result, nil
}

// encodeStringLiteral wraps `s` in TS quotes of the requested style and
// escapes characters that would break the literal. We don't try to
// normalize unicode, smart-quotes, or line-continuations — the source
// goes in verbatim except for what the parser would reject.
func encodeStringLiteral(s, quote string) string {
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if quote == "'" && r == '\'' {
				b.WriteString(`\'`)
			} else if quote == `"` && r == '"' {
				b.WriteString(`\"`)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteString(quote)
	return b.String()
}

// resolveFile picks the file path from explicit --file OR from
// <repo>/<i18nDir>/<locale>.ts. i18nDir defaults to src/lib/i18n.
func resolveFile(opts ApplyOptions) (string, error) {
	if opts.File != "" {
		return opts.File, nil
	}
	if opts.Repo == "" || opts.Locale == "" {
		return "", errors.New("apply: provide --file OR (--repo and --locale)")
	}
	dir := opts.I18nDir
	if dir == "" {
		dir = "src/lib/i18n"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(opts.Repo, dir)
	}
	return filepath.Join(dir, opts.Locale+".ts"), nil
}

// resolvePatchBinary mirrors ResolveExtractor — same QAI_I18N_EXTRACTOR
// install logic, just substituting `i18n_patch` for `i18n_extract`.
// The convention is that both binaries live in the same install dir.
func resolvePatchBinary() (string, error) {
	if p := strings.TrimSpace(os.Getenv("QAI_I18N_PATCH")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("QAI_I18N_PATCH=%s not found", p)
	}
	// If the extractor is reachable, the patcher should be in the same dir.
	if extractor, err := ResolveExtractor(); err == nil {
		guess := filepath.Join(filepath.Dir(extractor), "i18n_patch")
		if _, err := os.Stat(guess); err == nil {
			return guess, nil
		}
	}
	if p, err := exec.LookPath("i18n_patch"); err == nil {
		return p, nil
	}
	return "", errors.New("i18n_patch not found on PATH.\n" +
		"  Install with:\n" +
		"    cargo install --path /path/to/codebase_deity --bin i18n_patch\n" +
		"  Or set QAI_I18N_PATCH=/path/to/i18n_patch.")
}
