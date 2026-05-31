// Package projectid resolves a stable project tag for the current
// working directory, used by qai note --tag auto-attachment, the
// upcoming joplin bridge's project: tag classifier, and any future
// per-project context lookup.
//
// The resolution rules — directional from the spec review:
//
//  0. `.qai/project` override file (one-line literal name). Walks up
//     from cwd; first hit wins. Beats every other rule. Use this when
//     the directory layout doesn't match what you want as the tag
//     (aliasing "qai-cli" → "qai", for instance).
//
//  1. Manifest-before-git-root walk. Starting at cwd, walk upward
//     looking for a manifest AND a .git directory simultaneously. The
//     FIRST one hit wins:
//       - package.json  →  .name
//       - Cargo.toml    →  [package].name (or workspace-root-name when
//                          we're at a workspace root)
//       - pyproject.toml → [project].name  (or [tool.poetry].name)
//       - go.mod        →  last path segment of the module declaration
//
//     Manifest-before-git-root handles monorepos correctly:
//     /work/quantum-encoding-ecosystem/cosmic-duck/src-tauri/...
//     walks up, hits cosmic-duck/Cargo.toml BEFORE /work/.../.git, so
//     the tag is "cosmic-duck" not "quantum-encoding-ecosystem".
//
//  2. Git-root basename. If no manifest appeared between cwd and the
//     git root, the git root's parent directory's basename wins. This
//     is the classic "I'm in a single-project repo with no manifest"
//     case (a shell-script project, a docs repo, an arbitrary tree).
//
//  3. Bare cwd basename, last-ditch. ONLY if the basename isn't in the
//     noise list — common subdirectory names ("src", "internal", "cmd",
//     "src-tauri", etc.) that would tag half the world identically.
//     A noise hit returns an error suggesting --no-auto-project or a
//     .qai/project override.
//
// Output is always sanitised through Sanitise: lowercased, _ and
// whitespace replaced with -, anything outside [a-z0-9-] stripped.
// This makes "Quantum-AI-Backend", "quantum_ai_backend", and
// "quantum ai backend" all resolve to "quantum-ai-backend" so the
// bootstrap query doesn't miss notes due to a casing typo.
package projectid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Source identifies which resolution rule produced the result. Useful
// for diagnostics (e.g. `qai project name --explain`) and for callers
// that want to log how an automatic tag came to be.
type Source string

const (
	SourceOverride    Source = "override"     // .qai/project file
	SourceManifest    Source = "manifest"     // package.json / Cargo.toml / pyproject.toml / go.mod
	SourceGitRoot     Source = "git-root"     // basename of the dir containing .git
	SourceCwdBasename Source = "cwd-basename" // last-ditch bare cwd
)

// Result is what Resolve returns — the sanitised tag, the rule that
// produced it, and (for manifest hits) the path of the manifest file
// so callers can audit / link to it.
type Result struct {
	Project      string // sanitised: lowercase, dasherised, [a-z0-9-]
	Source       Source
	ManifestPath string // populated only when Source == SourceManifest
	RawName      string // the unsanitised name before normalisation, for diagnostics
}

// ErrNoise is returned when the only available signal would be a bare
// cwd basename matching the noise list (src, internal, cmd, etc.).
// Callers should treat this as "auto-resolution declined; ask the
// human to pass --no-auto-project or write a .qai/project override".
var ErrNoise = errors.New("project resolver: refusing to tag with a noise-list basename")

// noiseSet is the closed list of subdirectory names that almost never
// identify a project on their own. Spec-pinned; expand sparingly and
// only with a recorded rationale.
var noiseSet = map[string]bool{
	"src":         true,
	"src-tauri":   true,
	"internal":    true,
	"cmd":         true,
	"lib":         true,
	"pkg":         true,
	"app":         true,
	"apps":        true,
	"frontend":    true,
	"backend":     true,
	"web":         true,
	"api":         true,
	"server":      true,
	"client":      true,
	"dist":        true,
	"build":       true,
	"target":      true,
	"node_modules": true,
	"vendor":      true,
}

