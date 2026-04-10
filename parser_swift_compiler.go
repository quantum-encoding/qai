// parser_swift_compiler.go — Swift analyzer via swift script.
//
// Shells out to a Swift script that parses .swift files.
// Requires: swift (Xcode Command Line Tools).

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type SwiftAnalysis struct {
	File       string          `json:"file"`
	Functions  []SwiftFunction `json:"functions"`
	Classes    []SwiftClass    `json:"classes"`
	Extensions []SwiftExt      `json:"extensions"`
	Imports    []SwiftImport   `json:"imports"`
	CallGraph  []SwiftCallEdge `json:"callGraph"`
	Statistics map[string]any  `json:"statistics"`
	Error      *string         `json:"error"`
}

type SwiftFunction struct {
	Name       string       `json:"name"`
	FullName   string       `json:"fullName"`
	Line       int          `json:"line"`
	EndLine    int          `json:"endLine"`
	Parameters []SwiftParam `json:"parameters"`
	ReturnType *string      `json:"returnType"`
	IsAsync    bool         `json:"isAsync"`
	IsThrows   bool         `json:"isThrows"`
	IsStatic   bool         `json:"isStatic"`
	IsPrivate  bool         `json:"isPrivate"`
	IsMutating bool         `json:"isMutating"`
	Calls      []string     `json:"calls"`
	Complexity int          `json:"complexity"`
}

type SwiftParam struct {
	Name         string  `json:"name"`
	Type         *string `json:"type"`
	DefaultValue *string `json:"defaultValue"`
	IsInout      bool    `json:"isInout"`
	IsVariadic   bool    `json:"isVariadic"`
}

type SwiftClass struct {
	Name       string       `json:"name"`
	Line       int          `json:"line"`
	EndLine    int          `json:"endLine"`
	Superclass *string      `json:"superclass"`
	Protocols  []string     `json:"protocols"`
	IsStruct   bool         `json:"isStruct"`
	IsEnum     bool         `json:"isEnum"`
	IsProtocol bool         `json:"isProtocol"`
	IsActor    bool         `json:"isActor"`
	IsFinal    bool         `json:"isFinal"`
	Methods    []string     `json:"methods"`
	Properties []SwiftProp  `json:"properties"`
}

type SwiftProp struct {
	Name      string  `json:"name"`
	Type      *string `json:"type"`
	IsLet     bool    `json:"isLet"`
	IsStatic  bool    `json:"isStatic"`
	IsPrivate bool    `json:"isPrivate"`
}

type SwiftExt struct {
	TypeName    string   `json:"type"`
	Line        int      `json:"line"`
	EndLine     int      `json:"endLine"`
	Protocols   []string `json:"protocols"`
	Methods     []string `json:"methods"`
	WhereClause *string  `json:"whereClause"`
}

type SwiftImport struct {
	Module     string `json:"module"`
	IsTestable bool   `json:"isTestable"`
	Line       int    `json:"line"`
}

type SwiftCallEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Line    int    `json:"line"`
	IsAsync bool   `json:"isAsync"`
}

func findSwiftAnalyzerScript() (string, error) {
	return ensureScript("swift_analyzer.swift")
}

func AnalyzeSwiftFile(path string) (*SwiftAnalysis, error) {
	script, err := findSwiftAnalyzerScript()
	if err != nil {
		return nil, err
	}

	swift, err := exec.LookPath("swift")
	if err != nil {
		return nil, fmt.Errorf("swift not found on PATH (Xcode Command Line Tools required)")
	}

	cmd := exec.Command(swift, script, path, "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("swift analyzer failed: %v", err)
	}

	var result SwiftAnalysis
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parse swift analyzer output: %w", err)
	}
	if result.Error != nil && *result.Error != "" {
		return nil, fmt.Errorf("swift analyzer: %s", *result.Error)
	}
	return &result, nil
}

func AnalyzeSwiftTree(root string) ([]*SwiftAnalysis, error) {
	if _, err := findSwiftAnalyzerScript(); err != nil {
		return nil, err
	}

	var results []*SwiftAnalysis
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
		if !strings.HasSuffix(d.Name(), ".swift") {
			return nil
		}
		if strings.Contains(path, "Tests/") || strings.HasSuffix(d.Name(), "Tests.swift") {
			return nil
		}

		analysis, err := AnalyzeSwiftFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			return nil
		}
		results = append(results, analysis)
		return nil
	})
	return results, nil
}
