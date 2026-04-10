package graph

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func CmdGraph(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `usage: qai graph <path> [options]

Generates visual dependency/call graphs for codebases.

Options:
  --type <type>     Graph type: calls (default), modules, deps, mir
  -o <file>         Output file (default: graph.svg)
  --format <fmt>    Output format: svg (default), dot, png

Types:
  calls     Call graph (functions → function calls). Uses codebase_deity (tree-sitter)
            or Rust syn parser for .rs files.
  modules   Module/type dependency graph (cargo-modules for Rust)
  deps      Crate/package dependency graph
  mir       MIR control flow graph (Rust nightly only)

Examples:
  qai graph ./src                       # call graph as SVG
  qai graph ./src --type modules        # module dependency graph
  qai graph ./src -o callgraph.dot      # DOT format for custom rendering
  qai graph . --type deps               # package dependency graph`)
		os.Exit(1)
	}

	dir := args[0]
	graphType := "calls"
	output := "graph.svg"
	outputFmt := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--type", "-t":
			if i+1 < len(args) {
				graphType = args[i+1]
				i++
			}
		case "-o", "--output":
			if i+1 < len(args) {
				output = args[i+1]
				i++
			}
		case "--format", "-f":
			if i+1 < len(args) {
				outputFmt = args[i+1]
				i++
			}
		}
	}

	// Auto-detect format from output extension.
	if outputFmt == "" {
		ext := filepath.Ext(output)
		switch ext {
		case ".dot":
			outputFmt = "dot"
		case ".png":
			outputFmt = "png"
		default:
			outputFmt = "svg"
		}
	}

	absDir, _ := filepath.Abs(dir)

	switch graphType {
	case "calls":
		graphCalls(absDir, output, outputFmt)
	case "modules":
		graphModules(absDir, output, outputFmt)
	case "deps":
		graphDeps(absDir, output, outputFmt)
	case "mir":
		graphMIR(absDir, output)
	default:
		fmt.Fprintf(os.Stderr, "qai graph: unknown type %q (use: calls, modules, deps, mir)\n", graphType)
		os.Exit(1)
	}
}

func graphCalls(dir, output, format string) {
	// Check for codebase_deity first (supports 15 languages).
	deity, _ := exec.LookPath("codebase_deity")
	if deity != "" {
		fmt.Fprintf(os.Stderr, "Using codebase_deity (tree-sitter, 15 languages)...\n")

		// Run deity to get JSON, then use the scanner's SVG renderer.
		cmd := exec.Command(deity, "--path", dir)
		jsonOut, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "codebase_deity failed: %v\n", err)
			os.Exit(1)
		}

		// Write JSON to temp file, then use Go scanner to render SVG.
		tmpJSON := filepath.Join(os.TempDir(), "deity-output.json")
		_ = os.WriteFile(tmpJSON, jsonOut, 0644) // temp file, non-critical

		// Generate DOT from the call edges.
		dotContent := deityJSONToDot(jsonOut, filepath.Base(dir))
		if format == "dot" {
			if err := os.WriteFile(output, []byte(dotContent), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "qai graph: write %s: %v\n", output, err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Wrote %s (DOT format)\n", output)
			return
		}

		// Render with graphviz.
		renderDot(dotContent, output, format)
		return
	}

	// Fallback: check if it's a Rust project — use cargo-call-stack or syn.
	if isRustProject(dir) {
		fmt.Fprintf(os.Stderr, "Rust project detected. Install codebase_deity for best results.\n")
		fmt.Fprintf(os.Stderr, "Trying cargo-modules as fallback...\n")
		graphModules(dir, output, format)
		return
	}

	fmt.Fprintf(os.Stderr, "qai graph: codebase_deity not found on PATH.\n")
	fmt.Fprintf(os.Stderr, "Install from: https://github.com/quantum-encoding/codebase-deity\n")
	os.Exit(1)
}

func graphModules(dir, output, format string) {
	if !isRustProject(dir) {
		fmt.Fprintln(os.Stderr, "qai graph modules: only supported for Rust projects (needs Cargo.toml)")
		os.Exit(1)
	}

	cargoModules, _ := exec.LookPath("cargo-modules")
	if cargoModules == "" {
		fmt.Fprintln(os.Stderr, "Install: cargo install cargo-modules")
		os.Exit(1)
	}

	cmd := exec.Command("cargo", "modules", "generate", "graph", "--with-types")
	cmd.Dir = dir
	dotOut, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cargo-modules failed: %v\n", err)
		os.Exit(1)
	}

	if format == "dot" {
		os.WriteFile(output, dotOut, 0644)
		fmt.Fprintf(os.Stderr, "Wrote %s\n", output)
		return
	}

	renderDot(string(dotOut), output, format)
}