// Resolve walks the resolution table for cwd and returns the first
// non-empty result. cwd is the starting directory — pass the actual
// process cwd; the caller is responsible for resolving symlinks if
// they care about canonical paths (we don't, since both the .git walk
// and the manifest walk treat paths as opaque strings).
func Resolve(cwd string) (*Result, error) {
	if cwd == "" {
		return nil, fmt.Errorf("project resolver: empty cwd")
	}
	cwd = filepath.Clean(cwd)

	// 0. .qai/project override — searched first, beats everything.
	if r := resolveOverride(cwd); r != nil {
		return r, nil
	}

	// 1+2. Manifest-before-git-root walk. We walk in one pass; the
	// first thing we hit decides the outcome. A manifest beats a
	// .git encounter at the same level (manifest is checked first in
	// the loop body), which gives the monorepo case the right answer.
	if r := resolveManifestOrGitRoot(cwd); r != nil {
		return r, nil
	}

	// 3. Bare cwd basename — only if it's not a noise-list hit.
	base := filepath.Base(cwd)
	if noiseSet[strings.ToLower(base)] {
		return nil, ErrNoise
	}
	return &Result{
		Project: Sanitise(base),
		Source:  SourceCwdBasename,
		RawName: base,
	}, nil
}

// resolveOverride walks upward looking for .qai/project. First hit
// wins; an empty file is treated as "no override" so a partially-set-
// up repo doesn't tag with "".
func resolveOverride(cwd string) *Result {
	dir := cwd
	for {
		p := filepath.Join(dir, ".qai", "project")
		if data, err := os.ReadFile(p); err == nil {
			name := strings.TrimSpace(string(data))
			if name != "" {
				return &Result{
					Project:      Sanitise(name),
					Source:       SourceOverride,
					ManifestPath: p,
					RawName:      name,
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// resolveManifestOrGitRoot walks from cwd up to the filesystem root.
// At each level it checks for any manifest first; if found, returns
// immediately. .git presence is tracked so when we finally hit the
// root (or run out of parents) we can fall back to the git-root rule.
//
// This is the load-bearing step for monorepos: the inner-package
// manifest will always be found before the outer .git.
func resolveManifestOrGitRoot(cwd string) *Result {
	var gitRoot string
	dir := cwd
	for {
		if r := tryManifests(dir); r != nil {
			return r
		}
		if gitRoot == "" {
			if isDir(filepath.Join(dir, ".git")) {
				gitRoot = dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if gitRoot != "" {
		base := filepath.Base(gitRoot)
		if !noiseSet[strings.ToLower(base)] {
			return &Result{
				Project:      Sanitise(base),
				Source:       SourceGitRoot,
				ManifestPath: filepath.Join(gitRoot, ".git"),
				RawName:      base,
			}
		}
	}
	return nil
}

// tryManifests checks dir for each known manifest in priority order
// and returns a Result for the first hit. The priority is:
//
//	package.json → Cargo.toml → pyproject.toml → go.mod
//
// Rationale: when multiple coexist (rare but happens — a Rust crate
// with a package.json for its docs site, for instance), the developer
// almost always considers the package.json's `name` to be canonical
// for the npm-shaped artifact, while Cargo.toml is canonical for the
// crate. We pick a stable order rather than guess; users with mixed
// projects can override via .qai/project.
func tryManifests(dir string) *Result {
	if r := parsePackageJSON(filepath.Join(dir, "package.json")); r != nil {
		return r
	}
	if r := parseCargoToml(filepath.Join(dir, "Cargo.toml")); r != nil {
		return r
	}
	if r := parsePyprojectToml(filepath.Join(dir, "pyproject.toml")); r != nil {
		return r
	}
	if r := parseGoMod(filepath.Join(dir, "go.mod")); r != nil {
		return r
	}
	return nil
}

// ── manifest parsers ──────────────────────────────────────────────────────
//
// These are deliberately string-grep based rather than full TOML/JSON
// decoders. The resolver runs on every `qai note` call (and on every
// session bootstrap once the bridge ships), so it has to be fast and
// dependency-free. The grep is keyed to the canonical surface
// (single-line, double-quoted, top-level `name = "..."`) — anything
// pathological falls through to the next rule, which is almost always
// what the human wants anyway.

var (
	packageJSONNameRe   = regexp.MustCompile(`"name"\s*:\s*"([^"\\]+)"`)
	cargoTopNameRe      = regexp.MustCompile(`(?m)^name\s*=\s*"([^"\n]+)"`)
	pyprojectNameRe     = regexp.MustCompile(`(?m)^name\s*=\s*"([^"\n]+)"`)
	goModModuleRe       = regexp.MustCompile(`(?m)^module\s+(\S+)`)
)

func parsePackageJSON(path string) *Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := packageJSONNameRe.FindStringSubmatch(string(data))
	if len(m) != 2 {
		return nil
	}
	raw := m[1]
	// npm scoped packages: "@scope/pkg-name". The pkg part is what
	// developers usually think of as the project; the scope is a
	// distribution-channel prefix that pollutes tag-space.
	if strings.HasPrefix(raw, "@") {
		if i := strings.Index(raw, "/"); i >= 0 {
			raw = raw[i+1:]
		}
	}
	if raw == "" {
		return nil
	}
	return &Result{
		Project:      Sanitise(raw),
		Source:       SourceManifest,
		ManifestPath: path,
		RawName:      raw,
	}
}

// parseCargoToml extracts the package name. Handles two cases:
//
//   - Plain crate: `[package]` section with `name = "..."`. We only
//     accept a top-level `name = "..."` line; sub-section names (e.g.
//     `[[bin]] name = "..."`) are ignored.
//   - Workspace root: `[workspace]` with no `[package]`. We currently
//     fall through (return nil) — the caller will walk up and either
//     hit an outer manifest or use the git-root rule. A future
//     improvement is to read [workspace] members and pick the first.
func parseCargoToml(path string) *Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := string(data)
	pkgIdx := strings.Index(body, "[package]")
	if pkgIdx < 0 {
		return nil // workspace root or non-package manifest
	}
	// Scope the regex search to the [package] block — between
	// [package] and the next [section] header.
	scope := body[pkgIdx:]
	if next := strings.Index(scope[1:], "\n["); next >= 0 {
		scope = scope[:next+1]
	}
	m := cargoTopNameRe.FindStringSubmatch(scope)
	if len(m) != 2 || m[1] == "" {
		return nil
	}
	return &Result{
		Project:      Sanitise(m[1]),
		Source:       SourceManifest,
		ManifestPath: path,
		RawName:      m[1],
	}
}

// parsePyprojectToml extracts [project].name (PEP 621) or
// [tool.poetry].name as a fallback. We scan both sections;
// [project] wins when both are present.
func parsePyprojectToml(path string) *Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := string(data)
	for _, header := range []string{"[project]", "[tool.poetry]"} {
		hIdx := strings.Index(body, header)
		if hIdx < 0 {
			continue
		}
		scope := body[hIdx:]
		if next := strings.Index(scope[1:], "\n["); next >= 0 {
			scope = scope[:next+1]
		}
		m := pyprojectNameRe.FindStringSubmatch(scope)
		if len(m) == 2 && m[1] != "" {
			return &Result{
				Project:      Sanitise(m[1]),
				Source:       SourceManifest,
				ManifestPath: path,
				RawName:      m[1],
			}
		}
	}
	return nil
}

// parseGoMod reads the module declaration and uses the last path
// segment as the project name. "github.com/quantum-encoding/qai-cli"
// → "qai-cli", not "quantum-encoding" — the full path is too noisy as
// a tag, and the last segment matches how every Go developer says
// "the qai-cli module" in practice.
func parseGoMod(path string) *Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := goModModuleRe.FindStringSubmatch(string(data))
	if len(m) != 2 || m[1] == "" {
		return nil
	}
	raw := m[1]
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	if raw == "" {
		return nil
	}
	return &Result{
		Project:      Sanitise(raw),
		Source:       SourceManifest,
		ManifestPath: path,
		RawName:      raw,
	}
}

// ── helpers ───────────────────────────────────────────────────────────────

var sanitiseStripRe = regexp.MustCompile(`[^a-z0-9-]+`)

// Sanitise normalises a project name into the canonical tag form:
// lowercase, underscores/whitespace replaced with `-`, anything
// outside [a-z0-9-] stripped, runs of dashes collapsed, leading and
// trailing dashes removed. The transformation is idempotent so
// re-sanitising an already-sanitised input is a no-op.
func Sanitise(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Map underscore + whitespace to dash before stripping — without
	// this, "quantum_ai_backend" would become "quantumaibackend".
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "\t", "-")
	// Now strip anything still outside the allowed set.
	s = sanitiseStripRe.ReplaceAllString(s, "-")
	// Collapse runs of dashes, trim ends.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	return s
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	return fi.IsDir()
}
