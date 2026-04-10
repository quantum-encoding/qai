package compile

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/parser"
	"time"
)

// Default ignore lists matching the Rust codebase_compiler.


type collectedFile struct {
	RelPath  string
	FullPath string
	Size     int64
	ModTime  time.Time
}

func CmdCompile(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, `usage: qai compile <path> [-o output.md] [--format md|xml|json] [--diff [base]]

  --diff, -d [ref]   Only compile files changed in git
                      No ref: uncommitted + staged + untracked
                      With ref: all changes since that ref (e.g. main, HEAD~5)`)
		os.Exit(1)
	}

	dir := args[0]
	output := ""
	format := "md"
	diffMode := false
	diffBase := "" // empty = uncommitted + staged; otherwise compare against this ref

	// Parse flags.
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-o", "--output":
			if i+1 < len(args) {
				output = args[i+1]
				i++
			}
		case "--format", "-f":
			if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		case "--diff", "-d":
			diffMode = true
			// Optional base ref follows (e.g. "main", "HEAD~5", a commit SHA).
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				diffBase = args[i+1]
				i++
			}
		}
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai compile: %v\n", err)
		os.Exit(1)
	}

	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "qai compile: not a directory: %s\n", absDir)
		os.Exit(1)
	}

	// Collect files.
	start := time.Now()
	var files []collectedFile

	if diffMode {
		files, err = collectGitDiffFiles(absDir, diffBase)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai compile: git diff: %v\n", err)
			os.Exit(1)
		}
		if diffBase == "" {
			fmt.Fprintf(os.Stderr, "Compiling %d changed files (uncommitted + staged)...\n", len(files))
		} else {
			fmt.Fprintf(os.Stderr, "Compiling %d changed files (since %s)...\n", len(files), diffBase)
		}
	} else {
		fmt.Fprintf(os.Stderr, "Scanning %s...\n", absDir)
		files = collectAllFiles(absDir)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})

	fmt.Fprintf(os.Stderr, "Found %d files\n", len(files))

	// Format output.
	var result string
	switch format {
	case "xml":
		result = formatXML(files, absDir)
	case "json":
		result = formatJSON(files, absDir)
	default:
		result = formatMarkdown(files, absDir)
	}

	// Write output.
	if output == "" {
		output = "codebase." + format
	}
	if output == "-" {
		fmt.Print(result)
	} else {
		if err := os.WriteFile(output, []byte(result), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "qai compile: write %s: %v\n", output, err)
			os.Exit(1)
		}
		elapsed := time.Since(start)
		fmt.Fprintf(os.Stderr, "Wrote %s (%d files, %.1f KB, %.2fs)\n",
			output, len(files), float64(len(result))/1024, elapsed.Seconds())
	}
}

// ── Collectors ─────────────────────────────────────────────────────────────

// collectAllFiles walks a directory and returns all non-ignored files.
func collectAllFiles(absDir string) []collectedFile {
	var files []collectedFile
	filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if parser.IgnoreDirs[name] || (strings.HasPrefix(name, ".") && name != ".") {
				return fs.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if parser.IgnoreExts[ext] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > 1<<20 {
			return nil
		}
		rel, _ := filepath.Rel(absDir, path)
		files = append(files, collectedFile{
			RelPath:  rel,
			FullPath: path,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		})
		return nil
	})
	return files
}

// collectGitDiffFiles uses git to find changed files and returns their full contents.
// If base is empty, collects all uncommitted + staged files.
// If base is set (e.g. "main", "HEAD~5"), collects all files changed since that ref.
func collectGitDiffFiles(absDir string, base string) ([]collectedFile, error) {
	// Find git root for this directory.
	cmd := exec.Command("git", "-C", absDir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repo: %s", absDir)
	}
	gitRoot := strings.TrimSpace(string(out))

	// Collect changed file paths.
	changedSet := make(map[string]bool)

	if base == "" {
		// Uncommitted changes (working tree vs index).
		diffCmd := exec.Command("git", "-C", gitRoot, "diff", "--name-only")
		diffOut, _ := diffCmd.Output()
		for _, line := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
			if line != "" {
				changedSet[line] = true
			}
		}
		// Staged changes (index vs HEAD).
		stagedCmd := exec.Command("git", "-C", gitRoot, "diff", "--cached", "--name-only")
		stagedOut, _ := stagedCmd.Output()
		for _, line := range strings.Split(strings.TrimSpace(string(stagedOut)), "\n") {
			if line != "" {
				changedSet[line] = true
			}
		}
		// Untracked files.
		untrackedCmd := exec.Command("git", "-C", gitRoot, "ls-files", "--others", "--exclude-standard")
		untrackedOut, _ := untrackedCmd.Output()
		for _, line := range strings.Split(strings.TrimSpace(string(untrackedOut)), "\n") {
			if line != "" {
				changedSet[line] = true
			}
		}
	} else {
		// All files changed between base and HEAD (inclusive of working tree).
		// First: committed changes since base.
		diffCmd := exec.Command("git", "-C", gitRoot, "diff", "--name-only", base+"...HEAD")
		diffOut, _ := diffCmd.Output()
		for _, line := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
			if line != "" {
				changedSet[line] = true
			}
		}
		// Also include uncommitted changes on top.
		workCmd := exec.Command("git", "-C", gitRoot, "diff", "--name-only")
		workOut, _ := workCmd.Output()
		for _, line := range strings.Split(strings.TrimSpace(string(workOut)), "\n") {
			if line != "" {
				changedSet[line] = true
			}
		}
		stagedCmd := exec.Command("git", "-C", gitRoot, "diff", "--cached", "--name-only")
		stagedOut, _ := stagedCmd.Output()
		for _, line := range strings.Split(strings.TrimSpace(string(stagedOut)), "\n") {
			if line != "" {
				changedSet[line] = true
			}
		}
	}

	// Resolve paths relative to absDir and collect file info.
	// Git paths are relative to gitRoot.
	var files []collectedFile
	for relToRoot := range changedSet {
		fullPath := filepath.Join(gitRoot, relToRoot)

		// Only include files within our target directory.
		if !strings.HasPrefix(fullPath, absDir) {
			continue
		}

		// Skip ignored extensions.
		ext := strings.ToLower(filepath.Ext(relToRoot))
		if parser.IgnoreExts[ext] {
			continue
		}

		// File must still exist (not deleted).
		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			continue
		}

		// Skip large files.
		if info.Size() > 1<<20 {
			continue
		}

		rel, _ := filepath.Rel(absDir, fullPath)
		files = append(files, collectedFile{
			RelPath:  rel,
			FullPath: fullPath,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		})
	}

	return files, nil
}

