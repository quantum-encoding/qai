package joplinbridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SurrealQL statement builders. Pure functions — fed Joplin records,
// emit strings. Kept in their own file so the test suite can assert
// on exact output without standing up the orchestration layer.

// quote produces a SurrealQL string literal via JSON round-trip. Same
// trick as internal/patterns/patterns.go:quote — copied (not imported)
// so this package can ship without pulling the patterns surface in.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// quoteOpt emits NONE for empty strings, otherwise a JSON-quoted literal.
// Used for option<string> fields so Surreal stores NULL rather than ''.
func quoteOpt(s string) string {
	if strings.TrimSpace(s) == "" {
		return "NONE"
	}
	return quote(s)
}

// surrealDatetime emits a SurrealQL datetime literal from a Joplin
// millisecond timestamp. Joplin returns 0 for "unset"; we coerce those
// to "now" rather than NONE because the schema declares these required.
func surrealDatetime(joplinMillis int64) string {
	var t time.Time
	if joplinMillis <= 0 {
		t = epochZero
	} else {
		t = time.UnixMilli(joplinMillis).UTC()
	}
	return fmt.Sprintf("<datetime>%q", t.Format("2006-01-02T15:04:05.000Z"))
}

// epochZero is the fallback timestamp for unset Joplin fields. Stored
// as a constant rather than time.Time zero so the resulting SurrealQL
// renders as a real ISO datetime rather than '0001-01-01...'.
var epochZero = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// recordID renders `table:`<id>`` — backtick-quoting handles Joplin's
// hex IDs that may start with a digit (Surreal record IDs can't start
// with a digit without quoting). Used for every entity reference.
func recordID(table, id string) string {
	return fmt.Sprintf("%s:`%s`", table, id)
}

// edgeID is the same idea but combines two Joplin IDs into a
// deterministic edge identifier — `<in>_<out>` — so re-UPSERTing the
// same edge is a no-op rather than producing a duplicate row.
func edgeID(table, in, out string) string {
	return fmt.Sprintf("%s:`%s_%s`", table, in, out)
}

// excerpt is the leading slice of body stored on the note row so the
// agent read verb can show a preview without fetching body. Newlines
// collapse to single spaces, leading/trailing whitespace stripped.
// Bounded at 512 chars; "…" appended when truncated.
func excerpt(body string) string {
	clean := strings.Join(strings.Fields(body), " ")
	if len(clean) <= 512 {
		return clean
	}
	return clean[:511] + "…"
}

// wordCount returns the whitespace-tokenized word count. Cheap proxy
// for "how big is this note" used in the schema's word_count field.
func wordCount(body string) int {
	return len(strings.Fields(body))
}

// ---------------------------------------------------------------------------
// Node UPSERTs
// ---------------------------------------------------------------------------

// stmtNotebook emits the notebook UPSERT. parent_id is NONE at root.
func stmtNotebook(jid, title, parentID string, updatedMillis int64) string {
	return fmt.Sprintf(
		"UPSERT %s CONTENT { joplin_id: %s, title: %s, parent_id: %s, updated_at: %s }",
		recordID("notebook", jid),
		quote(jid),
		quote(title),
		quoteOpt(parentID),
		surrealDatetime(updatedMillis),
	)
}

// stmtNote emits the note UPSERT. body is consumed for excerpt + word
// count; the full body is not stored (Joplin remains the source of
// truth — Surreal is the graph index).
func stmtNote(jid, title, parentID, sourceURL, body string, createdMillis, updatedMillis int64) string {
	return fmt.Sprintf(
		"UPSERT %s CONTENT { joplin_id: %s, title: %s, excerpt: %s, parent_id: %s, source_url: %s, created_at: %s, updated_at: %s, word_count: %d }",
		recordID("note", jid),
		quote(jid),
		quote(title),
		quoteOpt(excerpt(body)),
		quote(parentID),
		quoteOpt(sourceURL),
		surrealDatetime(createdMillis),
		surrealDatetime(updatedMillis),
		wordCount(body),
	)
}

// stmtTag emits the tag UPSERT. storedTitle and kind come from ClassifyTag.
func stmtTag(jid, storedTitle, kind string) string {
	return fmt.Sprintf(
		"UPSERT %s CONTENT { joplin_id: %s, title: %s, kind: %s }",
		recordID("tag", jid),
		quote(jid),
		quote(storedTitle),
		quote(kind),
	)
}

