package scrape

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ─── entry point ─────────────────────────────────────────────────────────

// CmdScrape is the `qai scrape` subcommand.
func CmdScrape(args []string) {
	if len(args) == 0 || hasHelp(args) {
		fmt.Println(helpScrape())
		if len(args) == 0 {
			os.Exit(1)
		}
		return
	}

	opts := parseFlags(args)

	if opts.csvPath != "" {
		runBatch(opts)
		return
	}

	if opts.target == "" {
		fmt.Fprintln(os.Stderr, "qai scrape: missing <url>, --id, or --csv")
		fmt.Fprintln(os.Stderr, "Run 'qai scrape --help' for usage.")
		os.Exit(1)
	}

	productURL, preset, err := resolveTarget(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai scrape: %v\n", err)
		os.Exit(1)
	}

	brief, err := runOne(productURL, preset, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai scrape: %v\n", err)
		os.Exit(1)
	}
	emit(brief, opts)
}

// ─── flags ───────────────────────────────────────────────────────────────

type flags struct {
	target     string // URL or ID (based on idOnly)
	idOnly     bool
	preset     string
	notebook   string
	imageDir   string
	csvPath    string
	outPath    string
	parallel   int
	resume     bool
}

func parseFlags(args []string) *flags {
	opts := &flags{
		imageDir: "public/product-images",
		parallel: 1,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--preset":
			if i+1 < len(args) {
				opts.preset = args[i+1]
				i++
			}
		case "--amazon":
			opts.preset = "amazon"
		case "--notebook":
			if i+1 < len(args) {
				opts.notebook = args[i+1]
				i++
			}
		case "--image-dir":
			if i+1 < len(args) {
				opts.imageDir = args[i+1]
				i++
			}
		case "--id":
			if i+1 < len(args) {
				opts.target = args[i+1]
				opts.idOnly = true
				i++
			}
		case "--csv":
			if i+1 < len(args) {
				opts.csvPath = args[i+1]
				i++
			}
		case "--out", "-o":
			if i+1 < len(args) {
				opts.outPath = args[i+1]
				i++
			}
		case "--parallel":
			if i+1 < len(args) {
				n := 0
				_, _ = fmt.Sscanf(args[i+1], "%d", &n)
				if n > 0 {
					opts.parallel = n
				}
				i++
			}
		case "--resume":
			opts.resume = true
		default:
			if opts.target == "" && !strings.HasPrefix(args[i], "-") {
				opts.target = args[i]
			}
		}
	}
	return opts
}

func hasHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			return true
		}
	}
	return false
}

// ─── single-URL path ─────────────────────────────────────────────────────

// resolveTarget turns the user's target (URL or ID) into (productURL,
// preset), validating the preset can handle the input.
func resolveTarget(opts *flags) (string, *Preset, error) {
	var preset *Preset
	if opts.preset != "" {
		preset = PresetByName(opts.preset)
		if preset == nil {
			return "", nil, fmt.Errorf("unknown preset %q (available: %s)",
				opts.preset, strings.Join(PresetNames(), ", "))
		}
	}

	if opts.idOnly {
		if preset == nil {
			return "", nil, fmt.Errorf("--id requires --preset (cannot build URL without one)")
		}
		if preset.BuildURL == nil {
			return "", nil, fmt.Errorf("preset %q does not support --id (no BuildURL)", preset.Name)
		}
		return preset.BuildURL(opts.target), preset, nil
	}

	productURL := opts.target
	if preset == nil {
		preset = PresetForURL(productURL)
	}
	if preset == nil {
		return "", nil, fmt.Errorf("no preset matches %s — pass --preset <name>", productURL)
	}
	return productURL, preset, nil
}

// ─── brief output ────────────────────────────────────────────────────────

// Brief is the shape emitted per URL.
type Brief struct {
	URL          string         `json:"source_url"`
	Preset       string         `json:"preset"`
	ID           string         `json:"id"`
	Title        string         `json:"title,omitempty"`
	ImagePath    string         `json:"image_path,omitempty"`
	JoplinNoteID string         `json:"joplin_note_id,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
	Error        string         `json:"error,omitempty"`
}

func emit(b *Brief, opts *flags) {
	out, _ := json.MarshalIndent(b, "", "  ")
	if opts.outPath == "" || opts.outPath == "-" {
		fmt.Println(string(out))
		return
	}
	// Append one JSON object per line when writing to a file (JSONL).
	line, _ := json.Marshal(b)
	f, err := os.OpenFile(opts.outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai scrape: open %s: %v\n", opts.outPath, err)
		fmt.Println(string(out))
		return
	}
	defer f.Close()
	f.Write(line)
	f.Write([]byte{'\n'})
}

// ─── URL helpers (used by preset.go) ─────────────────────────────────────

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func stringContains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// ─── image-path normalization ────────────────────────────────────────────

// siteRelativePath converts an absolute saved-image path to the
// `/product-images/<file>` form used by static-site generators, when
// the destination lives under a `public/` directory. Falls back to the
// absolute path otherwise.
func siteRelativePath(absPath string) string {
	sep := string(filepath.Separator)
	marker := sep + "public" + sep
	if idx := strings.Index(absPath, marker); idx >= 0 {
		return filepath.ToSlash(absPath[idx+len(sep)+len("public"):])
	}
	return absPath
}
