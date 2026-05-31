package joplinbridge

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

//go:embed schema.surql
var embeddedSchema string

// EmbeddedSchema is exported for `qai joplin bridge schema` to print
// the schema for inspection without applying it.
func EmbeddedSchema() string { return embeddedSchema }

// Syncer pulls a Joplin library into SurrealDB. One-shot — call Run
// once per process. Resumable via bridge_state.bootstrap_progress.
type Syncer struct {
	J joplinAPI
	S surrealAPI

	// NotebookScope, when set, restricts the walk to this Joplin folder
	// ID and all its descendants. last_sync_completed is NOT stamped on
	// clean completion in this mode — the library as a whole isn't
	// synced.
	NotebookScope string

	// Now is the clock — tests pin it to make checkpoint / completion
	// statements deterministic. Defaults to time.Now in NewSyncer.
	Now func() time.Time

	// Logf is the progress sink. Defaults to a noop in NewSyncer; the
	// CmdSync caller wires it to fmt.Printf so the TTY user sees per-
	// notebook progress without sync needing to know about io.Writer.
	Logf func(format string, args ...any)
}

// NewSyncer wraps a real joplin + blast client with sane defaults.
// Tests construct Syncer directly with fakes.
func NewSyncer(j *joplin.Client, s *blast.Client) *Syncer {
	return &Syncer{
		J:    j,
		S:    s,
		Now:  time.Now,
		Logf: func(string, ...any) {},
	}
}

// ─── bridge_state shapes (for reading existing state on startup) ───────────

type bridgeState struct {
	Cursor             *string            `json:"cursor"`
	LastEventAt        *string            `json:"last_event_at"`
	LastSyncCompleted  *string            `json:"last_sync_completed"`
	LastError          *string            `json:"last_error"`
	BootstrapProgress  *bootstrapProgress `json:"bootstrap_progress"`
}

type bootstrapProgress struct {
	NotebooksDone     int    `json:"notebooks_done"`
	NotesDone         int    `json:"notes_done"`
	CurrentNotebookID string `json:"current_notebook_id"`
	CurrentPage       int    `json:"current_page"`
	StartedAt         string `json:"started_at"`
}

// ─── orchestration ─────────────────────────────────────────────────────────

