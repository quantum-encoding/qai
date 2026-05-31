package joplinbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// ─── tail fixtures ──────────────────────────────────────────────────────────

// fakeJoplinEvents extends the per-event-only surface tail needs.
// Used for tests that don't need the full sync fixture.
type fakeJoplinEvents struct {
	*fakeJoplin
	batches      [][]joplin.Event
	batchIdx     int
	cursorReturn string
	// failOnPoll, if >0, returns a transient error on the Nth poll.
	failOnPoll int
	polls      int
	// staleCursorErr, if non-nil, is returned on the next GetEvents call.
	staleCursorErr error
}

func (f *fakeJoplinEvents) GetEvents(cursor string, _ int) (*joplin.EventsResponse, error) {
	f.polls++
	if f.staleCursorErr != nil {
		err := f.staleCursorErr
		f.staleCursorErr = nil
		return nil, err
	}
	if f.failOnPoll > 0 && f.polls == f.failOnPoll {
		return nil, errors.New("simulated network error")
	}
	if f.batchIdx >= len(f.batches) {
		return &joplin.EventsResponse{Cursor: f.cursorReturn}, nil
	}
	batch := f.batches[f.batchIdx]
	f.batchIdx++
	last := f.cursorReturn
	if len(batch) > 0 {
		last = fmt.Sprintf("%d", batch[len(batch)-1].ID)
	}
	return &joplin.EventsResponse{Items: batch, Cursor: last}, nil
}

// ─── stateful fakeSurreal extension for tail tests ─────────────────────────
//
// The Stage 1 fakeSurreal records every statement string but the apply
// layer (loadPriorEdges) now ISSUES queries whose results matter — we
// need a fake that simulates the prior-edges SELECT and the recordExists
// SELECT. Extended below; the Stage 1 tests still use the simpler form.

// priorEdgesFixture is the in-memory graph state for loadPriorEdges
// to read against. Tail tests populate this before each Run() call.
type priorEdgesFixture struct {
	// parentByNote: noteJID → notebookJID currently in the contains edge.
	parentByNote map[string]string
	// tagsByNote: noteJID → tag JIDs currently in has_tag edges.
	tagsByNote map[string][]string
	// notebookExists: notebookJID → true if the notebook row is in the graph.
	notebookExists map[string]bool
}

func newTailSurreal(fixture *priorEdgesFixture) *tailSurreal {
	if fixture == nil {
		fixture = &priorEdgesFixture{}
	}
	return &tailSurreal{
		fakeSurreal: &fakeSurreal{},
		fixture:     fixture,
	}
}

type tailSurreal struct {
	*fakeSurreal
	fixture    *priorEdgesFixture
	cursorRow  *string // simulated bridge_state.cursor read at startup
	stateRow   *bridgeStateTailFields
	statePolls int
}

// bridgeStateTailFields is the subset of bridge_state the tail
// state-update parser maintains. Stage 1's fakeSurreal had a similar
// shape (BootstrapProgress / LastError) — we extend rather than
// replace so Stage 1 tests keep working.
type bridgeStateTailFields struct {
	LastPollAt    *string
	LastEventAt   *string
	TailState     *string
	Cursor        *string
	EventsApplied int
	LastError     *string
}

// Exec layers tail-specific behaviour on top of Stage 1's fakeSurreal:
//   1. Simulates the prior-edges SELECT.
//   2. Simulates the notebook-exists SELECT.
//   3. Reads the cursor at startup.
//   4. Captures bridge_state.last_poll_at / last_event_at updates.
func (t *tailSurreal) Exec(sql string) ([]blast.StatementResult, error) {
	t.fakeSurreal.execCount++
	t.fakeSurreal.stmts = append(t.fakeSurreal.stmts, sql)

	// 1. Prior edges — two-statement query loadPriorEdges issues.
	if strings.Contains(sql, "SELECT VALUE in FROM contains WHERE out = note:`") {
		return t.servePriorEdges(sql), nil
	}

	// 2. Notebook existence check from recordExists.
	if strings.Contains(sql, "SELECT id FROM notebook:`") {
		return t.serveNotebookExists(sql), nil
	}

	// 3. cursor read at tail startup.
	if strings.Contains(sql, "SELECT cursor FROM bridge_state:current") {
		t.statePolls++
		if t.cursorRow == nil {
			return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}, nil
		}
		raw, _ := json.Marshal([]map[string]any{{"cursor": *t.cursorRow}})
		return []blast.StatementResult{{Status: "OK", Result: raw}}, nil
	}

	// 4. State updates — capture last_poll_at / last_event_at /
	//    tail_state / cursor for assertions.
	if strings.Contains(sql, "bridge_state:current") {
		t.captureStateUpdate(sql)
	}

	return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`null`)}}, nil
}

