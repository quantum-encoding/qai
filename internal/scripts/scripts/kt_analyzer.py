#!/usr/bin/env python3
"""
Kotlin Analyzer — extracts functions, classes, interfaces, data classes,
imports, and call graphs from .kt files using regex parsing.

Kotlin's syntax is regular enough for accurate extraction:
- data class, class, interface, enum class, object, sealed class
- fun with suspend/inline/infix/operator modifiers
- @SerialName annotations for JSON field mapping
- typealias

Usage: python3 kt_analyzer.py <file.kt> [--json]
"""

import json
import re
import sys
import os


def analyze_kotlin_file(filepath):
    try:
        with open(filepath, "r") as f:
            source = f.read()
    except Exception as e:
        return {"file": filepath, "error": str(e),
                "functions": [], "classes": [], "imports": [], "callGraph": []}

    lines = source.split("\n")
    functions = []
    classes = []
    imports = []
    call_graph = []
    current_class = None
    current_function = None

    # Patterns
    import_re = re.compile(r"^import\s+(.+)")
    class_re = re.compile(
        r"^(\s*)((?:public|private|internal|protected|abstract|open|sealed|data|enum|inner|annotation)\s+)*"
        r"(class|interface|object)\s+(\w+)"
        r"(?:\s*<([^>]*)>)?"  # generics
        r"(?:\s*\(([^)]*)\))?"  # constructor params
        r"(?:\s*:\s*(.+?))?\s*\{?"  # superclass/interfaces
    )
    fun_re = re.compile(
        r"^(\s*)((?:public|private|internal|protected|override|open|abstract|final|inline|suspend|infix|operator|tailrec)\s+)*"
        r"fun\s+(?:<([^>]*)>\s+)?"  # generics
        r"(?:(\w+)\.)?(\w+)"  # receiver.name or just name
        r"\s*\(([^)]*)\)"  # params
        r"(?:\s*:\s*(\S+))?"  # return type
    )
    typealias_re = re.compile(r"^\s*typealias\s+(\w+)\s*=\s*(.+)")
    serial_name_re = re.compile(r'@SerialName\("([^"]+)"\)')
    call_re = re.compile(r"(\w+)\s*\(")

    for i, line in enumerate(lines, 1):
        stripped = line.strip()

        # Imports
        m = import_re.match(stripped)
        if m:
            imports.append({"module": m.group(1).strip(), "line": i})
            continue

        # Classes/interfaces/objects
        m = class_re.match(line)
        if m:
            indent = len(m.group(1) or "")
            modifiers = (m.group(2) or "").split()
            kind = m.group(3)
            name = m.group(4)
            generics = m.group(5)
            constructor_params = m.group(6) or ""
            bases = m.group(7) or ""

            # Parse constructor properties — may span multiple lines.
            cls = {
                "name": name,
                "line": i,
                "endLine": i,
                "kind": kind,
                "isData": "data" in modifiers,
                "isSealed": "sealed" in modifiers,
                "isAbstract": "abstract" in modifiers,
                "isEnum": "enum" in modifiers,
                "isExported": "private" not in modifiers and "internal" not in modifiers,
                "bases": [b.strip() for b in bases.split(",") if b.strip()] if bases else [],
                "properties": [],
                "methods": [],
                "generics": generics,
            }

            # Parse constructor properties — may span multiple lines.
            if "(" in line:
                paren_depth = line.count("(") - line.count(")")
                param_block = line
                j = i
                while paren_depth > 0 and j < len(lines):
                    param_block += "\n" + lines[j]
                    paren_depth += lines[j].count("(") - lines[j].count(")")
                    j += 1
                cls["endLine"] = j

                serial_name = None
                for pline in param_block.split("\n"):
                    sn = serial_name_re.search(pline)
                    if sn:
                        serial_name = sn.group(1)
                    pm = re.search(r"(?:val|var)\s+(\w+)\s*:\s*([^,=)]+)", pline)
                    if pm:
                        pname = pm.group(1)
                        ptype = pm.group(2).strip()
                        json_name = serial_name or pname
                        cls["properties"].append({"name": pname, "type": ptype, "jsonTag": json_name})
                        serial_name = None

            classes.append(cls)
            current_class = name
            continue

        # Functions
        m = fun_re.match(line)
        if m:
            indent = len(m.group(1) or "")
            modifiers = (m.group(2) or "").split()
            generics = m.group(3)
            receiver = m.group(4)
            name = m.group(5)
            params_str = m.group(6) or ""
            return_type = m.group(7)

            full_name = name
            is_method = current_class is not None
            if is_method:
                full_name = f"{current_class}.{name}"
            elif receiver:
                full_name = f"{receiver}.{name}"

            # Parse parameters
            params = []
            for param_match in re.finditer(r"(\w+)\s*:\s*([^,=]+?)(?:\s*=\s*[^,]+)?(?:,|$)", params_str):
                params.append({
                    "name": param_match.group(1),
                    "type": param_match.group(2).strip(),
                })

            fn = {
                "name": name,
                "fullName": full_name,
                "line": i,
                "endLine": i,
                "parameters": params,
                "returnType": return_type,
                "isSuspend": "suspend" in modifiers,
                "isInline": "inline" in modifiers,
                "isPrivate": "private" in modifiers,
                "isOverride": "override" in modifiers,
                "isAbstract": "abstract" in modifiers,
                "isMethod": is_method,
                "calls": [],
                "complexity": 1,
            }
            functions.append(fn)
            current_function = full_name

            # Add to class methods
            if is_method:
                for cls in classes:
                    if cls["name"] == current_class:
                        cls["methods"].append(name)
                        break
            continue

        # Typealias
        m = typealias_re.match(stripped)
        if m:
            classes.append({
                "name": m.group(1),
                "line": i,
                "endLine": i,
                "kind": "typealias",
                "isData": False, "isSealed": False, "isAbstract": False,
                "isEnum": False, "isExported": True,
                "bases": [m.group(2).strip()],
                "properties": [], "methods": [],
            })
            continue

        # Call detection (simple: any word followed by '(')
        if current_function:
            for call_match in call_re.finditer(stripped):
                callee = call_match.group(1)
                if callee not in ("if", "for", "while", "when", "return", "throw", "catch", "val", "var", "fun", "class"):
                    call_graph.append({
                        "from": current_function,
                        "to": callee,
                        "line": i,
                    })

        # Complexity counting
        if current_function and functions:
            if re.match(r"\s*(if|for|while|when|catch|&&|\|\|)", stripped):
                functions[-1]["complexity"] += 1

    return {
        "file": filepath,
        "functions": functions,
        "classes": classes,
        "imports": imports,
        "callGraph": call_graph,
        "statistics": {
            "total_functions": len(functions),
            "total_classes": len(classes),
            "total_imports": len(imports),
            "total_call_edges": len(call_graph),
            "data_classes": sum(1 for c in classes if c.get("isData")),
            "suspend_functions": sum(1 for f in functions if f.get("isSuspend")),
        },
        "error": None,
    }


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: kt_analyzer.py <file.kt> [--json]", file=sys.stderr)
        sys.exit(1)

    result = analyze_kotlin_file(sys.argv[1])
    print(json.dumps(result, indent=2, default=str))