func graphDeps(dir, output, format string) {
	if isRustProject(dir) {
		cmd := exec.Command("cargo", "metadata", "--format-version=1", "--no-deps")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cargo metadata failed: %v\n", err)
			os.Exit(1)
		}
		// Simple DOT from packages.
		dotContent := metadataToDot(out, filepath.Base(dir))
		if format == "dot" {
			if err := os.WriteFile(output, []byte(dotContent), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "qai graph: write %s: %v\n", output, err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Wrote %s\n", output)
			return
		}
		renderDot(dotContent, output, format)
		return
	}

	// Check for package.json (Node).
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		fmt.Fprintln(os.Stderr, "Node project: run 'npx depcruise --output-type dot src | dot -Tsvg -o deps.svg'")
		os.Exit(0)
	}

	// Check for go.mod.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		fmt.Fprintln(os.Stderr, "Go project: run 'go mod graph' for dependency list")
		os.Exit(0)
	}

	fmt.Fprintln(os.Stderr, "qai graph deps: no supported project file found (Cargo.toml, package.json, go.mod)")
	os.Exit(1)
}

func graphMIR(dir, output string) {
	if !isRustProject(dir) {
		fmt.Fprintln(os.Stderr, "qai graph mir: only for Rust projects (needs nightly)")
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "MIR CFG requires nightly Rust:")
	fmt.Fprintln(os.Stderr, "  rustup override set nightly")
	fmt.Fprintln(os.Stderr, "  RUSTFLAGS=\"-Z dump-mir=all -Z dump-mir-graphviz\" cargo build")
	fmt.Fprintln(os.Stderr, "  dot -Tsvg mir_dump/<function>.dot -o mir.svg")
}

// ── Helpers ────────────────────────────────────────────────────────────────

func isRustProject(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "Cargo.toml"))
	return err == nil
}

func renderDot(dot, output, format string) {
	dotBin, _ := exec.LookPath("dot")
	if dotBin == "" {
		// Write DOT and suggest online renderer.
		dotPath := strings.TrimSuffix(output, filepath.Ext(output)) + ".dot"
		if err := os.WriteFile(dotPath, []byte(dot), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "qai graph: write %s: %v\n", dotPath, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "graphviz not installed. Wrote %s\n", dotPath)
		fmt.Fprintln(os.Stderr, "View at: https://dreampuf.github.io/GraphvizOnline/")
		return
	}

	cmd := exec.Command("dot", "-T"+format, "-o", output)
	cmd.Stdin = strings.NewReader(dot)
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "dot render failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Wrote %s\n", output)
}

func deityJSONToDot(jsonData []byte, title string) string {
	// Parse deity JSON and extract call edges for DOT graph.
	// Deity outputs: { functions: [...], call_edges: [{caller, callee}] }
	// We simplify to DOT format.

	var b strings.Builder
	b.WriteString(fmt.Sprintf("digraph \"%s\" {\n", title))
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [shape=box, style=filled, fillcolor=\"#1e1e2e\", fontcolor=\"#cdd6f4\", fontname=\"monospace\"];\n")
	b.WriteString("  edge [color=\"#89b4fa\"];\n\n")

	// Quick JSON parse for call_edges.
	type edge struct {
		Caller string `json:"caller"`
		Callee string `json:"callee"`
	}
	type deityOutput struct {
		Functions []struct {
			Name string `json:"name"`
			File string `json:"file"`
		} `json:"functions"`
		CallEdges []edge `json:"call_edges"`
	}

	var d deityOutput
	if err := json.Unmarshal(jsonData, &d); err == nil {
		seen := make(map[string]bool)
		for _, fn := range d.Functions {
			if !seen[fn.Name] {
				b.WriteString(fmt.Sprintf("  \"%s\" [fillcolor=\"#313244\"];\n", fn.Name))
				seen[fn.Name] = true
			}
		}
		for _, e := range d.CallEdges {
			b.WriteString(fmt.Sprintf("  \"%s\" -> \"%s\";\n", e.Caller, e.Callee))
		}
	}

	b.WriteString("}\n")
	return b.String()
}

func metadataToDot(jsonData []byte, title string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("digraph \"%s\" {\n  rankdir=LR;\n  node [shape=box];\n", title))

	type dep struct {
		Name string `json:"name"`
	}
	type pkg struct {
		Name         string `json:"name"`
		Dependencies []dep  `json:"dependencies"`
	}
	type metadata struct {
		Packages []pkg `json:"packages"`
	}

	var m metadata
	if err := json.Unmarshal(jsonData, &m); err == nil {
		for _, p := range m.Packages {
			for _, d := range p.Dependencies {
				b.WriteString(fmt.Sprintf("  \"%s\" -> \"%s\";\n", p.Name, d.Name))
			}
		}
	}

	b.WriteString("}\n")
	return b.String()
}