func (t *tailSurreal) servePriorEdges(sql string) []blast.StatementResult {
	// Both halves of the loadPriorEdges sql have the note id embedded
	// after `out = note:` and `in = note:`. Cut the first.
	noteID := extractNoteID(sql)
	parent := t.fixture.parentByNote[noteID]
	tags := t.fixture.tagsByNote[noteID]

	parentJSON := json.RawMessage(`[]`)
	if parent != "" {
		raw, _ := json.Marshal([]string{"notebook:`" + parent + "`"})
		parentJSON = raw
	}
	tagJSONs := make([]string, len(tags))
	for i, tag := range tags {
		tagJSONs[i] = "tag:`" + tag + "`"
	}
	tagJSON, _ := json.Marshal(tagJSONs)

	return []blast.StatementResult{
		{Status: "OK", Result: parentJSON},
		{Status: "OK", Result: tagJSON},
	}
}

func (t *tailSurreal) serveNotebookExists(sql string) []blast.StatementResult {
	id := extractNotebookID(sql)
	if t.fixture.notebookExists[id] {
		raw, _ := json.Marshal([]map[string]any{{"id": "notebook:`" + id + "`"}})
		return []blast.StatementResult{{Status: "OK", Result: raw}}
	}
	return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}
}

func (t *tailSurreal) captureStateUpdate(sql string) {
	if t.stateRow == nil {
		t.stateRow = &bridgeStateTailFields{}
	}
	if strings.Contains(sql, "last_poll_at = <datetime>") {
		v := scanDatetime(sql, "last_poll_at = <datetime>")
		t.stateRow.LastPollAt = &v
	}
	if strings.Contains(sql, "last_event_at = <datetime>") {
		v := scanDatetime(sql, "last_event_at = <datetime>")
		t.stateRow.LastEventAt = &v
	}
	if strings.Contains(sql, "tail_state = ") {
		v := scanQuoted(sql, "tail_state = ")
		t.stateRow.TailState = &v
	}
	if strings.Contains(sql, "cursor = ") && !strings.Contains(sql, "SELECT cursor") {
		v := scanQuoted(sql, "cursor = ")
		t.stateRow.Cursor = &v
	}
	if strings.Contains(sql, "last_error = ") && !strings.Contains(sql, "NONE") {
		v := scanQuoted(sql, "last_error = ")
		t.stateRow.LastError = &v
	}
	// events_applied counter
	if strings.Contains(sql, "events_applied = (events_applied ?? 0) + ") {
		// Cheap parse of the integer increment.
		t.stateRow.EventsApplied += scanInt(sql, "events_applied = (events_applied ?? 0) + ")
	}
}

func extractNoteID(sql string) string {
	_, rest, ok := strings.Cut(sql, "note:`")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "`")
	if !ok {
		return ""
	}
	return id
}

func extractNotebookID(sql string) string {
	_, rest, ok := strings.Cut(sql, "notebook:`")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "`")
	if !ok {
		return ""
	}
	return id
}

// ─── tests ─────────────────────────────────────────────────────────────────

