package joplinbridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// Event-apply layer for the tail daemon.
//
// Split into pure statement builders (BuildApplyNote / BuildDeleteNote)
// and the orchestrator (ApplyEvent) that does the side-effectful work
// of fetching from Joplin, querying prior edges from Surreal, and
// executing the statements. The pure builders are exhaustively
// testable; the orchestrator is exercised against fakes.

// PriorEdges captures the relevant existing graph state for a note at
// the moment we're applying an event. Pulled by the orchestrator via
// two SELECT queries; passed to BuildApplyNote so reconciliation is
// a closed-form set difference rather than a guess.
//
// NotebookID="" means the note has no contains edge yet (brand-new or
// the bootstrap missed it). TagIDs nil means no has_tag edges yet.
type PriorEdges struct {
	NotebookID string   // current parent notebook's Joplin ID, or ""
	TagIDs     []string // current set of tag Joplin IDs attached
}

// BuildApplyNote constructs the SQL for a created or updated note
// event. Pure function — the orchestrator wires prior edges and
// current tag set in; the builder emits the idempotent statement
// list.
//
// Edge reconciliation: parent moves and tag removals are encoded as
// closed-form DELETE statements on deterministic edge IDs (the same
// `<table>:<in>_<out>` pattern Stage 1 established). This is the
// load-bearing correctness step — a naive "INSERT current edges"
// leaves stale entries that would show a moved note in two notebooks
// and a removed tag still attached.
//
// All edge writes use stmtContains / stmtHasTag (the existing
// `INSERT RELATION ... ON DUPLICATE KEY UPDATE` helpers from Stage 1).
// Do not inline edge SQL here — the test fakes won't catch a wrong
// shape, but Surreal v3 SCHEMAFULL RELATION enforcement will, as it
// did when Stage 1 first ran against the real library.
func BuildApplyNote(note joplin.Note, currentTagIDs []string, prior PriorEdges) []string {
	out := make([]string, 0, 4+len(currentTagIDs))

	// Note row UPSERT. Same shape Stage 1 sync uses; idempotent.
	out = append(out, stmtNote(
		note.ID, note.Title, note.ParentID, note.SourceURL, note.Body,
		note.UserCreatedTime, note.UserUpdatedTime,
	))

	// contains-edge reconciliation: if parent moved, delete the old
	// edge by its deterministic ID. Then upsert the current edge.
	if prior.NotebookID != "" && prior.NotebookID != note.ParentID {
		out = append(out, fmt.Sprintf("DELETE %s",
			edgeID("contains", prior.NotebookID, note.ID)))
	}
	out = append(out, stmtContains(note.ParentID, note.ID))

	// has_tag-edge reconciliation: closed-form set difference.
	currentSet := setOf(currentTagIDs)
	for _, oldTag := range prior.TagIDs {
		if _, stillThere := currentSet[oldTag]; !stillThere {
			out = append(out, fmt.Sprintf("DELETE %s",
				edgeID("has_tag", note.ID, oldTag)))
		}
	}
	for _, tagID := range currentTagIDs {
		out = append(out, stmtHasTag(note.ID, tagID))
	}

	return out
}

// BuildDeleteNote emits the closed-form delete: the note row and
// every edge touching it. Uses WHERE-clause deletes on the in/out
// record links rather than deterministic-ID deletes, because the
// delete event only carries item_id — we don't know the prior parent
// or tag set without a re-query (and re-querying just to compute IDs
// for a delete is more expensive than the WHERE-scoped delete itself).
//
// Re-applying a delete is a no-op (DELETE on nonexistent records
// succeeds with empty result).
func BuildDeleteNote(noteID string) []string {
	noteRec := recordID("note", noteID)
	return []string{
		fmt.Sprintf("DELETE contains WHERE out = %s", noteRec),
		fmt.Sprintf("DELETE has_tag WHERE in = %s", noteRec),
		fmt.Sprintf("DELETE %s", noteRec),
	}
}

