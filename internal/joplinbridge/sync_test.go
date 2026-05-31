package joplinbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// ─── fake joplinAPI ────────────────────────────────────────────────────────

type fakeJoplin struct {
	folders []joplin.Folder
	// notes keyed by folder ID — empty key returns nothing (sync only
	// calls with a folder ID, never "" — the spec says walk per notebook).
	notes map[string][]joplin.Note
	tags  []joplin.Tag
	// noteTags maps note ID → tag IDs on that note.
	noteTags map[string][]string
	tagByID  map[string]joplin.Tag

	// cursor returned by GetEvents("", _) — sync calls this once at the
	// end to capture Stage 2's starting point.
	cursor string

	// Fault injection — set to a folder ID to make ListNotesFull fail
	// when that folder is requested. Used by the error-preservation test.
	failOnFolderID string
}

func (f *fakeJoplin) ListFolders() ([]joplin.Folder, error) { return f.folders, nil }
func (f *fakeJoplin) ListNotesFull(folderID string, _ []string) ([]joplin.Note, error) {
	if folderID == f.failOnFolderID {
		return nil, errors.New("simulated joplin failure")
	}
	return f.notes[folderID], nil
}
func (f *fakeJoplin) ListTags() ([]joplin.Tag, error) { return f.tags, nil }
func (f *fakeJoplin) GetNoteTags(noteID string) ([]joplin.Tag, error) {
	ids := f.noteTags[noteID]
	out := make([]joplin.Tag, 0, len(ids))
	for _, id := range ids {
		if t, ok := f.tagByID[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}
func (f *fakeJoplin) GetEvents(_ string, _ int) (*joplin.EventsResponse, error) {
	return &joplin.EventsResponse{Cursor: f.cursor}, nil
}

// ─── fake surrealAPI ────────────────────────────────────────────────────────

// fakeSurreal records every SQL string and returns canned responses.
// state holds the row "stored" at bridge_state:current — incrementally
// updated when statements that target it are seen, so the resume test
// can read back the checkpoint a prior Run wrote.
type fakeSurreal struct {
	stmts []string

	// state is the simulated bridge_state row. nil means "no row yet"
	// — the first-run case.
	state *bridgeState

	// failAtStatement, if >0, causes Exec to return an error when this
	// many statements have been executed (1-indexed). Used by the
	// resume test to simulate a crash partway through.
	failAtStatement int
	execCount       int
}

func (s *fakeSurreal) Exec(sql string) ([]blast.StatementResult, error) {
	s.execCount++
	s.stmts = append(s.stmts, sql)
	if s.failAtStatement > 0 && s.execCount >= s.failAtStatement {
		return nil, fmt.Errorf("simulated surreal failure at stmt %d", s.execCount)
	}

	// Mock the SELECT FROM bridge_state:current that readState issues.
	if strings.HasPrefix(strings.TrimSpace(sql), "SELECT * FROM bridge_state:current") {
		if s.state == nil {
			return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`[]`)}}, nil
		}
		// Marshal the stored state as a one-row array.
		raw, _ := json.Marshal([]*bridgeState{s.state})
		return []blast.StatementResult{{Status: "OK", Result: raw}}, nil
	}

	// Track checkpoint / completion / error UPSERTs into our simulated
	// state so a subsequent readState reflects them.
	if strings.Contains(sql, "UPSERT bridge_state:current") {
		s.applyStateUpdate(sql)
	}

	return []blast.StatementResult{{Status: "OK", Result: json.RawMessage(`null`)}}, nil
}