// Run performs the sync. On success returns nil; bridge_state holds the
// /events cursor for Stage 2 to pick up. On failure returns the error
// after writing it to bridge_state.last_error (preserving any in-flight
// bootstrap_progress so the next run can resume).
func (s *Syncer) Run() error {
	// 1. Apply schema (idempotent — every DEFINE has IF NOT EXISTS).
	if err := s.applySchema(); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	// 2. Read existing bridge_state, if any.
	state, err := s.readState()
	if err != nil {
		return fmt.Errorf("read bridge_state: %w", err)
	}

	// 3. Decide: resume vs fresh.
	resumeFrom := ""
	skipDone := 0
	startedAt := s.Now()
	if state != nil && state.BootstrapProgress != nil {
		resumeFrom = state.BootstrapProgress.CurrentNotebookID
		skipDone = state.BootstrapProgress.NotebooksDone
		// Preserve the original started_at so progress timestamps stay
		// coherent across restarts. Falls back to now() if the stored
		// value can't be parsed (defensive — the schema ASSERTs datetime
		// so this should never fail, but a corrupted row shouldn't break
		// resume).
		if parsed, perr := time.Parse(time.RFC3339Nano, state.BootstrapProgress.StartedAt); perr == nil {
			startedAt = parsed
		}
		s.Logf("resuming: %d notebooks done, current=%s\n", skipDone, resumeFrom[:min(8, len(resumeFrom))])
	}

	// 4. Walk folders. Determine the working set: all folders (default)
	//    or the scope subtree (--notebook X).
	allFolders, err := s.J.ListFolders()
	if err != nil {
		s.recordError(fmt.Sprintf("list folders: %v", err))
		return fmt.Errorf("list folders: %w", err)
	}
	workingSet, err := s.resolveScope(allFolders)
	if err != nil {
		s.recordError(err.Error())
		return err
	}
	// Deterministic order — resume relies on the same sort ordering
	// every run. ID-sort is stable and independent of Joplin's internal
	// list order (which may shift as new folders get added).
	sort.Slice(workingSet, func(i, j int) bool {
		return workingSet[i].ID < workingSet[j].ID
	})

	// 5. Resume handling: skip already-done notebooks if applicable.
	startIdx := 0
	if skipDone > 0 && skipDone <= len(workingSet) {
		startIdx = skipDone
		// Sanity check: if resume points at a notebook ID we no longer
		// see (deleted? scope changed?), restart from skipDone but log it.
		if resumeFrom != "" && startIdx < len(workingSet) && workingSet[startIdx].ID != resumeFrom {
			s.Logf("warn: resume cursor at %s but next notebook is %s — proceeding from index %d\n",
				resumeFrom[:min(8, len(resumeFrom))],
				workingSet[startIdx].ID[:min(8, len(workingSet[startIdx].ID))],
				startIdx)
		}
	}

	// 6. Pre-walk tags (single global pass — Joplin tags are global, not
	//    per-notebook). Tag rows must exist before any has_tag edge.
	if err := s.upsertAllTags(); err != nil {
		s.recordError(fmt.Sprintf("upsert tags: %v", err))
		return fmt.Errorf("upsert tags: %w", err)
	}

	// 7. Walk notebooks. Each notebook is a single transaction; the
	//    checkpoint is a separate Exec after the txn commits so a crash
	//    between txn-commit and checkpoint-write means we re-walk the
	//    notebook on resume (UPSERT-safe, no row duplication).
	notesDone := 0
	if state != nil && state.BootstrapProgress != nil {
		notesDone = state.BootstrapProgress.NotesDone
	}
	for i := startIdx; i < len(workingSet); i++ {
		f := workingSet[i]
		s.Logf("notebook %d/%d: %s (%s…)\n", i+1, len(workingSet), f.Title, f.ID[:min(8, len(f.ID))])

		notes, err := s.syncOneNotebook(f)
		if err != nil {
			s.recordError(fmt.Sprintf("notebook %s: %v", f.ID, err))
			// Preserve bootstrap_progress so the next run resumes here.
			s.checkpoint(i, notesDone, f.ID, 1, startedAt)
			return fmt.Errorf("sync notebook %s (%s): %w", f.Title, f.ID, err)
		}
		notesDone += notes
		// Checkpoint after each notebook completes successfully.
		if err := s.checkpoint(i+1, notesDone, f.ID, 1, startedAt); err != nil {
			s.recordError(fmt.Sprintf("checkpoint after %s: %v", f.ID, err))
			return fmt.Errorf("checkpoint: %w", err)
		}
	}

	// 8. Clean completion — fetch current /events cursor and write final
	//    bridge_state. Scoped runs skip last_sync_completed.
	resp, err := s.J.GetEvents("", 1)
	if err != nil {
		s.recordError(fmt.Sprintf("fetch events cursor: %v", err))
		return fmt.Errorf("fetch events cursor: %w", err)
	}
	cursor := resp.Cursor
	if cursor == "" {
		// Defensive: an empty cursor would make Stage 2 refuse to start.
		// Joplin always returns a cursor; treat missing as a server bug
		// rather than swallowing.
		return errors.New("joplin returned empty events cursor — Stage 2 has no starting point")
	}
	var completion string
	if s.NotebookScope != "" {
		completion = stmtCompletionScoped(cursor, s.Now())
	} else {
		completion = stmtCompletion(cursor, s.Now())
	}
	if _, err := s.S.Exec(completion + ";"); err != nil {
		return fmt.Errorf("write completion state: %w", err)
	}
	s.Logf("sync complete: %d notebooks, %d notes, cursor=%s\n",
		len(workingSet)-startIdx, notesDone, cursor[:min(12, len(cursor))])
	return nil
}

// ─── per-step helpers ──────────────────────────────────────────────────────

// applySchema applies the embedded schema. Re-running is a no-op.
func (s *Syncer) applySchema() error {
	results, err := s.S.Exec(embeddedSchema)
	if err != nil {
		return err
	}
	return blast.FirstError(results)
}

