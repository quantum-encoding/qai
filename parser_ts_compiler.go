// parser_ts_compiler.go — Compiler-accurate TypeScript/JavaScript parser.
//
// Shells out to Node.js with the TypeScript Compiler API for 100% accurate
// analysis. Requires: node + typescript npm package.
//
// The ts_analyzer.js script is bundled in ~/.local/share/qai/scripts/ or
// alongside the qai binary.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ── TypeScript analysis result types ────────────────────────────────────────

type TSAnalysis struct {
	File       string        `json:"file"`
	Functions  []TSFunction  `json:"functions"`
	Classes    []TSClass     `json:"classes"`
	Interfaces []TSInterface `json:"interfaces"`
	Enums      []TSEnum      `json:"enums"`
	Imports    []TSImport    `json:"imports"`
	CallGraph  []TSCallEdge  `json:"callGraph"`
	Statistics map[string]any `json:"statistics"`
	Error      string        `json:"error"`
}

type TSFunction struct {
	Name           string    `json:"name"`
	FullName       string    `json:"fullName"`
	Line           int       `json:"line"`
	EndLine        int       `json:"endLine"`
	Parameters     []TSParam `json:"parameters"`
	ReturnType     string    `json:"returnType"`
	IsAsync        bool      `json:"isAsync"`
	IsArrow        bool      `json:"isArrow"`
	IsMethod       bool      `json:"isMethod"`
	IsStatic       bool      `json:"isStatic"`
	IsPrivate      bool      `json:"isPrivate"`
	IsAbstract     bool      `json:"isAbstract"`
	TypeParameters []string  `json:"typeParameters"`
	Calls          []string  `json:"calls"`
	Complexity     int       `json:"complexity"`
}

type TSParam struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	DefaultValue string `json:"defaultValue"`
	IsOptional   bool   `json:"isOptional"`
	IsRest       bool   `json:"isRest"`
}

type TSClass struct {
	Name           string       `json:"name"`
	Line           int          `json:"line"`
	EndLine        int          `json:"endLine"`
	IsAbstract     bool         `json:"isAbstract"`
	IsExported     bool         `json:"isExported"`
	SuperClass     string       `json:"superClass"`
	Interfaces     []string     `json:"interfaces"`
	Methods        []TSFunction `json:"methods"`
	Properties     []TSParam    `json:"properties"`
	TypeParameters []string     `json:"typeParameters"`
}

type TSInterface struct {
	Name           string    `json:"name"`
	Line           int       `json:"line"`
	EndLine        int       `json:"endLine"`
	Extends        []string  `json:"extends"`
	Methods        []TSFunction `json:"methods"`
	Properties     []TSParam `json:"properties"`
	TypeParameters []string  `json:"typeParameters"`
	IsExported     bool      `json:"isExported"`
}

type TSEnum struct {
	Name       string   `json:"name"`
	Line       int      `json:"line"`
	Members    []string `json:"members"`
	IsExported bool     `json:"isExported"`
}

type TSImport struct {
	Specifier   string `json:"specifier"`
	Source      string `json:"source"`
	Line        int    `json:"line"`
	IsDefault   bool   `json:"isDefault"`
	IsNamespace bool   `json:"isNamespace"`
	Alias       string `json:"alias"`
}

type TSCallEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Line    int    `json:"line"`
	IsAsync bool   `json:"isAsync"`
}

// ── Analyzer functions ──────────────────────────────────────────────────────

// findTSAnalyzerScript locates the TypeScript analyzer script.
// Extracts from the embedded binary on first use.
func findTSAnalyzerScript() (string, error) {
	return ensureScript("ts_analyzer.js")
}

// AnalyzeTSFile parses a single TypeScript/JavaScript file using the TS compiler.
func AnalyzeTSFile(path string) (*TSAnalysis, error) {
	script, err := findTSAnalyzerScript()
	if err != nil {
		return nil, err
	}

	// Check node is available.
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("node not found on PATH (required for TypeScript analysis)")
	}

	cmd := exec.Command(nodeBin, script, path, "--json")
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = string(exitErr.Stderr)
		}
		return nil, fmt.Errorf("ts analyzer failed: %v\n%s", err, stderr)
	}

	var result TSAnalysis
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse ts analyzer output: %w", err)
	}

	if result.Error != "" {
		return nil, fmt.Errorf("ts analyzer: %s", result.Error)
	}

	return &result, nil
}

// AnalyzeTSTree parses all .ts/.tsx/.js/.jsx files recursively.
func AnalyzeTSTree(root string) ([]*TSAnalysis, error) {
	script, err := findTSAnalyzerScript()
	if err != nil {
		return nil, err
	}
	_ = script // validated existence

	var results []*TSAnalysis
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if ignoreDirs[name] || (strings.HasPrefix(name, ".") && name != ".") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext != ".ts" && ext != ".tsx" && ext != ".js" && ext != ".jsx" {
			return nil
		}
		// Skip declaration files and test files.
		if strings.HasSuffix(d.Name(), ".d.ts") || strings.HasSuffix(d.Name(), ".test.ts") || strings.HasSuffix(d.Name(), ".spec.ts") {
			return nil
		}

		analysis, err := AnalyzeTSFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			return nil
		}
		results = append(results, analysis)
		return nil
	})
	return results, nil
}
