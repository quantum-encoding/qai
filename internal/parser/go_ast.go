// parser_go_ast.go — Compiler-accurate Go parser using go/ast.
//
// Extracts functions, structs, interfaces, constants, imports, call graphs,
// and cyclomatic complexity from Go source files with 100% accuracy.
// No regex guessing — uses the Go compiler's own parser.
//
// Adapted from Eye of Metatron's go_compiler_analyzer.go.

package parser

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// ── Analysis result types ───────────────────────────────────────────────────

type GoAnalysis struct {
	File          string          `json:"file"`
	Package       string          `json:"package"`
	Functions     []FuncInfo      `json:"functions"`
	Structs       []StructInfo    `json:"structs"`
	Interfaces    []IfaceInfo     `json:"interfaces"`
	Constants     []ConstInfo     `json:"constants"`
	Imports       []ImportInfo    `json:"imports"`
	CallGraph     []CallEdgeInfo  `json:"call_graph"`
	AvgComplexity float64         `json:"avg_complexity"`
}

type Param struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type FuncInfo struct {
	Name       string  `json:"name"`
	FullName   string  `json:"full_name"`
	Line       int     `json:"line"`
	EndLine    int     `json:"end_line"`
	Params     []Param `json:"parameters"`
	ReturnType string  `json:"return_type"`
	IsMethod   bool    `json:"is_method"`
	IsExported bool    `json:"is_exported"`
	Receiver   string  `json:"receiver,omitempty"`
	Calls      []string `json:"calls"`
	Complexity int     `json:"complexity"`
}

type StructInfo struct {
	Name       string  `json:"name"`
	Line       int     `json:"line"`
	Fields     []Param `json:"fields"`
	IsExported bool    `json:"is_exported"`
	Methods    []string `json:"methods"`
	Embedded   string  `json:"embedded_type,omitempty"`
}

type IfaceInfo struct {
	Name       string     `json:"name"`
	Line       int        `json:"line"`
	Methods    []FuncInfo `json:"methods"`
	IsExported bool       `json:"is_exported"`
}

type ConstInfo struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Value      string `json:"value"`
	Line       int    `json:"line"`
	IsExported bool   `json:"is_exported"`
}

type ImportInfo struct {
	Path  string `json:"path"`
	Alias string `json:"alias,omitempty"`
	Line  int    `json:"line"`
}

type CallEdgeInfo struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Line   int    `json:"line"`
}

// ── Analyzer ────────────────────────────────────────────────────────────────

type goAnalyzer struct {
	fset        *token.FileSet
	file        *ast.File
	filePath    string
	functions   []FuncInfo
	structs     []StructInfo
	interfaces  []IfaceInfo
	constants   []ConstInfo
	imports     []ImportInfo
	callGraph   []CallEdgeInfo
	currentFunc string
}

// AnalyzeGoFile parses a single .go file and returns the full analysis.
func AnalyzeGoFile(path string) (*GoAnalysis, error) {
	fset := token.NewFileSet()
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	a := &goAnalyzer{
		fset:     fset,
		file:     f,
		filePath: path,
	}

	a.extractImports()
	a.extractConstants()
	a.extractTypes()
	a.extractFunctions()

	avgComplexity := 0.0
	if len(a.functions) > 0 {
		total := 0
		for _, fn := range a.functions {
			total += fn.Complexity
		}
		avgComplexity = float64(total) / float64(len(a.functions))
	}

	return &GoAnalysis{
		File:          path,
		Package:       f.Name.Name,
		Functions:     a.functions,
		Structs:       a.structs,
		Interfaces:    a.interfaces,
		Constants:     a.constants,
		Imports:       a.imports,
		CallGraph:     a.callGraph,
		AvgComplexity: avgComplexity,
	}, nil
}

// AnalyzeGoDir parses all .go files in a directory (non-recursive).
func AnalyzeGoDir(dir string) ([]*GoAnalysis, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var results []*GoAnalysis
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		analysis, err := AnalyzeGoFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			continue
		}
		results = append(results, analysis)
	}
	return results, nil
}

