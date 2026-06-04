// registry.go — persistent name→pane-id map and live-session tracking.
//
// Claude Code rewrites every pane's title to "✳ Claude Code" on startup, which
// collapses tmux title-based pane resolution (every claude pane matches the
// same cleaned title). To address ad-hoc panes by the name they were spawned
// with, we persist a name→%id record at ~/.qai/term-panes.json and consult it
// before falling back to title matching. Records also carry the local Claude
// session id (the ~/.claude/projects/<enc>/<uuid>.jsonl basename) so `list`
// and `snapshot` can show which conversation each pane is running.
//
// This mirrors what internal/fleet does with its per-fleet panes.json, but for
// the imperative `qai term` surface, which has no manifest to anchor to.

package terminal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
)

// PaneRecord is the persisted metadata for one spawned pane.
type PaneRecord struct {
	Name      string `json:"name"`
	PaneID    string `json:"pane_id"`
	SessionID string `json:"session_id,omitempty"` // local Claude .jsonl uuid
	Cwd       string `json:"cwd,omitempty"`
	SpawnedAt int64  `json:"spawned_at,omitempty"` // unix seconds
}

func registryPath() string {
	return filepath.Join(config.ConfigDir(), "term-panes.json")
}

// livePaneIDsFn is the liveness probe, indirected so tests can stub it without
// a running tmux server.
var livePaneIDsFn = tmuxLivePaneIDs

func tmuxLivePaneIDs() map[string]bool {
	out := map[string]bool{}
	res, err := tmuxRun("list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(res, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out[s] = true
		}
	}
	return out
}

func livePaneIDs() map[string]bool { return livePaneIDsFn() }

func paneAlive(paneID string) bool { return livePaneIDs()[paneID] }

// ─── persistence ────────────────────────────────────────────────────────────

func loadRegistry() map[string]PaneRecord {
	data, err := os.ReadFile(registryPath())
	if err != nil {
		return map[string]PaneRecord{}
	}
	m := map[string]PaneRecord{}
	if err := json.Unmarshal(data, &m); err != nil {
		// Corrupt file — start fresh rather than wedge every spawn.
		return map[string]PaneRecord{}
	}
	return m
}

