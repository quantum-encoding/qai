// Package doctor implements `qai doctor` — a single diagnostic command
// that probes every dependency qai talks to and reports which
// subcommands are currently operable.
//
// The intent: the invariant "exit != 0 → stderr names what failed +
// how to fix" is enforced per-call across the rest of the codebase;
// doctor runs the same checks proactively so the user (or an agent)
// can answer "is this tool working?" in one command without invoking
// each subcommand independently.
//
// Output shape:
//
//	✓ broker     api.quantumencoding.ai reachable, QAI_API_KEY valid
//	✗ joplin     JOPLIN_TOKEN set but clipper unreachable
//	             → fix: launch Joplin desktop and enable Web Clipper
//	- brave      BRAVE_SEARCH_API_KEY not set (qai web/ask/context disabled)
//	             → fix: export BRAVE_SEARCH_API_KEY=<key>
//	✓ tmux       installed
//	✓ fleet      no active fleet (clean state)
//
//	5 ok, 1 missing (optional), 1 broken
//
// Three states per check: ✓ ok, ✗ broken, - not-configured-but-optional.
// Exit 0 unless something required is broken; broken-optional doesn't fail.

package doctor

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Cfg is injected from main, matching the package pattern used elsewhere.
var Cfg *config.Config

type status int

const (
	statusOK status = iota
	statusBroken
	statusMissingOptional
)

type result struct {
	name   string
	state  status
	detail string // first line — fact
	fix    string // optional second line — actionable remediation
}

// CmdDoctor is the `qai doctor` entry point.
func CmdDoctor(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Print(help)
			return
		}
	}

	results := []result{
		checkBroker(),
		checkJoplin(),
		checkGcloud(),
		checkBrave(),
		checkSurreal(),
		checkTmux(),
		checkFleet(),
	}

	var ok, broken, missing int
	for _, r := range results {
		switch r.state {
		case statusOK:
			ok++
		case statusBroken:
			broken++
		case statusMissingOptional:
			missing++
		}
	}

	for _, r := range results {
		printResult(r)
	}
	fmt.Println()
	fmt.Printf("%d ok, %d missing (optional), %d broken\n", ok, missing, broken)

	if broken > 0 {
		os.Exit(1)
	}
}

func printResult(r result) {
	var glyph string
	switch r.state {
	case statusOK:
		glyph = "✓"
	case statusBroken:
		glyph = "✗"
	case statusMissingOptional:
		glyph = "-"
	}
	fmt.Printf("%s %-10s  %s\n", glyph, r.name, r.detail)
	if r.fix != "" {
		fmt.Printf("            → fix: %s\n", r.fix)
	}
}

// ─── checks ────────────────────────────────────────────────────────────────

func checkBroker() result {
	if Cfg == nil || Cfg.API.APIKey == "" {
		return result{
			name:   "broker",
			state:  statusMissingOptional,
			detail: "QAI_API_KEY not set (image/video/tts/audit/conduct disabled)",
			fix:    "export QAI_API_KEY=<key>  (sign up at https://quantumencoding.ai)",
		}
	}
	base := Cfg.API.BaseURL
	if base == "" {
		base = "https://api.quantumencoding.ai"
	}
	// Cheap GET against the broker. Don't require auth on the probe;
	// any non-network response confirms reachability. If the broker
	// has no /health it'll return 404, which is also "reachable".
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/qai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+Cfg.API.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return result{
			name:   "broker",
			state:  statusBroken,
			detail: fmt.Sprintf("%s unreachable: %v", base, err),
			fix:    "check network connectivity; if self-hosting, verify QAI_BASE_URL points at a running gateway",
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return result{
			name:   "broker",
			state:  statusBroken,
			detail: fmt.Sprintf("%s reachable but QAI_API_KEY rejected (401)", base),
			fix:    "verify the key at https://quantumencoding.ai and re-export it",
		}
	}
	return result{
		name:   "broker",
		state:  statusOK,
		detail: fmt.Sprintf("%s reachable, key accepted", base),
	}
}

func checkJoplin() result {
	if Cfg == nil || Cfg.Joplin.Token == "" {
		return result{
			name:   "joplin",
			state:  statusMissingOptional,
			detail: "JOPLIN_TOKEN not set (clip/project/agent/note disabled)",
			fix:    "Tools → Options → Web Clipper in Joplin desktop, then export JOPLIN_TOKEN=<token>",
		}
	}
	c := joplin.New(joplin.Config{BaseURL: Cfg.Joplin.BaseURL, Token: Cfg.Joplin.Token})
	if err := c.Ping(); err != nil {
		// Ping() now returns a hand-tuned message per failure mode, so
		// just surface it. The fix line is the specific action that
		// resolves THIS error (not a generic "launch Joplin + enable
		// clipper" that's wrong half the time).
		errMsg := err.Error()
		fix := "launch Joplin desktop and enable Web Clipper (Tools → Options → Web Clipper)"
		switch {
		case strings.Contains(errMsg, "is not running"):
			fix = "launch Joplin desktop"
		case strings.Contains(errMsg, "Web Clipper Service is disabled"):
			fix = "in Joplin desktop, open Tools → Options → Web Clipper and toggle 'Enable Web Clipper Service'"
		}
		return result{
			name:   "joplin",
			state:  statusBroken,
			detail: errMsg,
			fix:    fix,
		}
	}
	base := Cfg.Joplin.BaseURL
	if base == "" {
		base = "http://127.0.0.1:41184"
	}
	_ = c
	return result{
		name:   "joplin",
		state:  statusOK,
		detail: fmt.Sprintf("clipper reachable at %s", base),
	}
}

