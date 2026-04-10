// parser_py_compiler.go — Compiler-accurate Python parser.
//
// Shells out to Python's ast module for 100% accurate analysis.
// Zero dependencies — uses Python stdlib only.

package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/quantum-encoding/qai-cli/internal/scripts"
	"path/filepath"
	"strings"
)

// ── Python analysis result types ────────────────────────────────────────────

type PyAnalysis struct {
	File       string        `json:"file"`
	Functions  []PyFunction  `json:"functions"`
	Classes    []PyClass     `json:"classes"`
	Imports    []PyImport    `json:"imports"`
	CallGraph  []PyCallEdge  `json:"callGraph"`
	Statistics map[string]any `json:"statistics"`
	Error      string        `json:"error"`
}

type PyFunction struct {
	Name          string    `json:"name"`
	FullName      string    `json:"fullName"`
	Line          int       `json:"line"`
	EndLine       int       `json:"endLine"`
	Parameters    []PyParam `json:"parameters"`
	ReturnType    *string   `json:"returnType"`
	IsAsync       bool      `json:"isAsync"`
	IsMethod      bool      `json:"isMethod"`
	IsPrivate     bool      `json:"isPrivate"`
	IsStatic      bool      `json:"isStatic"`
	IsClassmethod bool      `json:"isClassmethod"`
	Decorators    []string  `json:"decorators"`
	Calls         []string  `json:"calls"`
	Complexity    int       `json:"complexity"`
}

type PyParam struct {
	Name         string  `json:"name"`
	Type         *string `json:"type"`
	DefaultValue *string `json:"defaultValue,omitempty"`
	IsOptional   bool    `json:"isOptional,omitempty"`
	IsRest       bool    `json:"isRest,omitempty"`
}

type PyClass struct {
	Name        string    `json:"name"`
	Line        int       `json:"line"`
	EndLine     int       `json:"endLine"`
	Bases       []string  `json:"bases"`
	Decorators  []string  `json:"decorators"`
	Methods     []string  `json:"methods"`
	Properties  []PyParam `json:"properties"`
	IsExported  bool      `json:"isExported"`
	IsDataclass bool      `json:"isDataclass"`
}

type PyImport struct {
	Module     string  `json:"module"`
	Alias      *string `json:"alias"`
	Line       int     `json:"line"`
	FromImport bool    `json:"fromImport,omitempty"`
}

type PyCallEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Line int    `json:"line"`
}

// ── Analyzer functions ──────────────────────────────────────────────────────

func findPyAnalyzerScript() (string, error) {
	return scripts.EnsureScript("py_analyzer.py")
}

func AnalyzePyFile(path string) (*PyAnalysis, error) {
	script, err := findPyAnalyzerScript()
	if err != nil {
		return nil, err
	}

	python, err := exec.LookPath("python3")
	if err != nil {
		return nil, fmt.Errorf("python3 not found on PATH")
	}

	cmd := exec.Command(python, script, path, "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("py analyzer failed: %v", err)
	}

	var result PyAnalysis
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse py analyzer output: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("py analyzer: %s", result.Error)
	}
	return &result, nil
}

func AnalyzePyTree(root string) ([]*PyAnalysis, error) {
	if _, err := findPyAnalyzerScript(); err != nil {
		return nil, err
	}

	var results []*PyAnalysis
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if IgnoreDirs[name] || (strings.HasPrefix(name, ".") && name != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".py") {
			return nil
		}
		if strings.HasPrefix(d.Name(), "test_") || strings.HasSuffix(d.Name(), "_test.py") {
			return nil
		}

		analysis, err := AnalyzePyFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			return nil
		}
		results = append(results, analysis)
		return nil
	})
	return results, nil
}
