// spec.go — YAML manifest schema for `qai fleet`.
//
// The manifest is a declarative description of a parallel agent fleet:
// one document brings up N panes, each running a fresh or resumed agent
// with a single opaque prompt. The schema is intentionally small — see
// the package brief for the constraints (no templating, single
// orchestration primitive, no DAGs).

package fleet

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// nameRE constrains pane names to characters tmux + humans both like.
// (Letters, digits, '-', '_'. No spaces, no shell metachars.)
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Spec is the top-level fleet manifest.
type Spec struct {
	Version  int       `yaml:"version"`
	Defaults Defaults  `yaml:"defaults,omitempty"`
	Panes    []PaneDef `yaml:"panes"`
}

// Defaults supplies fallback values for fields that may be unset per pane.
type Defaults struct {
	Cwd            string        `yaml:"cwd,omitempty"`
	StartupTimeout time.Duration `yaml:"startup_timeout,omitempty"`
	Reporting      Reporting     `yaml:"reporting,omitempty"`
}

// Reporting controls the worker→architect reporting protocol injected
// into every pane's prompt. When Enabled, the runner appends an
// instruction block that teaches the agent to call `qai report` at the
// done/blocked moments.
//
// OnDone / OnBlocked are the literal command lines to run. Using
// strings (not bools) keeps the protocol open to alternate reporting
// channels later (e.g. JSON to a different endpoint) without re-shaping
// the schema.
type Reporting struct {
	Enabled   bool   `yaml:"enabled"`
	OnDone    string `yaml:"on_done,omitempty"`
	OnBlocked string `yaml:"on_blocked,omitempty"`
}

// DefaultOnDone returns the v1 done-reporting line if the manifest
// didn't override it.
func (r Reporting) DefaultOnDone() string {
	if r.OnDone != "" {
		return r.OnDone
	}
	return `qai report --status done --message "<one-line summary of what you did>"`
}

// DefaultOnBlocked returns the v1 blocked-reporting line.
func (r Reporting) DefaultOnBlocked() string {
	if r.OnBlocked != "" {
		return r.OnBlocked
	}
	return `qai report --status blocked --message "<what's blocking you, with context>"`
}

// PromptBlock returns the instruction block to append to each worker's
// prompt when reporting is enabled. Returns the empty string when
// disabled. The text is fixed — no templating, no variable interpolation
// (per the manifest brief). The OnDone / OnBlocked lines are
// substituted in as opaque strings, not parsed.
func (r Reporting) PromptBlock() string {
	if !r.Enabled {
		return ""
	}
	return "\n\n---\nReporting protocol: when you finish or get blocked, run one of these in your terminal so the architect knows:\n\n" +
		"  done:    " + r.DefaultOnDone() + "\n" +
		"  blocked: " + r.DefaultOnBlocked() + "\n\n" +
		"Do not skip this step — the architect is watching the inbox, not your pane."
}

// PaneDef describes a single pane to spawn.
type PaneDef struct {
	Name    string   `yaml:"name"`
	Cwd     string   `yaml:"cwd,omitempty"`
	Agent   AgentDef `yaml:"agent"`
	Prompt  string   `yaml:"prompt"`            // opaque, sent byte-for-byte
	WaitFor string   `yaml:"wait_for,omitempty"` // single orchestration primitive
}

// AgentDef selects how the agent is started.
//
// Kind=fresh:  a cold start. Cmd + Args are exec'd in the pane.
// Kind=resume: claude --resume <session>. Session can be a literal UUID
//              or "@<alias>" that the runner resolves against the on-disk
//              session store.
type AgentDef struct {
	Kind    string   `yaml:"kind"` // "fresh" | "resume"
	Cmd     string   `yaml:"cmd,omitempty"`
	Args    []string `yaml:"args,omitempty"`
	Session string   `yaml:"session,omitempty"` // required when Kind=resume
}

// LoadFile reads, parses and validates a manifest from disk. Returns the
// spec with defaults applied to each pane, ready to hand to the runner.
func LoadFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %v", err)
	}
	return Load(data, path)
}

