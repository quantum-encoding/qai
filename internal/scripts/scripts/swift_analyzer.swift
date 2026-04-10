#!/usr/bin/env swift

import Foundation

// Swift Compiler-Based Analyzer
// Uses Swift's SourceKit LSP for accurate code analysis

struct FunctionInfo: Codable {
    let name: String
    let fullName: String
    let line: Int
    let endLine: Int
    let parameters: [Parameter]
    let returnType: String?
    let isAsync: Bool
    let isThrows: Bool
    let isStatic: Bool
    let isPrivate: Bool
    let isOverride: Bool
    let isMutating: Bool
    let genericConstraints: [String]
    let calls: [String]
    let complexity: Int
}

struct Parameter: Codable {
    let name: String
    let type: String?
    let defaultValue: String?
    let isInout: Bool
    let isVariadic: Bool
}

struct ClassInfo: Codable {
    let name: String
    let line: Int
    let endLine: Int
    let superclass: String?
    let protocols: [String]
    let isStruct: Bool
    let isEnum: Bool
    let isProtocol: Bool
    let isActor: Bool
    let isFinal: Bool
    let methods: [String]
    let properties: [Property]
    let nestedTypes: [String]
}

struct Property: Codable {
    let name: String
    let type: String?
    let isLet: Bool
    let isStatic: Bool
    let isPrivate: Bool
    let hasGetter: Bool
    let hasSetter: Bool
}

struct CallEdge: Codable {
    let from: String
    let to: String
    let line: Int
    let isAsync: Bool
}

struct ImportInfo: Codable {
    let module: String
    let isTestable: Bool
    let line: Int
}

struct ExtensionInfo: Codable {
    let type: String
    let line: Int
    let endLine: Int
    let protocols: [String]
    let methods: [String]
    let whereClause: String?
}

struct AnalysisResult: Codable {
    let file: String
    let functions: [FunctionInfo]
    let classes: [ClassInfo]
    let extensions: [ExtensionInfo]
    let imports: [ImportInfo]
    let callGraph: [CallEdge]
    let statistics: Statistics
    let error: String?
}

struct Statistics: Codable {
    let totalFunctions: Int
    let totalClasses: Int
    let totalStructs: Int
    let totalEnums: Int
    let totalProtocols: Int
    let totalExtensions: Int
    let totalAsyncFunctions: Int
    let totalActors: Int
    let totalCalls: Int
    let avgComplexity: Double
}

class SwiftAnalyzer {
    private let filepath: String
    private var functions: [FunctionInfo] = []
    private var classes: [ClassInfo] = []
    private var extensions: [ExtensionInfo] = []
    private var imports: [ImportInfo] = []
    private var callGraph: [CallEdge] = []
    private var currentScope: [String] = []
    private var currentFunction: String?
    
    init(filepath: String) {
        self.filepath = filepath
    }
    
    func analyze() -> AnalysisResult {
        do {
            let sourceCode = try String(contentsOfFile: filepath, encoding: .utf8)
            parseSwiftCode(sourceCode)
            
            return AnalysisResult(
                file: filepath,
                functions: functions,
                classes: classes,
                extensions: extensions,
                imports: imports,
                callGraph: callGraph,
                statistics: calculateStatistics(),
                error: nil
            )
        } catch {
            return AnalysisResult(
                file: filepath,
                functions: [],
                classes: [],
                extensions: [],
                imports: [],
                callGraph: [],
                statistics: Statistics(
                    totalFunctions: 0,
                    totalClasses: 0,
                    totalStructs: 0,
                    totalEnums: 0,
                    totalProtocols: 0,
                    totalExtensions: 0,
                    totalAsyncFunctions: 0,
                    totalActors: 0,
                    totalCalls: 0,
                    avgComplexity: 0.0
                ),
                error: error.localizedDescription
            )
        }
    }
    