func checkGcloud() result {
	home, _ := os.UserHomeDir()
	adc := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
	if _, err := os.Stat(adc); err != nil {
		return result{
			name:   "gcloud",
			state:  statusMissingOptional,
			detail: "ADC not present (qai search --rag, Vertex RAG, qai token disabled)",
			fix:    "gcloud auth application-default login",
		}
	}
	return result{
		name:   "gcloud",
		state:  statusOK,
		detail: "ADC present at ~/.config/gcloud/",
	}
}

func checkBrave() result {
	if os.Getenv("BRAVE_SEARCH_API_KEY") == "" {
		return result{
			name:   "brave",
			state:  statusMissingOptional,
			detail: "BRAVE_SEARCH_API_KEY not set (qai web/ask/context disabled)",
			fix:    "export BRAVE_SEARCH_API_KEY=<key>  (free tier at https://api.search.brave.com/)",
		}
	}
	return result{
		name:   "brave",
		state:  statusOK,
		detail: "BRAVE_SEARCH_API_KEY set",
	}
}

func checkSurreal() result {
	if _, err := exec.LookPath("surreal"); err != nil {
		return result{
			name:   "surreal",
			state:  statusMissingOptional,
			detail: "surreal binary not on PATH (qai db / local RAG disabled)",
			fix:    "install from https://surrealdb.com/install",
		}
	}
	return result{
		name:   "surreal",
		state:  statusOK,
		detail: "surreal binary on PATH",
	}
}

func checkTmux() result {
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		return result{
			name:   "tmux",
			state:  statusMissingOptional,
			detail: "tmux not on PATH (qai term / qai fleet disabled)",
			fix:    "brew install tmux  (or your distro's package manager)",
		}
	}
	return result{
		name:   "tmux",
		state:  statusOK,
		detail: strings.TrimSpace(string(out)),
	}
}

func checkFleet() result {
	home, _ := os.UserHomeDir()
	activeFile := filepath.Join(home, ".qai", "fleet", "active")
	data, err := os.ReadFile(activeFile)
	if err != nil {
		return result{
			name:   "fleet",
			state:  statusOK,
			detail: "no active fleet (clean state)",
		}
	}
	fleetID := strings.TrimSpace(string(data))
	archFile := filepath.Join(home, ".qai", "fleet", fleetID, "architect-pane")
	archData, err := os.ReadFile(archFile)
	if err != nil {
		return result{
			name:   "fleet",
			state:  statusBroken,
			detail: fmt.Sprintf("active=%s but architect-pane file missing", fleetID),
			fix:    fmt.Sprintf("rm ~/.qai/fleet/active  (or run a fresh `qai fleet up`)"),
		}
	}
	archPane := strings.TrimSpace(string(archData))

	// Probe whether the recorded architect-pane is still alive.
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id}").Output()
	if err != nil {
		return result{
			name:   "fleet",
			state:  statusBroken,
			detail: fmt.Sprintf("active=%s but tmux unreachable", fleetID),
			fix:    "start tmux, or run `rm ~/.qai/fleet/active` to clear stale state",
		}
	}
	alive := false
	for _, p := range strings.Fields(string(out)) {
		if p == archPane {
			alive = true
			break
		}
	}
	if !alive {
		return result{
			name:   "fleet",
			state:  statusBroken,
			detail: fmt.Sprintf("active=%s recorded but pane %s is dead", fleetID, archPane),
			fix:    fmt.Sprintf("rm ~/.qai/fleet/active  (stale pointer from a previous tmux session)"),
		}
	}
	return result{
		name:   "fleet",
		state:  statusOK,
		detail: fmt.Sprintf("active=%s, architect pane %s alive", fleetID, archPane),
	}
}

// ─── help ──────────────────────────────────────────────────────────────────

const help = `qai doctor — health check for every dependency qai talks to

Runs a probe per subsystem and reports which features are currently
operable. Three states:

  ✓ ok                — configured and reachable
  ✗ broken            — configured but failing (with a fix hint)
  - missing optional  — not configured (the feature is disabled but
                        nothing is broken; ignore if you don't use it)

Exit 0 if nothing required is broken; exit 1 otherwise.

Checks:
  broker    QAI_API_KEY + api.quantumencoding.ai reachable
  joplin    JOPLIN_TOKEN + Web Clipper service reachable
  gcloud    ADC present (for Vertex RAG, qai token)
  brave     BRAVE_SEARCH_API_KEY (for qai web/ask/context)
  surreal   surreal binary on PATH (for qai db, local RAG)
  tmux      tmux binary (for qai term, qai fleet)
  fleet     active fleet pointer is coherent (no stale state)

Usage:
  qai doctor                    Run all checks, exit non-zero on failure
`
