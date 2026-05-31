package joplingraph

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/blast"
)

// SurrealQL builders for the four primary lookup arms + neighbourhood
// expansion + notebook-path resolver. Pure(ish) — the builders are
// pure string emitters; the executors take a surrealAPI seam.
//
// All operator-supplied strings flow through quote() (JSON round-trip)
// before reaching SurrealQL. The blast client does not yet expose
// $param binding so quote() is the only safe channel.

// quote produces a SurrealQL string literal via JSON round-trip.
// Copies joplinbridge.statements.go:quote (which is unexported there);
// keeping a local copy means this package can ship without the
// joplinbridge surface widening to expose it.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// recordID renders backtick-quoted record IDs for traversal queries.
// Matches the bridge's record-ID convention so a joplin_id stored
// via Stage 1's sync is queryable here.
func recordID(table, id string) string {
	return fmt.Sprintf("%s:`%s`", table, id)
}

// noteRow is the wire shape decoded from the primary lookup. Score is
// populated only on the full-text path; the project/tag paths return
// zero. ID is the full record-ID form ("note:`<jid>`") and gets
// stripped via stripTable before exposure.
type noteRow struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Excerpt    *string `json:"excerpt"`
	ParentID   string  `json:"parent_id"`
	SourceURL  *string `json:"source_url"`
	CreatedAt  string  `json:"created_at,omitempty"`
	UpdatedAt  string  `json:"updated_at,omitempty"`
	WordCount  int     `json:"word_count,omitempty"`
	Score      float64 `json:"score"`
}

