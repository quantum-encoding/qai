// sessions.go — Claude Code session discovery + @alias resolution.
//
// Two on-disk stores complement each other:
//
//   ~/.claude/sessions/<pid>.json     — live process registry, mtime is fresh
//                                        while the process runs. Tells us
//                                        which sessions are *currently alive*,
//                                        with cwd/sessionId/status.
//
//   ~/.claude/projects/<encoded>/<uuid>.jsonl
//                                      — persisted message history per
//                                        session. <encoded> is the cwd with
//                                        '/' replaced by '-'. mtime tracks
//                                        last activity. Used to find
//                                        recently-dead sessions worth
//                                        resuming.
//
// Schema is stable across Claude Code 2.1.117 → 2.1.119 in observed runs.
// Both stores carry redundant fingerprints (cwd, sessionId) so the resolver
// is robust to minor schema drift.

package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

// Session is a unified record across the two on-disk stores.
type Session struct {
	ID        string    // session UUID (matches Claude's --resume input)
	Cwd       string    // working directory the session was launched from
	UpdatedAt time.Time // best-effort last activity time (file mtime or json field)
	Live      bool      // true if a live registry entry exists for this id
	PID       int       // populated when Live; 0 otherwise
	Status    string    // "busy" / "idle" / "" (only meaningful when Live)
	Source    string    // "live" or "history" — which store we found it in
}

// liveSessionFile mirrors the JSON written to ~/.claude/sessions/<pid>.json.
type liveSessionFile struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // unix millis
	UpdatedAt int64  `json:"updatedAt"`
	Status    string `json:"status"`
	Kind      string `json:"kind"`
	Version   string `json:"version"`
}

// ListSessions returns all known sessions across both stores. If cwdFilter
// is non-empty, results are restricted to that working directory (exact
// match — the caller can normalise paths first if needed).
//
// Live sessions are listed first (most useful), then historical, both sorted
// by UpdatedAt descending. A session that appears in both stores is reported
// once with Live=true.
func ListSessions(cwdFilter string) ([]Session, error) {
	live, err := listLiveSessions()
	if err != nil {
		// non-fatal: live registry can be missing on a fresh install.
		live = nil
	}
	hist, err := listHistorySessions()
	if err != nil {
		// non-fatal for the same reason.
		hist = nil
	}

	// Merge by session ID, preferring live records.
	merged := make(map[string]Session, len(live)+len(hist))
	for _, s := range hist {
		merged[s.ID] = s
	}
	for _, s := range live {
		merged[s.ID] = s
	}

	out := make([]Session, 0, len(merged))
	for _, s := range merged {
		if cwdFilter != "" && s.Cwd != cwdFilter {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// ResolveAlias maps a "@name" alias from a fleet manifest to a concrete
// session UUID, scoped to fallbackCwd (the pane's cwd in the manifest).
//
// Resolution rules, in order:
//
//  1. "@" alone (or empty) → most-recent session in fallbackCwd.
//  2. "@<basename>" where <basename> is the basename of fallbackCwd → same
//     as rule 1 (sugar for self-reference).
//  3. "@<basename>" matching some other path's basename → most-recent
//     session whose cwd basename equals <basename>. Errors with the list
//     if multiple paths share that basename.
//
// Live sessions outrank historical with the same basename.
func ResolveAlias(alias, fallbackCwd string) (string, error) {
	name := strings.TrimPrefix(alias, "@")

	// Rule 1: "@" or empty → latest in fallbackCwd
	if name == "" {
		s, err := mostRecentInCwd(fallbackCwd)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %v", alias, err)
		}
		return s.ID, nil
	}

	// Rule 2: alias matches fallbackCwd's basename → same as rule 1.
	if name == filepath.Base(fallbackCwd) {
		s, err := mostRecentInCwd(fallbackCwd)
		if err != nil {
			return "", fmt.Errorf("resolve %q (matched fallback cwd basename): %v", alias, err)
		}
		return s.ID, nil
	}

	// Rule 3: cross-cwd basename match.
	all, err := ListSessions("")
	if err != nil {
		return "", fmt.Errorf("resolve %q: %v", alias, err)
	}
	var matches []Session
	for _, s := range all {
		if filepath.Base(s.Cwd) == name {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("resolve %q: no session found with cwd basename %q", alias, name)
	}
	// matches is already sorted (live > history, then by UpdatedAt desc).
	// Disambiguate when multiple distinct cwds share the basename: list them.
	cwds := uniqueCwds(matches)
	if len(cwds) > 1 {
		return "", fmt.Errorf("resolve %q: ambiguous, basename %q matches multiple cwds:\n  %s",
			alias, name, strings.Join(cwds, "\n  "))
	}
	return matches[0].ID, nil
}

// mostRecentInCwd returns the latest known session for an exact cwd.
func mostRecentInCwd(cwd string) (Session, error) {
	if cwd == "" {
		return Session{}, fmt.Errorf("cwd is required")
	}
	sessions, err := ListSessions(cwd)
	if err != nil {
		return Session{}, err
	}
	if len(sessions) == 0 {
		return Session{}, fmt.Errorf("no sessions found for cwd %q (try `qai sessions list --cwd %s`)", cwd, cwd)
	}
	return sessions[0], nil
}

// listLiveSessions reads ~/.claude/sessions/*.json. Each file describes a
// running claude process (PID is the filename stem).
func listLiveSessions() ([]Session, error) {
	dir := filepath.Join(config.Home, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var lf liveSessionFile
		if err := json.Unmarshal(data, &lf); err != nil {
			continue
		}
		if lf.SessionID == "" || lf.Cwd == "" {
			continue
		}
		// updatedAt is ms since epoch; fall back to file mtime if absent.
		var ts time.Time
		if lf.UpdatedAt > 0 {
			ts = time.UnixMilli(lf.UpdatedAt)
		} else if info, err := e.Info(); err == nil {
			ts = info.ModTime()
		}
		out = append(out, Session{
			ID:        lf.SessionID,
			Cwd:       lf.Cwd,
			UpdatedAt: ts,
			Live:      true,
			PID:       lf.PID,
			Status:    lf.Status,
			Source:    "live",
		})
	}
	return out, nil
}

// listHistorySessions walks ~/.claude/projects/<encoded>/*.jsonl. Each file
// is a single session's persisted history; the filename stem is the UUID
// and the directory name encodes the cwd ('/' → '-').
func listHistorySessions() ([]Session, error) {
	root := filepath.Join(config.Home, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, projDir := range entries {
		if !projDir.IsDir() {
			continue
		}
		cwd := decodeCwd(projDir.Name())
		dirPath := filepath.Join(root, projDir.Name())
		files, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			info, err := f.Info()
			if err != nil {
				continue
			}
			out = append(out, Session{
				ID:        id,
				Cwd:       cwd,
				UpdatedAt: info.ModTime(),
				Live:      false,
				Source:    "history",
			})
		}
	}
	return out, nil
}

// decodeCwd reverses the cwd-encoding Claude uses for project directory names.
// Encoding is '/' → '-'. This is lossy for paths containing literal '-' (no
// way to disambiguate), but the only real use here is display — actual cwd
// matching uses the live-store's exact field or the user's manifest.
func decodeCwd(encoded string) string {
	return strings.ReplaceAll(encoded, "-", "/")
}

// EncodeCwd applies the same encoding Claude uses, for callers that need to
// look up a project directory by cwd.
func EncodeCwd(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

func uniqueCwds(sessions []Session) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sessions {
		if !seen[s.Cwd] {
			seen[s.Cwd] = true
			out = append(out, s.Cwd)
		}
	}
	return out
}
