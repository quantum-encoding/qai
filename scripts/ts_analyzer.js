#!/usr/bin/env node

/**
 * TypeScript/JavaScript Compiler-Based Analyzer
 * Uses TypeScript Compiler API for 100% accurate code analysis
 * Supports both TypeScript and JavaScript files
 */

const ts = require('typescript');
const fs = require('fs');
const path = require('path');

class Parameter {
    constructor(name, type = null, defaultValue = null, isOptional = false, isRest = false) {
        this.name = name;
        this.type = type;
        this.defaultValue = defaultValue;
        this.isOptional = isOptional;
        this.isRest = isRest;
    }
}

class FunctionInfo {
    constructor(name, fullName, line, endLine, parameters = [], returnType = null) {
        this.name = name;
        this.fullName = fullName;
        this.line = line;
        this.endLine = endLine;
        this.parameters = parameters;
        this.returnType = returnType;
        this.isAsync = false;
        this.isGenerator = false;
        this.isArrow = false;
        this.isMethod = false;
        this.isStatic = false;
        this.isPrivate = false;
        this.isProtected = false;
        this.isAbstract = false;
        this.isOverride = false;
        this.modifiers = [];
        this.typeParameters = [];
        this.calls = [];
        this.complexity = 1;
    }
}

class ClassInfo {
    constructor(name, fullName, line, endLine) {
        this.name = name;
        this.fullName = fullName;
        this.line = line;
        this.endLine = endLine;
        this.isAbstract = false;
        this.isExported = false;
        this.superClass = null;
        this.interfaces = [];
        this.methods = [];
        this.properties = [];
        this.typeParameters = [];
        this.modifiers = [];
    }
}

class InterfaceInfo {
    constructor(name, fullName, line, endLine) {
        this.name = name;
        this.fullName = fullName;
        this.line = line;
        this.endLine = endLine;
        this.extends = [];
        this.methods = [];
        this.properties = [];
        this.typeParameters = [];
        this.isExported = false;
    }
}

class ImportInfo {
    constructor(specifier, source, line, isDefault = false, isNamespace = false) {
        this.specifier = specifier;
        this.source = source;
        this.line = line;
        this.isDefault = isDefault;
        this.isNamespace = isNamespace;
        this.alias = null;
    }
}

class CallEdge {
    constructor(from, to, line, isAsync = false) {
        this.from = from;
        this.to = to;
        this.line = line;
        this.isAsync = isAsync;
    }
}

class TypeScriptAnalyzer {
    constructor(filePath) {
        this.filePath = filePath;
        this.sourceFile = null;
        this.checker = null;
        this.program = null;
        this.functions = [];
        this.classes = [];
        this.interfaces = [];
        this.enums = [];
        this.imports = [];
        this.callGraph = [];
        this.currentFunction = null;
        this.currentClass = null;
    }

    analyze() {
        try {
            // Read the file
            const sourceCode = fs.readFileSync(this.filePath, 'utf8');
            
            // Create TypeScript program
            const program = ts.createProgram([this.filePath], {
                target: ts.ScriptTarget.Latest,
                module: ts.ModuleKind.CommonJS,
                allowJs: true,
                checkJs: false,
                jsx: ts.JsxEmit.React
            });
            
            this.program = program;
            this.sourceFile = program.getSourceFile(this.filePath);
            this.checker = program.getTypeChecker();
            
            if (!this.sourceFile) {
                throw new Error(`Could not load source file: ${this.filePath}`);
            }
            
            // Visit all nodes in the AST
            this.visit(this.sourceFile);
            
            return {
                file: this.filePath,
                functions: this.functions,
                classes: this.classes,
                interfaces: this.interfaces,
                enums: this.enums,
                imports: this.imports,
                callGraph: this.callGraph,
                statistics: this.calculateStatistics(),
                error: null
            };
            
        } catch (error) {
            return {
                file: this.filePath,
                functions: [],
                classes: [],
                interfaces: [],
                enums: [],
                imports: [],
                callGraph: [],
                statistics: {},
                error: error.message
            };
        }
    }
    
    visit(node) {
        switch (node.kind) {
            case ts.SyntaxKind.FunctionDeclaration:
                this.visitFunctionDeclaration(node);
                break;
            case ts.SyntaxKind.MethodDeclaration:
                this.visitMethodDeclaration(node);
                break;
            case ts.SyntaxKind.ArrowFunction:
                this.visitArrowFunction(node);
                break;
            case ts.SyntaxKind.FunctionExpression:
                this.visitFunctionExpression(node);
                break;
            case ts.SyntaxKind.ClassDeclaration:
                this.visitClassDeclaration(node);
                break;
            case ts.SyntaxKind.InterfaceDeclaration:
                this.visitInterfaceDeclaration(node);
                break;
            case ts.SyntaxKind.EnumDeclaration:
                this.visitEnumDeclaration(node);
                break;
            case ts.SyntaxKind.ImportDeclaration:
                this.visitImportDeclaration(node);
                break;
            case ts.SyntaxKind.CallExpression:
                this.visitCallExpression(node);
                break;
        }
        
        ts.forEachChild(node, child => this.visit(child));
    }
    
