package fleet

import (
	"strings"
	"testing"
	"time"
)

func TestLoadFile_Basic(t *testing.T) {
	t.Parallel()
	s, err := LoadFile("testdata/fleet-basic.yaml")
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if s.Version != 1 {
		t.Errorf("version: want 1, got %d", s.Version)
	}
	if got := s.Defaults.Cwd; got != "/tmp/work" {
		t.Errorf("defaults.cwd: %q", got)
	}
	if got := s.Defaults.StartupTimeout; got != 30*time.Second {
		t.Errorf("defaults.startup_timeout: %v", got)
	}
	if len(s.Panes) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(s.Panes))
	}
	if s.Panes[1].Agent.Kind != "resume" || s.Panes[1].Agent.Session != "@chronos_engine" {
		t.Errorf("chronos pane: kind=%q session=%q", s.Panes[1].Agent.Kind, s.Panes[1].Agent.Session)
	}
	if s.Panes[2].WaitFor != "/tmp/work/project/facts.json" {
		t.Errorf("writer pane wait_for wrong: %q", s.Panes[2].WaitFor)
	}
}

func TestLoad_RejectsUnknownVersion(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 2
panes:
  - name: a
    agent: {kind: fresh, cmd: claude}
    prompt: x
`)
	_, err := Load(yaml, "")
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version error, got %v", err)
	}
}

func TestLoad_RejectsResumeWithoutSession(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 1
panes:
  - name: a
    agent: {kind: resume, cmd: claude}
    prompt: x
`)
	_, err := Load(yaml, "")
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("want session-required error, got %v", err)
	}
}

func TestLoad_RejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 1
panes:
  - name: a
    agent: {kind: fresh, cmd: claude}
    prompt: x
  - name: a
    agent: {kind: fresh, cmd: claude}
    prompt: y
`)
	_, err := Load(yaml, "")
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestLoad_RejectsRelativeWaitFor(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 1
panes:
  - name: a
    agent: {kind: fresh, cmd: claude}
    prompt: x
    wait_for: facts.json
`)
	_, err := Load(yaml, "")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("want absolute-path error, got %v", err)
	}
}

func TestLoad_RejectsBadName(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 1
panes:
  - name: "has spaces"
    agent: {kind: fresh, cmd: claude}
    prompt: x
`)
	_, err := Load(yaml, "")
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("want name error, got %v", err)
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 1
panez:
  - name: a
    agent: {kind: fresh, cmd: claude}
    prompt: x
`)
	_, err := Load(yaml, "")
	if err == nil {
		t.Fatalf("want strict-mode error on typo, got nil")
	}
}

func TestApplyDefaults_FillsCwd(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 1
defaults:
  cwd: /home
panes:
  - name: a
    agent: {kind: fresh, cmd: claude}
    prompt: x
`)
	s, err := Load(yaml, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Panes[0].Cwd != "/home" {
		t.Errorf("expected default cwd inherited, got %q", s.Panes[0].Cwd)
	}
}

func TestApplyDefaults_FillsCmd(t *testing.T) {
	t.Parallel()
	yaml := []byte(`version: 1
panes:
  - name: a
    agent: {kind: fresh}
    prompt: x
`)
	s, err := Load(yaml, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Panes[0].Agent.Cmd != "claude" {
		t.Errorf("expected default cmd 'claude', got %q", s.Panes[0].Agent.Cmd)
	}
}