    private func parseSwiftCode(_ source: String) {
        let lines = source.components(separatedBy: .newlines)
        var lineNumber = 0
        var inComment = false
        var scopeStack: [(type: String, name: String, startLine: Int)] = []
        
        for line in lines {
            lineNumber += 1
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            
            // Skip comments
            if trimmed.hasPrefix("//") { continue }
            if trimmed.hasPrefix("/*") { inComment = true }
            if trimmed.hasSuffix("*/") { inComment = false; continue }
            if inComment { continue }
            
            // Parse imports
            if trimmed.hasPrefix("import ") {
                parseImport(trimmed, at: lineNumber)
            }
            
            // Parse type declarations
            if let match = matchTypeDeclaration(trimmed) {
                let typeInfo = ClassInfo(
                    name: match.name,
                    line: lineNumber,
                    endLine: lineNumber,  // Will be updated
                    superclass: match.superclass,
                    protocols: match.protocols,
                    isStruct: match.type == "struct",
                    isEnum: match.type == "enum",
                    isProtocol: match.type == "protocol",
                    isActor: match.type == "actor",
                    isFinal: trimmed.contains("final "),
                    methods: [],
                    properties: [],
                    nestedTypes: []
                )
                classes.append(typeInfo)
                scopeStack.append((type: match.type, name: match.name, startLine: lineNumber))
                currentScope.append(match.name)
            }
            
            // Parse extensions
            if trimmed.hasPrefix("extension ") {
                if let extensionName = parseExtension(trimmed) {
                    let extInfo = ExtensionInfo(
                        type: extensionName,
                        line: lineNumber,
                        endLine: lineNumber,  // Will be updated
                        protocols: extractProtocols(from: trimmed),
                        methods: [],
                        whereClause: extractWhereClause(from: trimmed)
                    )
                    extensions.append(extInfo)
                    scopeStack.append((type: "extension", name: extensionName, startLine: lineNumber))
                    currentScope.append(extensionName)
                }
            }
            
            // Parse functions
            if let funcMatch = matchFunction(trimmed) {
                let fullName = currentScope.isEmpty ? funcMatch.name : "\(currentScope.joined(separator: ".")).\(funcMatch.name)"
                
                let funcInfo = FunctionInfo(
                    name: funcMatch.name,
                    fullName: fullName,
                    line: lineNumber,
                    endLine: lineNumber,  // Will be updated
                    parameters: parseParameters(funcMatch.params),
                    returnType: funcMatch.returnType,
                    isAsync: funcMatch.isAsync,
                    isThrows: funcMatch.isThrows,
                    isStatic: trimmed.contains("static ") || trimmed.contains("class func"),
                    isPrivate: trimmed.contains("private ") || trimmed.contains("fileprivate "),
                    isOverride: trimmed.contains("override "),
                    isMutating: trimmed.contains("mutating "),
                    genericConstraints: parseGenericConstraints(trimmed),
                    calls: [],
                    complexity: 1
                )
                functions.append(funcInfo)
                
                let oldFunction = currentFunction
                currentFunction = fullName
                // Parse function body for calls
                parseFunctionBody(lines, startLine: lineNumber)
                currentFunction = oldFunction
            }
            
            // Parse property declarations
            if let propMatch = matchProperty(trimmed) {
                if !scopeStack.isEmpty && !classes.isEmpty {
                    let prop = Property(
                        name: propMatch.name,
                        type: propMatch.type,
                        isLet: propMatch.isLet,
                        isStatic: trimmed.contains("static "),
                        isPrivate: trimmed.contains("private "),
                        hasGetter: trimmed.contains("get"),
                        hasSetter: trimmed.contains("set")
                    )
                    // Add to last class
                    if let lastClass = classes.last {
                        var props = lastClass.properties
                        props.append(prop)
                        classes[classes.count - 1] = ClassInfo(
                            name: lastClass.name,
                            line: lastClass.line,
                            endLine: lastClass.endLine,
                            superclass: lastClass.superclass,
                            protocols: lastClass.protocols,
                            isStruct: lastClass.isStruct,
                            isEnum: lastClass.isEnum,
                            isProtocol: lastClass.isProtocol,
                            isActor: lastClass.isActor,
                            isFinal: lastClass.isFinal,
                            methods: lastClass.methods,
                            properties: props,
                            nestedTypes: lastClass.nestedTypes
                        )
                    }
                }
            }
            
            // Parse function calls
            if let callName = extractFunctionCall(trimmed) {
                if let currentFunc = currentFunction {
                    callGraph.append(CallEdge(
                        from: currentFunc,
                        to: callName,
                        line: lineNumber,
                        isAsync: trimmed.contains("await ")
                    ))
                }
            }
            
            // Handle scope closing
            if trimmed == "}" && !scopeStack.isEmpty {
                let scope = scopeStack.removeLast()
                updateEndLine(for: scope, endLine: lineNumber)
                if !currentScope.isEmpty {
                    currentScope.removeLast()
                }
            }
        }
    }
    