// applyStateUpdate parses the UPSERT bridge_state:current SQL with
// just enough regex-light awareness to update the in-memory state for
// resume tests. Not a full SQL parser — we only need to know whether
// bootstrap_progress is set or cleared, and whether last_sync_completed
// was stamped.
func (s *fakeSurreal) applyStateUpdate(sql string) {
	if s.state == nil {
		s.state = &bridgeState{}
	}
	if strings.Contains(sql, "bootstrap_progress = NONE") {
		s.state.BootstrapProgress = nil
	} else if strings.Contains(sql, "bootstrap_progress = {") {
		// Pull the embedded JSON-ish object. The actual stmtCheckpoint
		// output uses SurrealQL object syntax which isn't valid JSON, so
		// we cherry-pick the fields we need by string scanning.
		p := &bootstrapProgress{}
		p.NotebooksDone = scanInt(sql, "notebooks_done: ")
		p.NotesDone = scanInt(sql, "notes_done: ")
		p.CurrentNotebookID = scanQuoted(sql, "current_notebook_id: ")
		p.CurrentPage = scanInt(sql, "current_page: ")
		// started_at is the embedded ISO datetime — preserve verbatim
		// so the next run's parse round-trips.
		p.StartedAt = scanDatetime(sql, "started_at: <datetime>")
		s.state.BootstrapProgress = p
	}
	if strings.Contains(sql, "last_sync_completed = <datetime>") {
		v := scanDatetime(sql, "last_sync_completed = <datetime>")
		s.state.LastSyncCompleted = &v
	}
	if strings.Contains(sql, "last_error = ") && strings.Contains(sql, "UPSERT bridge_state:current SET last_error =") {
		v := scanQuoted(sql, "last_error = ")
		s.state.LastError = &v
	}
}