// TestTailRefusesOnNullCursor — acceptance criterion 1.
func TestTailRefusesOnNullCursor(t *testing.T) {
	s := newTailSurreal(nil)
	// Default cursorRow is nil → SELECT returns empty → tail must
	// refuse with the re-bootstrap message.
	tail := newTestTail(&fakeJoplinEvents{fakeJoplin: emptyFakeJoplin()}, s)
	err := tail.Run()
	if err == nil {
		t.Fatal("expected error on null cursor, got nil")
	}
	if !strings.Contains(err.Error(), "run 'qai joplin bridge sync' first") {
		t.Errorf("error doesn't mention sync bootstrap: %v", err)
	}
}

// TestTailHeartbeatAdvancesOnEmptyPoll — acceptance criterion 5.
// The load-bearing invariant: last_poll_at MUST advance on every
// successful poll, even one with zero events. last_event_at MUST NOT.
func TestTailHeartbeatAdvancesOnEmptyPoll(t *testing.T) {
	s := newTailSurreal(nil)
	cursor := "100"
	s.cursorRow = &cursor

	j := &fakeJoplinEvents{
		fakeJoplin:   emptyFakeJoplin(),
		batches:      [][]joplin.Event{{}}, // empty event batch
		cursorReturn: "100",                // no advance
	}
	tail := newTestTail(j, s)
	tail.Once = true
	if err := tail.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if s.stateRow == nil || s.stateRow.LastPollAt == nil {
		t.Fatal("heartbeat broken: last_poll_at was never written")
	}
	if s.stateRow.LastEventAt != nil {
		t.Errorf("invariant broken: last_event_at advanced on empty poll: %v", *s.stateRow.LastEventAt)
	}
}

// TestTailAppliesCreatedNote — acceptance criterion 2 (apply + edges).
func TestTailAppliesCreatedNote(t *testing.T) {
	s := newTailSurreal(&priorEdgesFixture{
		notebookExists: map[string]bool{"folder1": true},
	})
	cursor := "100"
	s.cursorRow = &cursor

	noteID := "n0000000000000000000000000000001"
	tagID := "t0000000000000000000000000000001"
	fj := emptyFakeJoplin()
	fj.notesByID[noteID] = joplin.Note{
		ID:              noteID,
		Title:           "Hello",
		Body:            "Hello world.",
		ParentID:        "folder1",
		UserCreatedTime: 1700000000000,
		UserUpdatedTime: 1700000100000,
	}
	fj.tagByID[tagID] = joplin.Tag{ID: tagID, Title: "decision"}
	fj.noteTags[noteID] = []string{tagID}

	j := &fakeJoplinEvents{
		fakeJoplin: fj,
		batches: [][]joplin.Event{{
			{ID: 101, ItemType: 1, ItemID: noteID, EventType: 1, CreatedTime: 1700000100000},
		}},
		cursorReturn: "101",
	}
	tail := newTestTail(j, s)
	tail.Once = true
	if err := tail.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Note row UPSERT, contains edge via INSERT RELATION, tag UPSERT,
	// has_tag edge via INSERT RELATION must all appear.
	assertAnyStmtContains(t, s.stmts, "UPSERT note:`"+noteID+"`")
	assertAnyStmtContains(t, s.stmts, "INSERT RELATION INTO contains { id: contains:`folder1_"+noteID+"`")
	assertAnyStmtContains(t, s.stmts, "UPSERT tag:`"+tagID+"`")
	assertAnyStmtContains(t, s.stmts, "INSERT RELATION INTO has_tag { id: has_tag:`"+noteID+"_"+tagID+"`")
	// last_event_at must advance now that we did apply something.
	if s.stateRow == nil || s.stateRow.LastEventAt == nil {
		t.Error("apply happened but last_event_at didn't advance")
	}
}