// AnalyzeGoTree parses all .go files recursively.
func AnalyzeGoTree(root string) ([]*GoAnalysis, error) {
	var results []*GoAnalysis
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
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		analysis, err := AnalyzeGoFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %v\n", err)
			return nil
		}
		results = append(results, analysis)
		return nil
	})
	return results, nil
}

// ── Extraction methods ──────────────────────────────────────────────────────

func (a *goAnalyzer) pos(p token.Pos) token.Position {
	return a.fset.Position(p)
}

func (a *goAnalyzer) extractImports() {
	for _, imp := range a.file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		alias := ""
		if imp.Name != nil && imp.Name.Name != filepath.Base(path) {
			alias = imp.Name.Name
		}
		a.imports = append(a.imports, ImportInfo{
			Path:  path,
			Alias: alias,
			Line:  a.pos(imp.Pos()).Line,
		})
	}
}

func (a *goAnalyzer) extractConstants() {
	ast.Inspect(a.file, func(n ast.Node) bool {
		genDecl, ok := n.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			return true
		}
		for _, spec := range genDecl.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				cType := "untyped"
				if vs.Type != nil {
					cType = exprStr(vs.Type)
				}
				cVal := ""
				if i < len(vs.Values) {
					cVal = exprStr(vs.Values[i])
				}
				a.constants = append(a.constants, ConstInfo{
					Name:       name.Name,
					Type:       cType,
					Value:      cVal,
					Line:       a.pos(name.Pos()).Line,
					IsExported: ast.IsExported(name.Name),
				})
			}
		}
		return true
	})
}

func (a *goAnalyzer) extractTypes() {
	ast.Inspect(a.file, func(n ast.Node) bool {
		genDecl, ok := n.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			return true
		}
		for _, spec := range genDecl.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			switch t := ts.Type.(type) {
			case *ast.StructType:
				a.extractStruct(ts, t)
			case *ast.InterfaceType:
				a.extractInterface(ts, t)
			}
		}
		return true
	})
}

func (a *goAnalyzer) extractStruct(ts *ast.TypeSpec, st *ast.StructType) {
	info := StructInfo{
		Name:       ts.Name.Name,
		Line:       a.pos(ts.Pos()).Line,
		IsExported: ast.IsExported(ts.Name.Name),
	}

	for _, field := range st.Fields.List {
		fieldType := exprStr(field.Type)
		if len(field.Names) == 0 {
			// Embedded field.
			info.Embedded = fieldType
		} else {
			for _, name := range field.Names {
				info.Fields = append(info.Fields, Param{
					Name: name.Name,
					Type: fieldType,
				})
			}
		}
	}

	a.structs = append(a.structs, info)
}

func (a *goAnalyzer) extractInterface(ts *ast.TypeSpec, it *ast.InterfaceType) {
	info := IfaceInfo{
		Name:       ts.Name.Name,
		Line:       a.pos(ts.Pos()).Line,
		IsExported: ast.IsExported(ts.Name.Name),
	}

	for _, method := range it.Methods.List {
		ft, ok := method.Type.(*ast.FuncType)
		if !ok {
			continue
		}
		for _, name := range method.Names {
			info.Methods = append(info.Methods, FuncInfo{
				Name:       name.Name,
				FullName:   fmt.Sprintf("%s.%s", ts.Name.Name, name.Name),
				Line:       a.pos(name.Pos()).Line,
				Params:     extractParams(ft.Params),
				ReturnType: extractReturn(ft.Results),
				IsMethod:   true,
				IsExported: ast.IsExported(name.Name),
			})
		}
	}

	a.interfaces = append(a.interfaces, info)
}

func (a *goAnalyzer) extractFunctions() {
	ast.Inspect(a.file, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}

		name := fd.Name.Name
		fullName := name
		receiver := ""
		isMethod := false

		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			receiver = exprStr(fd.Recv.List[0].Type)
			fullName = receiver + "." + name
			isMethod = true

			// Link method to struct.
			bare := strings.TrimPrefix(receiver, "*")
			for i := range a.structs {
				if a.structs[i].Name == bare {
					a.structs[i].Methods = append(a.structs[i].Methods, name)
					break
				}
			}
		}

		a.currentFunc = fullName
		var calls []string
		complexity := 1

		if fd.Body != nil {
			calls = a.extractCalls(fd.Body)
			complexity = calcComplexity(fd.Body)
		}

		a.functions = append(a.functions, FuncInfo{
			Name:       name,
			FullName:   fullName,
			Line:       a.pos(fd.Pos()).Line,
			EndLine:    a.pos(fd.End()).Line,
			Params:     extractParams(fd.Type.Params),
			ReturnType: extractReturn(fd.Type.Results),
			IsMethod:   isMethod,
			IsExported: ast.IsExported(name),
			Receiver:   receiver,
			Calls:      calls,
			Complexity: complexity,
		})

		return true
	})
}

