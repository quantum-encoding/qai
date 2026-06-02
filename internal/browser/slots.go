// slots.go — named tab slots for multi-tab workflows.
//
// The agent can pre-allocate a handful of tabs (e.g. TAB1..TAB5) and
// drive different work streams against each one without the friction
// of memorising 32-char hex tab IDs. Slot names are arbitrary strings
// (TAB1, main, cj, research, …) as long as they can't be confused
// with a tab-ID prefix.
//
// Persistence: ~/.qai/browser-slots.json, pruned automatically on
// every Load() so a slot whose tab the user closed manually doesn't
// linger as a dangling pointer.

package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// SlotMap is the persisted slot → tab-ID mapping.
type SlotMap map[string]string

var slotsMu sync.Mutex

// slotsPath is the on-disk location. Overridable in tests.
var slotsPath = ""

func defaultSlotsPath() string {
	if slotsPath != "" {
		return slotsPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".qai", "browser-slots.json")
}

// LoadSlots reads the slot map from disk and prunes entries whose
// tab no longer exists. The pruned map is written back if anything
// changed so subsequent reads are clean.
//
// liveTabIDs may be nil; when nil, no pruning happens (use for
// read-only inspections where you don't want a side-effecting write).
func LoadSlots(liveTabIDs map[string]bool) (SlotMap, error) {
	slotsMu.Lock()
	defer slotsMu.Unlock()
	return loadSlotsLocked(liveTabIDs)
}

func loadSlotsLocked(liveTabIDs map[string]bool) (SlotMap, error) {
	p := defaultSlotsPath()
	if p == "" {
		return SlotMap{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return SlotMap{}, nil
		}
		return nil, err
	}
	var m SlotMap
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if liveTabIDs == nil {
		return m, nil
	}
	// Prune stale entries — write back only if something dropped.
	changed := false
	for name, id := range m {
		if !liveTabIDs[id] {
			delete(m, name)
			changed = true
		}
	}
	if changed {
		_ = writeSlotsLocked(m)
	}
	return m, nil
}

// SaveSlot pins a tab ID to a slot name, persisting immediately.
// Overwrites an existing pin on that name without error.
func SaveSlot(name, tabID string) error {
	slotsMu.Lock()
	defer slotsMu.Unlock()
	m, err := loadSlotsLocked(nil)
	if err != nil {
		return err
	}
	m[name] = tabID
	return writeSlotsLocked(m)
}

// UnpinSlot removes a slot. Returns nil if the slot didn't exist.
func UnpinSlot(name string) error {
	slotsMu.Lock()
	defer slotsMu.Unlock()
	m, err := loadSlotsLocked(nil)
	if err != nil {
		return err
	}
	delete(m, name)
	return writeSlotsLocked(m)
}

// UnpinTabID removes ANY slot pinned to the given tab ID. Used after
// closing a tab via its hex ID rather than its slot name.
func UnpinTabID(tabID string) error {
	slotsMu.Lock()
	defer slotsMu.Unlock()
	m, err := loadSlotsLocked(nil)
	if err != nil {
		return err
	}
	changed := false
	for name, id := range m {
		if id == tabID {
			delete(m, name)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeSlotsLocked(m)
}

func writeSlotsLocked(m SlotMap) error {
	p := defaultSlotsPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// hex32Re matches a full 32-char hex tab ID. Anything outside this
// shape is treated as a slot name candidate so users can use plain
// strings like "TAB1" / "main" / "cj-research" without collision.
var hex32Re = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// LooksLikeSlotName returns true when arg is NOT a full hex tab ID,
// so the addressing layer should look it up in the slot map. A short
// hex string is ambiguous (could be a tab-ID prefix or a slot name);
// we err on the side of "prefix match first, slot map second" because
// existing scripts depend on prefix-match semantics.
func LooksLikeSlotName(arg string) bool {
	return !hex32Re.MatchString(arg)
}

// FormatSlotList renders a slot map for `qai browser list` output.
// Returns a stable, alpha-sorted string of "TAB1=<id8>" pairs so
// repeated calls produce identical output.
func FormatSlotList(m SlotMap) string {
	if len(m) == 0 {
		return ""
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var sb strings.Builder
	for i, n := range names {
		if i > 0 {
			sb.WriteString("  ")
		}
		id := m[n]
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(&sb, "%s=%s", n, short)
	}
	return sb.String()
}
