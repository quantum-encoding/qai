// parser_kt_compiler.go — Kotlin analyzer via Python regex script.
//
// Uses a Python script to parse .kt files with regex patterns
// matching Kotlin's syntax (data class, fun, typealias, etc.).
// Extracts @SerialName annotations for JSON field mapping.

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

type KtAnalysis struct {
	File       string         `json:"file"`
	Functions  []KtFunction   `json:"functions"`
	Classes    []KtClass      `json:"classes"`
	Imports    []KtImport     `json:"imports"`
	CallGraph  []KtCallEdge   `json:"callGraph"`
	Statistics map[string]any `json:"statistics"`
	Error      *string        `json:"error"`
}

type KtFunction struct {
	Name       string    `json:"name"`
	FullName   string    `json:"fullName"`
	Line       int       `json:"line"`
	Parameters []KtParam `json:"parameters"`
	ReturnType *string   `json:"returnType"`
	IsSuspend  bool      `json:"isSuspend"`
	IsInline   bool      `json:"isInline"`
	IsPrivate  bool      `json:"isPrivate"`
	IsOverride bool      `json:"isOverride"`
	IsMethod   bool      `json:"isMethod"`
	Calls      []string  `json:"calls"`
	Complexity int       `json:"complexity"`
}

type KtParam struct {
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	JSONTag string  `json:"jsonTag,omitempty"`
}

type KtClass struct {
	Name       string    `json:"name"`
	Line       int       `json:"line"`
	EndLine    int       `json:"endLine"`
	Kind       string    `json:"kind"`
	IsData     bool      `json:"isData"`
	IsSealed   bool      `json:"isSealed"`
	IsEnum     bool      `json:"isEnum"`
	IsExported bool      `json:"isExported"`
	Bases      []string  `json:"bases"`
	Properties []KtParam `json:"properties"`
	Methods    []string  `json:"methods"`
}

type KtImport struct {
	Module string `json:"module"`
	Line   int    `json:"line"`
}

type KtCallEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Line int    `json:"line"`
}

func findKtAnalyzerScript() (string, error) {
	return scripts.EnsureScript("kt_analyzer.py")
}

func AnalyzeKtFile(path string) (*KtAnalysis, error) {
	script, err := findKtAnalyzerScript()
	if err != nil {
		return nil, err
	}
	python, _ := exec.LookPath("python3")
	if python == "" {
		return nil, fmt.Errorf("python3 not found")
	}

	cmd := exec.Command(python, script, path, "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kt analyzer failed: %v", err)
	}

	var result KtAnalysis
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse kt analyzer output: %w", err)
	}
	return &result, nil
}

func AnalyzeKtTree(root string) ([]*KtAnalysis, error) {
	if _, err := findKtAnalyzerScript(); err != nil {
		return nil, err
	}
	var results []*KtAnalysis
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
		if !strings.HasSuffix(d.Name(), ".kt") && !strings.HasSuffix(d.Name(), ".kts") {
			return nil
		}
		analysis, err := AnalyzeKtFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			return nil
		}
		results = append(results, analysis)
		return nil
	})
	return results, nil
}