// ---------------------------------------------------------------------------
// Edge UPSERTs — deterministic edge IDs for idempotent re-run
// ---------------------------------------------------------------------------

// stmtContains relates a notebook to a note it owns.
func stmtContains(notebookJID, noteJID string) string {
	return fmt.Sprintf(
		"UPSERT %s SET in = %s, out = %s",
		edgeID("contains", notebookJID, noteJID),
		recordID("notebook", notebookJID),
		recordID("note", noteJID),
	)
}

// stmtNestedIn relates a child notebook to its parent.
func stmtNestedIn(parentJID, childJID string) string {
	return fmt.Sprintf(
		"UPSERT %s SET in = %s, out = %s",
		edgeID("nested_in", parentJID, childJID),
		recordID("notebook", parentJID),
		recordID("notebook", childJID),
	)
}

// stmtHasTag relates a note to a tag.
func stmtHasTag(noteJID, tagJID string) string {
	return fmt.Sprintf(
		"UPSERT %s SET in = %s, out = %s",
		edgeID("has_tag", noteJID, tagJID),
		recordID("note", noteJID),
		recordID("tag", tagJID),
	)
}

// ---------------------------------------------------------------------------
// bridge_state writes
// ---------------------------------------------------------------------------
//
// bridge_state is a single-row table — we keep it that way by always
// UPSERTing onto a fixed record ID (bridge_state:current).

const bridgeStateID = "bridge_state:current"

// stmtCheckpoint writes a fresh bootstrap_progress object after a unit
// of work completes (typically one notebook finishing). started_at is
// the run's wall-clock anchor, NOT re-stamped per checkpoint, so the
// resume-check (progress != NONE) survives until clean completion
// overwrites it.
func stmtCheckpoint(notebooksDone, notesDone int, currentNotebookID string, currentPage int, startedAt time.Time) string {
	progress := fmt.Sprintf(
		"{ notebooks_done: %d, notes_done: %d, current_notebook_id: %s, current_page: %d, started_at: %s }",
		notebooksDone,
		notesDone,
		quoteOpt(currentNotebookID),
		currentPage,
		fmt.Sprintf("<datetime>%q", startedAt.UTC().Format("2006-01-02T15:04:05.000Z")),
	)
	return fmt.Sprintf("UPSERT %s SET bootstrap_progress = %s, last_error = NONE", bridgeStateID, progress)
}

// stmtCompletion is the clean-handoff write — cursor populated, progress
// cleared, last_sync_completed stamped. Use stmtCompletionScoped instead
// when --notebook X was passed (no last_sync_completed update).
func stmtCompletion(cursor string, now time.Time) string {
	return fmt.Sprintf(
		"UPSERT %s SET cursor = %s, last_event_at = %s, last_sync_completed = %s, last_error = NONE, bootstrap_progress = NONE",
		bridgeStateID,
		quote(cursor),
		fmt.Sprintf("<datetime>%q", now.UTC().Format("2006-01-02T15:04:05.000Z")),
		fmt.Sprintf("<datetime>%q", now.UTC().Format("2006-01-02T15:04:05.000Z")),
	)
}

// stmtCompletionScoped is the --notebook-X variant. Writes cursor and
// last_event_at (so Stage 2 has a valid start point even after a partial
// adoption) and clears bootstrap_progress, but DOES NOT stamp
// last_sync_completed — the library hasn't been fully synced.
func stmtCompletionScoped(cursor string, now time.Time) string {
	return fmt.Sprintf(
		"UPSERT %s SET cursor = %s, last_event_at = %s, last_error = NONE, bootstrap_progress = NONE",
		bridgeStateID,
		quote(cursor),
		fmt.Sprintf("<datetime>%q", now.UTC().Format("2006-01-02T15:04:05.000Z")),
	)
}

// stmtError records a sync failure. bootstrap_progress is intentionally
// untouched so the next run can resume from the last checkpoint.
func stmtError(msg string) string {
	return fmt.Sprintf("UPSERT %s SET last_error = %s", bridgeStateID, quote(msg))
}
