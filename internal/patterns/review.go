package patterns

// review.go — `qai patterns review`
//
// Implements the three-query contract from `30-interceptor-agent.md`:
//
//   1. SMELL    — `polarity = 'smell'` patterns whose triggers appear in the
//                 diff/file (substring match against pattern.triggers).
//   2. CONTROL  — `polarity = 'control'` patterns for this file's role whose
//                 expected guard triggers are ABSENT from the diff.
//   3. AFFINITY — `polarity = 'smell'` patterns whose `applies_to_roles`
//                 contains this file's role but no trigger matched. Advisory.
//
// The interceptor LLM judges remediation-satisfaction per file 30 step 4 —
// this function only retrieves candidates and reports what matched on what,
// so the LLM can dismiss spurious smell hits when the guard is clearly
// already present.
//
// Two modes:
//   * `qai patterns review <repo>:<file>` — interactive. Resolves role via
//     code_file lookup, runs the three queries against the file's current
//     content + any --imports passed.
//   * `qai patterns review --diff [file|-]` — diff mode. Parses a unified
//     diff (stdin or file), infers each changed file's role via
//     blast.ClassifyFile against the added lines, runs the queries, emits
//     JSON ready for an LLM to consume per file 30's system prompt.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/blast"
)

// ---------------------------------------------------------------------------
// Public dispatch entry
// ---------------------------------------------------------------------------

func runReview(args []string) {
	fs := flag.NewFlagSet("patterns review", flag.ExitOnError)
	diffMode := fs.Bool("diff", false, "read a unified diff from stdin and emit candidate JSON")
	diffFilePath := fs.String("diff-file", "", "read the diff from this path instead of stdin")
	repoRoot := fs.String("repo-root", "", "filesystem root the diff paths are relative to (default: cwd); control checks load the full post-change file from here")
	asJSON := fs.Bool("json", false, "emit JSON (always implied in --diff mode)")
	importsArg := fs.String("imports", "", "comma-separated import/symbol list (interactive mode)")
	roleOverride := fs.String("role", "", "skip code_file lookup and use this role directly")
	getOpts := connectFlags(fs)
	fs.Parse(args)

	c := mustClient(getOpts())

	if *diffMode || *diffFilePath != "" {
		runReviewDiff(c, *diffFilePath, *repoRoot)
		return
	}

	if fs.NArg() != 1 && *roleOverride == "" {
		fmt.Fprintln(os.Stderr, "qai patterns review: pass <repo>:<file>, or --role, or --diff")
		os.Exit(2)
	}

	var repoName, relPath string
	if fs.NArg() == 1 {
		arg := fs.Arg(0)
		if i := strings.Index(arg, ":"); i > 0 {
			repoName, relPath = arg[:i], arg[i+1:]
		} else {
			fmt.Fprintln(os.Stderr, "qai patterns review: use <repo>:<file> form (e.g. quantum-backend:internal/server/routes_chat.go)")
			os.Exit(2)
		}
	}
	runReviewInteractive(c, repoName, relPath, *roleOverride, splitCSV(*importsArg), *asJSON)
}

// ---------------------------------------------------------------------------
// Shared types
// ---------------------------------------------------------------------------

type patternForReview struct {
	Slug           string   `json:"slug"`
	Name           string   `json:"name"`
	Category       string   `json:"category"`
	Detection      string   `json:"detection"`
	Remediation    string   `json:"remediation"`
	Polarity       string   `json:"polarity"`
	Triggers       []string `json:"triggers"`
	AppliesToRoles []string `json:"applies_to_roles"`
}

// Candidate is one match emitted by the three-query union.
type Candidate struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	Polarity     string `json:"polarity"`
	Kind         string `json:"kind"` // "smell" | "control" | "affinity"
	MatchedOn    string `json:"matched_on"`
	Detection    string `json:"detection,omitempty"`
	Remediation  string `json:"remediation,omitempty"`
	SeverityHint string `json:"severity_hint,omitempty"`
}

// ---------------------------------------------------------------------------
// SurrealQL — fetch the pattern pools for client-side substring matching
// ---------------------------------------------------------------------------

