// audit.go — LLM code audit with parallel per-file analysis.
//
// Walks a directory, filters cruft, and sends each source file to an LLM
// for analysis using a configurable profile (security-redteam, security-blueteam,
// code-review, documentation).
//
// Usage:
//
//	qai audit <path> [options]
//	qai audit . --profile security-redteam --model gemini-3.1-pro-preview
//	qai audit . --dry-run                  # show what would be sent
//	qai audit . -c 10 --max-tokens 60000   # 10 workers, 60K output tokens

package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"

)

// Cfg is set by main before any command runs.
var Cfg *config.Config

// ─── cruft filter (mirrors internal/scanner/upload.go) ─────────────────────

var skipExts = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".o": true, ".a": true, ".lib": true, ".obj": true,
	".class": true, ".pyc": true, ".pyo": true, ".wasm": true,
	".zip": true, ".tar": true, ".gz": true, ".7z": true,
	".rar": true, ".bz2": true, ".xz": true,
	".jar": true, ".war": true, ".aar": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".svg": true, ".bmp": true, ".tiff": true, ".webp": true,
	".ico": true, ".mp3": true, ".mp4": true, ".wav": true,
	".avi": true, ".mov": true, ".webm": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true,
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".lock": true, ".sum": true,
	".db": true, ".sqlite": true, ".sqlite3": true,
	".map": true,
	".framework": true, ".xcarchive": true,
	// Data and config that shouldn't be audited
	".csv": true, ".tsv": true, ".jsonl": true, ".ndjson": true,
	".g4": true, ".cbl": true, ".cpy": true, // grammar/COBOL test fixtures
	".txt": true, // plain text data files
	".mermaid": true, ".dot": true,
	".csproj": true, ".sln": true, // .NET project files
	".mod": true, // go.mod handled separately
}

var skipDirs = map[string]bool{
	".git": true, ".svn": true, ".hg": true,
	"node_modules": true, "bower_components": true,
	".next": true, ".nuxt": true, ".turbo": true, ".parcel-cache": true,
	"dist": true, "build": true, "out": true, "bin": true, "obj": true,
	"target": true, ".build": true,
	"__pycache__": true, ".mypy_cache": true, ".pytest_cache": true,
	".ruff_cache": true, ".tox": true,
	"vendor": true, ".cargo": true,
	".gradle": true, ".maven": true,
	"venv": true, ".venv": true, "env": true,
	"Pods": true, "DerivedData": true, ".swiftpm": true,
	"testdata": true, "test_fixtures": true, "fixtures": true,
	"eval-results": true, "docs": true, "examples": true,
	"coverage": true, ".nyc_output": true,
	".terraform": true, ".pulumi": true,
	".docker": true, ".vagrant": true,
}

// maxFileSize is the maximum file size to include (500KB — anything larger
// is likely generated code, binaries, or data files, not source).
const maxFileSize = 500 * 1024

func shouldSkip(rel string) bool {
	base := filepath.Base(rel)
	if strings.HasPrefix(base, ".") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(rel))
	if skipExts[ext] {
		return true
	}
	// No extension = likely a binary, Makefile, Dockerfile, or script.
	// Allow known extensionless source files, skip the rest.
	if ext == "" {
		lower := strings.ToLower(base)
		switch lower {
		case "makefile", "dockerfile", "rakefile", "gemfile",
			"procfile", "cmakelists.txt", "readme", "license":
			// keep
		default:
			return true
		}
	}
	lower := strings.ToLower(base)
	if strings.HasSuffix(lower, ".min.js") || strings.HasSuffix(lower, ".min.css") ||
		strings.HasSuffix(lower, ".bundle.js") || strings.HasSuffix(lower, ".chunk.js") {
		return true
	}
	for _, p := range strings.Split(rel, string(filepath.Separator)) {
		if skipDirs[p] {
			return true
		}
	}
	return false
}

// ─── language detection ────────────────────────────────────────────────────