// ── Formatters ──────────────────────────────────────────────────────────────

func langID(ext string) string {
	switch ext {
	case ".rs":
		return "rust"
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs":
		return "javascript"
	case ".py":
		return "python"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".java":
		return "java"
	case ".c":
		return "c"
	case ".cpp", ".cc", ".cxx":
		return "cpp"
	case ".h", ".hpp":
		return "cpp"
	case ".zig":
		return "zig"
	case ".rb":
		return "ruby"
	case ".sh", ".bash":
		return "bash"
	case ".sql":
		return "sql"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".json":
		return "json"
	case ".xml":
		return "xml"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".scss", ".sass":
		return "scss"
	case ".svelte":
		return "svelte"
	case ".vue":
		return "vue"
	case ".md":
		return "markdown"
	case ".sol":
		return "solidity"
	case ".dart":
		return "dart"
	case ".lua":
		return "lua"
	default:
		return "text"
	}
}

func formatMarkdown(files []collectedFile, root string) string {
	var b strings.Builder
	name := filepath.Base(root)

	b.WriteString(fmt.Sprintf("# Codebase: %s\n\n", name))
	b.WriteString(fmt.Sprintf("**Source:** `%s`\n", root))
	b.WriteString(fmt.Sprintf("**Files:** %d\n", len(files)))
	totalSize := int64(0)
	for _, f := range files {
		totalSize += f.Size
	}
	b.WriteString(fmt.Sprintf("**Size:** %.1f KB\n", float64(totalSize)/1024))
	b.WriteString(fmt.Sprintf("**Generated:** %s\n\n", time.Now().Format("2006-01-02 15:04:05")))

	// File tree.
	b.WriteString("## File Tree\n\n```\n")
	for _, f := range files {
		b.WriteString(f.RelPath + "\n")
	}
	b.WriteString("```\n\n---\n\n")

	// File contents.
	b.WriteString("## Files\n\n")
	for i, f := range files {
		data, err := os.ReadFile(f.FullPath)
		if err != nil {
			continue
		}
		ext := filepath.Ext(f.RelPath)
		lang := langID(ext)

		b.WriteString(fmt.Sprintf("### %s\n\n", f.RelPath))
		b.WriteString(fmt.Sprintf("```%s\n", lang))
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
		b.WriteString("```\n")

		if i < len(files)-1 {
			b.WriteString("\n---\n\n")
		}
	}

	return b.String()
}

func formatXML(files []collectedFile, root string) string {
	var b strings.Builder
	name := filepath.Base(root)

	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString(fmt.Sprintf("<codebase name=\"%s\" path=\"%s\" files=\"%d\">\n", name, root, len(files)))

	for _, f := range files {
		data, err := os.ReadFile(f.FullPath)
		if err != nil {
			continue
		}
		ext := filepath.Ext(f.RelPath)
		lang := langID(ext)

		b.WriteString(fmt.Sprintf("  <file path=\"%s\" language=\"%s\" size=\"%d\">\n",
			f.RelPath, lang, f.Size))
		b.WriteString("    <![CDATA[")
		b.Write(data)
		b.WriteString("]]>\n")
		b.WriteString("  </file>\n")
	}

	b.WriteString("</codebase>\n")
	return b.String()
}

func formatJSON(files []collectedFile, root string) string {
	var b strings.Builder
	name := filepath.Base(root)

	b.WriteString("{\n")
	b.WriteString(fmt.Sprintf("  \"name\": %q,\n", name))
	b.WriteString(fmt.Sprintf("  \"path\": %q,\n", root))
	b.WriteString(fmt.Sprintf("  \"files_count\": %d,\n", len(files)))
	b.WriteString("  \"files\": [\n")

	for i, f := range files {
		data, err := os.ReadFile(f.FullPath)
		if err != nil {
			continue
		}
		ext := filepath.Ext(f.RelPath)
		lang := langID(ext)

		// Escape JSON string content.
		content := strings.ReplaceAll(string(data), "\\", "\\\\")
		content = strings.ReplaceAll(content, "\"", "\\\"")
		content = strings.ReplaceAll(content, "\n", "\\n")
		content = strings.ReplaceAll(content, "\r", "\\r")
		content = strings.ReplaceAll(content, "\t", "\\t")

		b.WriteString(fmt.Sprintf("    {\"path\": %q, \"language\": %q, \"size\": %d, \"content\": \"%s\"}",
			f.RelPath, lang, f.Size, content))
		if i < len(files)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}

	b.WriteString("  ]\n}\n")
	return b.String()
}
