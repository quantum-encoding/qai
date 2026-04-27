// runner.go — Up / Down / Status / Snapshot orchestration for a fleet.
//
// Up brings every pane up in parallel. Each pane: spawn → wait for the
// agent's REPL → optionally wait_for a path → send the manifest's prompt.
// First failure cancels the rest cleanly via errgroup.
//
// Down kills only the panes named in the manifest. Status / Snapshot are
// thin wrappers over terminal package primitives, filtered to the
// manifest's pane set.

package fleet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/terminal"

	"golang.org/x/sync/errgroup"
)

// defaultWaitForTimeout — wait_for is for inter-pane producer/consumer
// coordination; it can legitimately wait minutes for a peer agent to
// produce a file. Startup timeout is per-pane, much shorter.
const defaultWaitForTimeout = 10 * time.Minute

// Up spawns every pane in parallel and sends each one's prompt once its
// agent is ready (and any wait_for path appears). The function returns
// only after every pane has accepted its prompt or one has failed.
//
// Side effects: Up generates a fleet id, captures the architect pane id
// from $TMUX_PANE, writes ~/.qai/fleet/<id>/{architect-pane,active}, and
// (when the manifest enables reporting) spawns a detached notifier
// background process. None of those side effects are rolled back on
// error — the user can `qai fleet down` to clean up.
//
// On first error, errgroup cancels ctx; in-flight panes that have already
// been spawned remain alive.
func Up(ctx context.Context, spec *Spec) (string, error) {
	fleetID, err := setupFleetState(spec)
	if err != nil {
		return "", err
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, pane := range spec.Panes {
		pane := pane
		g.Go(func() error {
			return spawnAndPrompt(gctx, spec, pane, fleetID)
		})
	}
	if err := g.Wait(); err != nil {
		return fleetID, err
	}

	if spec.Defaults.Reporting.Enabled {
		if err := startNotifier(fleetID); err != nil {
			// Notifier failure is non-fatal — fleet is up, the user can
			// retry with `qai fleet notifier <id>` manually.
			fmt.Fprintf(os.Stderr, "qai fleet: notifier did not start: %v (run `qai fleet notifier %s` to retry)\n", err, fleetID)
		}
	}
	return fleetID, nil
}

// setupFleetState generates the fleet id, captures the architect pane,
// and writes the per-fleet state pointers.
func setupFleetState(spec *Spec) (string, error) {
	architect := os.Getenv("TMUX_PANE")
	if architect == "" {
		return "", fmt.Errorf("$TMUX_PANE not set — `qai fleet up` must be invoked from inside a tmux pane (the architect pane)")
	}
	id := generateFleetID(spec)
	dir, err := EnsureFleetDir(id)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "architect-pane"), []byte(architect+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write architect-pane: %v", err)
	}
	if err := SetActiveFleet(id); err != nil {
		return "", fmt.Errorf("write active-fleet pointer: %v", err)
	}
	return id, nil
}

// generateFleetID produces a human-readable + collision-resistant id.
// Prefer the manifest-name flavour because operators read it more often
// than they sort by it.
func generateFleetID(spec *Spec) string {
	stem := "fleet"
	if len(spec.Panes) > 0 {
		stem = spec.Panes[0].Name
		if i := strings.IndexByte(stem, '-'); i > 0 {
			stem = stem[:i] // first segment of first pane name
		}
	}
	return fmt.Sprintf("%s-%d", stem, time.Now().Unix())
}

// startNotifier re-execs `qai fleet notifier <id>` as a detached
// background process. The notifier survives the lifetime of `qai fleet
// up` and is reaped by `qai fleet down`.
func startNotifier(fleetID string) error {
	self, err := os.Executable()
	if err != nil {
		// Fallback: if the running binary's path can't be resolved,
		// look up `qai` on PATH.
		if p, perr := exec.LookPath("qai"); perr == nil {
			self = p
		} else {
			return fmt.Errorf("locate qai binary: %v", err)
		}
	}
	cmd := exec.Command(self, "fleet", "notifier", fleetID)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start notifier: %v", err)
	}
	// Don't Wait — we want the child detached. Release the Go process
	// handle so the runtime doesn't try to reap.
	go func() { _ = cmd.Process.Release() }()
	return nil
}

// stopNotifier reads notifier.pid for the fleet and SIGTERMs the
// notifier. Idempotent — silently succeeds if the file is missing or
// the process is gone.
func stopNotifier(fleetID string) error {
	path := filepath.Join(FleetDir(fleetID), "notifier.pid")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(path)
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		// Stale pidfile or insufficient perms — both are "we did our
		// best." The notifier removes the pidfile on clean exit.
		_ = os.Remove(path)
		return nil
	}
	return nil
}

