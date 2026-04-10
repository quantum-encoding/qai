package analyze

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/parser"
)

func CmdAnalyze(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, `usage: qai analyze <path> [options]

Compiler-accurate code analysis using native language parsers.
Currently supports: Go (go/ast)

Options:
  --format <fmt>    Output format: json (default), md, summary
  --complexity      Show complexity hotspots (top 10)
  --calls           Show call graph
  --types           Show types + fields only
  -o <file>         Output file (default: stdout)`)
		os.Exit(1)
	}

	dir := args[0]
	format := "json"
	output := "-"
	showComplexity := false
	showCalls := false
	showTypes := false

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--format", "-f":
			if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		case "-o", "--output":
			if i+1 < len(args) {
				output = args[i+1]
				i++
			}
		case "--complexity":
			showComplexity = true
		case "--calls":
			showCalls = true
		case "--types":
			showTypes = true
		}
	}

	absDir, _ := filepath.Abs(dir)

	// Detect language: check for Go, TS, Python files.
	hasGo, hasTS, hasPy := detectLanguages(absDir)

	hasSwift := detectSwift(absDir)
	hasKt := detectKotlin(absDir)

	if hasKt && !hasGo && !hasTS && !hasPy && !hasSwift {
		fmt.Fprintf(os.Stderr, "Analyzing %s (Kotlin parser)...\n", absDir)
		ktResults, err := parser.AnalyzeKtTree(absDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Parsed %d files\n", len(ktResults))
		totalFns, totalClasses, totalEdges := 0, 0, 0
		for _, r := range ktResults {
			totalFns += len(r.Functions)
			totalClasses += len(r.Classes)
			totalEdges += len(r.CallGraph)
		}
		if format == "summary" {
			writeAnalyzeOutput(fmt.Sprintf("Analysis: %s (Kotlin)\n  Files:      %d\n  Functions:  %d\n  Types:      %d\n  Call edges: %d\n",
				filepath.Base(absDir), len(ktResults), totalFns, totalClasses, totalEdges), output)
		} else {
			data, _ := json.MarshalIndent(ktResults, "", "  ")
			writeAnalyzeOutput(string(data), output)
		}
		return
	}

	if hasSwift && !hasGo && !hasTS && !hasPy {
		fmt.Fprintf(os.Stderr, "Analyzing %s (Swift compiler)...\n", absDir)
		swiftResults, err := parser.AnalyzeSwiftTree(absDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Parsed %d files\n", len(swiftResults))
		writeAnalyzeOutput(formatSwiftSummary(swiftResults, absDir, format), output)
		return
	}

	if hasPy && !hasGo && !hasTS {
		// Python-only project.
		fmt.Fprintf(os.Stderr, "Analyzing %s (Python ast)...\n", absDir)
		pyResults, err := parser.AnalyzePyTree(absDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Parsed %d files\n", len(pyResults))
		writeAnalyzeOutput(formatPyOutput(pyResults, absDir, format, showComplexity, showCalls, showTypes), output)
		return
	}

	if hasTS && !hasGo {
		// TypeScript-only project.
		fmt.Fprintf(os.Stderr, "Analyzing %s (TypeScript compiler)...\n", absDir)
		tsResults, err := parser.AnalyzeTSTree(absDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Parsed %d files\n", len(tsResults))
		writeAnalyzeOutput(formatTSOutput(tsResults, absDir, format, showComplexity, showCalls, showTypes), output)
		return
	}

	fmt.Fprintf(os.Stderr, "Analyzing %s (go/ast)...\n", absDir)

	results, err := parser.AnalyzeGoTree(absDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Parsed %d files\n", len(results))

	var out string
	switch {
	case showComplexity:
		out = formatComplexity(results)
	case showCalls:
		out = formatCallGraph(results)
	case showTypes:
		out = formatTypes(results)
	case format == "md":
		out = formatAnalysisMD(results, absDir)
	case format == "summary":
		out = formatSummary(results, absDir)
	default:
		data, _ := json.MarshalIndent(results, "", "  ")
		out = string(data)
	}

	if output == "-" {
		fmt.Print(out)
	} else {
		if err := os.WriteFile(output, []byte(out), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "qai analyze: write %s: %v\n", output, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Wrote %s\n", output)
	}
}

func formatComplexity(results []*parser.GoAnalysis) string {
	type entry struct {
		Name       string
		File       string
		Complexity int
		Line       int
	}

	var all []entry
	for _, r := range results {
		rel := filepath.Base(r.File)
		for _, fn := range r.Functions {
			all = append(all, entry{fn.FullName, rel, fn.Complexity, fn.Line})
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Complexity > all[j].Complexity })

	var b strings.Builder
	b.WriteString("Complexity Hotspots (highest first):\n\n")
	limit := 20
	if len(all) < limit {
		limit = len(all)
	}
	for i := 0; i < limit; i++ {
		e := all[i]
		bar := strings.Repeat("█", e.Complexity)
		b.WriteString(fmt.Sprintf("  %2d  %-40s  %s:%d  %s\n", e.Complexity, e.Name, e.File, e.Line, bar))
	}
	return b.String()
}

func formatCallGraph(results []*parser.GoAnalysis) string {
	var b strings.Builder
	b.WriteString("Call Graph:\n\n")
	for _, r := range results {
		for _, edge := range r.CallGraph {
			b.WriteString(fmt.Sprintf("  %s → %s  (line %d)\n", edge.Caller, edge.Callee, edge.Line))
		}
	}
	return b.String()
}

func formatTypes(results []*parser.GoAnalysis) string {
	var b strings.Builder
	for _, r := range results {
		rel := filepath.Base(r.File)
		for _, s := range r.Structs {
			vis := "pub "
			if !s.IsExported {
				vis = ""
			}
			b.WriteString(fmt.Sprintf("%sstruct %s (%s:%d)\n", vis, s.Name, rel, s.Line))
			for _, f := range s.Fields {
				b.WriteString(fmt.Sprintf("  %s: %s\n", f.Name, f.Type))
			}
			if len(s.Methods) > 0 {
				b.WriteString(fmt.Sprintf("  methods: %s\n", strings.Join(s.Methods, ", ")))
			}
			b.WriteByte('\n')
		}
		for _, iface := range r.Interfaces {
			vis := "pub "
			if !iface.IsExported {
				vis = ""
			}
			b.WriteString(fmt.Sprintf("%sinterface %s (%s:%d)\n", vis, iface.Name, rel, iface.Line))
			for _, m := range iface.Methods {
				params := formatParamList(m.Params)
				b.WriteString(fmt.Sprintf("  %s(%s) %s\n", m.Name, params, m.ReturnType))
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func formatAnalysisMD(results []*parser.GoAnalysis, root string) string {
	var b strings.Builder
	name := filepath.Base(root)

	totalFuncs, totalStructs, totalIfaces, totalEdges := 0, 0, 0, 0
	for _, r := range results {
		totalFuncs += len(r.Functions)
		totalStructs += len(r.Structs)
		totalIfaces += len(r.Interfaces)
		totalEdges += len(r.CallGraph)
	}

	b.WriteString(fmt.Sprintf("# Analysis: %s\n\n", name))
	b.WriteString(fmt.Sprintf("**Files:** %d | **Functions:** %d | **Structs:** %d | **Interfaces:** %d | **Call edges:** %d\n\n",
		len(results), totalFuncs, totalStructs, totalIfaces, totalEdges))

	for _, r := range results {
		rel, _ := filepath.Rel(root, r.File)
		if rel == "" {
			rel = filepath.Base(r.File)
		}
		b.WriteString(fmt.Sprintf("## %s (pkg: %s)\n\n", rel, r.Package))

		if len(r.Structs) > 0 {
			for _, s := range r.Structs {
				b.WriteString(fmt.Sprintf("### struct %s\n\n", s.Name))
				if len(s.Fields) > 0 {
					b.WriteString("| Field | Type |\n|-------|------|\n")
					for _, f := range s.Fields {
						b.WriteString(fmt.Sprintf("| %s | `%s` |\n", f.Name, f.Type))
					}
					b.WriteByte('\n')
				}
				if len(s.Methods) > 0 {
					b.WriteString(fmt.Sprintf("Methods: %s\n\n", strings.Join(s.Methods, ", ")))
				}
			}
		}

		if len(r.Functions) > 0 {
			b.WriteString("### Functions\n\n")
			b.WriteString("| Function | Params | Returns | Complexity |\n|----------|--------|---------|------------|\n")
			for _, fn := range r.Functions {
				params := formatParamList(fn.Params)
				b.WriteString(fmt.Sprintf("| %s | `%s` | `%s` | %d |\n",
					fn.FullName, params, fn.ReturnType, fn.Complexity))
			}
			b.WriteByte('\n')
		}

		b.WriteString("---\n\n")
	}

	return b.String()
}

func formatSummary(results []*parser.GoAnalysis, root string) string {
	var b strings.Builder
	name := filepath.Base(root)

	totalFuncs, totalStructs, totalIfaces, totalConsts, totalEdges := 0, 0, 0, 0, 0
	totalComplexity := 0
	for _, r := range results {
		totalFuncs += len(r.Functions)
		totalStructs += len(r.Structs)
		totalIfaces += len(r.Interfaces)
		totalConsts += len(r.Constants)
		totalEdges += len(r.CallGraph)
		for _, fn := range r.Functions {
			totalComplexity += fn.Complexity
		}
	}

	avgComplexity := 0.0
	if totalFuncs > 0 {
		avgComplexity = float64(totalComplexity) / float64(totalFuncs)
	}

	b.WriteString(fmt.Sprintf("Analysis: %s\n", name))
	b.WriteString(fmt.Sprintf("  Files:      %d\n", len(results)))
	b.WriteString(fmt.Sprintf("  Functions:  %d\n", totalFuncs))
	b.WriteString(fmt.Sprintf("  Structs:    %d\n", totalStructs))
	b.WriteString(fmt.Sprintf("  Interfaces: %d\n", totalIfaces))
	b.WriteString(fmt.Sprintf("  Constants:  %d\n", totalConsts))
	b.WriteString(fmt.Sprintf("  Call edges: %d\n", totalEdges))
	b.WriteString(fmt.Sprintf("  Avg complexity: %.1f\n", avgComplexity))

	return b.String()
}

func detectLanguages(dir string) (hasGo, hasTS, hasPy bool) {
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && parser.IgnoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(d.Name())
		switch ext {
		case ".go":
			hasGo = true
		case ".ts", ".tsx":
			hasTS = true
		case ".py":
			hasPy = true
		}
		if hasGo && hasTS && hasPy {
			return filepath.SkipAll
		}
		return nil
	})
	return
}

func formatPyOutput(results []*parser.PyAnalysis, root, format string, showComplexity, showCalls, showTypes bool) string {
	switch {
	case showTypes:
		var b strings.Builder
		for _, r := range results {
			rel := filepath.Base(r.File)
			for _, cls := range r.Classes {
				dc := ""
				if cls.IsDataclass {
					dc = "@dataclass "
				}
				b.WriteString(fmt.Sprintf("%sclass %s (%s:%d)\n", dc, cls.Name, rel, cls.Line))
				for _, p := range cls.Properties {
					t := "Any"
					if p.Type != nil {
						t = *p.Type
					}
					b.WriteString(fmt.Sprintf("  %s: %s\n", p.Name, t))
				}
				if len(cls.Methods) > 0 {
					b.WriteString(fmt.Sprintf("  methods: %s\n", strings.Join(cls.Methods, ", ")))
				}
				b.WriteByte('\n')
			}
		}
		return b.String()
	case showCalls:
		var b strings.Builder
		b.WriteString("Call Graph:\n\n")
		for _, r := range results {
			for _, e := range r.CallGraph {
				b.WriteString(fmt.Sprintf("  %s → %s  (line %d)\n", e.From, e.To, e.Line))
			}
		}
		return b.String()
	case showComplexity:
		type entry struct {
			Name       string
			File       string
			Complexity int
		}
		var all []entry
		for _, r := range results {
			rel := filepath.Base(r.File)
			for _, fn := range r.Functions {
				all = append(all, entry{fn.FullName, rel, fn.Complexity})
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Complexity > all[j].Complexity })
		var b strings.Builder
		b.WriteString("Complexity Hotspots:\n\n")
		limit := 20
		if len(all) < limit {
			limit = len(all)
		}
		for i := 0; i < limit; i++ {
			e := all[i]
			bar := strings.Repeat("█", e.Complexity)
			b.WriteString(fmt.Sprintf("  %2d  %-40s  %s  %s\n", e.Complexity, e.Name, e.File, bar))
		}
		return b.String()
	case format == "summary":
		totalFns, totalClasses, totalEdges := 0, 0, 0
		for _, r := range results {
			totalFns += len(r.Functions)
			totalClasses += len(r.Classes)
			totalEdges += len(r.CallGraph)
		}
		return fmt.Sprintf("Analysis: %s\n  Files:      %d\n  Functions:  %d\n  Classes:    %d\n  Call edges: %d\n",
			filepath.Base(root), len(results), totalFns, totalClasses, totalEdges)
	default:
		data, _ := json.MarshalIndent(results, "", "  ")
		return string(data)
	}
}

func writeAnalyzeOutput(content, output string) {
	if output == "-" {
		fmt.Print(content)
	} else {
		if err := os.WriteFile(output, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "qai analyze: write %s: %v\n", output, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Wrote %s\n", output)
	}
}

func formatTSOutput(results []*parser.TSAnalysis, root, format string, showComplexity, showCalls, showTypes bool) string {
	switch {
	case showTypes:
		return formatTSTypes(results)
	case showCalls:
		return formatTSCalls(results)
	case showComplexity:
		return formatTSComplexity(results)
	case format == "summary":
		return formatTSSummary(results, root)
	case format == "md":
		return formatTSMarkdown(results, root)
	default:
		data, _ := json.MarshalIndent(results, "", "  ")
		return string(data)
	}
}

func formatTSSummary(results []*parser.TSAnalysis, root string) string {
	totalFns, totalClasses, totalIfaces, totalEdges := 0, 0, 0, 0
	for _, r := range results {
		totalFns += len(r.Functions)
		totalClasses += len(r.Classes)
		totalIfaces += len(r.Interfaces)
		totalEdges += len(r.CallGraph)
		for _, cls := range r.Classes {
			totalFns += len(cls.Methods)
		}
	}
	return fmt.Sprintf("Analysis: %s\n  Files:      %d\n  Functions:  %d\n  Classes:    %d\n  Interfaces: %d\n  Call edges: %d\n",
		filepath.Base(root), len(results), totalFns, totalClasses, totalIfaces, totalEdges)
}

func formatTSTypes(results []*parser.TSAnalysis) string {
	var b strings.Builder
	for _, r := range results {
		rel := filepath.Base(r.File)
		for _, iface := range r.Interfaces {
			exported := ""
			if iface.IsExported {
				exported = "export "
			}
			b.WriteString(fmt.Sprintf("%sinterface %s (%s:%d)\n", exported, iface.Name, rel, iface.Line))
			for _, p := range iface.Properties {
				opt := ""
				if p.IsOptional {
					opt = "?"
				}
				b.WriteString(fmt.Sprintf("  %s%s: %s\n", p.Name, opt, p.Type))
			}
			b.WriteByte('\n')
		}
		for _, cls := range r.Classes {
			b.WriteString(fmt.Sprintf("class %s (%s:%d)\n", cls.Name, rel, cls.Line))
			for _, p := range cls.Properties {
				b.WriteString(fmt.Sprintf("  %s: %s\n", p.Name, p.Type))
			}
			b.WriteString(fmt.Sprintf("  methods: %d\n\n", len(cls.Methods)))
		}
	}
	return b.String()
}

func formatTSCalls(results []*parser.TSAnalysis) string {
	var b strings.Builder
	b.WriteString("Call Graph:\n\n")
	for _, r := range results {
		for _, edge := range r.CallGraph {
			b.WriteString(fmt.Sprintf("  %s → %s  (line %d)\n", edge.From, edge.To, edge.Line))
		}
	}
	return b.String()
}

func formatTSComplexity(results []*parser.TSAnalysis) string {
	type entry struct {
		Name       string
		File       string
		Complexity int
	}
	var all []entry
	for _, r := range results {
		rel := filepath.Base(r.File)
		for _, fn := range r.Functions {
			all = append(all, entry{fn.FullName, rel, fn.Complexity})
		}
		for _, cls := range r.Classes {
			for _, m := range cls.Methods {
				all = append(all, entry{cls.Name + "." + m.Name, rel, m.Complexity})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Complexity > all[j].Complexity })

	var b strings.Builder
	b.WriteString("Complexity Hotspots:\n\n")
	limit := 20
	if len(all) < limit {
		limit = len(all)
	}
	for i := 0; i < limit; i++ {
		e := all[i]
		bar := strings.Repeat("█", e.Complexity)
		b.WriteString(fmt.Sprintf("  %2d  %-40s  %s  %s\n", e.Complexity, e.Name, e.File, bar))
	}
	return b.String()
}

func formatTSMarkdown(results []*parser.TSAnalysis, root string) string {
	var b strings.Builder
	name := filepath.Base(root)
	b.WriteString(fmt.Sprintf("# Analysis: %s (TypeScript)\n\n", name))
	b.WriteString(formatTSSummary(results, root))
	b.WriteString("\n---\n\n")

	for _, r := range results {
		rel := filepath.Base(r.File)
		b.WriteString(fmt.Sprintf("## %s\n\n", rel))
		if len(r.Interfaces) > 0 {
			b.WriteString(fmt.Sprintf("**Interfaces:** %d\n\n", len(r.Interfaces)))
		}
		if len(r.Classes) > 0 {
			for _, cls := range r.Classes {
				b.WriteString(fmt.Sprintf("### class %s (%d methods)\n\n", cls.Name, len(cls.Methods)))
			}
		}
		if len(r.Functions) > 0 {
			b.WriteString("### Functions\n\n| Function | Params | Returns | Complexity |\n|----------|--------|---------|------------|\n")
			for _, fn := range r.Functions {
				params := formatTSParamList(fn.Parameters)
				b.WriteString(fmt.Sprintf("| %s | `%s` | `%s` | %d |\n", fn.Name, params, fn.ReturnType, fn.Complexity))
			}
		}
		b.WriteString("\n---\n\n")
	}
	return b.String()
}

func formatTSParamList(params []parser.TSParam) string {
	var parts []string
	for _, p := range params {
		s := p.Name
		if p.IsOptional {
			s += "?"
		}
		if p.Type != "" {
			s += ": " + p.Type
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func detectKotlin(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && parser.IgnoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) == ".kt" || filepath.Ext(d.Name()) == ".kts" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func detectSwift(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && parser.IgnoreDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(d.Name()) == ".swift" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func formatSwiftSummary(results []*parser.SwiftAnalysis, root, format string) string {
	totalFns, totalClasses, totalEdges := 0, 0, 0
	for _, r := range results {
		totalFns += len(r.Functions)
		totalClasses += len(r.Classes)
		totalEdges += len(r.CallGraph)
	}

	if format == "summary" {
		return fmt.Sprintf("Analysis: %s (Swift)\n  Files:      %d\n  Functions:  %d\n  Types:      %d\n  Call edges: %d\n",
			filepath.Base(root), len(results), totalFns, totalClasses, totalEdges)
	}

	// Default: JSON
	data, _ := json.MarshalIndent(results, "", "  ")
	return string(data)
}

func formatParamList(params []parser.Param) string {
	var parts []string
	for _, p := range params {
		if p.Name != "" {
			parts = append(parts, p.Name+" "+p.Type)
		} else {
			parts = append(parts, p.Type)
		}
	}
	return strings.Join(parts, ", ")
}
