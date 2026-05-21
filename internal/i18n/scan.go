package i18n

// scan.go — top-level Scan orchestrator.
//
// Given a repo root, locate the i18n dir, list locale files, fan out
// extraction across them (in parallel — extractor is single-process,
// no shared state), then compute the cross-tab report.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type ScanOptions struct {
	Repo     string // absolute path to the repo root
	I18nDir  string // path relative to repo; default "src/lib/i18n"
	Baseline string // baseline locale name; default "en"
}

func (o *ScanOptions) absoluteI18nDir() string {
	d := o.I18nDir
	if d == "" {
		d = "src/lib/i18n"
	}
	if filepath.IsAbs(d) {
		return d
	}
	return filepath.Join(o.Repo, d)
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

	dir := opts.absoluteI18nDir()
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("i18n dir not found: %s", dir)
	}

	// List .ts files. Hidden files (.exempt.json) and non-.ts files are skipped.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var localeFiles []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		// Skip likely non-locale files even if they live here (utils, index, etc.).
		base := strings.TrimSuffix(e.Name(), ".ts")
		if base == "index" || strings.Contains(base, "-") || strings.Contains(base, "_") {
			continue
		}
		localeFiles = append(localeFiles, filepath.Join(dir, e.Name()))
	}
	if len(localeFiles) == 0 {
		return nil, fmt.Errorf("no locale .ts files found in %s", dir)
	}

	// Fan out — extractor is independent per file.
	type result struct {
		res *ExtractResult
		err error
	}
	resCh := make(chan result, len(localeFiles))
	var wg sync.WaitGroup
	for _, f := range localeFiles {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()
			r, err := Extract(extractor, file)
			resCh <- result{res: r, err: err}
		}(f)
	}
	wg.Wait()
	close(resCh)

	extracts := make(map[string]*ExtractResult, len(localeFiles))
	for r := range resCh {
		if r.err != nil {
			return nil, r.err
		}
		extracts[r.res.Locale] = r.res
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
	report.CommitSHA = gitCommitSHA(opts.Repo)
	return report, nil
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