func saveRegistry(m map[string]PaneRecord) error {
	if err := os.MkdirAll(config.ConfigDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := registryPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// withRegistry runs fn under an exclusive cross-process file lock, passing the
// loaded map; whatever fn leaves in the map is saved. The lock guards the
// read-modify-write so concurrent `qai term spawn` processes don't clobber each
// other's records.
func withRegistry(fn func(m map[string]PaneRecord) error) error {
	if err := os.MkdirAll(config.ConfigDir(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(registryPath()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	m := loadRegistry()
	if err := fn(m); err != nil {
		return err
	}
	return saveRegistry(m)
}

// ─── public API ──────────────────────────────────────────────────────────────

// RegisterPane records (or updates) the name→id mapping for a spawned pane.
func RegisterPane(name, paneID, cwd string) error {
	return withRegistry(func(m map[string]PaneRecord) error {
		rec := m[name]
		rec.Name = name
		rec.PaneID = paneID
		if cwd != "" {
			rec.Cwd = cwd
		}
		rec.SpawnedAt = time.Now().Unix()
		m[name] = rec
		return nil
	})
}

// SetPaneSession attaches a detected local session id to a registered pane.
// No-op if the name isn't registered.
func SetPaneSession(name, sessionID string) error {
	return withRegistry(func(m map[string]PaneRecord) error {
		if rec, ok := m[name]; ok {
			rec.SessionID = sessionID
			m[name] = rec
		}
		return nil
	})
}

// UnregisterPaneRef removes the record matching ref, by name first and then by
// pane id, so closing a pane by either form prunes its entry. No-op if absent.
func UnregisterPaneRef(ref string) error {
	return withRegistry(func(m map[string]PaneRecord) error {
		if _, ok := m[ref]; ok {
			delete(m, ref)
			return nil
		}
		for name, rec := range m {
			if rec.PaneID == ref {
				delete(m, name)
			}
		}
		return nil
	})
}

// PruneDead drops records whose pane id is no longer present in the tmux
// server. Best-effort; called before listing.
func PruneDead() {
	_ = withRegistry(func(m map[string]PaneRecord) error {
		live := livePaneIDs()
		for name, rec := range m {
			if rec.PaneID != "" && !live[rec.PaneID] {
				delete(m, name)
			}
		}
		return nil
	})
}

// AllRecords returns a snapshot of the registry keyed by name.
func AllRecords() map[string]PaneRecord {
	return loadRegistry()
}

// recordsByID returns the registry indexed by pane id (last writer wins on the
// rare duplicate id).
func recordsByID() map[string]PaneRecord {
	out := map[string]PaneRecord{}
	for _, rec := range loadRegistry() {
		if rec.PaneID != "" {
			out[rec.PaneID] = rec
		}
	}
	return out
}

// lookupPaneID resolves a registered name to a live tmux pane id. It verifies
// the recorded id is still present in the server; a stale record is pruned and
// (false) returned so the caller falls back to title matching.
func lookupPaneID(name string) (string, bool) {
	rec, ok := loadRegistry()[name]
	if !ok || rec.PaneID == "" {
		return "", false
	}
	if paneAlive(rec.PaneID) {
		return rec.PaneID, true
	}
	_ = UnregisterPaneRef(name) // stale — drop it
	return "", false
}

// ─── local Claude session detection ──────────────────────────────────────────

// encodeProjectPath maps a filesystem path to Claude Code's project-dir name:
// every non-alphanumeric character becomes '-' (e.g. /Users/x/work/qai →
// -Users-x-work-qai). Verified against the live ~/.claude/projects layout.
func encodeProjectPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func claudeProjectDir(cwd string) string {
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	return filepath.Join(config.Home, ".claude", "projects", encodeProjectPath(cwd))
}

// BackfillSessions fills in the local Claude session id for any live registered
// pane that doesn't have one yet, by correlating each pane's spawn time with the
// creation (birth) time of the *.jsonl transcripts in its project dir. This is
// lazy and non-blocking — `spawn` returns immediately, and the next `list`/
// `snapshot` populates session ids once each pane's transcript exists.
//
// Birth time, not mtime, is the anchor: an active session's .jsonl is modified
// continuously (so mtime is always "now"), but it is created once, at launch.
// Earliest-spawned panes claim earliest-born sessions, so concurrent spawns in
// one cwd still line up in order.
func BackfillSessions() {
	_ = withRegistry(func(m map[string]PaneRecord) error {
		claimed := map[string]bool{}
		var pending []string
		for name, rec := range m {
			if rec.SessionID != "" {
				claimed[rec.SessionID] = true
			} else if rec.Cwd != "" {
				pending = append(pending, name)
			}
		}
		// Earliest spawn first, so order-of-creation lines up.
		sort.Slice(pending, func(i, j int) bool {
			return m[pending[i]].SpawnedAt < m[pending[j]].SpawnedAt
		})
		for _, name := range pending {
			rec := m[name]
			born := sessionsBornIn(rec.Cwd)
			// Allow a few seconds of skew between the recorded spawn time and
			// the transcript's birth time.
			since := time.Unix(rec.SpawnedAt, 0).Add(-3 * time.Second)
			if id := pickEarliestUnclaimed(born, since, claimed); id != "" {
				rec.SessionID = id
				claimed[id] = true
				m[name] = rec
			}
		}
		return nil
	})
}

// sessionsBornIn returns uuid→birth-time for every *.jsonl in cwd's Claude
// project dir.
func sessionsBornIn(cwd string) map[string]time.Time {
	out := map[string]time.Time{}
	entries, err := os.ReadDir(claudeProjectDir(cwd))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(e.Name(), ".jsonl")] = fileCreatedAt(info)
	}
	return out
}

// pickEarliestUnclaimed returns the uuid whose birth time is the earliest at or
// after `since`, skipping anything already claimed. "" if none qualifies. Pure
// (no I/O) so the selection logic is unit-testable without controlling birth
// times on disk.
func pickEarliestUnclaimed(born map[string]time.Time, since time.Time, claimed map[string]bool) string {
	var best string
	var bestT time.Time
	for uuid, t := range born {
		if claimed[uuid] || t.Before(since) {
			continue
		}
		if best == "" || t.Before(bestT) {
			best, bestT = uuid, t
		}
	}
	return best
}

// ─── remote control URL scraping ─────────────────────────────────────────────

// rcURLRe matches a Claude Code Remote Control URL. The canonical form is
// https://app.claude.com/rc/<uuid>; the docs also reference claude.ai/code, so
// both are accepted. Only present when a pane's session has remote control
// active (`claude --rc` / `/remote-control`).
var rcURLRe = regexp.MustCompile(`https://(?:app\.claude\.com/rc|claude\.ai/code)/[0-9A-Za-z-]+`)

// GetPaneRemoteControlURL captures a pane's recent output and returns the
// Remote Control URL if one is present, else "". Best-effort.
func GetPaneRemoteControlURL(pane string) string {
	paneID, err := resolvePane(pane)
	if err != nil {
		return ""
	}
	out, err := tmuxRun("capture-pane", "-p", "-t", paneID, "-S", "-2000")
	if err != nil {
		return ""
	}
	return rcURLRe.FindString(stripAnsi(out))
}

// shortUUID trims a uuid to its first segment for compact display.
func shortUUID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}
