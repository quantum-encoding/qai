// Package i18n — qai i18n — Translation coverage scanner.
//
// Subcommands:
//
//	qai i18n scan <repo>                  Full report → terminal table.
//	qai i18n scan <repo> --format json    Same data as JSON (for the conductor UI).
//	qai i18n missing <repo>               Flat (locale, key) work-queue table.
//	qai i18n stats <repo>                 Just the per-locale coverage summary.
//
// Flags shared by all subcommands:
//
//	--i18n-dir <rel>     directory under <repo> (default src/lib/i18n)
//	--baseline <locale>  baseline locale (default en)
//
// Requires the `i18n_extract` binary on PATH (or QAI_I18N_EXTRACTOR set).
// Install with:
//
//	cargo install --path ~/work/poly-repo/quantum-ai-polyrepo/codebase_deity --bin i18n_extract

package i18n

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

func Cmd(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}
	switch args[0] {
	case "scan":
		runScan(args[1:])
	case "missing":
		runMissing(args[1:])
	case "stats":
		runStats(args[1:])
	case "translate":
		runTranslate(args[1:])
	case "apply":
		runApply(args[1:])
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "qai i18n: unknown subcommand %q\n\n", args[0])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: qai i18n <subcommand> <repo> [flags]

Translation coverage scanner. Reads <repo>'s i18n dictionary files and
reports which keys each locale has translated, has missing, or has
identical-to-English (likely untranslated).

Layouts auto-detected (no --i18n-dir needed for the common cases):
  • per-locale   one file per locale (en.ts, fr.ts, …). Baseline may also
                 live as 'const en = {…}' inside a shared ui.ts.
  • nested       a single Astro file with 'Record<Locale, T>' dictionaries
                 (export const ui = { en:{…}, es:{…} }).
The i18n dir is auto-detected among src/lib/i18n, src/i18n/locales, src/i18n,
src/locales; the 'type Locale' union (src/lib/i18n.ts) is read so locales
declared-but-untranslated still show as 0%.

Subcommands:
  scan       Full grid report.
  missing    Flat (locale, key) work-queue (missing + untranslated).
  stats      Per-locale coverage summary.
  translate  Single-shot LLM translation. One English string → JSON of
             translations for every --to locale.
  apply      Write a translation BACK into a locale .ts file. Replaces
             the string literal at the dot-path. Only works for keys
             that already exist (untranslated case); for truly-missing
             keys, add them manually for now.

translate flags:
  --english "..."       source string (required)
  --to <codes>          comma-separated target codes (e.g. es,fr,de)
  --key  <dot.path>     optional context for the model
  --app  <name>         optional app name for tone context
  --model <id>          chat model id (default claude-sonnet-4-6)

apply flags:
  --repo <path>         repo root (uses --i18n-dir + --locale to resolve file)
  --locale <code>       locale code (e.g. fr)
  --file <path>         absolute file path (alternative to --repo + --locale)
  --key <dot.path>      required
  --value "..."         required — the new value to write
  --expect-current "…"  optional safety: fail if on-disk value doesn't match
  --dry-run             print the planned change without writing

Flags:
  --i18n-dir <rel>     directory under <repo>  (default: auto-detect)
  --baseline <locale>  baseline locale         (default en)
  --format table|json  output format           (default table)
  --locale <code>      filter to one locale    (missing / stats only)
  (flags work before or after <repo>)

Examples:
  qai i18n scan    ~/work/tauri_apps/kitchen-share
  qai i18n stats   ~/work/websites/dropship-accelerator        # Astro, per-locale
  qai i18n stats   ~/work/websites/reformas-costa-sol-astro    # Astro, nested ui.ts
  qai i18n missing ~/work/websites/dropship-accelerator --locale ja

Setup (one-time, scanner binary):
  cargo install --path ~/work/poly-repo/quantum-ai-polyrepo/codebase_deity --bin i18n_extract