func langFromExt(ext string) string {
	m := map[string]string{
		".go": "go", ".rs": "rust", ".ts": "typescript", ".tsx": "typescript",
		".js": "javascript", ".jsx": "javascript", ".py": "python",
		".rb": "ruby", ".java": "java", ".kt": "kotlin", ".swift": "swift",
		".c": "c", ".cpp": "cpp", ".h": "c", ".hpp": "cpp", ".cs": "csharp",
		".php": "php", ".scala": "scala", ".zig": "zig", ".lua": "lua",
		".sh": "bash", ".bash": "bash", ".zsh": "zsh", ".sql": "sql",
		".yaml": "yaml", ".yml": "yaml", ".toml": "toml", ".json": "json",
		".xml": "xml", ".html": "html", ".css": "css", ".scss": "scss",
		".tf": "hcl", ".proto": "protobuf", ".md": "markdown",
		".dart": "dart", ".vue": "vue", ".svelte": "svelte",
	}
	if l, ok := m[ext]; ok {
		return l
	}
	if ext != "" && ext[0] == '.' {
		return ext[1:]
	}
	return "text"
}

// ─── profiles (loaded from profiles.go via go:embed + ~/.qai/profiles/) ───

func renderPrompt(tmpl, path, lang, code string) string {
	s := strings.ReplaceAll(tmpl, "{{PATH}}", path)
	s = strings.ReplaceAll(s, "{{LANG}}", lang)
	s = strings.ReplaceAll(s, "{{CODE}}", code)
	return s
}

// ─── QAI API client ────────────────────────────────────────────────────────

func callLLM(apiKey, baseURL, model, systemPrompt, userPrompt string, maxTokens int) (string, int, int, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
	}
	jsonBody, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", baseURL+"/qai/v1/chat", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 429 {
		return "", 0, 0, fmt.Errorf("429 rate limit")
	}
	if resp.StatusCode != 200 {
		return "", 0, 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", 0, 0, fmt.Errorf("parse response: %w", err)
	}

	var text strings.Builder
	for _, c := range result.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return text.String(), result.Usage.InputTokens, result.Usage.OutputTokens, nil
}

// ─── file result ───────────────────────────────────────────────────────────

type fileResult struct {
	Path      string `json:"path"`
	Language  string `json:"language"`
	SizeBytes int64  `json:"size_bytes"`
	TokensIn  int    `json:"tokens_in"`
	TokensOut int    `json:"tokens_out"`
	Findings  string `json:"findings"`
	Error     string `json:"error,omitempty"`
}

// ─── command ───────────────────────────────────────────────────────────────