func scanInt(s, prefix string) int {
	_, tail, ok := strings.Cut(s, prefix)
	if !ok {
		return 0
	}
	n := 0
	for j := 0; j < len(tail); j++ {
		c := tail[j]
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func scanQuoted(s, prefix string) string {
	_, tail, ok := strings.Cut(s, prefix)
	if !ok || len(tail) < 2 || tail[0] != '"' {
		return ""
	}
	end := strings.IndexByte(tail[1:], '"')
	if end < 0 {
		return ""
	}
	return tail[1 : 1+end]
}

func scanDatetime(s, prefix string) string {
	_, tail, ok := strings.Cut(s, prefix)
	if !ok || len(tail) < 2 || tail[0] != '"' {
		return ""
	}
	end := strings.IndexByte(tail[1:], '"')
	if end < 0 {
		return ""
	}
	return tail[1 : 1+end]
}

// ─── fixture ───────────────────────────────────────────────────────────────

// fixture builds a small Joplin library with a clear parent/child
// nesting (so the nested_in edge gets exercised), two notes (one with
// tags), and one tag of each kind class (project, kind-reserved,
// concept, freeform).
func fixture() *fakeJoplin {
	tags := []joplin.Tag{
		{ID: "11111111111111111111111111111111", Title: "project:qai-cli"},
		{ID: "22222222222222222222222222222222", Title: "decision"},
		{ID: "33333333333333333333333333333333", Title: "concept:billing"},
		{ID: "44444444444444444444444444444444", Title: "research"},
	}
	tagByID := map[string]joplin.Tag{}
	for _, t := range tags {
		tagByID[t.ID] = t
	}
	return &fakeJoplin{
		folders: []joplin.Folder{
			{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Title: "qai", ParentID: ""},
			{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Title: "qai/sessions", ParentID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{ID: "cccccccccccccccccccccccccccccccc", Title: "other", ParentID: ""},
		},
		notes: map[string][]joplin.Note{
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {},
			"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": {
				{
					ID:              "n0000000000000000000000000000001",
					Title:           "Session 1",
					Body:            "Notes about the qai sync feature.",
					ParentID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					SourceURL:       "",
					UserCreatedTime: 1700000000000,
					UserUpdatedTime: 1700000100000,
				},
				{
					ID:              "n0000000000000000000000000000002",
					Title:           "Session 2",
					Body:            "Followup notes.",
					ParentID:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					UserCreatedTime: 1700001000000,
					UserUpdatedTime: 1700001100000,
				},
			},
			"cccccccccccccccccccccccccccccccc": {
				{
					ID:              "n0000000000000000000000000000003",
					Title:           "Other note",
					Body:            "Unrelated.",
					ParentID:        "cccccccccccccccccccccccccccccccc",
					UserCreatedTime: 1700002000000,
					UserUpdatedTime: 1700002100000,
				},
			},
		},
		tags:    tags,
		tagByID: tagByID,
		noteTags: map[string][]string{
			"n0000000000000000000000000000001": {
				"11111111111111111111111111111111", // project:qai-cli
				"22222222222222222222222222222222", // decision
			},
		},
		cursor: "cursor-abc123",
	}
}

func newSyncerFromFakes(j *fakeJoplin, s *fakeSurreal) *Syncer {
	return &Syncer{
		J:    j,
		S:    s,
		Now:  func() time.Time { return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC) },
		Logf: func(string, ...any) {},
	}
}

// ─── tests ─────────────────────────────────────────────────────────────────

// TestSyncBasicWriteThrough confirms the canonical happy path:
//   - schema applied
//   - bridge_state read
//   - tags upserted in one batch
//   - every notebook walked
//   - cursor + completion stamped at the end
func TestSyncBasicWriteThrough(t *testing.T) {
	j := fixture()
	s := &fakeSurreal{}
	syncer := newSyncerFromFakes(j, s)

	if err := syncer.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every notebook should produce a transaction containing its UPSERT.
	for _, f := range j.folders {
		assertAnyStmtContains(t, s.stmts, "UPSERT notebook:`"+f.ID+"`")
	}
	// Each note row should be UPSERTed once.
	for _, notes := range j.notes {
		for _, n := range notes {
			assertAnyStmtContains(t, s.stmts, "UPSERT note:`"+n.ID+"`")
			assertAnyStmtContains(t, s.stmts, "INSERT RELATION INTO contains { id: contains:`")
		}
	}
	// Tag rows should be UPSERTed once (in the pre-walk batch).
	for _, tag := range j.tags {
		assertAnyStmtContains(t, s.stmts, "UPSERT tag:`"+tag.ID+"`")
	}
	// Tag classifier should drop the project: prefix in stored title.
	assertAnyStmtContains(t, s.stmts, `title: "qai-cli"`)
	assertAnyStmtContains(t, s.stmts, `kind: "project"`)
	assertAnyStmtContains(t, s.stmts, `kind: "kind"`)
	assertAnyStmtContains(t, s.stmts, `kind: "concept"`)
	assertAnyStmtContains(t, s.stmts, `kind: "freeform"`)

	// nested_in for the qai/sessions → qai relationship.
	assertAnyStmtContains(t, s.stmts,
		"INSERT RELATION INTO nested_in { id: nested_in:`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`")

	// has_tag edges for note 1 (which has two tags).
	assertAnyStmtContains(t, s.stmts,
		"INSERT RELATION INTO has_tag { id: has_tag:`n0000000000000000000000000000001_11111111111111111111111111111111`")
	assertAnyStmtContains(t, s.stmts,
		"INSERT RELATION INTO has_tag { id: has_tag:`n0000000000000000000000000000001_22222222222222222222222222222222`")

	// Completion must stamp the cursor AND last_sync_completed.
	assertAnyStmtContains(t, s.stmts, `cursor = "cursor-abc123"`)
	assertAnyStmtContains(t, s.stmts, "last_sync_completed = <datetime>")
}

// TestSyncIdempotentSecondRun runs Run() twice back-to-back with the
// same fixture and asserts the second run emits the same statement
// shape — the no-op-on-re-run guarantee. (Statement strings may differ
// where bridge_state writes encode a now() stamp; we compare the
// notebook/note/tag/edge writes only.)
func TestSyncIdempotentSecondRun(t *testing.T) {
	j := fixture()
	s1 := &fakeSurreal{}
	if err := newSyncerFromFakes(j, s1).Run(); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Carry forward s1's state so the second run sees the cleared
	// bootstrap_progress and stamped last_sync_completed.
	s2 := &fakeSurreal{state: s1.state}
	if err := newSyncerFromFakes(j, s2).Run(); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	// Strip the UPSERT bridge_state lines (those vary with timestamps);
	// compare the remaining entity / edge writes for exact equality.
	first := filterStateStmts(s1.stmts)
	second := filterStateStmts(s2.stmts)

	if len(first) != len(second) {
		t.Fatalf("idempotency broken: first run emitted %d entity stmts, second %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("idempotency broken at stmt %d:\n  first:  %s\n  second: %s",
				i, first[i], second[i])
		}
	}
}

// TestSyncResumeFromCheckpoint primes bridge_state with a checkpoint
// halfway through the walk, then runs Run() and asserts the second run
// skips the already-done notebooks.
//
// Note: the fixture sorts folder IDs ascending, so 'a' < 'b' < 'c'.
// A checkpoint with notebooks_done=2 means "skip a, b, resume from c".
func TestSyncResumeFromCheckpoint(t *testing.T) {
	j := fixture()
	s := &fakeSurreal{
		state: &bridgeState{
			BootstrapProgress: &bootstrapProgress{
				NotebooksDone:     2,
				NotesDone:         2,
				CurrentNotebookID: "cccccccccccccccccccccccccccccccc",
				CurrentPage:       1,
				StartedAt:         "2026-05-31T11:00:00.000Z",
			},
		},
	}
	syncer := newSyncerFromFakes(j, s)
	if err := syncer.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 'a' and 'b' should have been SKIPPED — no UPSERT notebook:`aaa…`
	// or `bbb…` should appear among entity-write statements (state
	// SELECTs / UPSERTs are filtered out).
	entity := filterStateStmts(s.stmts)
	if anyContains(entity, "UPSERT notebook:`aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`") {
		t.Error("resume failed: notebook 'a' was re-walked")
	}
	if anyContains(entity, "UPSERT notebook:`bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`") {
		t.Error("resume failed: notebook 'b' was re-walked")
	}
	// 'c' SHOULD have been walked.
	if !anyContains(entity, "UPSERT notebook:`cccccccccccccccccccccccccccccccc`") {
		t.Error("resume failed: notebook 'c' was not picked up")
	}
}

// TestSyncScopedSkipsLastSyncCompleted confirms --notebook X mode never
// stamps last_sync_completed.
func TestSyncScopedSkipsLastSyncCompleted(t *testing.T) {
	j := fixture()
	s := &fakeSurreal{}
	syncer := newSyncerFromFakes(j, s)
	syncer.NotebookScope = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := syncer.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Cursor MUST still be stamped — Stage 2 starts from it regardless.
	assertAnyStmtContains(t, s.stmts, `cursor = "cursor-abc123"`)
	// last_sync_completed MUST NOT appear in any UPSERT.
	for _, stmt := range s.stmts {
		if strings.Contains(stmt, "last_sync_completed = <datetime>") {
			t.Errorf("scoped run stamped last_sync_completed: %s", stmt)
		}
	}
	// Notebook 'c' is outside scope and must NOT have been walked.
	if anyContains(s.stmts, "UPSERT notebook:`cccccccccccccccccccccccccccccccc`") {
		t.Error("scoped run included out-of-scope notebook 'c'")
	}
}

// TestSyncErrorPreservesCheckpoint forces a Joplin failure on notebook
// 'b' and asserts last_error gets written, bootstrap_progress points at
// 'b' so the next run resumes, and last_sync_completed is NOT stamped.
func TestSyncErrorPreservesCheckpoint(t *testing.T) {
	j := fixture()
	j.failOnFolderID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	s := &fakeSurreal{}
	syncer := newSyncerFromFakes(j, s)

	err := syncer.Run()
	if err == nil {
		t.Fatal("Run: expected error on failOnFolderID, got nil")
	}

	// last_error must have been recorded.
	if s.state == nil || s.state.LastError == nil || *s.state.LastError == "" {
		t.Error("error path did not write last_error")
	}

	// bootstrap_progress must point at the failing notebook for resume.
	if s.state == nil || s.state.BootstrapProgress == nil {
		t.Fatal("error path did not preserve bootstrap_progress")
	}
	if s.state.BootstrapProgress.CurrentNotebookID != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("checkpoint pointed at %q, want bbb… (the failing notebook)",
			s.state.BootstrapProgress.CurrentNotebookID)
	}

	// last_sync_completed must NOT be stamped.
	if s.state.LastSyncCompleted != nil {
		t.Errorf("error path stamped last_sync_completed: %v", *s.state.LastSyncCompleted)
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// filterStateStmts drops anything that touches bridge_state — those
// embed wall-clock timestamps and aren't part of the idempotency
// guarantee. The schema apply is also dropped (multi-statement; its
// presence/order is identical run-to-run anyway).
func filterStateStmts(stmts []string) []string {
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if strings.Contains(s, "bridge_state") {
			continue
		}
		if strings.Contains(s, "DEFINE NAMESPACE") {
			continue
		}
		out = append(out, s)
	}
	return out
}

func anyContains(stmts []string, needle string) bool {
	for _, s := range stmts {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func assertAnyStmtContains(t *testing.T, stmts []string, needle string) {
	t.Helper()
	if anyContains(stmts, needle) {
		return
	}
	t.Errorf("no emitted statement contained %q\n  stmts:\n    %s",
		needle, strings.Join(stmts, "\n    "))
}