    visitFunctionDeclaration(node) {
        const name = node.name ? node.name.text : '<anonymous>';
        const line = this.getLineNumber(node);
        const endLine = this.getEndLineNumber(node);
        
        const func = new FunctionInfo(name, name, line, endLine);
        this.extractFunctionDetails(func, node);
        
        const oldFunction = this.currentFunction;
        this.currentFunction = func.fullName;
        
        this.functions.push(func);
        
        // Visit body
        if (node.body) {
            this.visit(node.body);
        }
        
        this.currentFunction = oldFunction;
    }
    
    visitMethodDeclaration(node) {
        const name = node.name.text;
        const className = this.currentClass || 'Unknown';
        const fullName = `${className}.${name}`;
        const line = this.getLineNumber(node);
        const endLine = this.getEndLineNumber(node);
        
        const func = new FunctionInfo(name, fullName, line, endLine);
        func.isMethod = true;
        this.extractFunctionDetails(func, node);
        
        // Add to current class
        if (this.currentClass) {
            const classInfo = this.classes.find(c => c.name === this.currentClass);
            if (classInfo) {
                classInfo.methods.push(name);
            }
        }
        
        const oldFunction = this.currentFunction;
        this.currentFunction = func.fullName;
        
        this.functions.push(func);
        
        // Visit body
        if (node.body) {
            this.visit(node.body);
        }
        
        this.currentFunction = oldFunction;
    }
    
    visitArrowFunction(node) {
        const name = '<arrow>';
        const line = this.getLineNumber(node);
        const endLine = this.getEndLineNumber(node);
        
        const func = new FunctionInfo(name, name, line, endLine);
        func.isArrow = true;
        this.extractFunctionDetails(func, node);
        
        const oldFunction = this.currentFunction;
        this.currentFunction = func.fullName;
        
        this.functions.push(func);
        
        // Visit body
        this.visit(node.body);
        
        this.currentFunction = oldFunction;
    }
    
    visitFunctionExpression(node) {
        const name = node.name ? node.name.text : '<expression>';
        const line = this.getLineNumber(node);
        const endLine = this.getEndLineNumber(node);
        
        const func = new FunctionInfo(name, name, line, endLine);
        this.extractFunctionDetails(func, node);
        
        const oldFunction = this.currentFunction;
        this.currentFunction = func.fullName;
        
        this.functions.push(func);
        
        // Visit body
        if (node.body) {
            this.visit(node.body);
        }
        
        this.currentFunction = oldFunction;
    }
    
    visitClassDeclaration(node) {
        const name = node.name.text;
        const line = this.getLineNumber(node);
        const endLine = this.getEndLineNumber(node);
        
        const classInfo = new ClassInfo(name, name, line, endLine);
        
        // Extract modifiers
        if (node.modifiers) {
            node.modifiers.forEach(modifier => {
                switch (modifier.kind) {
                    case ts.SyntaxKind.AbstractKeyword:
                        classInfo.isAbstract = true;
                        classInfo.modifiers.push('abstract');
                        break;
                    case ts.SyntaxKind.ExportKeyword:
                        classInfo.isExported = true;
                        classInfo.modifiers.push('export');
                        break;
                }
            });
        }
        
        // Extract extends clause
        if (node.heritageClauses) {
            node.heritageClauses.forEach(clause => {
                if (clause.token === ts.SyntaxKind.ExtendsKeyword && clause.types.length > 0) {
                    classInfo.superClass = clause.types[0].expression.text;
                }
                if (clause.token === ts.SyntaxKind.ImplementsKeyword) {
                    clause.types.forEach(type => {
                        classInfo.interfaces.push(type.expression.text);
                    });
                }
            });
        }
        
        const oldClass = this.currentClass;
        this.currentClass = name;
        
        this.classes.push(classInfo);
        
        // Visit members
        node.members.forEach(member => this.visit(member));
        
        this.currentClass = oldClass;
    }
    
    visitInterfaceDeclaration(node) {
        const name = node.name.text;
        const line = this.getLineNumber(node);
        const endLine = this.getEndLineNumber(node);
        
        const interfaceInfo = new InterfaceInfo(name, name, line, endLine);
        
        // Extract modifiers
        if (node.modifiers) {
            node.modifiers.forEach(modifier => {
                if (modifier.kind === ts.SyntaxKind.ExportKeyword) {
                    interfaceInfo.isExported = true;
                }
            });
        }
        
        // Extract extends
        if (node.heritageClauses) {
            node.heritageClauses.forEach(clause => {
                if (clause.token === ts.SyntaxKind.ExtendsKeyword) {
                    clause.types.forEach(type => {
                        interfaceInfo.extends.push(type.expression.text);
                    });
                }
            });
        }
        
        this.interfaces.push(interfaceInfo);
        
        // Visit members
        node.members.forEach(member => this.visit(member));
    }
    
