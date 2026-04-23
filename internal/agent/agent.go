// Package agent implements `qai agent` — a portable, Joplin-backed store
// for agent profiles, skills, and operational instructions.
//
// One top-level notebook called "AGENT" with three sub-notebooks:
//
//	AGENT/
//	  Skills/        — one note per SKILL.md (or standalone skill .md)
//	  Agents/        — one note per agent profile .md
//	  Instructions/  — CLAUDE.md and similar operational docs
//
// Think of it as a Joplin-hosted `~/.claude/`: the knowledge every project
// depends on but isn't tied to any single one. Portable across machines,
// searchable via `qai search --joplin`, and editable in the Joplin UI.
//
// Commands:
//
//	qai agent init              # create notebook + seed from ~/.claude
//	qai agent init --deep       # also walk ~/work for SKILL.md / CLAUDE.md
//	qai agent init --dry-run    # preview without writing
//	qai agent list              # list what's stored
//	qai agent add <file>        # add a single file (auto-classified by path)
package agent

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Cfg is injected from main (matches the conduct.Cfg / search.Cfg pattern).
var Cfg *config.Config

const (
	rootNotebook       = "AGENT"
	folderSkills       = "Skills"
	folderAgents       = "Agents"
	folderInstructions = "Instructions"
)

// CmdAgent is the entry point.
func CmdAgent(args []string) {
	if len(args) == 0 {
		doList(false)
		return
	}
	action := args[0]
	rest := args[1:]

	var dryRun, deep, asJSON bool
	var positional []string
	for _, a := range rest {
		switch a {
		case "--dry-run", "-n":
			dryRun = true
		case "--deep":
			deep = true
		case "--json", "-j":
			asJSON = true
		default:
			positional = append(positional, a)
		}
	}

	switch action {
	case "--help", "-h", "help":
		usage()
	case "init", "sync":
		doInit(dryRun, deep)
	case "list", "ls":
		doList(asJSON)
	case "add":
		if len(positional) == 0 {
			die(fmt.Errorf("qai agent add requires a file path"))
		}
		doAdd(positional[0], dryRun)
	case "status":
		doStatus(asJSON)
	default:
		fmt.Fprintf(os.Stderr, "qai agent: unknown action %q\n", action)
		usage()
		os.Exit(1)
	}
}

// ── actions ─────────────────────────────────────────────────────────────────

func doInit(dryRun, deep bool) {
	client, err := newClient()
	if err != nil {
		die(err)
	}

	root, err := findOrCreateFolder(client, rootNotebook, "")
	if err != nil {
		die(err)
	}
	skills, err := findOrCreateFolder(client, folderSkills, root.ID)
	if err != nil {
		die(err)
	}
	agents, err := findOrCreateFolder(client, folderAgents, root.ID)
	if err != nil {
		die(err)
	}
	instructions, err := findOrCreateFolder(client, folderInstructions, root.ID)
	if err != nil {
		die(err)
	}

	// Pre-scan existing notes so re-runs update-in-place.
	skillMap := titleMap(client, skills.ID)
	agentMap := titleMap(client, agents.ID)
	instrMap := titleMap(client, instructions.ID)

	var skillHits, agentHits, instrHits []seed
	sources := defaultSources()
	if deep {
		sources = append(sources, deepSources()...)
	}
	for _, src := range sources {
		walkSource(src, &skillHits, &agentHits, &instrHits)
	}

	// Stable order — same file wins every time if there are duplicates.
	skillHits = dedupByTitle(skillHits)
	agentHits = dedupByTitle(agentHits)
	instrHits = dedupByTitle(instrHits)

	fmt.Printf("AGENT notebook: %s (%s)\n", root.Title, root.ID)
	if deep {
		fmt.Println("  sources: ~/.claude + ~/work (--deep)")
	} else {
		fmt.Println("  sources: ~/.claude (pass --deep to also walk ~/work)")
	}
	fmt.Printf("  found: %d skills, %d agents, %d instructions\n",
		len(skillHits), len(agentHits), len(instrHits))

	if dryRun {
		fmt.Println("\n[dry-run] would write:")
		for _, s := range skillHits {
			fmt.Printf("  Skills/%s        ← %s\n", s.title, s.path)
		}
		for _, s := range agentHits {
			fmt.Printf("  Agents/%s        ← %s\n", s.title, s.path)
		}
		for _, s := range instrHits {
			fmt.Printf("  Instructions/%s  ← %s\n", s.title, s.path)
		}
		return
	}

	cSkill, uSkill := writeSeeds(client, skills.ID, skillHits, skillMap)
	cAgent, uAgent := writeSeeds(client, agents.ID, agentHits, agentMap)
	cInstr, uInstr := writeSeeds(client, instructions.ID, instrHits, instrMap)

	fmt.Printf("\nSkills:       created %d, updated %d\n", cSkill, uSkill)
	fmt.Printf("Agents:       created %d, updated %d\n", cAgent, uAgent)
	fmt.Printf("Instructions: created %d, updated %d\n", cInstr, uInstr)
}

