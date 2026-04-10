#!/usr/bin/env python3
"""
Python Compiler-Based Analyzer — uses ast module for 100% accurate analysis.
No dependencies required (ast is stdlib).

Usage: python3 py_analyzer.py <file.py> [--json]
"""

import ast
import json
import sys
import os


class PythonAnalyzer(ast.NodeVisitor):
    def __init__(self, filepath):
        self.filepath = filepath
        self.functions = []
        self.classes = []
        self.imports = []
        self.call_graph = []
        self.current_function = None
        self.current_class = None

    def analyze(self):
        try:
            with open(self.filepath, "r") as f:
                source = f.read()
            tree = ast.parse(source, filename=self.filepath)
        except SyntaxError as e:
            return {"file": self.filepath, "error": str(e),
                    "functions": [], "classes": [], "imports": [], "callGraph": []}

        self.visit(tree)

        return {
            "file": self.filepath,
            "functions": self.functions,
            "classes": self.classes,
            "imports": self.imports,
            "callGraph": self.call_graph,
            "statistics": {
                "total_functions": len(self.functions),
                "total_classes": len(self.classes),
                "total_imports": len(self.imports),
                "total_call_edges": len(self.call_graph),
                "async_functions": sum(1 for f in self.functions if f.get("isAsync")),
            },
            "error": None,
        }

    def _get_annotation(self, node):
        """Convert an annotation AST node to a string."""
        if node is None:
            return None
        return ast.unparse(node)

    def _extract_params(self, args):
        """Extract parameters from a function's arguments."""
        params = []
        defaults_offset = len(args.args) - len(args.defaults)

        for i, arg in enumerate(args.args):
            if arg.arg == "self" or arg.arg == "cls":
                continue
            p = {
                "name": arg.arg,
                "type": self._get_annotation(arg.annotation),
                "isOptional": False,
            }
            default_idx = i - defaults_offset
            if default_idx >= 0 and default_idx < len(args.defaults):
                p["defaultValue"] = ast.unparse(args.defaults[default_idx])
                p["isOptional"] = True
            params.append(p)

        # *args
        if args.vararg:
            params.append({
                "name": "*" + args.vararg.arg,
                "type": self._get_annotation(args.vararg.annotation),
                "isRest": True,
            })

        # keyword-only args
        kw_defaults_iter = iter(args.kw_defaults)
        for kwarg in args.kwonlyargs:
            default = next(kw_defaults_iter, None)
            p = {
                "name": kwarg.arg,
                "type": self._get_annotation(kwarg.annotation),
                "isOptional": default is not None,
            }
            if default:
                p["defaultValue"] = ast.unparse(default)
            params.append(p)

        # **kwargs
        if args.kwarg:
            params.append({
                "name": "**" + args.kwarg.arg,
                "type": self._get_annotation(args.kwarg.annotation),
            })

        return params

    def _extract_calls(self, node):
        """Extract function calls from a function body."""
        calls = []
        seen = set()

        for child in ast.walk(node):
            if isinstance(child, ast.Call):
                name = None
                if isinstance(child.func, ast.Name):
                    name = child.func.id
                elif isinstance(child.func, ast.Attribute):
                    name = f".{child.func.attr}"
                if name and name not in seen:
                    seen.add(name)
                    calls.append(name)
                    if self.current_function:
                        self.call_graph.append({
                            "from": self.current_function,
                            "to": name,
                            "line": child.lineno,
                        })

        return calls

    def _calc_complexity(self, node):
        """Calculate cyclomatic complexity."""
        complexity = 1
        for child in ast.walk(node):
            if isinstance(child, (ast.If, ast.For, ast.While, ast.ExceptHandler,
                                  ast.With, ast.Assert)):
                complexity += 1
            elif isinstance(child, ast.BoolOp):
                complexity += len(child.values) - 1
        return complexity

    def visit_FunctionDef(self, node):
        self._visit_function(node, is_async=False)

    def visit_AsyncFunctionDef(self, node):
        self._visit_function(node, is_async=True)

    def _visit_function(self, node, is_async):
        full_name = node.name
        is_method = self.current_class is not None
        if is_method:
            full_name = f"{self.current_class}.{node.name}"

        old_fn = self.current_function
        self.current_function = full_name

        decorators = [ast.unparse(d) for d in node.decorator_list]

        fn = {
            "name": node.name,
            "fullName": full_name,
            "line": node.lineno,
            "endLine": node.end_lineno or node.lineno,
            "parameters": self._extract_params(node.args),
            "returnType": self._get_annotation(node.returns),
            "isAsync": is_async,
            "isMethod": is_method,
            "isPrivate": node.name.startswith("_") and not node.name.startswith("__"),
            "isStatic": "staticmethod" in decorators,
            "isClassmethod": "classmethod" in decorators,
            "decorators": decorators,
            "calls": self._extract_calls(node),
            "complexity": self._calc_complexity(node),
        }

        self.functions.append(fn)

        # If this is a method, add to the class
        if is_method:
            for cls in self.classes:
                if cls["name"] == self.current_class:
                    cls["methods"].append(node.name)
                    break

        self.generic_visit(node)
        self.current_function = old_fn

    def visit_ClassDef(self, node):
        bases = [ast.unparse(b) for b in node.bases]
        decorators = [ast.unparse(d) for d in node.decorator_list]

        # Extract properties (class-level assignments with type annotations)
        properties = []
        for item in node.body:
            if isinstance(item, ast.AnnAssign) and isinstance(item.target, ast.Name):
                properties.append({
                    "name": item.target.id,
                    "type": self._get_annotation(item.annotation),
                })

        cls = {
            "name": node.name,
            "line": node.lineno,
            "endLine": node.end_lineno or node.lineno,
            "bases": bases,
            "decorators": decorators,
            "methods": [],
            "properties": properties,
            "isExported": not node.name.startswith("_"),
            "isDataclass": "dataclass" in decorators,
        }
        self.classes.append(cls)

        old_class = self.current_class
        self.current_class = node.name
        self.generic_visit(node)
        self.current_class = old_class

    def visit_Import(self, node):
        for alias in node.names:
            self.imports.append({
                "module": alias.name,
                "alias": alias.asname,
                "line": node.lineno,
            })

    def visit_ImportFrom(self, node):
        module = node.module or ""
        for alias in node.names:
            self.imports.append({
                "module": f"{module}.{alias.name}" if module else alias.name,
                "alias": alias.asname,
                "line": node.lineno,
                "fromImport": True,
            })


def main():
    if len(sys.argv) < 2:
        print("Usage: py_analyzer.py <file.py> [--json]", file=sys.stderr)
        sys.exit(1)

    filepath = sys.argv[1]
    json_output = "--json" in sys.argv

    if not os.path.isfile(filepath):
        print(f"Error: File not found: {filepath}", file=sys.stderr)
        sys.exit(1)

    analyzer = PythonAnalyzer(filepath)
    result = analyzer.analyze()

    if json_output or True:  # Always JSON for qai integration
        print(json.dumps(result, indent=2, default=str))
    else:
        print(f"Python Analysis: {os.path.basename(filepath)}")
        print(f"  Functions: {len(result['functions'])}")
        print(f"  Classes: {len(result['classes'])}")
        print(f"  Imports: {len(result['imports'])}")
        print(f"  Call edges: {len(result['callGraph'])}")


if __name__ == "__main__":
    main()