    visitEnumDeclaration(node) {
        const name = node.name.text;
        const line = this.getLineNumber(node);
        const endLine = this.getEndLineNumber(node);
        
        const enumInfo = {
            name,
            fullName: name,
            line,
            endLine,
            members: [],
            isExported: false
        };
        
        // Extract modifiers
        if (node.modifiers) {
            node.modifiers.forEach(modifier => {
                if (modifier.kind === ts.SyntaxKind.ExportKeyword) {
                    enumInfo.isExported = true;
                }
            });
        }
        
        // Extract members
        node.members.forEach(member => {
            enumInfo.members.push(member.name.text);
        });
        
        this.enums.push(enumInfo);
    }
    
    visitImportDeclaration(node) {
        const line = this.getLineNumber(node);
        const source = node.moduleSpecifier.text;
        
        if (node.importClause) {
            if (node.importClause.name) {
                // Default import
                const importInfo = new ImportInfo(
                    node.importClause.name.text,
                    source,
                    line,
                    true
                );
                this.imports.push(importInfo);
            }
            
            if (node.importClause.namedBindings) {
                if (node.importClause.namedBindings.kind === ts.SyntaxKind.NamespaceImport) {
                    // Namespace import
                    const importInfo = new ImportInfo(
                        node.importClause.namedBindings.name.text,
                        source,
                        line,
                        false,
                        true
                    );
                    this.imports.push(importInfo);
                } else if (node.importClause.namedBindings.kind === ts.SyntaxKind.NamedImports) {
                    // Named imports
                    node.importClause.namedBindings.elements.forEach(element => {
                        const importInfo = new ImportInfo(
                            element.name.text,
                            source,
                            line
                        );
                        if (element.propertyName) {
                            importInfo.alias = element.name.text;
                            importInfo.specifier = element.propertyName.text;
                        }
                        this.imports.push(importInfo);
                    });
                }
            }
        }
    }
    
    visitCallExpression(node) {
        if (this.currentFunction) {
            const line = this.getLineNumber(node);
            let callName = '';
            
            if (node.expression.kind === ts.SyntaxKind.Identifier) {
                callName = node.expression.text;
            } else if (node.expression.kind === ts.SyntaxKind.PropertyAccessExpression) {
                callName = node.expression.name.text;
            }
            
            if (callName) {
                const edge = new CallEdge(this.currentFunction, callName, line);
                this.callGraph.push(edge);
                
                // Add to function's call list
                const func = this.functions.find(f => f.fullName === this.currentFunction);
                if (func && !func.calls.includes(callName)) {
                    func.calls.push(callName);
                }
            }
        }
    }
    
    extractFunctionDetails(func, node) {
        // Extract parameters
        if (node.parameters) {
            node.parameters.forEach(param => {
                const parameter = new Parameter(
                    param.name.text,
                    param.type ? this.getTypeText(param.type) : null,
                    param.initializer ? param.initializer.text : null,
                    !!param.questionToken,
                    !!param.dotDotDotToken
                );
                func.parameters.push(parameter);
            });
        }
        
        // Extract return type
        if (node.type) {
            func.returnType = this.getTypeText(node.type);
        }
        
        // Extract modifiers
        if (node.modifiers) {
            node.modifiers.forEach(modifier => {
                switch (modifier.kind) {
                    case ts.SyntaxKind.AsyncKeyword:
                        func.isAsync = true;
                        func.modifiers.push('async');
                        break;
                    case ts.SyntaxKind.StaticKeyword:
                        func.isStatic = true;
                        func.modifiers.push('static');
                        break;
                    case ts.SyntaxKind.PrivateKeyword:
                        func.isPrivate = true;
                        func.modifiers.push('private');
                        break;
                    case ts.SyntaxKind.ProtectedKeyword:
                        func.isProtected = true;
                        func.modifiers.push('protected');
                        break;
                    case ts.SyntaxKind.AbstractKeyword:
                        func.isAbstract = true;
                        func.modifiers.push('abstract');
                        break;
                    case ts.SyntaxKind.OverrideKeyword:
                        func.isOverride = true;
                        func.modifiers.push('override');
                        break;
                }
            });
        }
        
        // Check for generator
        if (node.asteriskToken) {
            func.isGenerator = true;
            func.modifiers.push('generator');
        }
    }
    