// decodeNoteRows unmarshals the last statement's Result into the
// wire-shape rows. Empty result → nil slice + no error.
func decodeNoteRows(raw json.RawMessage) ([]noteRow, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []noteRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// buildPrimarySQL returns the SurrealQL for the primary lookup and the
// lookup kind. First-match-wins per the spec's resolution table.
func buildPrimarySQL(args Args) (string, LookupKind) {
	if args.Query != "" {
		return buildFullTextSQL(args.Query, args.Limit), KindFullText
	}
	if args.ProjectFlag {
		return buildProjectSQL(args.Project, args.Limit), KindProject
	}
	if args.Tag != "" && args.With != "" {
		return buildTagIntersectionSQL(args.Tag, args.With, args.Limit), KindTag
	}
	if args.Tag != "" {
		return buildTagSQL(args.Tag, args.Limit), KindTag
	}
	// Should not reach here — the dispatcher resolves --project bare
	// before runPrimaryLookup. Defensive default to empty fulltext.
	return buildFullTextSQL("", args.Limit), KindFullText
}

// buildFullTextSQL emits the BM25 search over title + excerpt. Score
// is the sum of the per-index BM25 scores (Surreal's ranker returns
// each separately; summing gives "match in both fields ranks higher",
// which is the agent-recall behaviour we want).
//
// Per-WHERE-clause @N@ references the search index by position in the
// WHERE clause, not by index name — confirmed against v3.0.5.
func buildFullTextSQL(query string, limit int) string {
	q := quote(query)
	return fmt.Sprintf(
		"SELECT id, title, excerpt, parent_id, source_url, created_at, updated_at, word_count, "+
			"(search::score(1) + search::score(2)) AS score "+
			"FROM note WHERE title @1@ %s OR excerpt @2@ %s "+
			"ORDER BY score DESC LIMIT %d;",
		q, q, limit,
	)
}

// buildProjectSQL — notes carrying the project:<name> tag. The
// classifier stores the title WITHOUT the project: prefix and the
// kind axis as 'project', so the match is on title=<name> AND
// kind='project'.
//
// Every subselect uses `SELECT VALUE` so the result is a flat list of
// IDs rather than a list of {id: ...} objects. `IN` against the
// wrapped form silently matches nothing — a v3-confirmed footgun the
// Stage 4 spec's sample SQL would have hit on first run.
func buildProjectSQL(project string, limit int) string {
	p := quote(project)
	return fmt.Sprintf(
		"SELECT id, title, excerpt, parent_id, source_url, created_at, updated_at, word_count, "+
			"0 AS score FROM note WHERE id IN ("+
			"SELECT VALUE in FROM has_tag WHERE out IN ("+
			"SELECT VALUE id FROM tag WHERE kind = 'project' AND title = %s)) "+
			"ORDER BY updated_at DESC LIMIT %d;",
		p, limit,
	)
}

// buildTagSQL — notes carrying tag with the given title regardless of
// kind. The classifier strips project:/concept: prefixes from stored
// titles, so the operator passes the stripped form ('qai-cli', not
// 'project:qai-cli').
func buildTagSQL(tag string, limit int) string {
	t := quote(tag)
	return fmt.Sprintf(
		"SELECT id, title, excerpt, parent_id, source_url, created_at, updated_at, word_count, "+
			"0 AS score FROM note WHERE id IN ("+
			"SELECT VALUE in FROM has_tag WHERE out IN ("+
			"SELECT VALUE id FROM tag WHERE title = %s)) "+
			"ORDER BY updated_at DESC LIMIT %d;",
		t, limit,
	)
}

// buildTagIntersectionSQL — notes carrying BOTH tags. Uses LET to
// avoid two passes over has_tag at query time. Surreal returns each
// LET's result as its own StatementResult; runPrimaryLookup picks the
// final SELECT off the tail of the results slice.
func buildTagIntersectionSQL(tag, with string, limit int) string {
	t := quote(tag)
	w := quote(with)
	return fmt.Sprintf(
		"LET $a = (SELECT VALUE in FROM has_tag WHERE out IN ("+
			"SELECT VALUE id FROM tag WHERE title = %s)); "+
			"LET $b = (SELECT VALUE in FROM has_tag WHERE out IN ("+
			"SELECT VALUE id FROM tag WHERE title = %s)); "+
			"SELECT id, title, excerpt, parent_id, source_url, created_at, updated_at, word_count, "+
			"0 AS score FROM note WHERE id IN $a AND id IN $b "+
			"ORDER BY updated_at DESC LIMIT %d;",
		t, w, limit,
	)
}

// ────────────────────────────────────────────────────────────────────────
// Neighbourhood expansion
// ────────────────────────────────────────────────────────────────────────

// neighbourhood is the cross-row enrichment result. Keyed by note-ID
// (the stripped form, not the record-ID) so attach helpers can look up
// per-note data without re-parsing.
type neighbourhood struct {
	NotebookByNoteID  map[string]NotebookRef
	TagsByNoteID      map[string][]TagRef
	LinksOut          map[string][]NoteRef
	LinksIn           map[string][]NoteRef
	LinkTargetNoteIDs []string // unique IDs from links_out + links_in for --hops 2
	LinkTargetNotes   []noteRow
}

// expandNeighbourhood pulls notebook + tags + links_to (both directions)
// in four SELECTs concatenated into one round-trip. Empty primary →
// zero-value neighbourhood, no error.
//
// links_to queries return zero rows today (Stage 5 will populate the
// table). The no-op-safe path is the whole point — Stage 5 needs no
// Stage 4 code change to light up.
func expandNeighbourhood(s surrealAPI, primaryIDs []string) (neighbourhood, error) {
	nbh := neighbourhood{
		NotebookByNoteID: map[string]NotebookRef{},
		TagsByNoteID:     map[string][]TagRef{},
		LinksOut:         map[string][]NoteRef{},
		LinksIn:          map[string][]NoteRef{},
	}
	if len(primaryIDs) == 0 {
		return nbh, nil
	}
	idsArr := joinIDs("note", primaryIDs)

	sql := fmt.Sprintf(
		// 0: contains edges → notebook for each primary note. We pull
		//    the edge in/out so we can key on the note ID; otherwise the
		//    notebook row alone doesn't carry the back-pointer.
		"SELECT in AS notebook_id, out AS note_id FROM contains WHERE out IN [%s];"+
			// 1: notebook details (one row per unique notebook).
			"SELECT id, title FROM notebook WHERE id IN ("+
			"SELECT VALUE in FROM contains WHERE out IN [%s]);"+
			// 2: has_tag edges + tag detail. SELECT-on-edge to keep the
			//    note-ID linkage; we'll join tag titles in a second pass.
			"SELECT in AS note_id, out AS tag_id FROM has_tag WHERE in IN [%s];"+
			// 3: tag details — title, kind.
			"SELECT id, title, kind FROM tag WHERE id IN ("+
			"SELECT VALUE out FROM has_tag WHERE in IN [%s]);"+
			// 4: links_to outbound — empty today, lights up Stage 5.
			"SELECT in AS note_id, out AS target_id FROM links_to WHERE in IN [%s];"+
			// 5: target note details for outbound links.
			"SELECT id, title FROM note WHERE id IN ("+
			"SELECT VALUE out FROM links_to WHERE in IN [%s]);"+
			// 6: links_to inbound.
			"SELECT in AS source_id, out AS note_id FROM links_to WHERE out IN [%s];"+
			// 7: source note details for inbound links.
			"SELECT id, title FROM note WHERE id IN ("+
			"SELECT VALUE in FROM links_to WHERE out IN [%s]);",
		idsArr, idsArr, idsArr, idsArr, idsArr, idsArr, idsArr, idsArr,
	)
	results, err := s.Exec(sql)
	if err != nil {
		return nbh, err
	}
	if err := blast.FirstError(results); err != nil {
		return nbh, err
	}
	if len(results) < 8 {
		return nbh, fmt.Errorf("neighbourhood: expected 8 result sets, got %d", len(results))
	}

	// ── notebook ────────────────────────────────────────────────────
	containsRows, _ := decodeEdgeRows(results[0].Result, "notebook_id", "note_id")
	notebookByID, _ := decodeNotebookRows(results[1].Result)
	for _, c := range containsRows {
		nb, ok := notebookByID[c.A]
		if !ok {
			continue
		}
		nbh.NotebookByNoteID[stripTable(c.B)] = NotebookRef{
			ID:    stripTable(nb.ID),
			Title: nb.Title,
		}
	}

	// ── tags ────────────────────────────────────────────────────────
	tagEdges, _ := decodeEdgeRows(results[2].Result, "note_id", "tag_id")
	tagByID, _ := decodeTagRows(results[3].Result)
	for _, e := range tagEdges {
		tag, ok := tagByID[e.B]
		if !ok {
			continue
		}
		noteID := stripTable(e.A)
		nbh.TagsByNoteID[noteID] = append(nbh.TagsByNoteID[noteID], TagRef{
			Title: tag.Title,
			Kind:  tag.Kind,
		})
	}

	// ── links_out + their target notes ──────────────────────────────
	outEdges, _ := decodeEdgeRows(results[4].Result, "note_id", "target_id")
	outNotes, _ := decodeNoteIDTitleRows(results[5].Result)
	for _, e := range outEdges {
		tgt, ok := outNotes[e.B]
		if !ok {
			continue
		}
		src := stripTable(e.A)
		nbh.LinksOut[src] = append(nbh.LinksOut[src], NoteRef{
			ID:    stripTable(tgt.ID),
			Title: tgt.Title,
		})
	}

	// ── links_in + their source notes ───────────────────────────────
	inEdges, _ := decodeEdgeRows(results[6].Result, "source_id", "note_id")
	inNotes, _ := decodeNoteIDTitleRows(results[7].Result)
	for _, e := range inEdges {
		src, ok := inNotes[e.A]
		if !ok {
			continue
		}
		dst := stripTable(e.B)
		nbh.LinksIn[dst] = append(nbh.LinksIn[dst], NoteRef{
			ID:    stripTable(src.ID),
			Title: src.Title,
		})
	}

	// ── link-target IDs for --hops 2 (dedup outbound + inbound) ─────
	seen := map[string]struct{}{}
	for _, refs := range nbh.LinksOut {
		for _, r := range refs {
			if _, ok := seen[r.ID]; !ok {
				seen[r.ID] = struct{}{}
				nbh.LinkTargetNoteIDs = append(nbh.LinkTargetNoteIDs, r.ID)
			}
		}
	}
	for _, refs := range nbh.LinksIn {
		for _, r := range refs {
			if _, ok := seen[r.ID]; !ok {
				seen[r.ID] = struct{}{}
				nbh.LinkTargetNoteIDs = append(nbh.LinkTargetNoteIDs, r.ID)
			}
		}
	}

	return nbh, nil
}

// expandNeighbourhoodNoLinks is the --hops 2 variant: pull notebook +
// tags for the second-pass neighbour notes WITHOUT recursing into a
// third link layer. Avoids unbounded fan-out.
func expandNeighbourhoodNoLinks(s surrealAPI, noteIDs []string) (neighbourhood, error) {
	nbh := neighbourhood{
		NotebookByNoteID: map[string]NotebookRef{},
		TagsByNoteID:     map[string][]TagRef{},
		LinksOut:         map[string][]NoteRef{},
		LinksIn:          map[string][]NoteRef{},
	}
	if len(noteIDs) == 0 {
		return nbh, nil
	}
	idsArr := joinIDs("note", noteIDs)
	sql := fmt.Sprintf(
		"SELECT in AS notebook_id, out AS note_id FROM contains WHERE out IN [%s];"+
			"SELECT id, title FROM notebook WHERE id IN ("+
			"SELECT VALUE in FROM contains WHERE out IN [%s]);"+
			"SELECT in AS note_id, out AS tag_id FROM has_tag WHERE in IN [%s];"+
			"SELECT id, title, kind FROM tag WHERE id IN ("+
			"SELECT VALUE out FROM has_tag WHERE in IN [%s]);",
		idsArr, idsArr, idsArr, idsArr,
	)
	results, err := s.Exec(sql)
	if err != nil {
		return nbh, err
	}
	if err := blast.FirstError(results); err != nil {
		return nbh, err
	}
	if len(results) < 4 {
		return nbh, fmt.Errorf("neighbourhood (hop2): expected 4 result sets, got %d", len(results))
	}

	containsRows, _ := decodeEdgeRows(results[0].Result, "notebook_id", "note_id")
	notebookByID, _ := decodeNotebookRows(results[1].Result)
	for _, c := range containsRows {
		nb, ok := notebookByID[c.A]
		if !ok {
			continue
		}
		nbh.NotebookByNoteID[stripTable(c.B)] = NotebookRef{
			ID:    stripTable(nb.ID),
			Title: nb.Title,
		}
	}
	tagEdges, _ := decodeEdgeRows(results[2].Result, "note_id", "tag_id")
	tagByID, _ := decodeTagRows(results[3].Result)
	for _, e := range tagEdges {
		tag, ok := tagByID[e.B]
		if !ok {
			continue
		}
		noteID := stripTable(e.A)
		nbh.TagsByNoteID[noteID] = append(nbh.TagsByNoteID[noteID], TagRef{
			Title: tag.Title,
			Kind:  tag.Kind,
		})
	}
	return nbh, nil
}

// selectNotesByID pulls the wire-shape note rows for a list of stripped
// note IDs. Used by the --hops 2 second pass to materialise the link
// targets into the payload.
func selectNotesByID(s surrealAPI, ids []string) ([]noteRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	sql := fmt.Sprintf(
		"SELECT id, title, excerpt, parent_id, source_url, created_at, updated_at, word_count, "+
			"0 AS score FROM note WHERE id IN [%s];",
		joinIDs("note", ids),
	)
	results, err := s.Exec(sql)
	if err != nil {
		return nil, err
	}
	if err := blast.FirstError(results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return decodeNoteRows(results[0].Result)
}

// ────────────────────────────────────────────────────────────────────────
// Notebook path resolver
// ────────────────────────────────────────────────────────────────────────

// resolveNotebookPath walks nested_in upward from the leaf notebook to
// the root, slash-joining titles. Cache is keyed on the leaf ID and
// shared across rows in the same payload — a 20-note payload all in
// 'qai/sessions' walks the chain once. Errors degrade silently to ""
// rather than fail the whole payload.
func resolveNotebookPath(s surrealAPI, leafID string, cache map[string]string) string {
	if leafID == "" {
		return ""
	}
	if p, ok := cache[leafID]; ok {
		return p
	}
	parts := []string{}
	cur := leafID
	for cur != "" {
		row, err := selectNotebookRow(s, cur)
		if err != nil || row == nil {
			break
		}
		parts = append([]string{row.Title}, parts...)
		if row.ParentID == nil || *row.ParentID == "" {
			break
		}
		cur = *row.ParentID
	}
	path := strings.Join(parts, "/")
	cache[leafID] = path
	return path
}

type notebookPathRow struct {
	Title    string  `json:"title"`
	ParentID *string `json:"parent_id"`
}

// selectNotebookRow pulls title + parent_id for one notebook by its
// stripped ID. Returns (nil, nil) when no row.
func selectNotebookRow(s surrealAPI, id string) (*notebookPathRow, error) {
	sql := fmt.Sprintf("SELECT title, parent_id FROM %s LIMIT 1;", recordID("notebook", id))
	results, err := s.Exec(sql)
	if err != nil {
		return nil, err
	}
	if err := blast.FirstError(results); err != nil {
		return nil, err
	}
	if len(results) == 0 || len(results[0].Result) == 0 {
		return nil, nil
	}
	var rows []notebookPathRow
	if err := json.Unmarshal(results[0].Result, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ────────────────────────────────────────────────────────────────────────
// shared decode helpers
// ────────────────────────────────────────────────────────────────────────

// edgeRow is the shape SELECT-on-edge queries emit when we project two
// fields. Field names are the SQL aliases ('a', 'b' here just for
// passing through json.Unmarshal); the caller provides the real field
// names at decode time.
type edgeRow struct {
	A string
	B string
}

// decodeEdgeRows decodes a JSON array of objects whose keys are
// `aField` and `bField`. Surreal returns record-link values as
// "table:id" strings; we keep them verbatim and let the caller call
// stripTable when it needs the bare ID.
func decodeEdgeRows(raw json.RawMessage, aField, bField string) ([]edgeRow, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, err
	}
	out := make([]edgeRow, 0, len(arr))
	for _, m := range arr {
		a, _ := m[aField].(string)
		b, _ := m[bField].(string)
		if a == "" && b == "" {
			continue
		}
		out = append(out, edgeRow{A: a, B: b})
	}
	return out, nil
}

type notebookDetailRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func decodeNotebookRows(raw json.RawMessage) (map[string]notebookDetailRow, error) {
	if len(raw) == 0 {
		return map[string]notebookDetailRow{}, nil
	}
	var rows []notebookDetailRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]notebookDetailRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

type tagDetailRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

func decodeTagRows(raw json.RawMessage) (map[string]tagDetailRow, error) {
	if len(raw) == 0 {
		return map[string]tagDetailRow{}, nil
	}
	var rows []tagDetailRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]tagDetailRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

type noteIDTitleRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func decodeNoteIDTitleRows(raw json.RawMessage) (map[string]noteIDTitleRow, error) {
	if len(raw) == 0 {
		return map[string]noteIDTitleRow{}, nil
	}
	var rows []noteIDTitleRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]noteIDTitleRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

// joinIDs builds the inline array literal for IN [...] clauses. Each
// ID is rendered as `table:`<id>`` to match Stage 1's record-ID
// convention. Used everywhere we need a set membership test.
func joinIDs(table string, ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, recordID(table, id))
	}
	return strings.Join(parts, ", ")
}

// stripTable peels the "<table>:" prefix and any backtick / angle
// quoting off a Surreal record id. Mirror of joplinbridge.apply.go's
// stripTable (unexported there) — duplicate to keep this package
// self-contained.
func stripTable(rec string) string {
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