const fetchPoolsSQL = `
SELECT slug, name, category, detection, remediation, polarity, triggers, applies_to_roles
FROM failure_pattern WHERE polarity = 'smell' AND array::len(triggers ?? []) > 0;

SELECT slug, name, category, detection, remediation, polarity, triggers, applies_to_roles
FROM failure_pattern WHERE polarity = 'smell' AND array::len(applies_to_roles ?? []) > 0;

SELECT slug, name, category, detection, remediation, polarity, triggers, applies_to_roles
FROM failure_pattern WHERE polarity = 'control';
`

// fetchPools fetches three slices in one HTTP round-trip:
//   - smellByTrigger:  smell patterns that carry at least one trigger
//   - smellByRole:     smell patterns with applies_to_roles populated (affinity)
//   - control:         every control pattern (small set)
func fetchPools(c *blast.Client) (smellByTrigger, smellByRole, control []patternForReview, err error) {
	results, err := c.Exec(fetchPoolsSQL)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(results) < 3 {
		return nil, nil, nil, fmt.Errorf("expected 3 result sets, got %d", len(results))
	}
	if e := blast.FirstError(results); e != nil {
		return nil, nil, nil, e
	}
	if err = json.Unmarshal(results[0].Result, &smellByTrigger); err != nil {
		return
	}
	if err = json.Unmarshal(results[1].Result, &smellByRole); err != nil {
		return
	}
	if err = json.Unmarshal(results[2].Result, &control); err != nil {
		return
	}
	return
}

// ---------------------------------------------------------------------------
// Candidate matching (the core: text-substring + role logic)
// ---------------------------------------------------------------------------

// matchCandidates runs the three-query union for one file/diff-fragment.
//
// The smell/control asymmetry is load-bearing:
//
//   - SMELL is evaluated against `smellText` — what just got introduced.
//     A `+` line containing http.DefaultClient is a new SSRF risk.
//   - CONTROL is evaluated against `controlText` — the whole post-change
//     file. "Guard absent" only means something if you've looked at the
//     entire file. Evaluating it against the diff hunk alone would fire
//     on every routine handler edit (rate limiter lives in main.go,
//     handler being touched lives elsewhere). That's a false-positive
//     factory and would kill the pre-commit hook on contact.
//   - controlVisible says whether controlText is the *real* post-change
//     file (true, from disk) vs a stand-in (false, e.g. we only have the
//     diff hunk). When false, control checks are skipped — better silent
//     than wrong; the LLM can be re-invoked against the real file later.
//
// AFFINITY is role-only and doesn't consume either text.
func matchCandidates(role, smellText, controlText string, controlVisible bool, smellByTrigger, smellByRole, control []patternForReview) []Candidate {
	var out []Candidate
	matched := map[string]bool{} // slug → true to dedupe across the three buckets

	// 1. SMELL — triggers appear in the smell text (diff additions).
	for _, p := range smellByTrigger {
		hitTrigger := ""
		for _, t := range p.Triggers {
			if t == "" {
				continue
			}
			if strings.Contains(smellText, t) {
				hitTrigger = t
				break
			}
		}
		if hitTrigger == "" {
			continue
		}
		matched[p.Slug] = true
		out = append(out, Candidate{
			Slug: p.Slug, Name: p.Name, Category: p.Category,
			Polarity: p.Polarity, Kind: "smell",
			MatchedOn:    "trigger: " + hitTrigger,
			Detection:    p.Detection,
			Remediation:  p.Remediation,
			SeverityHint: severityFromCategory(p.Category),
		})
	}

	// 2. CONTROL — file's role is at-risk AND no guard appears in the
	// FULL post-change file. Suppressed entirely when controlVisible is
	// false (we'd otherwise false-positive on every routine handler edit).
	if role != "" && controlVisible {
		for _, p := range control {
			if !containsStr(p.AppliesToRoles, role) {
				continue
			}
			anyGuardPresent := false
			for _, t := range p.Triggers {
				if t == "" {
					continue
				}
				if strings.Contains(controlText, t) {
					anyGuardPresent = true
					break
				}
			}
			if anyGuardPresent {
				continue
			}
			matched[p.Slug] = true
			out = append(out, Candidate{
				Slug: p.Slug, Name: p.Name, Category: p.Category,
				Polarity: p.Polarity, Kind: "control",
				MatchedOn:    "role:" + role + " + guard absent in full file",
				Detection:    p.Detection,
				Remediation:  p.Remediation,
				SeverityHint: severityFromCategory(p.Category),
			})
		}
	}

	// 3. AFFINITY — role-applicable smell patterns NOT already raised as a
	// trigger hit. Surfaced as advisory; the LLM decides whether to raise.
	if role != "" {
		for _, p := range smellByRole {
			if matched[p.Slug] {
				continue
			}
			if !containsStr(p.AppliesToRoles, role) {
				continue
			}
			out = append(out, Candidate{
				Slug: p.Slug, Name: p.Name, Category: p.Category,
				Polarity: p.Polarity, Kind: "affinity",
				MatchedOn:    "role:" + role,
				Detection:    p.Detection,
				Remediation:  p.Remediation,
				SeverityHint: severityFromCategory(p.Category),
			})
		}
	}
	return out
}

