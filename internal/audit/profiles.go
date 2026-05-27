package audit

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/config"

	"gopkg.in/yaml.v3"
)

//go:embed profiles/*.yaml
var profilesFS embed.FS

type auditProfile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	System      string `yaml:"system"`
	User        string `yaml:"user"`
}

// profiles is populated at init from embedded YAML, then overlaid with
// user profiles from ~/.qai/profiles/.
var profiles map[string]auditProfile

func init() {
	profiles = make(map[string]auditProfile)

	// Layer 1: embedded defaults. These are baked into the binary; any
	// failure here means the binary itself is broken (corrupt embed FS
	// or a YAML file that got corrupted at build time) — there's nothing
	// the user can fix at runtime, so we abort with a clear stderr note
	// instead of panicking with a stack trace.
	entries, err := fs.ReadDir(profilesFS, "profiles")
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai audit: embedded profiles dir missing: %v\n", err)
		fmt.Fprintln(os.Stderr, "  -> fix: this is a build defect; rebuild qai from a clean checkout (go build ./cmd/qai)")
		os.Exit(1)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := profilesFS.ReadFile("profiles/" + e.Name())
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai audit: read embedded profile %s: %v\n", e.Name(), err)
			fmt.Fprintln(os.Stderr, "  -> fix: this is a build defect; rebuild qai from a clean checkout")
			os.Exit(1)
		}
		var p auditProfile
		if err := yaml.Unmarshal(data, &p); err != nil {
			fmt.Fprintf(os.Stderr, "qai audit: parse embedded profile %s: %v\n", e.Name(), err)
			fmt.Fprintln(os.Stderr, "  -> fix: this is a build defect; rebuild qai from a clean checkout")
			os.Exit(1)
		}
		key := strings.TrimSuffix(e.Name(), ".yaml")
		profiles[key] = p
	}

	// Layer 2: user profiles from ~/.qai/profiles/ (override embedded on collision).
	userDir := filepath.Join(config.Home, ".qai", "profiles")
	if dirEntries, err := os.ReadDir(userDir); err == nil {
		for _, e := range dirEntries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(userDir, e.Name()))
			if err != nil {
				continue // non-fatal — skip unreadable files
			}
			var p auditProfile
			if err := yaml.Unmarshal(data, &p); err != nil {
				fmt.Fprintf(os.Stderr, "qai: skip bad profile %s: %v\n", e.Name(), err)
				continue
			}
			key := strings.TrimSuffix(e.Name(), ".yaml")
			profiles[key] = p
		}
	}
}

// LookupProfile returns the system and user prompt for the named profile,
// loaded from embedded defaults or ~/.qai/profiles/ overrides. Both strings
// are templates ({{PATH}} {{LANG}} {{CODE}} placeholders) — caller renders.
// Returns ok=false if the name is unknown so callers can choose how to handle
// the miss (chat falls back to no system prompt; audit errors out).
func LookupProfile(name string) (system, user string, ok bool) {
	p, found := profiles[name]
	if !found {
		return "", "", false
	}
	return p.System, p.User, true
}

// profileNames returns sorted profile keys for deterministic help output.
func ProfileNames() []string {
	names := make([]string, 0, len(profiles))
	for k := range profiles {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// loadProfileFromFile loads a single profile from an external YAML path.
func loadProfileFromFile(path string) (string, auditProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", auditProfile{}, err
	}
	var p auditProfile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return "", auditProfile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	key := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return key, p, nil
}