// TestTailReconcilesStaleTagAndParent — acceptance criterion 2.
// Verifies edge reconciliation removes the stale entries.
func TestTailReconcilesStaleTagAndParent(t *testing.T) {
	noteID := "n0000000000000000000000000000001"
	oldParent := "folder_old"
	oldTag := "tag_old"
	newParent := "folder_new"
	newTag := "tag_new"

	s := newTailSurreal(&priorEdgesFixture{
		notebookExists: map[string]bool{oldParent: true, newParent: true},
		parentByNote:   map[string]string{noteID: oldParent},
		tagsByNote:     map[string][]string{noteID: {oldTag}},
	})
	cursor := "100"
	s.cursorRow = &cursor

	fj := emptyFakeJoplin()
	fj.notesByID[noteID] = joplin.Note{
		ID:       noteID,
		Title:    "Moved + retagged",
		ParentID: newParent,
	}
	fj.tagByID[newTag] = joplin.Tag{ID: newTag, Title: "research"}
	fj.noteTags[noteID] = []string{newTag}

	j := &fakeJoplinEvents{
		fakeJoplin: fj,
		batches: [][]joplin.Event{{
			{ID: 101, ItemType: 1, ItemID: noteID, EventType: 2, CreatedTime: 1700000100000},
		}},
	}
	tail := newTestTail(j, s)
	tail.Once = true
	if err := tail.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Old parent's contains edge MUST be deleted.
	assertAnyStmtContains(t, s.stmts, "DELETE contains:`"+oldParent+"_"+noteID+"`")
	// New parent's contains edge MUST be created.
	assertAnyStmtContains(t, s.stmts, "INSERT RELATION INTO contains { id: contains:`"+newParent+"_"+noteID+"`")
	// Old tag's has_tag edge MUST be deleted.
	assertAnyStmtContains(t, s.stmts, "DELETE has_tag:`"+noteID+"_"+oldTag+"`")
	// New tag's has_tag edge MUST be created.
	assertAnyStmtContains(t, s.stmts, "INSERT RELATION INTO has_tag { id: has_tag:`"+noteID+"_"+newTag+"`")
}

// TestTailLazyNotebookMaterialisation — acceptance criterion 4.
// A note event whose parent_id isn't in the graph: tail must fetch the
// folder from Joplin and UPSERT it before relating contains.
func TestTailLazyNotebookMaterialisation(t *testing.T) {
	noteID := "n0000000000000000000000000000001"
	unknownFolder := "folder_brand_new"

	s := newTailSurreal(&priorEdgesFixture{
		notebookExists: map[string]bool{}, // folder NOT present
	})
	cursor := "100"
	s.cursorRow = &cursor

	fj := emptyFakeJoplin()
	fj.notesByID[noteID] = joplin.Note{
		ID:       noteID,
		Title:    "First note in new folder",
		ParentID: unknownFolder,
	}
	fj.foldersByID[unknownFolder] = joplin.Folder{
		ID:    unknownFolder,
		Title: "Brand New",
	}
	j := &fakeJoplinEvents{
		fakeJoplin: fj,
		batches: [][]joplin.Event{{
			{ID: 101, ItemType: 1, ItemID: noteID, EventType: 1, CreatedTime: 1700000100000},
		}},
	}
	tail := newTestTail(j, s)
	tail.Once = true
	if err := tail.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The unknown folder must have been materialised before the
	// contains edge.
	notebookIdx := stmtIndexOf(s.stmts, "UPSERT notebook:`"+unknownFolder+"`")
	containsIdx := stmtIndexOf(s.stmts, "INSERT RELATION INTO contains { id: contains:`"+unknownFolder+"_"+noteID+"`")
	if notebookIdx < 0 {
		t.Error("notebook was not lazily materialised")
	}
	if containsIdx < 0 {
		t.Error("contains edge was not created")
	}
	if notebookIdx >= 0 && containsIdx >= 0 && notebookIdx > containsIdx {
		t.Errorf("notebook materialised AFTER contains edge (idx %d > %d) — ordering broken",
			notebookIdx, containsIdx)
	}
}

