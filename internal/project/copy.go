package project

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Directories we never descend into — VCS, deps, build artefacts, caches.
// A leading-dot directory is also skipped regardless (handled in walk).
var skipDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "target": true,
	"__pycache__": true, "venv": true, "env": true,
}

// Extensions that are binary or otherwise not worth copying as a text note.
var skipExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".ico": true, ".bmp": true, ".tiff": true, ".heic": true,
	".mp3": true, ".mp4": true, ".wav": true, ".ogg": true, ".flac": true,
	".mov": true, ".avi": true, ".webm": true, ".mkv": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true, ".7z": true,
	".so": true, ".dylib": true, ".dll": true, ".exe": true, ".a": true, ".o": true,
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ttf": true, ".otf": true, ".woff": true, ".woff2": true, ".eot": true,
}

// Hard cap so an accidental `--copy` on a log-heavy tree doesn't wedge Joplin.
const maxFileBytes = 1 << 20 // 1 MiB

func doCopy(dir string, dryRun bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		die(fmt.Errorf("project --copy requires a directory: qai project --copy ./src"))
	}
	info, err := os.Stat(dir)
	if err != nil {
		die(fmt.Errorf("could not stat %q: %w", dir, err))
	}
	if !info.IsDir() {
		die(fmt.Errorf("%q is not a directory", dir))
	}
	if Cfg.Project.JoplinNotebookID == "" {
		die(fmt.Errorf("no active project set. Run: qai project --set \"<name>\""))
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		die(err)
	}

	// Build title→id map of existing notes so repeat runs update-in-place
	// instead of creating duplicates. Skip the lookup in dry-run mode.
	var client *joplin.Client
	existing := map[string]string{}
	if !dryRun {
		client, err = newClient()
		if err != nil {
			die(err)
		}
		notes, err := client.ListNotes(Cfg.Project.JoplinNotebookID)
		if err != nil {
			die(err)
		}
		for _, n := range notes {
			existing[n.Title] = n.ID
		}
	}

	var created, updated, skipped int
	walkErr := filepath.WalkDir(absDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			// Never skip the root we were asked to copy, even if it's a dotdir.
			if path == absDir {
				return nil
			}
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			skipped++
			return nil
		}
		if skipExts[strings.ToLower(filepath.Ext(name))] {
			skipped++
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			skipped++
			return nil
		}
		if fi.Size() > maxFileBytes {
			fmt.Fprintf(os.Stderr, "  skip (>%d bytes): %s\n", maxFileBytes, path)
			skipped++
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			skipped++
			return nil
		}
		if isBinary(data) {
			skipped++
			return nil
		}

		rel, _ := filepath.Rel(absDir, path)
		title := rel
		body := renderBody(rel, data)

		if dryRun {
			fmt.Printf("  would copy: %s  (%d bytes)\n", title, len(data))
			created++
			return nil
		}

		if id, ok := existing[title]; ok {
			if err := client.UpdateNoteBody(id, body); err != nil {
				fmt.Fprintf(os.Stderr, "  warn: update %s: %v\n", title, err)
				return nil
			}
			updated++
			return nil
		}
		_, err = client.CreateNote(joplin.Note{
			Title:    title,
			Body:     body,
			ParentID: Cfg.Project.JoplinNotebookID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: create %s: %v\n", title, err)
			return nil
		}
		created++
		return nil
	})
	if walkErr != nil {
		die(walkErr)
	}

	verb := "Copied"
	if dryRun {
		verb = "Would copy"
	}
	fmt.Printf("%s from %s → %s\n", verb, absDir, Cfg.Project.Name)
	fmt.Printf("  created: %d\n", created)
	if !dryRun {
		fmt.Printf("  updated: %d\n", updated)
	}
	fmt.Printf("  skipped: %d\n", skipped)
}

// isBinary uses the classic "NUL byte in the first 512 bytes" heuristic.
// Good enough for source-tree copying; avoids pulling in mimetype deps.
func isBinary(data []byte) bool {
	n := len(data)
	if n > 512 {
		n = 512
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func renderBody(rel string, data []byte) string {
	ext := strings.ToLower(filepath.Ext(rel))
	if ext == ".md" || ext == ".markdown" {
		return fmt.Sprintf("**Source:** `%s`\n\n%s", rel, string(data))
	}
	lang := langFor(ext, filepath.Base(rel))
	return fmt.Sprintf("**Source:** `%s`\n\n```%s\n%s\n```\n", rel, lang, string(data))
}

func langFor(ext, basename string) string {
	switch strings.ToLower(basename) {
	case "dockerfile", "containerfile":
		return "dockerfile"
	case "makefile", "gnumakefile":
		return "makefile"
	}
	switch ext {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "jsx"
	case ".py":
		return "python"
	case ".rb":
		return "ruby"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".swift":
		return "swift"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp", ".hxx":
		return "cpp"
	case ".sh", ".bash", ".zsh":
		return "bash"
	case ".fish":
		return "fish"
	case ".ps1":
		return "powershell"
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	case ".toml":
		return "toml"
	case ".xml":
		return "xml"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".scss", ".sass":
		return "scss"
	case ".sql":
		return "sql"
	case ".lua":
		return "lua"
	case ".zig":
		return "zig"
	case ".svelte":
		return "svelte"
	case ".vue":
		return "vue"
	case ".dart":
		return "dart"
	case ".ex", ".exs":
		return "elixir"
	case ".erl":
		return "erlang"
	case ".hs":
		return "haskell"
	case ".scala":
		return "scala"
	case ".r":
		return "r"
	case ".jl":
		return "julia"
	}
	return ""
}