func doList(asJSON bool) {
	client, err := newClient()
	if err != nil {
		die(err)
	}
	root, err := findFolderByTitleUnder(client, "", rootNotebook)
	if err != nil {
		die(err)
	}
	if root == nil {
		fmt.Println("No AGENT notebook yet. Run: qai agent init")
		return
	}
	sections := []struct {
		name string
		id   string
	}{}
	for _, sub := range []string{folderSkills, folderAgents, folderInstructions} {
		f, err := findFolderByTitleUnder(client, root.ID, sub)
		if err != nil || f == nil {
			continue
		}
		sections = append(sections, struct {
			name string
			id   string
		}{sub, f.ID})
	}

	if asJSON {
		// Walk once and dump a structured snapshot.
		out := map[string][]string{}
		for _, s := range sections {
			notes, _ := client.ListNotes(s.id)
			titles := make([]string, 0, len(notes))
			for _, n := range notes {
				titles = append(titles, n.Title)
			}
			out[s.name] = titles
		}
		jsonEncode(out)
		return
	}

	for _, s := range sections {
		notes, err := client.ListNotes(s.id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", s.name, err)
			continue
		}
		fmt.Printf("%s/ (%d)\n", s.name, len(notes))
		for _, n := range notes {
			fmt.Printf("  %s\n", strings.TrimSpace(n.Title))
		}
	}
}

func doAdd(path string, dryRun bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		die(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		die(err)
	}
	if info.IsDir() {
		die(fmt.Errorf("qai agent add takes a file, not a directory: %s", abs))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		die(err)
	}
	s := classify(abs, data)
	if s == nil {
		die(fmt.Errorf("couldn't classify %s — not a SKILL.md, agent .md, or CLAUDE.md", abs))
	}

	if dryRun {
		fmt.Printf("[dry-run] would add %s/%s ← %s\n", s.kind, s.title, s.path)
		return
	}
	client, err := newClient()
	if err != nil {
		die(err)
	}
	root, err := findOrCreateFolder(client, rootNotebook, "")
	if err != nil {
		die(err)
	}
	sub, err := findOrCreateFolder(client, folderFor(s.kind), root.ID)
	if err != nil {
		die(err)
	}
	existing := titleMap(client, sub.ID)
	body := renderBody(s.path, data)
	if id, ok := existing[s.title]; ok {
		if err := client.UpdateNoteBody(id, body); err != nil {
			die(err)
		}
		fmt.Printf("Updated %s/%s\n", s.kind, s.title)
		return
	}
	if _, err := client.CreateNote(joplin.Note{
		Title: s.title, Body: body, ParentID: sub.ID,
	}); err != nil {
		die(err)
	}
	fmt.Printf("Created %s/%s\n", s.kind, s.title)
}

func doStatus(asJSON bool) {
	client, err := newClient()
	if err != nil {
		die(err)
	}
	root, err := findFolderByTitleUnder(client, "", rootNotebook)
	if err != nil {
		die(err)
	}
	if root == nil {
		fmt.Println("No AGENT notebook yet. Run: qai agent init")
		return
	}
	counts := map[string]int{}
	for _, sub := range []string{folderSkills, folderAgents, folderInstructions} {
		f, err := findFolderByTitleUnder(client, root.ID, sub)
		if err != nil || f == nil {
			continue
		}
		notes, _ := client.ListNotes(f.ID)
		counts[sub] = len(notes)
	}
	if asJSON {
		jsonEncode(map[string]any{"notebook_id": root.ID, "counts": counts})
		return
	}
	fmt.Printf("AGENT notebook: %s\n", root.ID)
	for _, k := range []string{folderSkills, folderAgents, folderInstructions} {
		fmt.Printf("  %-14s %d\n", k+":", counts[k])
	}
}

