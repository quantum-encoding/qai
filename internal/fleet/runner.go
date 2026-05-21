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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

	// Two-phase bringup.
	//
	// Phase 1 — SERIAL spawn. Concurrent split-window races tmux past
	// ~7-8 panes (split-window picks the active pane to split, and
	// 15 goroutines all racing on "the active pane" produces "exit
	// status 1" failures after a handful of splits). Doing them one
	// at a time, with the per-spawn retile rebalancing in between,
	// matches the behaviour of running `qai term spawn` 15 times
	// from a shell — which works.
	//
	// Phase 2 — PARALLEL ready+prompt. Once panes exist, waiting on
	// their REPLs and sending prompts is per-pane and doesn't touch
	// shared tmux state. Run them concurrently for speed.
	//
	// Failure model: a single pane failing to spawn doesn't abort
	// the rest. Log it and continue — surviving panes still get
	// prompts and the user can retry the failed ones later.
	type spawnedPane struct {
		def    PaneDef
		paneID string
	}
	var spawned []spawnedPane
	panes := make(map[string]string, len(spec.Panes))

	for _, pane := range spec.Panes {
		paneID, err := spawnOnly(spec, pane, fleetID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai fleet: pane %q failed to spawn: %v (continuing)\n", pane.Name, err)
			continue
		}
		spawned = append(spawned, spawnedPane{pane, paneID})
		panes[pane.Name] = paneID
	}

	// Persist mapping before phase 2 so partial state is recoverable.
	if perr := writePaneMap(fleetID, panes); perr != nil {
		fmt.Fprintf(os.Stderr, "qai fleet: persist pane map: %v\n", perr)
	}

	if len(spawned) == 0 {
		return fleetID, fmt.Errorf("no panes spawned")
	}
	if len(spawned) < len(spec.Panes) {
		fmt.Fprintf(os.Stderr, "qai fleet: %d/%d panes spawned, proceeding with survivors\n",
			len(spawned), len(spec.Panes))
	}

	// Phase 2a: parallel wait for each pane's REPL to be ready.
	// Phase 2b: serial prompt send. The send path uses tmux's GLOBAL
	// clipboard buffer (load-buffer / paste-buffer) — concurrent
	// goroutines stomp each other's buffer between the load and the
	// paste, so most panes receive the wrong prompt or none at all.
	// Sending one at a time avoids that and looks like a clean wave
	// of activations on video.
	type readyPane struct {
		def     PaneDef
		paneID  string
		readyOK bool
		readyErr error
	}
	results := make([]readyPane, len(spawned))
	g, gctx := errgroup.WithContext(ctx)
	for i, sp := range spawned {
		i, sp := i, sp
		results[i] = readyPane{def: sp.def, paneID: sp.paneID}
		g.Go(func() error {
			if err := WaitForPrompt(sp.paneID, matcherFor(AgentClaude), spec.EffectiveStartupTimeout()); err != nil {
				results[i].readyErr = err
				return nil // log, don't fail the group
			}
			if sp.def.WaitFor != "" {
				if err := waitForCtx(gctx, sp.def.WaitFor, defaultWaitForTimeout); err != nil {
					results[i].readyErr = err
					return nil
				}
			}
			results[i].readyOK = true
			return nil
		})
	}
	_ = g.Wait()

	// Serial prompt-send wave. ~1.5s per pane (paste + 500ms + Enter
	// + 500ms + Enter), ~22s total for 15 — fine for the demo, and
	// the cadence reads cleanly on video.
	for _, r := range results {
		if !r.readyOK {
			if r.readyErr != nil {
				fmt.Fprintf(os.Stderr, "qai fleet: pane %q (%s) not ready: %v\n", r.def.Name, r.paneID, r.readyErr)
			}
			continue
		}
		prompt := r.def.Prompt + spec.Defaults.Reporting.PromptBlock()
		if err := terminal.Send(r.paneID, prompt, true); err != nil {
			fmt.Fprintf(os.Stderr, "qai fleet: pane %q (%s) send: %v\n", r.def.Name, r.paneID, err)
		}
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
	cmd.SysProcAttr = detachAttr()
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
	if err := sigTerm(pid); err != nil {
		// Stale pidfile or insufficient perms — both are "we did our
		// best." The notifier removes the pidfile on clean exit.
		_ = os.Remove(path)
		return nil
	}
	return nil
}