func (a *goAnalyzer) extractCalls(block *ast.BlockStmt) []string {
	var calls []string
	seen := make(map[string]bool)

	ast.Inspect(block, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := exprStr(ce.Fun)
		if !seen[callee] {
			calls = append(calls, callee)
			seen[callee] = true
			a.callGraph = append(a.callGraph, CallEdgeInfo{
				Caller: a.currentFunc,
				Callee: callee,
				Line:   a.pos(ce.Pos()).Line,
			})
		}
		return true
	})

	return calls
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func extractParams(fl *ast.FieldList) []Param {
	if fl == nil {
		return nil
	}
	var params []Param
	for _, field := range fl.List {
		t := exprStr(field.Type)
		if len(field.Names) == 0 {
			params = append(params, Param{Type: t})
		} else {
			for _, name := range field.Names {
				params = append(params, Param{Name: name.Name, Type: t})
			}
		}
	}
	return params
}

func extractReturn(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	if len(fl.List) == 1 && len(fl.List[0].Names) == 0 {
		return exprStr(fl.List[0].Type)
	}
	var parts []string
	for _, field := range fl.List {
		t := exprStr(field.Type)
		if len(field.Names) > 0 {
			for _, n := range field.Names {
				parts = append(parts, n.Name+" "+t)
			}
		} else {
			parts = append(parts, t)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func calcComplexity(block *ast.BlockStmt) int {
	c := 1
	ast.Inspect(block, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
			*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			c++
		case *ast.CaseClause, *ast.CommClause:
			c++
		}
		return true
	})
	return c
}

// exprStr converts an ast.Expr to a human-readable type string.
func exprStr(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprStr(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprStr(e.X)
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + exprStr(e.Elt)
		}
		return "[" + exprStr(e.Len) + "]" + exprStr(e.Elt)
	case *ast.MapType:
		return "map[" + exprStr(e.Key) + "]" + exprStr(e.Value)
	case *ast.ChanType:
		switch e.Dir {
		case ast.SEND:
			return "chan<- " + exprStr(e.Value)
		case ast.RECV:
			return "<-chan " + exprStr(e.Value)
		default:
			return "chan " + exprStr(e.Value)
		}
	case *ast.FuncType:
		return "func"
	case *ast.InterfaceType:
		if len(e.Methods.List) == 0 {
			return "any"
		}
		return "interface{...}"
	case *ast.Ellipsis:
		return "..." + exprStr(e.Elt)
	case *ast.BasicLit:
		return e.Value
	case *ast.CallExpr:
		return exprStr(e.Fun)
	case *ast.IndexExpr:
		return exprStr(e.X) + "[" + exprStr(e.Index) + "]"
	case *ast.IndexListExpr:
		var indices []string
		for _, idx := range e.Indices {
			indices = append(indices, exprStr(idx))
		}
		return exprStr(e.X) + "[" + strings.Join(indices, ", ") + "]"
	case *ast.ParenExpr:
		return "(" + exprStr(e.X) + ")"
	case *ast.UnaryExpr:
		return e.Op.String() + exprStr(e.X)
	case *ast.BinaryExpr:
		return exprStr(e.X) + " " + e.Op.String() + " " + exprStr(e.Y)
	case *ast.CompositeLit:
		if e.Type != nil {
			return exprStr(e.Type) + "{...}"
		}
		return "{...}"
	case *ast.KeyValueExpr:
		return exprStr(e.Key) + ": " + exprStr(e.Value)
	case *ast.StructType:
		return "struct{...}"
	case nil:
		return ""
	default:
		return fmt.Sprintf("<%T>", expr)
	}
}