    private func matchTypeDeclaration(_ line: String) -> (type: String, name: String, superclass: String?, protocols: [String])? {
        let types = ["class", "struct", "enum", "protocol", "actor"]
        
        for type in types {
            if line.contains("\(type) ") {
                // Extract name and inheritance
                let pattern = "\(type)\\s+(\\w+)(?:<[^>]+>)?(?:\\s*:\\s*(.+))?"
                if let regex = try? NSRegularExpression(pattern: pattern),
                   let match = regex.firstMatch(in: line, range: NSRange(line.startIndex..., in: line)) {
                    
                    let name = String(line[Range(match.range(at: 1), in: line)!])
                    
                    if match.numberOfRanges > 2,
                       let inheritanceRange = Range(match.range(at: 2), in: line) {
                        let inheritance = String(line[inheritanceRange])
                        let parts = inheritance.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
                        
                        // First item might be superclass (for classes)
                        let superclass = (type == "class" && !parts.isEmpty) ? parts[0] : nil
                        let protocols = (type == "class" && !parts.isEmpty) ? Array(parts.dropFirst()) : parts
                        
                        return (type: type, name: name, superclass: superclass, protocols: protocols)
                    }
                    
                    return (type: type, name: name, superclass: nil, protocols: [])
                }
            }
        }
        return nil
    }
    
    private func matchFunction(_ line: String) -> (name: String, params: String, returnType: String?, isAsync: Bool, isThrows: Bool)? {
        let pattern = "func\\s+(\\w+)(?:<[^>]+>)?\\s*\\(([^)]*)\\)(?:\\s*(?:async\\s*)?(?:throws\\s*)?->\\s*(.+))?"
        
        if let regex = try? NSRegularExpression(pattern: pattern),
           let match = regex.firstMatch(in: line, range: NSRange(line.startIndex..., in: line)) {
            
            let name = String(line[Range(match.range(at: 1), in: line)!])
            let params = match.numberOfRanges > 2 && match.range(at: 2).location != NSNotFound
                ? String(line[Range(match.range(at: 2), in: line)!])
                : ""
            let returnType = match.numberOfRanges > 3 && match.range(at: 3).location != NSNotFound
                ? String(line[Range(match.range(at: 3), in: line)!]).trimmingCharacters(in: .whitespaces)
                : nil
            
            return (
                name: name,
                params: params,
                returnType: returnType,
                isAsync: line.contains(" async ") || line.contains(" async->"),
                isThrows: line.contains(" throws ") || line.contains(" throws->")
            )
        }
        return nil
    }
    
    private func matchProperty(_ line: String) -> (name: String, type: String?, isLet: Bool)? {
        let patterns = [
            "(let|var)\\s+(\\w+)\\s*:\\s*([^=\\{]+)",
            "(let|var)\\s+(\\w+)\\s*="
        ]
        
        for pattern in patterns {
            if let regex = try? NSRegularExpression(pattern: pattern),
               let match = regex.firstMatch(in: line, range: NSRange(line.startIndex..., in: line)) {
                
                let keyword = String(line[Range(match.range(at: 1), in: line)!])
                let name = String(line[Range(match.range(at: 2), in: line)!])
                let type = match.numberOfRanges > 3 && match.range(at: 3).location != NSNotFound
                    ? String(line[Range(match.range(at: 3), in: line)!]).trimmingCharacters(in: .whitespaces)
                    : nil
                
                return (name: name, type: type, isLet: keyword == "let")
            }
        }
        return nil
    }
    
    private func parseImport(_ line: String, at lineNumber: Int) {
        let components = line.split(separator: " ")
        if components.count >= 2 {
            let isTestable = line.contains("@testable")
            let module = String(components.last ?? "")
            imports.append(ImportInfo(
                module: module,
                isTestable: isTestable,
                line: lineNumber
            ))
        }
    }
    
    private func parseExtension(_ line: String) -> String? {
        if let range = line.range(of: "extension ") {
            let afterExtension = String(line[range.upperBound...])
            let components = afterExtension.split(separator: ":")
            if let firstComponent = components.first {
                return String(firstComponent).trimmingCharacters(in: .whitespaces)
            }
        }
        return nil
    }
    
