package projectid

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitise(t *testing.T) {
	cases := []struct{ in, want string }{
		{"quantum-ai-backend", "quantum-ai-backend"},
		{"Quantum-AI-Backend", "quantum-ai-backend"},
		{"quantum_ai_backend", "quantum-ai-backend"},
		{"quantum ai backend", "quantum-ai-backend"},
		{"  Quantum  AI  Backend  ", "quantum-ai-backend"},
		{"foo--bar---baz", "foo-bar-baz"},
		{"-foo-", "foo"},
		{"weird/path:slug", "weird-path-slug"},
		{"@scope/pkg", "scope-pkg"},
		{"", ""},
	}
	for _, c := range cases {
		got := Sanitise(c.in)
		if got != c.want {
			t.Errorf("Sanitise(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestResolveOverride pins the .qai/project rule as the highest-priority
// signal — even with a misleading manifest in the way, the override wins.
func TestResolveOverride(t *testing.T) {
	root := t.TempDir()
	// Manifest that would otherwise win:
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/x/wrong-name\n")
	// Override that should beat it:
	mustMkdirAll(t, filepath.Join(root, ".qai"))
	writeFile(t, filepath.Join(root, ".qai", "project"), "the-real-name\n")

	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Source != SourceOverride {
		t.Errorf("Source = %v, want %v", r.Source, SourceOverride)
	}
	if r.Project != "the-real-name" {
		t.Errorf("Project = %q, want %q", r.Project, "the-real-name")
	}
}

// TestResolveOverrideFromSubdir confirms the .qai/project override is
// found by walking upward from a deep cwd — typical case is the file
// at the repo root with the user invoked from a nested package dir.
func TestResolveOverrideFromSubdir(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".qai"))
	writeFile(t, filepath.Join(root, ".qai", "project"), "rooted-name\n")
	deep := filepath.Join(root, "src", "internal", "deep")
	mustMkdirAll(t, deep)

	r, err := Resolve(deep)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Source != SourceOverride || r.Project != "rooted-name" {
		t.Errorf("got %+v, want override=rooted-name", r)
	}
}

// TestResolveOverrideEmpty confirms an empty override file is ignored —
// otherwise a partially-set-up repo would tag everything with "".
func TestResolveOverrideEmpty(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".qai"))
	writeFile(t, filepath.Join(root, ".qai", "project"), "   \n")
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/x/from-gomod\n")

	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Source != SourceManifest || r.Project != "from-gomod" {
		t.Errorf("got %+v, want manifest=from-gomod", r)
	}
}

// TestMonorepoManifestBeatsOuterGit is the load-bearing case the spec
// reviewer flagged: a Cargo crate under a monorepo's outer .git should
// tag with the crate name, not the monorepo's root basename. Without
// the manifest-before-git-root rule the bridge would lump every
// package under the same tag.
func TestMonorepoManifestBeatsOuterGit(t *testing.T) {
	root := t.TempDir()
	// Outer monorepo with .git at top
	monorepo := filepath.Join(root, "quantum-encoding-ecosystem")
	mustMkdirAll(t, filepath.Join(monorepo, ".git"))
	// Inner crate with its own Cargo.toml
	crate := filepath.Join(monorepo, "cosmic-duck", "src-tauri")
	mustMkdirAll(t, crate)
	writeFile(t, filepath.Join(monorepo, "cosmic-duck", "Cargo.toml"), `
[package]
name = "cosmic-duck"
version = "0.1.0"
`)

	r, err := Resolve(crate)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Source != SourceManifest {
		t.Errorf("Source = %v, want manifest (monorepo should resolve via inner Cargo.toml)", r.Source)
	}
	if r.Project != "cosmic-duck" {
		t.Errorf("Project = %q, want cosmic-duck (NOT quantum-encoding-ecosystem)", r.Project)
	}
}

// TestGitRootFallback covers the bare-shell-repo case: a git repo with
// no manifest at all → use the git root's basename.
func TestGitRootFallback(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "my-shell-scripts")
	mustMkdirAll(t, filepath.Join(repo, ".git"))
	mustMkdirAll(t, filepath.Join(repo, "scripts"))

	r, err := Resolve(filepath.Join(repo, "scripts"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Source != SourceGitRoot {
		t.Errorf("Source = %v, want git-root", r.Source)
	}
	if r.Project != "my-shell-scripts" {
		t.Errorf("Project = %q, want my-shell-scripts", r.Project)
	}
}

// TestNoiseListRefusal pins the contract that a bare "src" cwd does
// NOT silently tag every project's src dir with the same value — the
// reviewer's specific concern. The resolver returns ErrNoise so the
// caller can fall back to --no-auto-project advice.
func TestNoiseListRefusal(t *testing.T) {
	root := t.TempDir()
	// No git, no manifest. Just a directory called "src".
	noisy := filepath.Join(root, "src")
	mustMkdirAll(t, noisy)

	_, err := Resolve(noisy)
	if err != ErrNoise {
		t.Fatalf("Resolve: got %v, want ErrNoise", err)
	}
}

// TestPackageJSONScopedName confirms npm scoped packages strip the
// @scope/ prefix — "@quantum-encoding/qai-cli" → "qai-cli".
func TestPackageJSONScopedName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"name": "@quantum-encoding/qai-cli", "version": "0.1.0"}`)
	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Project != "qai-cli" {
		t.Errorf("Project = %q, want qai-cli", r.Project)
	}
}

// TestGoModLastSegment confirms the module path collapses to its last
// segment — "github.com/quantum-encoding/qai-cli" → "qai-cli", not
// "quantum-encoding".
func TestGoModLastSegment(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module github.com/quantum-encoding/qai-cli\n\ngo 1.22\n")
	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Project != "qai-cli" {
		t.Errorf("Project = %q, want qai-cli", r.Project)
	}
}

// TestPyprojectPep621 covers the modern [project].name shape.
func TestPyprojectPep621(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pyproject.toml"), `
[project]
name = "my-py-tool"
version = "0.1.0"
`)
	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Project != "my-py-tool" {
		t.Errorf("Project = %q, want my-py-tool", r.Project)
	}
}

// TestPyprojectPoetryFallback confirms older Poetry-only manifests work
// when [project] isn't present.
func TestPyprojectPoetryFallback(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "pyproject.toml"), `
[tool.poetry]
name = "legacy-tool"
version = "0.1.0"
`)
	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Project != "legacy-tool" {
		t.Errorf("Project = %q, want legacy-tool", r.Project)
	}
}

// TestCargoSkipsBinSectionName confirms [[bin]] sections' name don't
// leak through as the project name when [package].name is present.
func TestCargoSkipsBinSectionName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cargo.toml"), `
[package]
name = "my-crate"
version = "0.1.0"

[[bin]]
name = "the-binary"
path = "src/main.rs"
`)
	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Project != "my-crate" {
		t.Errorf("Project = %q, want my-crate (NOT the-binary)", r.Project)
	}
}

// TestSanitiseAppliedToManifest confirms that manifest-extracted names
// also go through the sanitiser — a "Quantum AI Backend" name field
// becomes "quantum-ai-backend", matching what we'd want for a tag.
func TestSanitiseAppliedToManifest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"name": "Quantum AI Backend"}`)
	r, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Project != "quantum-ai-backend" {
		t.Errorf("Project = %q, want quantum-ai-backend", r.Project)
	}
}

// ── tiny test helpers ────────────────────────────────────────────────────

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
