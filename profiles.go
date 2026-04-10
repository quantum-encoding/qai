package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

	// Layer 1: embedded defaults.
	entries, err := fs.ReadDir(profilesFS, "profiles")
	if err != nil {
		panic("embedded profiles dir missing: " + err.Error())
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := profilesFS.ReadFile("profiles/" + e.Name())
		if err != nil {
			panic("read embedded profile " + e.Name() + ": " + err.Error())
		}
		var p auditProfile
		if err := yaml.Unmarshal(data, &p); err != nil {
			panic("parse profile " + e.Name() + ": " + err.Error())
		}
		key := strings.TrimSuffix(e.Name(), ".yaml")
		profiles[key] = p
	}

	// Layer 2: user profiles from ~/.qai/profiles/ (override embedded on collision).
	userDir := filepath.Join(home, ".qai", "profiles")
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

// profileNames returns sorted profile keys for deterministic help output.
func profileNames() []string {
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