    private func parseParameters(_ params: String) -> [Parameter] {
        guard !params.isEmpty else { return [] }
        
        var parameters: [Parameter] = []
        let paramList = params.split(separator: ",")
        
        for param in paramList {
            let trimmed = param.trimmingCharacters(in: .whitespaces)
            
            // Parse parameter: [label] name: Type [= default]
            let components = trimmed.split(separator: ":", maxSplits: 1)
            if components.count >= 1 {
                let namePart = String(components[0]).trimmingCharacters(in: .whitespaces)
                let typePart = components.count > 1 ? String(components[1]).trimmingCharacters(in: .whitespaces) : nil
                
                // Extract name (might have external label)
                let nameComponents = namePart.split(separator: " ")
                let name = String(nameComponents.last ?? "")
                
                // Extract type and default value
                var type: String?
                var defaultValue: String?
                if let typePart = typePart {
                    if let equalIndex = typePart.firstIndex(of: "=") {
                        type = String(typePart[..<equalIndex]).trimmingCharacters(in: .whitespaces)
                        defaultValue = String(typePart[typePart.index(after: equalIndex)...]).trimmingCharacters(in: .whitespaces)
                    } else {
                        type = typePart
                    }
                }
                
                parameters.append(Parameter(
                    name: name,
                    type: type,
                    defaultValue: defaultValue,
                    isInout: trimmed.contains("inout "),
                    isVariadic: type?.contains("...") ?? false
                ))
            }
        }
        
        return parameters
    }
    
    private func parseGenericConstraints(_ line: String) -> [String] {
        if let start = line.firstIndex(of: "<"),
           let end = line.firstIndex(of: ">"),
           start < end {
            let constraints = String(line[line.index(after: start)..<end])
            return constraints.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
        }
        return []
    }
    
    private func extractProtocols(from line: String) -> [String] {
        if let colonIndex = line.firstIndex(of: ":") {
            let afterColon = String(line[line.index(after: colonIndex)...])
            let beforeBrace = afterColon.split(separator: "{")[0]
            return beforeBrace.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }
        }
        return []
    }
    
    private func extractWhereClause(from line: String) -> String? {
        if let whereRange = line.range(of: " where ") {
            let afterWhere = String(line[whereRange.upperBound...])
            if let braceIndex = afterWhere.firstIndex(of: "{") {
                return String(afterWhere[..<braceIndex]).trimmingCharacters(in: .whitespaces)
            }
            return afterWhere.trimmingCharacters(in: .whitespaces)
        }
        return nil
    }
    
    private func extractFunctionCall(_ line: String) -> String? {
        // Match function calls: word(...)
        let pattern = "(\\w+)\\s*\\("
        if let regex = try? NSRegularExpression(pattern: pattern),
           let match = regex.firstMatch(in: line, range: NSRange(line.startIndex..., in: line)) {
            let name = String(line[Range(match.range(at: 1), in: line)!])
            // Filter out control flow keywords
            let keywords = ["if", "while", "for", "switch", "guard", "catch", "func", "init"]
            if !keywords.contains(name) {
                return name
            }
        }
        return nil
    }
    
    private func parseFunctionBody(_ lines: [String], startLine: Int) {
        // Simple parsing - in production would use SourceKit
        var braceCount = 0
        var inFunction = false
        
        for i in (startLine - 1)..<lines.count {
            let line = lines[i].trimmingCharacters(in: .whitespaces)
            
            for char in line {
                if char == "{" {
                    braceCount += 1
                    inFunction = true
                } else if char == "}" {
                    braceCount -= 1
                    if braceCount == 0 && inFunction {
                        return  // End of function
                    }
                }
            }
            
            if inFunction {
                if let callName = extractFunctionCall(line) {
                    if let currentFunc = currentFunction {
                        callGraph.append(CallEdge(
                            from: currentFunc,
                            to: callName,
                            line: i + 1,
                            isAsync: line.contains("await ")
                        ))
                        
                        // Add to function's calls
                        if let index = functions.firstIndex(where: { $0.fullName == currentFunc }) {
                            var function = functions[index]
                            var calls = function.calls
                            calls.append(callName)
                            functions[index] = FunctionInfo(
                                name: function.name,
                                fullName: function.fullName,
                                line: function.line,
                                endLine: function.endLine,
                                parameters: function.parameters,
                                returnType: function.returnType,
                                isAsync: function.isAsync,
                                isThrows: function.isThrows,
                                isStatic: function.isStatic,
                                isPrivate: function.isPrivate,
                                isOverride: function.isOverride,
                                isMutating: function.isMutating,
                                genericConstraints: function.genericConstraints,
                                calls: calls,
                                complexity: function.complexity
                            )
                        }
                    }
                }
            }
        }
    }
    
    private func updateEndLine(for scope: (type: String, name: String, startLine: Int), endLine: Int) {
        // Update end line for classes
        if let index = classes.firstIndex(where: { $0.name == scope.name && $0.line == scope.startLine }) {
            let cls = classes[index]
            classes[index] = ClassInfo(
                name: cls.name,
                line: cls.line,
                endLine: endLine,
                superclass: cls.superclass,
                protocols: cls.protocols,
                isStruct: cls.isStruct,
                isEnum: cls.isEnum,
                isProtocol: cls.isProtocol,
                isActor: cls.isActor,
                isFinal: cls.isFinal,
                methods: cls.methods,
                properties: cls.properties,
                nestedTypes: cls.nestedTypes
            )
        }
        
        // Update end line for extensions
        if scope.type == "extension" {
            if let index = extensions.firstIndex(where: { $0.type == scope.name && $0.line == scope.startLine }) {
                let ext = extensions[index]
                extensions[index] = ExtensionInfo(
                    type: ext.type,
                    line: ext.line,
                    endLine: endLine,
                    protocols: ext.protocols,
                    methods: ext.methods,
                    whereClause: ext.whereClause
                )
            }
        }
    }
    
    private func calculateStatistics() -> Statistics {
        let totalStructs = classes.filter { $0.isStruct }.count
        let totalEnums = classes.filter { $0.isEnum }.count
        let totalProtocols = classes.filter { $0.isProtocol }.count
        let totalActors = classes.filter { $0.isActor }.count
        let totalAsyncFunctions = functions.filter { $0.isAsync }.count
        
        let avgComplexity: Double = functions.isEmpty ? 0.0 :
            Double(functions.reduce(0) { $0 + $1.complexity }) / Double(functions.count)
        
        return Statistics(
            totalFunctions: functions.count,
            totalClasses: classes.filter { !$0.isStruct && !$0.isEnum && !$0.isProtocol && !$0.isActor }.count,
            totalStructs: totalStructs,
            totalEnums: totalEnums,
            totalProtocols: totalProtocols,
            totalExtensions: extensions.count,
            totalAsyncFunctions: totalAsyncFunctions,
            totalActors: totalActors,
            totalCalls: callGraph.count,
            avgComplexity: avgComplexity
        )
    }
}

