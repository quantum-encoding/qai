// embedded_scripts.go — Bundle analyzer scripts into the binary using embed.FS.
//
// Scripts are extracted to ~/.local/share/qai/scripts/ on first use, so they
// can also be overridden by placing a newer version at that path.

package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed scripts/ts_analyzer.js scripts/py_analyzer.py scripts/kt_analyzer.py scripts/swift_analyzer.swift
var embeddedScripts embed.FS

// scriptDir returns the directory where extracted scripts live.
func scriptDir() string {
	return filepath.Join(home, ".local", "share", "qai", "scripts")
}

// ensureScript extracts an embedded script to the script directory if it
// doesn't already exist there. Returns the path to the script.
func ensureScript(name string) (string, error) {
	dir := scriptDir()
	outPath := filepath.Join(dir, name)

	// Already extracted? Use the existing one (allows user overrides).
	if _, err := os.Stat(outPath); err == nil {
		return outPath, nil
	}

	// Extract from embedded FS.
	data, err := embeddedScripts.ReadFile("scripts/" + name)
	if err != nil {
		return "", fmt.Errorf("embedded script %q not found: %w", name, err)
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create script dir: %w", err)
	}

	if err := os.WriteFile(outPath, data, 0755); err != nil {
		return "", fmt.Errorf("extract script %q: %w", name, err)
	}

	return outPath, nil
}