// severityFromCategory follows file 30's hint:
//
//	injection / ssrf / billing / crypto / sandboxing → high (or critical)
//	ui_state / i18n / observability                  → low (or medium)
//	everything else                                   → medium
func severityFromCategory(c string) string {
	switch c {
	case "injection", "ssrf", "crypto", "sandboxing":
		return "high"
	case "billing", "auth", "permission":
		return "high"
	case "ui_state", "i18n", "observability":
		return "low"
	}
	return "medium"
}

// ---------------------------------------------------------------------------
// Interactive mode: resolve role from code_file, then run the union
// ---------------------------------------------------------------------------

func runReviewInteractive(c *blast.Client, repoName, relPath, roleOverride string, imports []string, asJSON bool) {
	role := roleOverride
	if role == "" {
		sql := fmt.Sprintf(
			`SELECT VALUE role FROM code_file WHERE repo.name = %s AND path = %s LIMIT 1;`,
			quote(repoName), quote(relPath),
		)
		results, err := c.Exec(sql)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai patterns review: %v\n", err)
			os.Exit(1)
		}
		var roles []string
		_ = firstResult(results, &roles)
		if len(roles) == 0 {
			fmt.Fprintf(os.Stderr, "no code_file row for %s:%s — pass --role to override, or run `qai blast ingest` first\n", repoName, relPath)
			os.Exit(1)
		}
		role = roles[0]
	}

	smellByTrigger, smellByRole, control, err := fetchPools(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns review: %v\n", err)
		os.Exit(1)
	}

	// In interactive mode the substring corpus is whatever --imports the
	// caller provided (whitespace-joined). If empty, smell candidates
	// just don't fire — affinity still does. Control is suppressed in
	// interactive mode unless we explicitly load the file (TODO: read
	// from <repo>:<file> path when available).
	triggerText := strings.Join(imports, "\n")
	candidates := matchCandidates(role, triggerText, "", false, smellByTrigger, smellByRole, control)

	if asJSON {
		out := struct {
			Role       string      `json:"role"`
			RepoName   string      `json:"repo,omitempty"`
			Path       string      `json:"path,omitempty"`
			Candidates []Candidate `json:"candidates"`
		}{Role: role, RepoName: repoName, Path: relPath, Candidates: candidates}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}

	// Text output.
	fmt.Printf("Role: %s", role)
	if repoName != "" {
		fmt.Printf("   (%s:%s)", repoName, relPath)
	}
	fmt.Println()

	if len(candidates) == 0 {
		fmt.Println("\n(no candidate patterns for this role/imports combination)")
		return
	}

	byKind := map[string][]Candidate{}
	for _, c := range candidates {
		byKind[c.Kind] = append(byKind[c.Kind], c)
	}
	for _, kind := range []string{"smell", "control", "affinity"} {
		bucket := byKind[kind]
		if len(bucket) == 0 {
			continue
		}
		fmt.Printf("\n%s:\n", strings.ToUpper(kind))
		for _, cc := range bucket {
			fmt.Printf("  [%s] %s  (%s)\n", cc.SeverityHint, cc.Name, cc.Category)
			fmt.Printf("      ↳ %s\n", cc.MatchedOn)
			if cc.Detection != "" {
				fmt.Printf("      Q: %s\n", cc.Detection)
			}
			if cc.Remediation != "" {
				fmt.Printf("      →  %s\n", cc.Remediation)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Diff mode: parse unified diff, infer role per file, emit JSON
// ---------------------------------------------------------------------------

// diffFile is one file's contribution to a unified diff: its post-change
// path and the concatenated added lines (no leading "+", newline-terminated).
type diffFile struct {
	Path  string `json:"path"`
	Added string `json:"added"`
}

func runReviewDiff(c *blast.Client, diffPath, repoRoot string) {
	var r io.Reader = os.Stdin
	if diffPath != "" && diffPath != "-" {
		f, err := os.Open(diffPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai patterns review --diff: open %s: %v\n", diffPath, err)
			os.Exit(1)
		}
		defer f.Close()
		r = f
	}

	files := parseUnifiedDiff(r)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "qai patterns review --diff: no files parsed from diff")
		os.Exit(1)
	}

	if repoRoot == "" {
		repoRoot, _ = os.Getwd()
	}

	smellByTrigger, smellByRole, control, err := fetchPools(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns review --diff: %v\n", err)
		os.Exit(1)
	}

	type fileReport struct {
		Path         string      `json:"path"`
		Role         string      `json:"role"`
		Added        string      `json:"added"`
		FullVisible  bool        `json:"full_file_visible"`
		Candidates   []Candidate `json:"candidates"`
	}
	type report struct {
		Files []fileReport `json:"files"`
	}

	out := report{}
	for _, f := range files {
		// Try to load the full post-change file from disk. Used by control
		// checks; smell checks still match against the diff additions only.
		// If the read fails (new file before commit, repo-root mismatch,
		// rename without disk copy), control checks are suppressed for
		// this file rather than running over the diff hunk and false-positiving.
		var controlText string
		fullVisible := false
		if body, readErr := os.ReadFile(filepath.Join(repoRoot, f.Path)); readErr == nil {
			controlText = string(body)
			fullVisible = true
		}

		// Role inference: prefer the on-disk content when we have it
		// (post-change view of the whole file). Fall back to the added
		// text otherwise so new files still get a role from their +lines.
		var roleContent []byte
		if fullVisible {
			roleContent = []byte(controlText)
		} else {
			roleContent = []byte(f.Added)
		}
		role, _, _ := blast.ClassifyFile(f.Path, roleContent)

		candidates := matchCandidates(role, f.Added, controlText, fullVisible,
			smellByTrigger, smellByRole, control)
		out.Files = append(out.Files, fileReport{
			Path:        f.Path,
			Role:        role,
			Added:       f.Added,
			FullVisible: fullVisible,
			Candidates:  candidates,
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns review --diff: encode: %v\n", err)
		os.Exit(1)
	}
}

// parseUnifiedDiff handles the standard `git diff` / `diff -u` format:
//
//	diff --git a/X b/Y
//	--- a/X
//	+++ b/Y
//	@@ -...
//	-removed
//	+added
//
// Returns one entry per changed file with the new-side path (b/...) and the
// concatenated added-line bodies (no leading "+"). Binary files and deletes
// (where the +++ side is /dev/null) are skipped.
//
// Conservative parser: anything that doesn't fit the shape above is ignored
// rather than guessed at. Matches the existing scan.go principle of
// "missing edge > wrong edge."
func parseUnifiedDiff(r io.Reader) []diffFile {
	var files []diffFile
	var cur *diffFile

	flush := func() {
		if cur != nil && cur.Path != "" {
			files = append(files, *cur)
		}
		cur = nil
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &diffFile{}
		case strings.HasPrefix(line, "+++ "):
			if cur == nil {
				cur = &diffFile{}
			}
			path := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			if path == "/dev/null" {
				// deletion: nothing post-change to review
				cur = nil
				continue
			}
			// Git quotes paths with spaces or special chars: `+++ "b/has space.go"`
			// with C-style escapes for \n / \t / \\ etc. Use strconv.Unquote to
			// reverse it (which handles escapes correctly); fall back to raw
			// trim if that fails (Unquote rejects invalid escapes).
			if strings.HasPrefix(path, `"`) && strings.HasSuffix(path, `"`) {
				if unq, err := strconv.Unquote(path); err == nil {
					path = unq
				} else {
					path = strings.Trim(path, `"`)
				}
			}
			path = strings.TrimPrefix(path, "b/")
			cur.Path = path
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if cur != nil {
				cur.Added += line[1:] + "\n"
			}
		}
	}
	flush()
	return files
}

// containsStr is a tiny helper — Go's stdlib has slices.Contains in 1.21+
// but we keep this for clarity against older builds.
func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