// Down kills the panes named in the spec. Panes not currently alive are
// silently skipped (already-down is the goal anyway). Errors from
// individual kills are aggregated, not fatal.
func Down(spec *Spec) error {
	var failures []string
	for _, pane := range spec.Panes {
		if err := terminal.Close(pane.Name, true); err != nil {
			// Ignore "not found" — that's a successful Down.
			if !strings.Contains(err.Error(), "not found") {
				failures = append(failures, fmt.Sprintf("%s: %v", pane.Name, err))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("down: %s", strings.Join(failures, "; "))
	}
	return nil
}

// PaneStatus is one row in the Status output.
type PaneStatus struct {
	Name  string
	Alive bool
	ID    string // tmux pane id when Alive
	Cwd   string
	Cmd   string // current command running in the pane (claude.exe / zsh / …)
}

// Status enumerates which manifest panes are alive vs missing.
func Status(spec *Spec) ([]PaneStatus, error) {
	live, err := terminal.List()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]terminal.Pane, len(live))
	for _, p := range live {
		// Pane titles include leading spinner glyphs once claude takes
		// over, so we match on the trimmed name. terminal.List exposes
		// the raw title; trim consistently with cleanName below.
		byName[trimSpinner(p.Name)] = p
	}
	out := make([]PaneStatus, 0, len(spec.Panes))
	for _, def := range spec.Panes {
		if p, ok := byName[def.Name]; ok {
			out = append(out, PaneStatus{
				Name: def.Name, Alive: true, ID: p.ID, Cwd: p.Cwd, Cmd: p.Cmd,
			})
		} else {
			out = append(out, PaneStatus{Name: def.Name, Alive: false, Cwd: def.Cwd})
		}
	}
	return out, nil
}

// Snapshot returns a tail of every manifest pane's recent output.
func Snapshot(spec *Spec) ([]terminal.PaneSnapshot, error) {
	all, err := terminal.Snapshot()
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(spec.Panes))
	for _, p := range spec.Panes {
		keep[p.Name] = true
	}
	out := make([]terminal.PaneSnapshot, 0, len(spec.Panes))
	for _, s := range all {
		if keep[trimSpinner(s.Title)] {
			out = append(out, s)
		}
	}
	return out, nil
}

// ─── implementation ──────────────────────────────────────────────────────

func spawnAndPrompt(ctx context.Context, spec *Spec, pane PaneDef, fleetID string) error {
	cmd, err := buildCommand(pane)
	if err != nil {
		return fmt.Errorf("pane %q: build cmd: %v", pane.Name, err)
	}
	// Inject fleet identity so the worker's `qai report` knows where
	// to write. POSIX shells parse `KEY=val cmd` as a one-shot env-var
	// assignment scoped to the command; this is safer than `export`.
	cmd = fmt.Sprintf("QAI_FLEET_ID=%s QAI_FLEET_PANE=%s %s",
		shellQuote(fleetID), shellQuote(pane.Name), cmd)

	if _, err := terminal.Spawn(terminal.SpawnOpts{
		Name: pane.Name,
		Cwd:  pane.Cwd,
		Cmd:  cmd,
	}); err != nil {
		return fmt.Errorf("pane %q: spawn: %v", pane.Name, err)
	}

	// Wait for the agent's REPL. We assume claude here — the only
	// supported agent in v1; AgentKind in ready.go is the place to plug
	// in others later.
	if err := WaitForPrompt(pane.Name, matcherFor(AgentClaude), spec.EffectiveStartupTimeout()); err != nil {
		return fmt.Errorf("pane %q: %v", pane.Name, err)
	}

	if pane.WaitFor != "" {
		if err := waitForCtx(ctx, pane.WaitFor, defaultWaitForTimeout); err != nil {
			return fmt.Errorf("pane %q: wait_for %q: %v", pane.Name, pane.WaitFor, err)
		}
	}

	prompt := pane.Prompt + spec.Defaults.Reporting.PromptBlock()
	if err := terminal.Send(pane.Name, prompt, true); err != nil {
		return fmt.Errorf("pane %q: send: %v", pane.Name, err)
	}
	return nil
}

// buildCommand assembles the shell-line that gets sent to a fresh shell
// to launch the agent. Agent.Cmd + (--resume <uuid>) + Agent.Args.
func buildCommand(p PaneDef) (string, error) {
	parts := []string{p.Agent.Cmd}
	if p.Agent.Kind == "resume" {
		sessionID := p.Agent.Session
		if strings.HasPrefix(sessionID, "@") {
			resolved, err := ResolveAlias(sessionID, p.Cwd)
			if err != nil {
				return "", err
			}
			sessionID = resolved
		}
		parts = append(parts, "--resume", sessionID)
	}
	parts = append(parts, p.Agent.Args...)
	return joinShell(parts), nil
}

// joinShell single-quotes any token containing whitespace or shell
// metacharacters. We control most of these strings (cmd, --resume <uuid>,
// args from a YAML the user wrote) — this is correctness, not a hostile-
// input filter.
func joinShell(parts []string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = shellQuote(p)
	}
	return strings.Join(out, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\n\"'\\$`*?<>|&;()[]{}") {
		// single-quote and escape any embedded single quotes.
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// waitForCtx wraps WaitForFile with context cancellation so a parallel
// failure can unblock waiters quickly.
func waitForCtx(ctx context.Context, path string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- WaitForFile(path, timeout) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func trimSpinner(s string) string {
	// claude prepends one of these spinner runes once it's running.
	for _, r := range []string{"⠂ ", "⠐ ", "⠂", "⠐", "⠁ ", "⠁", "✳ ", "✳", "⏺ ", "⏺", "✽ ", "✽"} {
		s = strings.TrimPrefix(s, r)
	}
	return strings.TrimSpace(s)
}