`)
}

func addCommonFlags(fs *flag.FlagSet) (*ScanOptions, *string) {
	opts := &ScanOptions{}
	// Empty default → Scan auto-detects among src/lib/i18n, src/i18n,
	// src/i18n/locales, src/locales. An explicit --i18n-dir still wins.
	fs.StringVar(&opts.I18nDir, "i18n-dir", "", "i18n dir relative to repo (default: auto-detect)")
	fs.StringVar(&opts.Baseline, "baseline", "en", "baseline locale")
	format := fs.String("format", "table", "output format (table | json)")
	return opts, format
}

// scanValueFlags are the flags shared by scan/missing/stats that take a
// value. Used by flagsFirst to reorder so flags work after <repo>.
var scanValueFlags = map[string]bool{
	"--i18n-dir": true, "--baseline": true, "--format": true, "--locale": true,
	"-i18n-dir": true, "-baseline": true, "-format": true, "-locale": true,
}

// flagsFirst moves every flag (and the value of a value-bearing flag) ahead
// of the positional args. Go's flag package stops parsing at the first
// non-flag token, so `qai i18n stats <repo> --i18n-dir x` would otherwise
// silently ignore the flag. This makes flag position irrelevant.
func flagsFirst(args []string, valueFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			// `--flag=value` carries its own value; otherwise consume next.
			if valueFlags[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

// ---------------------------------------------------------------------
// scan — full report
// ---------------------------------------------------------------------

func runScan(args []string) {
	fs := flag.NewFlagSet("i18n scan", flag.ExitOnError)
	opts, format := addCommonFlags(fs)
	_ = fs.Parse(flagsFirst(args, scanValueFlags))
	if fs.NArg() < 1 {
		fail("scan: missing <repo>")
	}
	opts.Repo = fs.Arg(0)

	r, err := Scan(*opts)
	if err != nil {
		fail("%v", err)
	}

	switch *format {
	case "json":
		emitJSON(r)
	default:
		emitGrid(r)
	}
}

// ---------------------------------------------------------------------
// missing — flat (locale, key) work-queue
// ---------------------------------------------------------------------

func runMissing(args []string) {
	fs := flag.NewFlagSet("i18n missing", flag.ExitOnError)
	opts, format := addCommonFlags(fs)
	locale := fs.String("locale", "", "filter to one locale")
	_ = fs.Parse(flagsFirst(args, scanValueFlags))
	if fs.NArg() < 1 {
		fail("missing: missing <repo>")
	}
	opts.Repo = fs.Arg(0)

	r, err := Scan(*opts)
	if err != nil {
		fail("%v", err)
	}
	pairs := r.MissingPairs()
	if *locale != "" {
		filt := pairs[:0]
		for _, p := range pairs {
			if p.Locale == *locale {
				filt = append(filt, p)
			}
		}
		pairs = filt
	}

	if *format == "json" {
		b, _ := json.MarshalIndent(pairs, "", "  ")
		fmt.Println(string(b))
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LOCALE\tSTATUS\tKEY\tEN")
	fmt.Fprintln(w, "------\t------\t---\t--")
	for _, p := range pairs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Locale, p.Status, p.Path, trunc(p.EnglishVal, 60))
	}
	w.Flush()
	fmt.Fprintf(os.Stderr, "\n(%d missing/untranslated pair%s)\n", len(pairs), plural(len(pairs)))
}

// ---------------------------------------------------------------------
// apply — write a translation back into a locale TS file
// ---------------------------------------------------------------------

func runApply(args []string) {
	fs := flag.NewFlagSet("i18n apply", flag.ExitOnError)
	file := fs.String("file", "", "absolute path to the locale .ts file")
	repo := fs.String("repo", "", "repo root (use with --locale)")
	i18nDir := fs.String("i18n-dir", "src/lib/i18n", "i18n dir relative to --repo")
	locale := fs.String("locale", "", "locale code (e.g. fr)")
	key := fs.String("key", "", "dot.path of the leaf key")
	value := fs.String("value", "", "new value to write (raw text, will be quoted/escaped)")
	expect := fs.String("expect-current", "", "optional: fail if on-disk value differs")
	dryRun := fs.Bool("dry-run", false, "print the planned change without writing")
	format := fs.String("format", "json", "output format (json | table)")
	_ = fs.Parse(args)

	if *key == "" {
		fail("apply: --key required")
	}
	if *value == "" {
		fail("apply: --value required (use --value '' explicitly to set an empty string)")
	}

	res, err := Apply(ApplyOptions{
		File:          *file,
		Repo:          *repo,
		I18nDir:       *i18nDir,
		Locale:        *locale,
		Key:           *key,
		Value:         *value,
		ExpectCurrent: *expect,
		DryRun:        *dryRun,
	})
	if err != nil {
		fail("%v", err)
	}

	if *format == "table" {
		fmt.Printf("file:        %s\n", res.File)
		fmt.Printf("key:         %s\n", res.Key)
		fmt.Printf("quote style: %s\n", res.Quote)
		fmt.Printf("old value:   %q\n", res.OldValue)
		fmt.Printf("new value:   %q\n", res.NewValue)
		fmt.Printf("bytes:       %d → %d\n", res.BytesBefore, res.BytesAfter)
		if res.Wrote {
			fmt.Println("status:      written")
		} else {
			fmt.Println("status:      dry-run (no write)")
		}
		return
	}
	emitJSON(res)
}

// ---------------------------------------------------------------------
// translate — single-shot LLM translation
// ---------------------------------------------------------------------

func runTranslate(args []string) {
	fs := flag.NewFlagSet("i18n translate", flag.ExitOnError)
	english := fs.String("english", "", "source English string (required)")
	to := fs.String("to", "", "comma-separated target locale codes (e.g. es,fr,de)")
	key := fs.String("key", "", "optional key path for context")
	app := fs.String("app", "", "optional app name for tone context")
	model := fs.String("model", "", "chat model id (default claude-sonnet-4-6)")
	format := fs.String("format", "json", "output format (json | table)")
	_ = fs.Parse(args)

	if *english == "" {
		fail("translate: --english required")
	}
	if *to == "" {
		fail("translate: --to required")
	}
	targets := splitCsv(*to)
	if len(targets) == 0 {
		fail("translate: --to is empty after split")
	}

	res, err := Translate(TranslateRequest{
		English: *english,
		KeyPath: *key,
		AppName: *app,
		Targets: targets,
		Model:   *model,
	})
	if err != nil && res == nil {
		fail("%v", err)
	}
	if err != nil {
		// Partial success: surface the warning but still emit what we got.
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	if *format == "table" {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "LOCALE\tTRANSLATION")
		fmt.Fprintln(w, "------\t-----------")
		for _, code := range targets {
			fmt.Fprintf(w, "%s\t%s\n", code, res.Translations[code])
		}
		w.Flush()
		return
	}
	emitJSON(res)
}

func splitCsv(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------
// stats — per-locale coverage summary
// ---------------------------------------------------------------------

func runStats(args []string) {
	fs := flag.NewFlagSet("i18n stats", flag.ExitOnError)
	opts, format := addCommonFlags(fs)
	_ = fs.Parse(flagsFirst(args, scanValueFlags))
	if fs.NArg() < 1 {
		fail("stats: missing <repo>")
	}
	opts.Repo = fs.Arg(0)

	r, err := Scan(*opts)
	if err != nil {
		fail("%v", err)
	}

	if *format == "json" {
		b, _ := json.MarshalIndent(map[string]any{
			"repo":       r.Repo,
			"baseline":   r.Baseline,
			"commit":     r.CommitSHA,
			"scanned_at": r.ScannedAt,
			"stats":      r.Stats,
		}, "", "  ")
		fmt.Println(string(b))
		return
	}

	fmt.Fprintf(os.Stderr, "%s\n\n", scanHeader(r))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LOCALE\tCOVERAGE\tPRESENT\tMISSING\tUNTRANSLATED\tEXEMPT")
	fmt.Fprintln(w, "------\t--------\t-------\t-------\t------------\t------")
	for _, s := range r.Stats {
		fmt.Fprintf(w, "%s\t%.1f%% (%d/%d)\t%d\t%d\t%d\t%d\n",
			s.Locale, s.Coverage*100, s.Present, s.Total,
			s.Present, s.Missing, s.Untranslated, s.Exempt)
	}
	w.Flush()
}

// ---------------------------------------------------------------------
// grid output (scan default)
// ---------------------------------------------------------------------

func emitGrid(r *Report) {
	fmt.Fprintf(os.Stderr, "%s · baseline=%s\n", scanHeader(r), r.Baseline)
	fmt.Fprintln(os.Stderr)

	// Header row
	locs := nonBaselineLocales(r)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	header := []string{"KEY"}
	for _, l := range locs {
		header = append(header, strings.ToUpper(l))
	}
	fmt.Fprintln(w, strings.Join(header, "\t"))

	for _, row := range r.Keys {
		cells := []string{row.Path}
		for _, l := range locs {
			cells = append(cells, statusGlyph(row.Locales[l].Status))
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush()

	// Footer: per-locale stats
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "── coverage ──")
	w2 := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w2, "LOCALE\tCOVERAGE\tMISSING\tUNTRANSLATED")
	for _, s := range r.Stats {
		fmt.Fprintf(w2, "%s\t%.1f%% (%d/%d)\t%d\t%d\n",
			s.Locale, s.Coverage*100, s.Present, s.Total, s.Missing, s.Untranslated)
	}
	w2.Flush()
}

func statusGlyph(s CellStatus) string {
	switch s {
	case StatusPresent:
		return "✓"
	case StatusMissing:
		return "✗"
	case StatusUntranslated:
		return "~" // present but identical to EN
	case StatusExempt:
		return "·" // intentionally untranslated
	default:
		return "?"
	}
}

func nonBaselineLocales(r *Report) []string {
	out := make([]string, 0, len(r.Locales))
	for _, l := range r.Locales {
		if l != r.Baseline {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func emitJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// scanHeader is the one-line provenance shown above every report: repo,
// commit, detected convention, and which dir was scanned (relative to the
// repo so auto-detect's choice is visible at a glance).
func scanHeader(r *Report) string {
	dir := r.I18nDir
	if rel, err := filepath.Rel(r.Repo, r.I18nDir); err == nil {
		dir = rel
	}
	parts := []string{r.Repo}
	if r.CommitSHA != "" {
		parts = append(parts, "@ "+r.CommitSHA)
	}
	parts = append(parts,
		"· scanned "+r.ScannedAt.Format("2006-01-02 15:04"),
		"· mode="+r.Mode,
		"· dir="+dir,
	)
	return strings.Join(parts, " ")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "qai i18n: "+format+"\n", a...)
	os.Exit(1)
}