func CmdAudit(args []string) {
	if len(args) == 0 {
		var profileHelp strings.Builder
		for _, n := range ProfileNames() {
			p := profiles[n]
			def := ""
			if n == "security-redteam" {
				def = " (default)"
			}
			fmt.Fprintf(&profileHelp, "                           %s%s\n", n, def)
			if p.Description != "" {
				fmt.Fprintf(&profileHelp, "                             %s\n", p.Description)
			}
		}
		fmt.Fprintf(os.Stderr, `usage: qai audit <path> [options]

LLM code audit — parallel per-file analysis with configurable profiles.

Options:
  --profile <name>     Audit profile:
%s  --profile-file <path>  Load profile from external YAML file
  --model <model>      LLM model (default: gemini-3.1-pro-preview)
  --max-tokens <n>     Max output tokens per file (default: 60000)
  -c <n>               Concurrency / parallel workers (default: 10)
  --dry-run            Show what files would be analyzed without calling the LLM
  --api <url>          API base URL (default: https://api.quantumencoding.ai)
  -o <dir>             Output directory (default: audit-{timestamp})

Profiles are loaded from:
  1. Built-in (embedded in binary)
  2. ~/.qai/profiles/*.yaml (user overrides, no rebuild needed)
  3. --profile-file (one-shot, highest priority)
`, profileHelp.String())
		os.Exit(1)
	}

	dir := args[0]
	profileName := "security-redteam"
	model := "gemini-3.1-pro-preview"
	maxTokens := 60000
	concurrency := 10
	dryRun := false
	apiURL := Cfg.API.BaseURL
	outputDir := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--profile", "-p":
			if i+1 < len(args) { profileName = args[i+1]; i++ }
		case "--model", "-m":
			if i+1 < len(args) { model = args[i+1]; i++ }
		case "--max-tokens":
			if i+1 < len(args) { fmt.Sscanf(args[i+1], "%d", &maxTokens); i++ }
		case "-c", "--concurrency":
			if i+1 < len(args) { fmt.Sscanf(args[i+1], "%d", &concurrency); i++ }
		case "--profile-file":
			if i+1 < len(args) {
				key, p, err := loadProfileFromFile(args[i+1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "load profile file: %v\n", err)
					os.Exit(1)
				}
				profileName = key
				profiles[key] = p
				i++
			}
		case "--dry-run":
			dryRun = true
		case "--api":
			if i+1 < len(args) { apiURL = args[i+1]; i++ }
		case "-o", "--output":
			if i+1 < len(args) { outputDir = args[i+1]; i++ }
		}
	}

	profile, ok := profiles[profileName]
	if !ok {
		var names []string
		for n := range profiles { names = append(names, n) }
		sort.Strings(names)
		fmt.Fprintf(os.Stderr, "unknown profile %q (available: %s)\n", profileName, strings.Join(names, ", "))
		os.Exit(1)
	}

	absDir, _ := filepath.Abs(dir)

	// Walk and collect files.
	type sourceFile struct {
		path string
		rel  string
		size int64
		lang string
	}
	var files []sourceFile
	var totalBytes int64
	var skipped int

	filepath.Walk(absDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(absDir, path)
		if shouldSkip(rel) {
			skipped++
			return nil
		}
		if info.Size() > maxFileSize {
			skipped++
			return nil
		}
		lang := langFromExt(filepath.Ext(path))
		files = append(files, sourceFile{path, rel, info.Size(), lang})
		totalBytes += info.Size()
		return nil
	})

	estTokensIn := totalBytes / 4
	estTokensOut := int64(len(files)) * int64(maxTokens/4) // rough

	// Language breakdown.
	langCount := map[string]int{}
	langBytes := map[string]int64{}
	for _, f := range files {
		langCount[f.lang]++
		langBytes[f.lang] += f.size
	}

	fmt.Fprintf(os.Stderr, "╔══════════════════════════════════════════╗\n")
	fmt.Fprintf(os.Stderr, "║  Code Audit: %-27s ║\n", profileName)
	fmt.Fprintf(os.Stderr, "╚══════════════════════════════════════════╝\n\n")
	fmt.Fprintf(os.Stderr, "  Directory:    %s\n", absDir)
	fmt.Fprintf(os.Stderr, "  Model:        %s\n", model)
	fmt.Fprintf(os.Stderr, "  Max tokens:   %d\n", maxTokens)
	fmt.Fprintf(os.Stderr, "  Concurrency:  %d\n", concurrency)
	fmt.Fprintf(os.Stderr, "  Files:        %d (skipped %d)\n", len(files), skipped)
	fmt.Fprintf(os.Stderr, "  Total size:   %.1f KB\n", float64(totalBytes)/1024)
	fmt.Fprintf(os.Stderr, "  Est. tokens:  ~%d in, ~%d out\n\n", estTokensIn, estTokensOut)

	// Language table.
	type ls struct{ lang string; count int; bytes int64 }
	var langStats []ls
	for l, c := range langCount { langStats = append(langStats, ls{l, c, langBytes[l]}) }
	sort.Slice(langStats, func(i, j int) bool { return langStats[i].bytes > langStats[j].bytes })

	fmt.Fprintf(os.Stderr, "  %-12s %5s %8s\n", "Language", "Files", "Size")
	fmt.Fprintf(os.Stderr, "  %s\n", strings.Repeat("─", 28))
	for _, s := range langStats {
		fmt.Fprintf(os.Stderr, "  %-12s %5d %6.1f KB\n", s.lang, s.count, float64(s.bytes)/1024)
	}

	// Sort by size for display.
	sort.Slice(files, func(i, j int) bool { return files[i].size > files[j].size })

	fmt.Fprintf(os.Stderr, "\n  Top 15 largest files:\n")
	for i, f := range files {
		if i >= 15 { break }
		fmt.Fprintf(os.Stderr, "    %6.1f KB  %s\n", float64(f.size)/1024, f.rel)
	}

	if dryRun {
		fmt.Fprintf(os.Stderr, "\n  All %d files:\n", len(files))
		sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
		for _, f := range files {
			fmt.Fprintf(os.Stderr, "    %6.1f KB  %-12s %s\n", float64(f.size)/1024, f.lang, f.rel)
		}
		fmt.Fprintf(os.Stderr, "\n  --dry-run: no LLM calls made.\n")
		return
	}

	// Real run.
	apiKey := Cfg.API.APIKey
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "\nAPI key not configured — run: qai init")
		os.Exit(1)
	}

	if outputDir == "" {
		outputDir = fmt.Sprintf("audit-%s-%s", profileName, time.Now().Format("20060102-150405"))
	}
	os.MkdirAll(outputDir, 0755)

	fmt.Fprintf(os.Stderr, "\n  Output: %s/\n", outputDir)
	fmt.Fprintf(os.Stderr, "  Starting %d workers...\n\n", concurrency)

	start := time.Now()
	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	results := make([]fileResult, len(files))
	var completed int

	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(idx int, sf sourceFile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			code, err := os.ReadFile(sf.path)
			if err != nil {
				mu.Lock()
				results[idx] = fileResult{Path: sf.rel, Language: sf.lang, Error: err.Error()}
				mu.Unlock()
				return
			}

			prompt := renderPrompt(profile.User, sf.rel, sf.lang, string(code))

			// Retry with exponential backoff on 429.
			var text string
			var tokIn, tokOut int
			const maxRetries = 5
			for attempt := range maxRetries {
				text, tokIn, tokOut, err = callLLM(apiKey, apiURL, model, profile.System, prompt, maxTokens)
				if err == nil {
					break
				}
				if !strings.Contains(err.Error(), "429") && !strings.Contains(strings.ToLower(err.Error()), "rate limit") {
					break // non-retryable error
				}
				backoff := time.Duration(math.Pow(2, float64(attempt))) * time.Second
				jitter := time.Duration(rand.IntN(1000)) * time.Millisecond
				fmt.Fprintf(os.Stderr, "  ⏳ rate limited on %s, retry in %v\n", sf.rel, backoff+jitter)
				time.Sleep(backoff + jitter)
			}

			r := fileResult{
				Path:      sf.rel,
				Language:  sf.lang,
				SizeBytes: sf.size,
				TokensIn:  tokIn,
				TokensOut: tokOut,
			}
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Findings = text
				// Write individual file result.
				outPath := filepath.Join(outputDir, strings.ReplaceAll(sf.rel, "/", "__")+".md")
				if wErr := os.WriteFile(outPath, []byte(fmt.Sprintf("# %s\n\n%s\n", sf.rel, text)), 0644); wErr != nil {
					fmt.Fprintf(os.Stderr, "  warning: write %s: %v\n", outPath, wErr)
				}
			}

			mu.Lock()
			results[idx] = r
			completed++
			status := "✓"
			if r.Error != "" { status = "✗" }
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s %s (%d→%d tok)\n",
				completed, len(files), status, sf.rel, tokIn, tokOut)
			mu.Unlock()
		}(i, f)
	}
	wg.Wait()

	elapsed := time.Since(start)

	// Build manifest.
	var totalIn, totalOut int
	var errored int
	for _, r := range results {
		totalIn += r.TokensIn
		totalOut += r.TokensOut
		if r.Error != "" { errored++ }
	}

	manifest := map[string]any{
		"profile":          profileName,
		"model":            model,
		"directory":        absDir,
		"files_analyzed":   len(files) - errored,
		"files_errored":    errored,
		"total_tokens_in":  totalIn,
		"total_tokens_out": totalOut,
		"duration_seconds": elapsed.Seconds(),
		"files":            results,
	}
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	manifestPath := filepath.Join(outputDir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestJSON, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write manifest: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "\n══════════════════════════════════════════\n")
	fmt.Fprintf(os.Stderr, "  Done in %.1fs\n", elapsed.Seconds())
	fmt.Fprintf(os.Stderr, "  Files: %d analyzed, %d errored\n", len(files)-errored, errored)
	fmt.Fprintf(os.Stderr, "  Tokens: %d in, %d out\n", totalIn, totalOut)
	fmt.Fprintf(os.Stderr, "  Output: %s/\n", outputDir)
	fmt.Fprintf(os.Stderr, "══════════════════════════════════════════\n")
}
