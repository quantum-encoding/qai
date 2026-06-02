package i18n

// scan.go — top-level Scan orchestrator.
//
// Given a repo root, locate the i18n dir, decide which storage convention
// the project uses, extract every locale, then compute the cross-tab report.
//
// Two conventions are supported:
//
//   per-locale — one file per locale (`en.ts`, `fr.ts`, …), each a single
//     `export const <locale> = {...}`. The baseline may instead live as a
//     `const en = {...}` inside a shared `ui.ts` (Astro 6 sites that keep
//     translations in src/i18n/locales/ but author English in src/i18n/ui.ts).
//
//   nested — a single file holding one or more `Record<Locale, T>` consts
//     (`export const ui: Record<Locale, T> = { en: {...}, es: {...} }`).
//     Common in Astro sites following the official i18n recipe.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type ScanOptions struct {
	Repo     string // absolute path to the repo root
	I18nDir  string // path relative to repo; "" = auto-detect
	Baseline string // baseline locale name; default "en"
}

// candidateDirs are searched (in order) when --i18n-dir isn't given. The
// locales/ subdir is listed before its parent so a per-locale layout is
// preferred over a parent that only holds a shared ui.ts.
var candidateDirs = []string{
	"src/lib/i18n",
	"src/i18n/locales",
	"src/i18n",
	"src/locales",
	"i18n",
	"locales",
}