// readState reads the singleton bridge_state row. Returns (nil, nil) on
// the first-run case where the row doesn't exist yet.
func (s *Syncer) readState() (*bridgeState, error) {
	results, err := s.S.Exec("SELECT * FROM " + bridgeStateID + ";")
	if err != nil {
		return nil, err
	}
	if err := blast.FirstError(results); err != nil {
		return nil, err
	}
	if len(results) == 0 || len(results[0].Result) == 0 {
		return nil, nil
	}
	var rows []bridgeState
	if err := json.Unmarshal(results[0].Result, &rows); err != nil {
		return nil, fmt.Errorf("decode bridge_state: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// resolveScope returns the set of folders to walk. With no scope, the
// full list. With NotebookScope, the named folder plus all descendants
// (transitive).
func (s *Syncer) resolveScope(all []joplin.Folder) ([]joplin.Folder, error) {
	if s.NotebookScope == "" {
		return all, nil
	}
	// Index by parent so we can walk the subtree iteratively.
	children := make(map[string][]joplin.Folder, len(all))
	byID := make(map[string]joplin.Folder, len(all))
	for _, f := range all {
		children[f.ParentID] = append(children[f.ParentID], f)
		byID[f.ID] = f
	}
	root, ok := byID[s.NotebookScope]
	if !ok {
		return nil, fmt.Errorf("notebook scope %q not found in Joplin", s.NotebookScope)
	}
	var subtree []joplin.Folder
	queue := []joplin.Folder{root}
	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]
		subtree = append(subtree, head)
		queue = append(queue, children[head.ID]...)
	}
	return subtree, nil
}

// upsertAllTags upserts every Joplin tag in a single batch. Run once,
// before any notebook walk, so the has_tag edges in later steps always
// resolve to existing tag rows.
func (s *Syncer) upsertAllTags() error {
	tags, err := s.J.ListTags()
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("BEGIN TRANSACTION;\n")
	for _, t := range tags {
		title, kind := ClassifyTag(t.Title)
		if title == "" {
			// Defensive: a tag with an empty title would fail the NOT NONE
			// implicit constraint on title. Skip — Joplin shouldn't allow
			// these but old corrupted libraries occasionally have one.
			continue
		}
		b.WriteString(stmtTag(t.ID, title, kind))
		b.WriteString(";\n")
	}
	b.WriteString("COMMIT TRANSACTION;\n")
	results, err := s.S.Exec(b.String())
	if err != nil {
		return err
	}
	return blast.FirstError(results)
}

// syncOneNotebook walks one notebook's notes, building one transaction
// containing the notebook UPSERT, the nested_in edge (if has parent),
// every note UPSERT, every contains edge, and every has_tag edge. The
// txn is atomic: either the whole notebook lands or nothing does — so
// a mid-walk failure preserves the prior notebook's checkpoint as the
// resume point. Returns the count of notes synced.
func (s *Syncer) syncOneNotebook(f joplin.Folder) (int, error) {
	notes, err := s.J.ListNotesFull(f.ID, []string{"body", "source_url"})
	if err != nil {
		return 0, fmt.Errorf("list notes: %w", err)
	}

	var b strings.Builder
	b.WriteString("BEGIN TRANSACTION;\n")
	b.WriteString(stmtNotebook(f.ID, f.Title, f.ParentID, 0))
	b.WriteString(";\n")
	if f.ParentID != "" {
		b.WriteString(stmtNestedIn(f.ParentID, f.ID))
		b.WriteString(";\n")
	}
	for _, n := range notes {
		b.WriteString(stmtNote(n.ID, n.Title, f.ID, n.SourceURL, n.Body, n.UserCreatedTime, n.UserUpdatedTime))
		b.WriteString(";\n")
		b.WriteString(stmtContains(f.ID, n.ID))
		b.WriteString(";\n")
		// Fetch this note's tags. Joplin's /notes/{}/tags is one HTTP
		// per note — slow on large notebooks. Acceptable for v1; if it
		// becomes a bottleneck the right fix is a single bulk
		// /tags/{}/notes pass + invert, not parallelism here.
		tags, terr := s.J.GetNoteTags(n.ID)
		if terr != nil {
			return 0, fmt.Errorf("note %s tags: %w", n.ID, terr)
		}
		for _, t := range tags {
			b.WriteString(stmtHasTag(n.ID, t.ID))
			b.WriteString(";\n")
		}
	}
	b.WriteString("COMMIT TRANSACTION;\n")
	results, err := s.S.Exec(b.String())
	if err != nil {
		return 0, err
	}
	if err := blast.FirstError(results); err != nil {
		return 0, err
	}
	return len(notes), nil
}

// checkpoint writes bootstrap_progress to bridge_state. Called after a
// notebook commits cleanly; on failure (during error recovery), called
// with the in-flight notebook ID so the next run resumes there.
func (s *Syncer) checkpoint(notebooksDone, notesDone int, currentID string, currentPage int, startedAt time.Time) error {
	stmt := stmtCheckpoint(notebooksDone, notesDone, currentID, currentPage, startedAt)
	results, err := s.S.Exec(stmt + ";")
	if err != nil {
		return err
	}
	return blast.FirstError(results)
}

// recordError writes last_error to bridge_state on failure. Best-effort
// — if the error write itself fails, log and return: the caller is
// already on an error path.
func (s *Syncer) recordError(msg string) {
	_, _ = s.S.Exec(stmtError(msg) + ";")
}