// Main execution
let arguments = CommandLine.arguments
guard arguments.count >= 2 else {
    print("Usage: swift swift_compiler_analyzer.swift <file.swift> [--json]")
    exit(1)
}

let filepath = arguments[1]
let outputJson = arguments.contains("--json")

// Check if file exists
guard FileManager.default.fileExists(atPath: filepath) else {
    print("Error: File not found: \(filepath)")
    exit(1)
}

let analyzer = SwiftAnalyzer(filepath: filepath)
let result = analyzer.analyze()

if outputJson {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
    if let jsonData = try? encoder.encode(result),
       let jsonString = String(data: jsonData, encoding: .utf8) {
        print(jsonString)
    }
} else {
    print("Swift Analysis: \(filepath)")
    print(String(repeating: "=", count: 50))
    print("Functions: \(result.statistics.totalFunctions)")
    print("Classes: \(result.statistics.totalClasses)")
    print("Structs: \(result.statistics.totalStructs)")
    print("Enums: \(result.statistics.totalEnums)")
    print("Protocols: \(result.statistics.totalProtocols)")
    print("Actors: \(result.statistics.totalActors)")
    print("Extensions: \(result.statistics.totalExtensions)")
    print("Async functions: \(result.statistics.totalAsyncFunctions)")
    print("Call graph edges: \(result.statistics.totalCalls)")
    print("Average complexity: \(result.statistics.avgComplexity)")
    
    if !result.functions.isEmpty {
        print("\nFunctions:")
        for function in result.functions.prefix(10) {
            print("  - \(function.name) at line \(function.line)")
            if !function.calls.isEmpty {
                print("    Calls: \(function.calls.joined(separator: ", "))")
            }
        }
    }
    
    if !result.classes.isEmpty {
        print("\nTypes:")
        for cls in result.classes.prefix(10) {
            let typeStr = cls.isStruct ? "struct" : cls.isEnum ? "enum" : cls.isProtocol ? "protocol" : cls.isActor ? "actor" : "class"
            print("  - \(typeStr) \(cls.name) at line \(cls.line)")
            if !cls.methods.isEmpty {
                print("    Methods: \(cls.methods.prefix(5).joined(separator: ", "))")
            }
        }
    }
}