// localeFileRe matches a filename stem that is a locale code: `en`, `pt`,
// `pt-BR`, `zh-Hans`.
var localeFileRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,4})?$`)

func isLocaleName(stem string) bool { return localeFileRe.MatchString(stem) }

// resolveI18nDir honors an explicit --i18n-dir, else auto-detects the first
// candidate dir under the repo that holds at least one usable .ts file.
func resolveI18nDir(opts ScanOptions) (string, error) {
	if opts.I18nDir != "" {
		d := opts.I18nDir
		if !filepath.IsAbs(d) {
			d = filepath.Join(opts.Repo, d)
		}
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			return "", fmt.Errorf("i18n dir not found: %s", d)
		}
		return d, nil
	}
	var tried []string
	for _, rel := range candidateDirs {
		d := filepath.Join(opts.Repo, rel)
		tried = append(tried, rel)
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			continue
		}
		if len(listLocaleFiles(d)) > 0 {
			return d, nil
		}
	}
	return "", fmt.Errorf("no i18n dir found under %s (looked in: %s)\n"+
		"  → fix: pass --i18n-dir <rel> pointing at your locale dictionaries",
		opts.Repo, strings.Join(tried, ", "))
}

// listLocaleFiles returns the candidate dictionary .ts files in a dir:
// non-hidden, non-index .ts files. Unlike the old scanner this keeps files
// with hyphens (pt-BR.ts) and a lone ui.ts (the nested-layout entrypoint);
// the convention detector decides how to treat them.
func listLocaleFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".ts")
		if base == "index" || strings.HasSuffix(base, ".d") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// Scan runs the full pipeline. Returns a populated Report or error.
func Scan(opts ScanOptions) (*Report, error) {
	if opts.Repo == "" {
		return nil, fmt.Errorf("Scan: Repo is required")
	}
	baseline := opts.Baseline
	if baseline == "" {
		baseline = "en"
	}

	extractor, err := ResolveExtractor()
	if err != nil {
		return nil, err
	}

	dir, err := resolveI18nDir(opts)
	if err != nil {
		return nil, err
	}

	files := listLocaleFiles(dir)
	if len(files) == 0 {
		return nil, fmt.Errorf("no locale .ts files found in %s", dir)
	}

	declared := readLocaleUnion(opts.Repo, dir) // may be nil

	// Decide convention. Count files whose stem is a locale code.
	var localeFiles, otherFiles []string
	for _, f := range files {
		stem := strings.TrimSuffix(filepath.Base(f), ".ts")
		if isLocaleName(stem) {
			localeFiles = append(localeFiles, f)
		} else {
			otherFiles = append(otherFiles, f)
		}
	}

	var (
		extracts map[string]*ExtractResult
		mode     string
	)

	switch {
	case len(localeFiles) >= 2:
		// per-locale layout (kitchen-share, dropship locales/).
		extracts, err = scanPerLocale(extractor, dir, localeFiles, baseline)
		mode = "per-locale"
	default:
		// 0–1 locale-named files. Try the nested layout on the most
		// likely entrypoint (ui.ts / the lone file); fall back to
		// treating the single file as a per-locale dictionary.
		nestedFile := pickNestedFile(files)
		if res, ok := probeNested(extractor, nestedFile); ok {
			extracts = indexByLocale(res)
			mode = "nested"
		} else {
			extracts, err = scanPerLocale(extractor, dir, files, baseline)
			mode = "per-locale"
		}
	}
	if err != nil {
		return nil, err
	}

	// Surface locales declared in the `type Locale` union but absent from
	// every dictionary (e.g. reformas' `sv`) as empty → 0% coverage rather
	// than silently invisible.
	for _, l := range declared {
		if _, ok := extracts[l]; !ok {
			extracts[l] = &ExtractResult{Locale: l}
		}
	}

	if _, ok := extracts[baseline]; !ok {
		return nil, fmt.Errorf("baseline locale %q not found in %s (mode=%s)\n"+
			"  → fix: pass --baseline <code> matching an existing locale, or check the dir with --i18n-dir",
			baseline, dir, mode)
	}

	exempt, err := LoadExemptList(dir)
	if err != nil {
		return nil, err
	}

	report, err := ComputeReport(extracts, baseline, exempt)
	if err != nil {
		return nil, err
	}
	report.Repo = opts.Repo
	report.I18nDir = dir
	report.Mode = mode
	report.CommitSHA = gitCommitSHA(opts.Repo)
	return report, nil
}

// scanPerLocale extracts one file per locale. The baseline is resolved
// separately because it may not be a file of its own — it can live as a
// named const inside a shared ui.ts (resolveBaseline handles both).
func scanPerLocale(extractor, dir string, files []string, baseline string) (map[string]*ExtractResult, error) {
	// Fan out the per-file extracts — extractor is independent per file.
	type result struct {
		res *ExtractResult
		err error
	}
	resCh := make(chan result, len(files))
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			r, err := Extract(extractor, file)
			resCh <- result{res: r, err: err}
		}(f)
	}
	wg.Wait()
	close(resCh)

	extracts := make(map[string]*ExtractResult, len(files))
	for r := range resCh {
		if r.err != nil {
			return nil, r.err
		}
		extracts[r.res.Locale] = r.res
	}

	// Resolve baseline if it wasn't one of the per-locale files.
	if _, ok := extracts[baseline]; !ok {
		if bl, err := resolveBaseline(extractor, dir, baseline); err == nil && bl != nil {
			extracts[baseline] = bl
		}
	}
	return extracts, nil
}

// resolveBaseline finds a baseline that isn't its own <baseline>.ts file.
// Looks for a `const <baseline> = {...}` inside a shared dictionary file —
// ui.ts in the locales dir, then in the parent dir (dropship authors
// `const en` in src/i18n/ui.ts while translations live in src/i18n/locales/).
func resolveBaseline(extractor, dir, baseline string) (*ExtractResult, error) {
	candidates := []string{
		filepath.Join(dir, "ui.ts"),
		filepath.Join(dir, "index.ts"),
		filepath.Join(filepath.Dir(dir), "ui.ts"),
		filepath.Join(filepath.Dir(dir), "index.ts"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		if r, err := ExtractConst(extractor, c, baseline); err == nil && len(r.Entries) > 0 {
			return r, nil
		}
	}
	return nil, fmt.Errorf("baseline %q not found as a file or as `const %s` in ui.ts/index.ts", baseline, baseline)
}

// pickNestedFile chooses the most likely nested-layout entrypoint: a file
// named ui.ts (any dir depth) if present, else the lone/first file.
func pickNestedFile(files []string) string {
	for _, f := range files {
		if filepath.Base(f) == "ui.ts" {
			return f
		}
	}
	return files[0]
}

func indexByLocale(res []*ExtractResult) map[string]*ExtractResult {
	m := make(map[string]*ExtractResult, len(res))
	for _, r := range res {
		m[r.Locale] = r
	}
	return m
}

// localeUnionRe captures the RHS of `export type Locale = 'en' | 'es' | ...`.
var localeUnionRe = regexp.MustCompile(`type\s+Locale\s*=\s*([^;\n]+)`)
var quotedRe = regexp.MustCompile(`['"]([A-Za-z0-9-]+)['"]`)

// readLocaleUnion reads the authoritative locale set from a `type Locale`
// union declaration, so locales that exist in the type but are absent from
// every dictionary still appear (as 0% coverage). Searches the usual
// helper-file locations; returns nil if none found.
func readLocaleUnion(repo, dir string) []string {
	candidates := []string{
		filepath.Join(repo, "src/lib/i18n.ts"),
		filepath.Join(dir, "index.ts"),
		filepath.Join(filepath.Dir(dir), "i18n.ts"),
		filepath.Join(dir, "..", "lib", "i18n.ts"),
	}
	for _, c := range candidates {
		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		m := localeUnionRe.FindSubmatch(data)
		if m == nil {
			continue
		}
		var out []string
		for _, q := range quotedRe.FindAllSubmatch(m[1], -1) {
			out = append(out, string(q[1]))
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// gitCommitSHA — best-effort. Empty string if not a git repo (don't
// fail the whole scan for a missing .git, the report is still useful).
func gitCommitSHA(repo string) string {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