// ── discovery ───────────────────────────────────────────────────────────────

// seed is a classified file we plan to write into Joplin.
type seed struct {
	kind  string // "skill" | "agent" | "instruction"
	title string
	path  string
}

func defaultSources() []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".claude", "agents"),
		filepath.Join(home, ".claude", "plugins"),
		filepath.Join(home, ".claude", "quantum-agents"),
		filepath.Join(home, ".claude", "CLAUDE.md"), // direct file
	}
}

func deepSources() []string {
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, "work")}
}

// Directories we never descend into. Same list as project/copy.go plus a few
// Claude-Code-specific caches that live inside ~/.claude.
var skipDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "target": true,
	"__pycache__": true, "venv": true, "env": true,
	// ~/.claude-specific: huge, noisy, no skills in there.
	"projects": true, "file-history": true, "paste-cache": true,
	"cache": true, "sessions": true, "session-env": true,
	"messages": true, "backups": true, "debug": true, "ide": true,
	"image-cache": true, "history.jsonl": true,
}

func walkSource(root string, skills, agents, instructions *[]seed) {
	info, err := os.Stat(root)
	if err != nil {
		return // missing source is fine — user may not have it
	}
	if !info.IsDir() {
		// Direct-file source (e.g. ~/.claude/CLAUDE.md).
		data, err := os.ReadFile(root)
		if err == nil {
			if s := classify(root, data); s != nil {
				*dispatch(s.kind, skills, agents, instructions) = append(*dispatch(s.kind, skills, agents, instructions), *s)
			}
		}
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if s := classify(path, data); s != nil {
			dest := dispatch(s.kind, skills, agents, instructions)
			*dest = append(*dest, *s)
		}
		return nil
	})
}

func dispatch(kind string, skills, agents, instructions *[]seed) *[]seed {
	switch kind {
	case "skill":
		return skills
	case "agent":
		return agents
	default:
		return instructions
	}
}

// classify decides whether a file is a skill, agent profile, instruction,
// or nothing-worth-storing. Path-based: structure beats content here because
// SKILL.md / agent .md both use frontmatter and would otherwise look similar.
func classify(absPath string, data []byte) *seed {
	base := filepath.Base(absPath)
	dir := filepath.Dir(absPath)
	parent := filepath.Base(dir)
	parts := strings.Split(absPath, string(filepath.Separator))

	hasSegment := func(seg string) bool {
		for _, p := range parts {
			if p == seg {
				return true
			}
		}
		return false
	}

	switch {
	case base == "SKILL.md":
		return &seed{kind: "skill", title: parent, path: absPath}
	case base == "CLAUDE.md":
		// Use the folder name for context (e.g. "global" for ~/.claude/CLAUDE.md,
		// project dir name otherwise).
		name := parent
		if strings.HasSuffix(dir, "/.claude") || strings.HasSuffix(dir, "\\.claude") {
			grand := filepath.Base(filepath.Dir(dir))
			if grand == "director" || grand == os.Getenv("USER") {
				name = "global"
			} else {
				name = grand
			}
		}
		return &seed{kind: "instruction", title: name, path: absPath}
	case hasSegment("agents") && strings.HasSuffix(base, ".md"):
		if strings.EqualFold(base, "README.md") {
			return nil
		}
		// Only direct children of an agents/ dir count as profiles.
		// Anything nested deeper (e.g. agents/foo/refs/bar.md) is support material.
		if filepath.Base(dir) != "agents" {
			return nil
		}
		return &seed{kind: "agent", title: strings.TrimSuffix(base, ".md"), path: absPath}
	case hasSegment("skills") && strings.HasSuffix(base, ".md") && !strings.EqualFold(base, "README.md"):
		// Walk up looking for a SKILL.md. If one exists above this file
		// (within the skills/ tree), we've already captured that skill and
		// this file is just reference/template/example material — skip it.
		cur := dir
		for {
			bn := filepath.Base(cur)
			if bn == "skills" || bn == "/" || bn == "." {
				break
			}
			if _, err := os.Stat(filepath.Join(cur, "SKILL.md")); err == nil {
				return nil
			}
			next := filepath.Dir(cur)
			if next == cur {
				break
			}
			cur = next
		}
		return &seed{kind: "skill", title: strings.TrimSuffix(base, ".md"), path: absPath}
	}
	return nil
}