// setOf builds a presence map from a slice. Used by BuildApplyNote for
// the has_tag set-difference computation.
func setOf(xs []string) map[string]struct{} {
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}

// ---------------------------------------------------------------------------
// Orchestrator: ApplyEvent
// ---------------------------------------------------------------------------

// ApplyResult tells the caller what changed so it can advance the
// bridge_state fields appropriately.
//
// LastEventAt is the apply's reference timestamp (the Joplin event's
// created_time). Zero on a no-op (e.g., a delete event for a record
// we never had).
type ApplyResult struct {
	LastEventAt time.Time
	Applied     bool
}

// ApplyEvent is the side-effectful entrypoint the tail loop calls per
// event. The flow:
//
//  1. Dispatch on item_type (Stage 2 handles item_type=note only —
//     other types are recorded as "skipped" and don't halt the loop).
//  2. For create/update: fetch the note from Joplin (it may have been
//     updated again since the event fired — we always apply current
//     state). Discover prior edges from Surreal. Lazily materialise
//     the parent notebook if it doesn't exist in the graph yet. Build
//     and exec the statement list inside one BEGIN/COMMIT TRANSACTION.
//  3. For delete: skip the Joplin fetch (record is gone). Build and
//     exec the delete sequence.
//
// On any Surreal write error the caller should treat it as transient
// and let the outer loop back off — we don't try to retry from inside
// ApplyEvent because the apply-then-advance invariant lives in the
// caller, and partial retry inside this function would muddy that.
func ApplyEvent(j joplinAPI, s surrealAPI, ev joplin.Event) (ApplyResult, error) {
	if ev.ItemType != itemTypeNote {
		// Folder/tag/resource events aren't handled in Stage 2 (Joplin's
		// /events stream is notes-only on the versions we've verified;
		// other item types are present only if a future Joplin version
		// adds them — we skip rather than fail so the cursor advances).
		return ApplyResult{}, nil
	}

	switch ev.EventType {
	case eventTypeCreate, eventTypeUpdate:
		return applyCreateOrUpdate(j, s, ev)
	case eventTypeDelete:
		return applyDelete(s, ev)
	default:
		// Unknown event type — skip rather than fail. Defensive against
		// future Joplin schema additions.
		return ApplyResult{}, nil
	}
}

// Joplin enum constants — duplicated here to keep this file self-
// contained without forcing the test fake to import joplin's helpers.
const (
	itemTypeNote     = 1
	eventTypeCreate  = 1
	eventTypeUpdate  = 2
	eventTypeDelete  = 3
)

func applyCreateOrUpdate(j joplinAPI, s surrealAPI, ev joplin.Event) (ApplyResult, error) {
	// Re-fetch the note. We need body for excerpt/word_count, plus the
	// canonical title / parent_id / source_url / timestamps. Joplin's
	// GET /notes/:id supports a comma-joined fields list.
	note, err := j.GetNote(ev.ItemID,
		"id", "title", "parent_id", "body", "source_url",
		"user_created_time", "user_updated_time")
	if err != nil {
		// Note may have been deleted between event-emission and apply.
		// Joplin returns 404 in that case. Treat as no-op so the cursor
		// advances; the eventual delete event (already queued behind
		// this one) will clean up any stale graph state.
		if isNotFoundErr(err) {
			return ApplyResult{LastEventAt: eventTime(ev)}, nil
		}
		return ApplyResult{}, fmt.Errorf("fetch note %s: %w", ev.ItemID, err)
	}

	// Lazy notebook materialisation — if the parent notebook isn't in
	// the graph, fetch it from Joplin and UPSERT it. Without this step,
	// the contains edge would relate to a non-existent record, breaking
	// Stage 4 traversals (Surreal accepts the relation but graph walks
	// won't resolve through it).
	if err := ensureNotebook(j, s, note.ParentID); err != nil {
		return ApplyResult{}, fmt.Errorf("materialise notebook %s: %w", note.ParentID, err)
	}

	// Discover prior edges before constructing the apply statements,
	// so reconciliation deletes can target deterministic IDs.
	prior, err := loadPriorEdges(s, note.ID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("load prior edges: %w", err)
	}

	// Get the current tag set from Joplin. Like Stage 1 sync, this is
	// one HTTP per event — acceptable for v1.
	tags, err := j.GetNoteTags(note.ID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("fetch note tags: %w", err)
	}
	currentTagIDs := make([]string, 0, len(tags))
	for _, t := range tags {
		currentTagIDs = append(currentTagIDs, t.ID)
		// Also ensure the tag row exists. New tags created since
		// bootstrap fall under the notes-only /events scope limitation
		// — Joplin doesn't notify us about their creation, so the
		// only chance to catch them is when a note carries them in.
		if err := ensureTag(s, t); err != nil {
			return ApplyResult{}, fmt.Errorf("materialise tag %s: %w", t.ID, err)
		}
	}

	statements := BuildApplyNote(*note, currentTagIDs, prior)
	if err := execTxn(s, statements); err != nil {
		return ApplyResult{}, fmt.Errorf("apply note %s: %w", note.ID, err)
	}
	return ApplyResult{LastEventAt: eventTime(ev), Applied: true}, nil
}

