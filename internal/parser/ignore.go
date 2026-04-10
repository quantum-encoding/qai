package parser

var IgnoreDirs = map[string]bool{
	"target": true, "node_modules": true, ".git": true, "build": true,
	"dist": true, "venv": true, ".venv": true, "__pycache__": true,
	".next": true, ".build": true, "vendor": true, ".cache": true,
	".idea": true, ".vscode": true, "coverage": true, ".turbo": true,
	".svelte-kit": true, ".output": true, ".nuxt": true, "Pods": true,
	".gradle": true, ".dart_tool": true, "zig-cache": true, "zig-out": true,
}

var IgnoreExts = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".o": true,
	".a": true, ".lib": true, ".bin": true, ".png": true, ".jpg": true,
	".jpeg": true, ".gif": true, ".ico": true, ".svg": true, ".webp": true,
	".mp3": true, ".mp4": true, ".wav": true, ".avi": true, ".mov": true,
	".zip": true, ".tar": true, ".gz": true, ".rar": true, ".7z": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".pdf": true, ".psd": true, ".ai": true, ".sketch": true,
	".lock": true, ".sum": true, ".map": true,
}