// dedupByTitle keeps the first occurrence of a title and drops later ones.
// Walk order determines who wins; for MVP that's good enough.
func dedupByTitle(in []seed) []seed {
	seen := map[string]bool{}
	out := make([]seed, 0, len(in))
	for _, s := range in {
		if seen[s.title] {
			continue
		}
		seen[s.title] = true
		out = append(out, s)
	}
	return out
}

// ── Joplin helpers ──────────────────────────────────────────────────────────

func writeSeeds(client *joplin.Client, parentID string, seeds []seed, existing map[string]string) (created, updated int) {
	for _, s := range seeds {
		data, err := os.ReadFile(s.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: read %s: %v\n", s.path, err)
			continue
		}
		body := renderBody(s.path, data)
		if id, ok := existing[s.title]; ok {
			if err := client.UpdateNoteBody(id, body); err != nil {
				fmt.Fprintf(os.Stderr, "  warn: update %s: %v\n", s.title, err)
				continue
			}
			updated++
			continue
		}
		if _, err := client.CreateNote(joplin.Note{
			Title: s.title, Body: body, ParentID: parentID,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: create %s: %v\n", s.title, err)
			continue
		}
		created++
	}
	return
}

func renderBody(path string, data []byte) string {
	return fmt.Sprintf("**Source:** `%s`\n\n---\n\n%s", path, string(data))
}

// findFolderByTitleUnder filters ListFolders by parent so "Skills" under
// AGENT doesn't collide with a top-level "Skills" the user might have.
func findFolderByTitleUnder(client *joplin.Client, parentID, title string) (*joplin.Folder, error) {
	folders, err := client.ListFolders()
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(title)
	for i := range folders {
		if folders[i].ParentID == parentID && strings.TrimSpace(folders[i].Title) == want {
			return &folders[i], nil
		}
	}
	return nil, nil
}

func findOrCreateFolder(client *joplin.Client, title, parentID string) (*joplin.Folder, error) {
	f, err := findFolderByTitleUnder(client, parentID, title)
	if err != nil {
		return nil, err
	}
	if f != nil {
		return f, nil
	}
	return client.CreateFolder(title, parentID)
}

func titleMap(client *joplin.Client, folderID string) map[string]string {
	out := map[string]string{}
	notes, err := client.ListNotes(folderID)
	if err != nil {
		return out
	}
	for _, n := range notes {
		out[strings.TrimSpace(n.Title)] = n.ID
	}
	return out
}

func folderFor(kind string) string {
	switch kind {
	case "skill":
		return folderSkills
	case "agent":
		return folderAgents
	default:
		return folderInstructions
	}
}

// ── small utilities ─────────────────────────────────────────────────────────

func newClient() (*joplin.Client, error) {
	if Cfg.Joplin.Token == "" {
		return nil, fmt.Errorf(
			"Joplin token not configured. Set one with either:\n" +
				"  export JOPLIN_TOKEN=...            (Tools → Options → Web Clipper in Joplin)\n" +
				"  or add joplin.token to ~/.qai/config.yaml")
	}
	c := joplin.New(joplin.Config{
		BaseURL: Cfg.Joplin.BaseURL,
		Token:   Cfg.Joplin.Token,
	})
	if err := c.Ping(); err != nil {
		return nil, err
	}
	return c, nil
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "qai agent: %v\n", err)
	os.Exit(1)
}

func jsonEncode(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func usage() {
	fmt.Println(`qai agent — portable store for agent skills, profiles, and instructions.

USAGE
  qai agent                           # same as: qai agent list
  qai agent init      [--deep] [-n]   # create AGENT notebook + seed from ~/.claude
  qai agent sync      [--deep] [-n]   # alias for init (re-runs are idempotent)
  qai agent list      [-j]            # list stored skills / agents / instructions
  qai agent add <file> [-n]           # classify a single file and add it
  qai agent status    [-j]            # show notebook id + counts

FLAGS
  --deep          Also walk ~/work for SKILL.md / CLAUDE.md / agents/*.md
  -n, --dry-run   Preview without writing to Joplin
  -j, --json      Machine-readable JSON

LAYOUT
  AGENT/
    Skills/        ← SKILL.md files + standalone skills/<name>.md
    Agents/        ← .md files under any agents/ directory
    Instructions/  ← CLAUDE.md files (global, per-project)

The AGENT notebook is the agent's portable brain — persists across machines,
searchable via 'qai search --joplin', and editable from the Joplin UI.`)
}