func applyDelete(s surrealAPI, ev joplin.Event) (ApplyResult, error) {
	statements := BuildDeleteNote(ev.ItemID)
	if err := execTxn(s, statements); err != nil {
		return ApplyResult{}, fmt.Errorf("delete note %s: %w", ev.ItemID, err)
	}
	return ApplyResult{LastEventAt: eventTime(ev), Applied: true}, nil
}

// ---------------------------------------------------------------------------
// Side-effectful helpers used by the orchestrator
// ---------------------------------------------------------------------------

// ensureNotebook UPSERTs a notebook row if it doesn't exist in the
// graph yet. Lazy materialisation for note events whose parent_id is
// a notebook created since the last full sync.
//
// Idempotent — if the notebook already exists, the UPSERT is a no-op
// at the row level (the row's content is refreshed from Joplin, which
// is correct: a notebook rename between syncs propagates here).
func ensureNotebook(j joplinAPI, s surrealAPI, notebookID string) error {
	if notebookID == "" {
		return nil
	}
	exists, err := recordExists(s, recordID("notebook", notebookID))
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	folder, err := j.GetFolder(notebookID)
	if err != nil {
		return fmt.Errorf("fetch folder: %w", err)
	}
	stmts := []string{stmtNotebook(folder.ID, folder.Title, folder.ParentID, 0)}
	if folder.ParentID != "" {
		// Recursively ensure the parent of this notebook exists so
		// nested_in edges aren't dangling either.
		if err := ensureNotebook(j, s, folder.ParentID); err != nil {
			return err
		}
		stmts = append(stmts, stmtNestedIn(folder.ParentID, folder.ID))
	}
	return execTxn(s, stmts)
}

// ensureTag mirrors ensureNotebook for the tag table. New tags don't
// emit events on the notes-only /events scope, so the first time we
// see a tag is via a note carrying it. ClassifyTag handles the kind
// axis the same way as Stage 1's sync prewalk.
func ensureTag(s surrealAPI, t joplin.Tag) error {
	storedTitle, kind := ClassifyTag(t.Title)
	if storedTitle == "" {
		// Defensive — Joplin shouldn't allow empty-title tags but old
		// libraries occasionally have one. Skip.
		return nil
	}
	return execTxn(s, []string{stmtTag(t.ID, storedTitle, kind)})
}

