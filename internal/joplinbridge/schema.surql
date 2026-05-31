-- ============================================================================
-- joplin-bridge schema — quantumencoding / notes_graph
--
-- Stage 1 of the joplin → SurrealDB bridge. Models Joplin's notebook
-- / note / tag world as a graph so an agent can query notes by
-- project, by tag, by notebook, and (in Stage 5) by wiki-link.
--
-- Idempotent: every DEFINE uses IF NOT EXISTS. Re-applying is a no-op.
--
-- This file is applied via the qai blast HTTP client (header-driven
-- NS/DB selection — see internal/blast/client.go), which is why no
-- USE NS / USE DB statements appear here. The blast convention is
-- "header sets target, body defines schema".
--
-- BUT unlike blast_radius / patterns, this is the first qai surface
-- that may run against a fresh namespace, so DEFINE NAMESPACE and
-- DEFINE DATABASE live at the top with IF NOT EXISTS. They're root-
-- level statements; SurrealDB accepts them regardless of the request's
-- ns/db headers and they self-heal a fresh instance.
-- ============================================================================

DEFINE NAMESPACE IF NOT EXISTS quantumencoding;
DEFINE DATABASE IF NOT EXISTS notes_graph;

-- ----------------------------------------------------------------------------
-- notebook — a Joplin folder
-- ----------------------------------------------------------------------------
DEFINE TABLE IF NOT EXISTS notebook SCHEMAFULL;
  DEFINE FIELD IF NOT EXISTS joplin_id  ON notebook TYPE string;
  DEFINE INDEX IF NOT EXISTS nb_jid     ON notebook FIELDS joplin_id UNIQUE;
  DEFINE FIELD IF NOT EXISTS title      ON notebook TYPE string;
  DEFINE INDEX IF NOT EXISTS nb_title   ON notebook FIELDS title;
  DEFINE FIELD IF NOT EXISTS parent_id  ON notebook TYPE option<string>;
  DEFINE FIELD IF NOT EXISTS updated_at ON notebook TYPE datetime;

-- ----------------------------------------------------------------------------
-- note — a single Joplin note record
-- ----------------------------------------------------------------------------
-- excerpt: a leading slice of body (~512 chars) the agent read verb can
-- show without pulling the full body. word_count: cheap aggregate for
-- "how big is this note" answers without re-counting body each query.
DEFINE TABLE IF NOT EXISTS note SCHEMAFULL;
  DEFINE FIELD IF NOT EXISTS joplin_id  ON note TYPE string;
  DEFINE INDEX IF NOT EXISTS note_jid   ON note FIELDS joplin_id UNIQUE;
  DEFINE FIELD IF NOT EXISTS title      ON note TYPE string;
  DEFINE INDEX IF NOT EXISTS note_title ON note FIELDS title;
  DEFINE FIELD IF NOT EXISTS excerpt    ON note TYPE option<string>;
  DEFINE FIELD IF NOT EXISTS parent_id  ON note TYPE string;
  DEFINE FIELD IF NOT EXISTS source_url ON note TYPE option<string>;
  DEFINE FIELD IF NOT EXISTS created_at ON note TYPE datetime;
  DEFINE FIELD IF NOT EXISTS updated_at ON note TYPE datetime;
  DEFINE FIELD IF NOT EXISTS word_count ON note TYPE int;

-- ----------------------------------------------------------------------------
-- tag — Joplin's global tags, classified into a kind axis
-- ----------------------------------------------------------------------------
-- kind is a closed enum. Stage 1 classifier:
--   project:* prefix → 'project'  (title stored without prefix)
--   {decision,bug,scratch,postmortem,handoff,todo} → 'kind'
--   concept:* prefix → 'concept'   (title stored without prefix)
--   else → 'freeform'
-- The ASSERT is intentional — extending the closed-set forces a schema
-- migration AND a classifier update at the same time.
DEFINE TABLE IF NOT EXISTS tag SCHEMAFULL;
  DEFINE FIELD IF NOT EXISTS joplin_id  ON tag TYPE string;
  DEFINE INDEX IF NOT EXISTS tag_jid    ON tag FIELDS joplin_id UNIQUE;
  DEFINE FIELD IF NOT EXISTS title      ON tag TYPE string;
  DEFINE INDEX IF NOT EXISTS tag_title  ON tag FIELDS title;
  DEFINE FIELD IF NOT EXISTS kind       ON tag TYPE string
    ASSERT $value IN ['project','kind','concept','freeform'];
  DEFINE INDEX IF NOT EXISTS tag_kind   ON tag FIELDS kind;

-- ----------------------------------------------------------------------------
-- Edges
-- ----------------------------------------------------------------------------
-- contains:  notebook → note  (a folder owns its notes)
-- nested_in: notebook → notebook  (folder hierarchy)
-- has_tag:   note → tag
DEFINE TABLE IF NOT EXISTS contains  TYPE RELATION IN notebook OUT note;
DEFINE TABLE IF NOT EXISTS nested_in TYPE RELATION IN notebook OUT notebook;
DEFINE TABLE IF NOT EXISTS has_tag   TYPE RELATION IN note     OUT tag;

-- ----------------------------------------------------------------------------
-- links_to — Stage 5. Defined here so the migration is single-shot.
-- ----------------------------------------------------------------------------
-- anchor_text: the visible link text (often the wiki target or the
-- title at link time — useful for query intent).
-- link_kind: 'wiki' for [[wiki]] / 'joplin-id' for :/<32-hex>.
-- resolved: false means anchor_text matched zero or multiple notes;
-- ambiguous_candidates lists the joplin_ids of the matches so a
-- human can disambiguate later. Stage 1 leaves this table empty.
DEFINE TABLE IF NOT EXISTS links_to TYPE RELATION IN note OUT note SCHEMAFULL;
  DEFINE FIELD IF NOT EXISTS anchor_text          ON links_to TYPE option<string>;
  DEFINE FIELD IF NOT EXISTS link_kind            ON links_to TYPE string
    ASSERT $value IN ['wiki','joplin-id'];
  DEFINE FIELD IF NOT EXISTS resolved             ON links_to TYPE bool DEFAULT true;
  DEFINE FIELD IF NOT EXISTS ambiguous_candidates ON links_to TYPE option<array<string>>;

-- ----------------------------------------------------------------------------
-- bridge_state — a single-row table tracking sync progress
-- ----------------------------------------------------------------------------
-- cursor:              Joplin /events cursor — Stage 2's starting point.
-- last_event_at:       timestamp of last applied event (for status lag).
-- last_sync_completed: clean-bootstrap completion timestamp.
-- last_error:          last error string for surface in `bridge status`.
-- bootstrap_progress:  resumable checkpoint shape:
--   { notebooks_done, notes_done, current_notebook_id, current_page, started_at }
--   non-null = bootstrap is in-flight; cleared on clean completion.
DEFINE TABLE IF NOT EXISTS bridge_state SCHEMAFULL;
  DEFINE FIELD IF NOT EXISTS cursor              ON bridge_state TYPE option<string>;
  DEFINE FIELD IF NOT EXISTS last_event_at       ON bridge_state TYPE option<datetime>;
  DEFINE FIELD IF NOT EXISTS last_sync_completed ON bridge_state TYPE option<datetime>;
  DEFINE FIELD IF NOT EXISTS last_error          ON bridge_state TYPE option<string>;
  DEFINE FIELD IF NOT EXISTS bootstrap_progress  ON bridge_state TYPE option<object> FLEXIBLE;