// TestTailDeletedNoteRemovesAllEdges — acceptance criterion 3.
func TestTailDeletedNoteRemovesAllEdges(t *testing.T) {
	noteID := "n0000000000000000000000000000001"
	s := newTailSurreal(nil)
	cursor := "100"
	s.cursorRow = &cursor

	j := &fakeJoplinEvents{
		fakeJoplin: emptyFakeJoplin(),
		batches: [][]joplin.Event{{
			{ID: 101, ItemType: 1, ItemID: noteID, EventType: 3, CreatedTime: 1700000100000},
		}},
	}
	tail := newTestTail(j, s)
	tail.Once = true
	if err := tail.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	noteRec := "note:`" + noteID + "`"
	assertAnyStmtContains(t, s.stmts, "DELETE contains WHERE out = "+noteRec)
	assertAnyStmtContains(t, s.stmts, "DELETE has_tag WHERE in = "+noteRec)
	assertAnyStmtContains(t, s.stmts, "DELETE "+noteRec)
}

// TestTailStaleCursorRefuses — acceptance criterion 7.
// A cursor that Joplin rejects as stale must set tail_state=error and
// exit non-zero with the re-bootstrap message; never silently skip.
func TestTailStaleCursorRefuses(t *testing.T) {
	s := newTailSurreal(nil)
	cursor := "stale-100"
	s.cursorRow = &cursor

	j := &fakeJoplinEvents{
		fakeJoplin:     emptyFakeJoplin(),
		staleCursorErr: errors.New("joplin GET: HTTP 400: cursor invalid"),
	}
	tail := newTestTail(j, s)
	err := tail.Run()
	if err == nil {
		t.Fatal("expected error on stale cursor, got nil")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error should mention stale cursor: %v", err)
	}
	if s.stateRow == nil || s.stateRow.TailState == nil || *s.stateRow.TailState != "error" {
		t.Errorf("tail_state should be 'error', got %v", s.stateRow)
	}
	if s.stateRow.LastError == nil || !strings.Contains(*s.stateRow.LastError, "re-bootstrap") {
		t.Errorf("last_error should mention re-bootstrap, got %v", s.stateRow.LastError)
	}
}

// TestTailGracefulShutdown — acceptance criterion 8.
// Pre-close Stop; Run should exit cleanly with tail_state=stopped.
func TestTailGracefulShutdown(t *testing.T) {
	s := newTailSurreal(nil)
	cursor := "100"
	s.cursorRow = &cursor

	j := &fakeJoplinEvents{
		fakeJoplin:   emptyFakeJoplin(),
		batches:      [][]joplin.Event{{}, {}}, // ensure tail tries to sleep
		cursorReturn: "100",
	}
	tail := newTestTail(j, s)
	tail.Stop = make(chan struct{})
	// Trigger shutdown after first poll: replace Sleep with a closer.
	tail.Sleep = func(time.Duration) {
		close(tail.Stop)
	}
	if err := tail.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s.stateRow == nil || s.stateRow.TailState == nil || *s.stateRow.TailState != "stopped" {
		t.Errorf("tail_state should be 'stopped', got %v", s.stateRow.TailState)
	}
}