// loadPriorEdges discovers the current contains-parent and has_tag set
// for a note. Used by BuildApplyNote to compute the reconciliation
// deletes.
//
// Returns zero-value PriorEdges on a brand-new note (no rows match).
func loadPriorEdges(s surrealAPI, noteID string) (PriorEdges, error) {
	noteRec := recordID("note", noteID)
	// Two SELECTs in one round trip. Result[0] is the parent (in of
	// the contains edge); Result[1] is the tags (out of has_tag edges).
	sql := fmt.Sprintf(
		"SELECT VALUE in FROM contains WHERE out = %s; SELECT VALUE out FROM has_tag WHERE in = %s;",
		noteRec, noteRec,
	)
	results, err := s.Exec(sql)
	if err != nil {
		return PriorEdges{}, err
	}
	if err := blast.FirstError(results); err != nil {
		return PriorEdges{}, err
	}
	if len(results) < 2 {
		return PriorEdges{}, fmt.Errorf("prior-edge query returned %d results, expected 2", len(results))
	}

	var prior PriorEdges
	// First result: array of record-link strings like "notebook:`<id>`"
	// — SELECT VALUE flattens to a primitive list.
	var parents []string
	if err := json.Unmarshal(results[0].Result, &parents); err == nil && len(parents) > 0 {
		prior.NotebookID = stripTable(parents[0])
	}
	var tagRecs []string
	if err := json.Unmarshal(results[1].Result, &tagRecs); err == nil {
		prior.TagIDs = make([]string, 0, len(tagRecs))
		for _, r := range tagRecs {
			prior.TagIDs = append(prior.TagIDs, stripTable(r))
		}
	}
	return prior, nil
}

// recordExists returns true if the given full record id (e.g.
// "notebook:`abc...`") has a row. Used by ensureNotebook to skip the
// Joplin fetch when the notebook is already in the graph.
func recordExists(s surrealAPI, fullID string) (bool, error) {
	// We can't directly query "SELECT * FROM <fullID>" because fullID
	// contains backticks; safest is via record::exists() function which
	// takes a thing parameter. The simpler form below works in
	// SurrealDB v3: SELECT id FROM <table>:<id-with-quoting>.
	sql := fmt.Sprintf("SELECT id FROM %s LIMIT 1;", fullID)
	results, err := s.Exec(sql)
	if err != nil {
		return false, err
	}
	if err := blast.FirstError(results); err != nil {
		return false, err
	}
	if len(results) == 0 {
		return false, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(results[0].Result, &rows); err != nil {
		return false, nil
	}
	return len(rows) > 0, nil
}

// execTxn wraps a statement list in BEGIN/COMMIT and execs. Errors
// surface from FirstError so a single failed statement aborts the
// transaction.
func execTxn(s surrealAPI, statements []string) error {
	if len(statements) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("BEGIN TRANSACTION;\n")
	for _, stmt := range statements {
		b.WriteString(stmt)
		b.WriteString(";\n")
	}
	b.WriteString("COMMIT TRANSACTION;\n")
	results, err := s.Exec(b.String())
	if err != nil {
		return err
	}
	return blast.FirstError(results)
}

// stripTable peels the "<table>:" prefix and any backtick quoting off
// a Surreal record id string. "notebook:`abc`" → "abc".
func stripTable(rec string) string {
	// Surreal returns record ids as either "table:id" or "table:⟨id⟩"
	// or (in our backtick-quoted form) "table:`id`". Handle all three.
	_, id, ok := strings.Cut(rec, ":")
	if !ok {
		return rec
	}
	id = strings.TrimPrefix(id, "`")
	id = strings.TrimSuffix(id, "`")
	id = strings.TrimPrefix(id, "⟨")
	id = strings.TrimSuffix(id, "⟩")
	return id
}

// eventTime returns the wall-clock time the Joplin event was recorded
// at, used to update bridge_state.last_event_at. Joplin stores this
// as milliseconds since epoch; we convert to UTC time.Time.
func eventTime(ev joplin.Event) time.Time {
	if ev.CreatedTime <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ev.CreatedTime).UTC()
}

// isNotFoundErr matches the shape Joplin's REST client surfaces when
// /notes/:id is gone (404). The joplin client wraps HTTP errors with
// the status code in the message — see joplin.go:getJSON.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "HTTP 404")
}