// Load parses + validates raw YAML bytes. originForErrors is used in error
// messages and may be empty.
func Load(data []byte, originForErrors string) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // refuse silently-ignored typos
	if err := dec.Decode(&s); err != nil {
		if originForErrors != "" {
			return nil, fmt.Errorf("parse %s: %v", originForErrors, err)
		}
		return nil, fmt.Errorf("parse manifest: %v", err)
	}
	if err := s.validate(); err != nil {
		if originForErrors != "" {
			return nil, fmt.Errorf("validate %s: %v", originForErrors, err)
		}
		return nil, fmt.Errorf("validate manifest: %v", err)
	}
	s.applyDefaults()
	return &s, nil
}

// validate checks required-field invariants and reports the first problem.
// We could batch all errors, but the manifest is small enough that
// fail-fast is friendlier than a wall of warnings.
func (s *Spec) validate() error {
	if s.Version != 1 {
		return fmt.Errorf("version: must be 1 (got %d)", s.Version)
	}
	if len(s.Panes) == 0 {
		return fmt.Errorf("panes: at least one pane required")
	}
	if s.Defaults.StartupTimeout < 0 {
		return fmt.Errorf("defaults.startup_timeout: must be non-negative")
	}
	seen := map[string]int{}
	for i, p := range s.Panes {
		if err := validatePane(p, i); err != nil {
			return err
		}
		if prev, ok := seen[p.Name]; ok {
			return fmt.Errorf("panes[%d].name: duplicate %q (also at panes[%d])", i, p.Name, prev)
		}
		seen[p.Name] = i
	}
	return nil
}

func validatePane(p PaneDef, i int) error {
	if p.Name == "" {
		return fmt.Errorf("panes[%d].name: required", i)
	}
	if !nameRE.MatchString(p.Name) {
		return fmt.Errorf("panes[%d].name: %q must match %s", i, p.Name, nameRE)
	}
	if p.Prompt == "" {
		return fmt.Errorf("panes[%d] (%s).prompt: required (use empty string to opt out, but then why is this pane here?)", i, p.Name)
	}
	switch p.Agent.Kind {
	case "fresh":
		// nothing extra required.
	case "resume":
		if p.Agent.Session == "" {
			return fmt.Errorf("panes[%d] (%s).agent.session: required when kind=resume (use @<alias> or a UUID)", i, p.Name)
		}
	case "":
		return fmt.Errorf("panes[%d] (%s).agent.kind: required (\"fresh\" or \"resume\")", i, p.Name)
	default:
		return fmt.Errorf("panes[%d] (%s).agent.kind: unknown %q (expected \"fresh\" or \"resume\")", i, p.Name, p.Agent.Kind)
	}
	if p.WaitFor != "" && !strings.HasPrefix(p.WaitFor, "/") && !strings.HasPrefix(p.WaitFor, "~") {
		return fmt.Errorf("panes[%d] (%s).wait_for: must be an absolute path (got %q)", i, p.Name, p.WaitFor)
	}
	return nil
}

// applyDefaults fills empty per-pane fields from the Defaults section.
// Cwd is the only inheritable field today; future fields (e.g. env, model)
// would slot in here.
func (s *Spec) applyDefaults() {
	for i := range s.Panes {
		if s.Panes[i].Cwd == "" {
			s.Panes[i].Cwd = s.Defaults.Cwd
		}
		if s.Panes[i].Agent.Cmd == "" {
			s.Panes[i].Agent.Cmd = "claude"
		}
	}
}

// EffectiveStartupTimeout returns the timeout to apply to a single pane's
// readiness wait. Per-pane override could be added later; today, all panes
// share Defaults.StartupTimeout, with a 30s fallback.
func (s *Spec) EffectiveStartupTimeout() time.Duration {
	if s.Defaults.StartupTimeout > 0 {
		return s.Defaults.StartupTimeout
	}
	return 30 * time.Second
}