// Down kills the panes named in the spec by their persisted tmux ids.
// Panes not currently alive are silently skipped (already-down is the
// goal anyway). Falls back to name-lookup for entries missing from the
// map (e.g. a fleet brought up by an older runner).
func Down(spec *Spec) error {
	fleetID, err := ResolveActiveFleet()
	if err != nil {
		return fmt.Errorf("down: %v", err)
	}
	mapping, _ := readPaneMap(fleetID)

	var failures []string
	for _, pane := range spec.Panes {
		target := pane.Name
		if id, ok := mapping[pane.Name]; ok {
			target = id
		}
		if err := terminal.Close(target, true); err != nil {
			if !strings.Contains(err.Error(), "not found") {
				failures = append(failures, fmt.Sprintf("%s: %v", pane.Name, err))
			}
		}
	}
	// Stop the notifier daemon and clear the active-fleet pointer so the
	// next session starts clean. Stale pointers (recorded architect-pane
	// from a tmux session that no longer exists) confused conductor MCP
	// role-detection after every reboot — fix the disease at teardown.
	_ = stopNotifier(fleetID)
	if err := ClearActiveFleet(); err != nil {
		failures = append(failures, fmt.Sprintf("clear active pointer: %v", err))
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

// Status enumerates which manifest panes are alive vs missing. Looks
// up by persisted tmux id (Claude Code rewrites titles, making name
// lookup unreliable).
func Status(spec *Spec) ([]PaneStatus, error) {
	live, err := terminal.List()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]terminal.Pane, len(live))
	for _, p := range live {
		byID[p.ID] = p
	}

	fleetID, _ := ResolveActiveFleet()
	mapping, _ := readPaneMap(fleetID)

	out := make([]PaneStatus, 0, len(spec.Panes))
	for _, def := range spec.Panes {
		paneID, ok := mapping[def.Name]
		if !ok {
			out = append(out, PaneStatus{Name: def.Name, Alive: false, Cwd: def.Cwd})
			continue
		}
		if p, alive := byID[paneID]; alive {
			out = append(out, PaneStatus{
				Name: def.Name, Alive: true, ID: paneID, Cwd: p.Cwd, Cmd: p.Cmd,
			})
		} else {
			out = append(out, PaneStatus{Name: def.Name, Alive: false, ID: paneID, Cwd: def.Cwd})
		}
	}
	return out, nil
}

// Snapshot returns a tail of every manifest pane's recent output, in
// manifest order. Looks up by persisted pane id and re-labels with the
// manifest name (otherwise every snapshot title would be "Claude Code").
func Snapshot(spec *Spec) ([]terminal.PaneSnapshot, error) {
	all, err := terminal.Snapshot()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]terminal.PaneSnapshot, len(all))
	for _, s := range all {
		byID[s.ID] = s
	}

	fleetID, _ := ResolveActiveFleet()
	mapping, _ := readPaneMap(fleetID)

	out := make([]terminal.PaneSnapshot, 0, len(spec.Panes))
	for _, def := range spec.Panes {
		paneID, ok := mapping[def.Name]
		if !ok {
			continue
		}
		if s, alive := byID[paneID]; alive {
			s.Title = def.Name + " (" + paneID + ")"
			out = append(out, s)
		}
	}
	return out, nil
}

// ─── implementation ──────────────────────────────────────────────────────

// spawnOnly creates the tmux pane and returns its id. Caller is expected
// to hold a mutex around this so the tmux server isn't asked to split N
// times in parallel — past ~8 simultaneous splits, tmux's layout
// rebalancing races and refuses some.
//
// We deliberately skip the per-spawn `select-layout tiled` here; one
// final retile after all spawns is enough and side-steps the race.
func spawnOnly(spec *Spec, pane PaneDef, fleetID string) (string, error) {
	cmd, err := buildCommand(pane)
	if err != nil {
		return "", fmt.Errorf("pane %q: build cmd: %v", pane.Name, err)
	}
	// Inject fleet identity so the worker's `qai report` knows where
	// to write. POSIX shells parse `KEY=val cmd` as a one-shot env-var
	// assignment scoped to the command; this is safer than `export`.
	cmd = fmt.Sprintf("QAI_FLEET_ID=%s QAI_FLEET_PANE=%s %s",
		shellQuote(fleetID), shellQuote(pane.Name), cmd)

	paneID, err := terminal.Spawn(terminal.SpawnOpts{
		Name: pane.Name,
		Cwd:  pane.Cwd,
		Cmd:  cmd,
	})
	if err != nil {
		return "", fmt.Errorf("pane %q: spawn: %v", pane.Name, err)
	}
	return paneID, nil
}

// readyAndPrompt waits for the agent's REPL, optionally waits on a
// wait_for path, and sends the prompt. Address by paneID — Claude Code
// rewrites the title once it takes over.
func readyAndPrompt(ctx context.Context, spec *Spec, pane PaneDef, paneID string) error {
	if err := WaitForPrompt(paneID, matcherFor(AgentClaude), spec.EffectiveStartupTimeout()); err != nil {
		return fmt.Errorf("pane %q (%s): %v", pane.Name, paneID, err)
	}
	if pane.WaitFor != "" {
		if err := waitForCtx(ctx, pane.WaitFor, defaultWaitForTimeout); err != nil {
			return fmt.Errorf("pane %q (%s): wait_for %q: %v", pane.Name, paneID, pane.WaitFor, err)
		}
	}
	prompt := pane.Prompt + spec.Defaults.Reporting.PromptBlock()
	if err := terminal.Send(paneID, prompt, true); err != nil {
		return fmt.Errorf("pane %q (%s): send: %v", pane.Name, paneID, err)
	}
	return nil
}


// writePaneMap persists the manifest-name → tmux-pane-id mapping for a
// fleet. Read by Status / Snapshot / Down to address panes by id even
// after Claude Code has rewritten their titles.
func writePaneMap(fleetID string, panes map[string]string) error {
	if _, err := EnsureFleetDir(fleetID); err != nil {
		return err
	}
	path := filepath.Join(FleetDir(fleetID), "panes.json")
	data, err := json.MarshalIndent(panes, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readPaneMap loads the persisted mapping. Returns (nil, nil) if no
// fleet has been brought up yet (file missing).
func readPaneMap(fleetID string) (map[string]string, error) {
	path := filepath.Join(FleetDir(fleetID), "panes.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
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