// TestTailApplyThenAdvanceCrashSafety — acceptance criterion 6.
// Simulates re-applying the same event twice (as would happen after
// a crash between apply and cursor-advance). Idempotency on edges
// (INSERT RELATION ON DUPLICATE KEY UPDATE) and rows (UPSERT) means
// the second apply is a no-op — same statement shape, no errors.
func TestTailApplyThenAdvanceCrashSafety(t *testing.T) {
	noteID := "n0000000000000000000000000000001"
	folderID := "folder1"

	// Run 1.
	s1 := newTailSurreal(&priorEdgesFixture{
		notebookExists: map[string]bool{folderID: true},
	})
	cursor := "100"
	s1.cursorRow = &cursor
	fj := emptyFakeJoplin()
	fj.notesByID[noteID] = joplin.Note{ID: noteID, Title: "X", ParentID: folderID}
	j := &fakeJoplinEvents{
		fakeJoplin: fj,
		batches: [][]joplin.Event{{
			{ID: 101, ItemType: 1, ItemID: noteID, EventType: 1, CreatedTime: 1700000100000},
		}},
	}
	tail := newTestTail(j, s1)
	tail.Once = true
	if err := tail.Run(); err != nil {
		t.Fatalf("Run1: %v", err)
	}

	// Run 2 — same fixture, simulating crash-restart: cursor was NOT
	// advanced (pre-crash state), so we re-poll same event.
	s2 := newTailSurreal(&priorEdgesFixture{
		notebookExists: map[string]bool{folderID: true},
		// Note is now in the graph from run 1; surface it to prior
		// edges so reconciliation has the right starting state.
		parentByNote: map[string]string{noteID: folderID},
	})
	s2.cursorRow = &cursor
	j2 := &fakeJoplinEvents{
		fakeJoplin: fj,
		batches: [][]joplin.Event{{
			{ID: 101, ItemType: 1, ItemID: noteID, EventType: 1, CreatedTime: 1700000100000},
		}},
	}
	tail2 := newTestTail(j2, s2)
	tail2.Once = true
	if err := tail2.Run(); err != nil {
		t.Fatalf("Run2 (re-apply): %v", err)
	}
	// The same UPSERT note + INSERT RELATION should appear — second run
	// must not have flagged the duplicate.
	assertAnyStmtContains(t, s2.stmts, "UPSERT note:`"+noteID+"`")
	assertAnyStmtContains(t, s2.stmts, "INSERT RELATION INTO contains { id: contains:`"+folderID+"_"+noteID+"`")
}

// TestNoEdgeUpsertSyntax — acceptance criterion 11. Scans the package
// directory for any line emitting "UPSERT contains" / "UPSERT has_tag"
// / "UPSERT nested_in" — the wrong-shape SQL that passed unit tests
// in Stage 1 but failed the first real run. Excludes test files and
// comments.
func TestNoEdgeUpsertSyntax(t *testing.T) {
	bannedPrefixes := []string{
		"UPSERT contains",
		"UPSERT has_tag",
		"UPSERT nested_in",
	}
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for lineno, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, bad := range bannedPrefixes {
				if strings.Contains(trimmed, bad) {
					t.Errorf("%s:%d emits %q — must use stmtContains/stmtHasTag/stmtNestedIn (Surreal v3 rejects UPSERT on RELATION tables)",
						path, lineno+1, bad)
				}
			}
		}
	}
}

// TestApplyEventSkipsNonNoteItemType — defensive coverage. Confirms
// the documented behaviour: tail skips non-note item types without
// failing, so a future Joplin version that adds folder events
// doesn't crash this build.
func TestApplyEventSkipsNonNoteItemType(t *testing.T) {
	s := newTailSurreal(nil)
	cursor := "100"
	s.cursorRow = &cursor

	folderEvent := joplin.Event{ID: 101, ItemType: 2, ItemID: "any", EventType: 1, CreatedTime: 1700000100000}
	res, err := ApplyEvent(emptyFakeJoplin(), s, folderEvent)
	if err != nil {
		t.Fatalf("ApplyEvent: %v", err)
	}
	if res.Applied {
		t.Error("non-note event should not have been Applied=true")
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

func emptyFakeJoplin() *fakeJoplin {
	return &fakeJoplin{
		folders:     []joplin.Folder{},
		notes:       map[string][]joplin.Note{},
		tags:        []joplin.Tag{},
		noteTags:    map[string][]string{},
		tagByID:     map[string]joplin.Tag{},
		notesByID:   map[string]joplin.Note{},
		foldersByID: map[string]joplin.Folder{},
	}
}

func newTestTail(j joplinAPI, s surrealAPI) *Tail {
	stop := make(chan struct{})
	return &Tail{
		J:          j,
		S:          s,
		Now:        func() time.Time { return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC) },
		Logf:       func(string, ...any) {},
		Interval:   time.Millisecond,
		MaxBackoff: time.Millisecond,
		BatchLimit: 100,
		Sleep:      func(time.Duration) {},
		Stop:       stop,
	}
}

func stmtIndexOf(stmts []string, needle string) int {
	for i, s := range stmts {
		if strings.Contains(s, needle) {
			return i
		}
	}
	return -1
}