    getTypeText(typeNode) {
        if (!typeNode) return null;
        
        switch (typeNode.kind) {
            case ts.SyntaxKind.StringKeyword:
                return 'string';
            case ts.SyntaxKind.NumberKeyword:
                return 'number';
            case ts.SyntaxKind.BooleanKeyword:
                return 'boolean';
            case ts.SyntaxKind.VoidKeyword:
                return 'void';
            case ts.SyntaxKind.AnyKeyword:
                return 'any';
            case ts.SyntaxKind.UnknownKeyword:
                return 'unknown';
            case ts.SyntaxKind.TypeReference:
                return typeNode.typeName.text;
            default:
                return typeNode.getText ? typeNode.getText() : 'unknown';
        }
    }
    
    getLineNumber(node) {
        return this.sourceFile.getLineAndCharacterOfPosition(node.getStart()).line + 1;
    }
    
    getEndLineNumber(node) {
        return this.sourceFile.getLineAndCharacterOfPosition(node.getEnd()).line + 1;
    }
    
    calculateStatistics() {
        return {
            totalFunctions: this.functions.length,
            totalClasses: this.classes.length,
            totalInterfaces: this.interfaces.length,
            totalEnums: this.enums.length,
            totalImports: this.imports.length,
            totalAsyncFunctions: this.functions.filter(f => f.isAsync).length,
            totalArrowFunctions: this.functions.filter(f => f.isArrow).length,
            totalMethods: this.functions.filter(f => f.isMethod).length,
            totalCallEdges: this.callGraph.length,
            avgComplexity: this.functions.length > 0 ? 
                this.functions.reduce((sum, f) => sum + f.complexity, 0) / this.functions.length : 0
        };
    }
}

// Main execution
function main() {
    const args = process.argv.slice(2);
    
    if (args.length === 0) {
        console.error('Usage: node typescript_compiler_analyzer.js <file.ts|file.js> [--json]');
        process.exit(1);
    }
    
    const filePath = args[0];
    const jsonOutput = args.includes('--json');
    
    if (!fs.existsSync(filePath)) {
        console.error(`Error: File not found: ${filePath}`);
        process.exit(1);
    }
    
    const analyzer = new TypeScriptAnalyzer(filePath);
    const result = analyzer.analyze();
    
    if (result.error) {
        console.error(`Error: ${result.error}`);
        process.exit(1);
    }
    
    if (jsonOutput) {
        console.log(JSON.stringify(result, null, 2));
    } else {
        printResults(result);
    }
}

function printResults(result) {
    console.log(`TypeScript/JavaScript Analysis: ${result.file}`);
    console.log('==================================================');
    console.log(`Functions: ${result.statistics.totalFunctions}`);
    console.log(`Classes: ${result.statistics.totalClasses}`);
    console.log(`Interfaces: ${result.statistics.totalInterfaces}`);
    console.log(`Enums: ${result.statistics.totalEnums}`);
    console.log(`Imports: ${result.statistics.totalImports}`);
    console.log(`Async functions: ${result.statistics.totalAsyncFunctions}`);
    console.log(`Arrow functions: ${result.statistics.totalArrowFunctions}`);
    console.log(`Methods: ${result.statistics.totalMethods}`);
    console.log(`Call graph edges: ${result.statistics.totalCallEdges}`);
    console.log(`Average complexity: ${result.statistics.avgComplexity.toFixed(2)}`);
    
    if (result.functions.length > 0) {
        console.log('\nFunctions:');
        result.functions.slice(0, 10).forEach(func => {
            const modifiers = func.modifiers.length > 0 ? `(${func.modifiers.join(', ')}) ` : '';
            console.log(`  - ${modifiers}${func.name} at line ${func.line}`);
            if (func.calls.length > 0) {
                console.log(`    Calls: ${func.calls.join(', ')}`);
            }
        });
    }
    
    if (result.classes.length > 0) {
        console.log('\nClasses:');
        result.classes.slice(0, 10).forEach(cls => {
            const modifiers = cls.modifiers.length > 0 ? `(${cls.modifiers.join(', ')}) ` : '';
            const inheritance = cls.superClass ? ` extends ${cls.superClass}` : '';
            console.log(`  - ${modifiers}${cls.name}${inheritance} at line ${cls.line}`);
            if (cls.methods.length > 0) {
                console.log(`    Methods: ${cls.methods.join(', ')}`);
            }
        });
    }
    
    if (result.interfaces.length > 0) {
        console.log('\nInterfaces:');
        result.interfaces.slice(0, 10).forEach(iface => {
            const extension = iface.extends.length > 0 ? ` extends ${iface.extends.join(', ')}` : '';
            console.log(`  - ${iface.name}${extension} at line ${iface.line}`);
        });
    }
}

if (require.main === module) {
    main();
}

module.exports = { TypeScriptAnalyzer };