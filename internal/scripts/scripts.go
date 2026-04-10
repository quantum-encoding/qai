// Package scripts provides embedded analyzer scripts.
package scripts

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

//go:embed scripts/ts_analyzer.js scripts/py_analyzer.py scripts/kt_analyzer.py scripts/swift_analyzer.swift
var embeddedScripts embed.FS

func scriptDir() string {
	return filepath.Join(config.Home, ".local", "share", "qai", "scripts")
}

// EnsureScript extracts an embedded script if it doesn't exist. Returns the path.
func EnsureScript(name string) (string, error) {
	dir := scriptDir()
	outPath := filepath.Join(dir, name)
	if _, err := os.Stat(outPath); err == nil {
		return outPath, nil
	}
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
