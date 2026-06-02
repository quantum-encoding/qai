-- ============================================================================
-- 23-qai-extend-blast.surql
--
-- Additive schema for qai's existing `blast_radius` database. Adds the
-- failure layer (failure_pattern, edge_case + edges) and the role-discriminator
-- anchor (code_file) on top of qai's repo / api_endpoint / sdk_struct /
-- error_code graph. Nothing existing is modified.
--
-- Apply via:
--   qai patterns init                                  (recommended; uses qai's HTTP client)
-- or directly:
--   surreal import --ns quantumencoding --db blast_radius 23-qai-extend-blast.surql
--
-- DO NOT include `USE NS ... DB ...` in this file. qai's client sets the
-- target via Surreal-NS / Surreal-DB headers (matches the policy in
-- internal/blast/schema.surql lines 14-16). A USE statement here would
-- silently override the caller's choice and dump rows into the wrong place.
--
-- Idempotent: every DDL uses IF NOT EXISTS or OVERWRITE. Safe to re-run.
-- ============================================================================


-- ----------------------------------------------------------------------------
-- code_file — the role-discriminator anchor
-- ----------------------------------------------------------------------------
-- Owned by a repo, deterministic from (repo, path), so populating it from
-- the scanner adds no false-positive risk (matches blast's "missing edge >
-- wrong edge" principle). The `role` field is the closed enum that the
-- headline failure-layer query joins on.

DEFINE TABLE code_file SCHEMAFULL;
DEFINE FIELD repo       ON code_file TYPE record<repo>  ASSERT $value != NONE;
DEFINE FIELD path       ON code_file TYPE string         ASSERT $value != NONE AND $value != '';
DEFINE FIELD language   ON code_file TYPE string         DEFAULT 'unknown';
DEFINE FIELD line_count ON code_file TYPE option<int>;
DEFINE FIELD role       ON code_file TYPE string
    DEFAULT 'unknown'
    ASSERT $value INSIDE [
        'handler','middleware','sdk_client','data_access','serializer','parser',
        'lib','ipc','i18n','job','ui','migration','schema','config','test','unknown'
    ];
DEFINE FIELD created_at ON code_file TYPE datetime DEFAULT time::now();
DEFINE INDEX code_file_unique ON code_file FIELDS repo, path UNIQUE;
DEFINE INDEX code_file_role   ON code_file FIELDS role;
DEFINE INDEX code_file_lang   ON code_file FIELDS language;


-- ----------------------------------------------------------------------------
-- failure_pattern — transferable taxonomy (the institutional-memory layer)
-- ----------------------------------------------------------------------------
-- `category` is a closed set (finalised from the file-20/21 corpora). Adding
-- a category is a deliberate schema change — edit THIS list, never invent
-- category strings in calling code.
--
-- `triggers` carries import/symbol signals the interceptor matches against
-- generated diffs. `tags` carries cross-cutting axes (a pattern is often
-- multi-dimensional — e.g. idempotency_double_bill is concurrency + billing).

DEFINE TABLE failure_pattern SCHEMAFULL;
DEFINE FIELD name        ON failure_pattern TYPE string ASSERT $value != NONE;
DEFINE FIELD slug        ON failure_pattern TYPE string ASSERT $value != NONE;
DEFINE FIELD version     ON failure_pattern TYPE int    DEFAULT 1;
DEFINE FIELD category    ON failure_pattern TYPE string
    ASSERT $value INSIDE [
        'auth','permission',
        'validation','injection','ssrf','serialization','schema_violation','parsing',
        'billing',
        'rate_limit','resource_exhaustion','memory',
        'concurrency','timeout','retry','distributed',
        'crypto','sandboxing','io','panic',
        'ui_state','i18n',
        'agentic',
        'migration','dependency','observability'
    ];
DEFINE FIELD summary     ON failure_pattern TYPE string;
DEFINE FIELD detection   ON failure_pattern TYPE option<string>;
DEFINE FIELD remediation ON failure_pattern TYPE option<string>;
DEFINE FIELD tags        ON failure_pattern TYPE option<array<string>>;
DEFINE FIELD triggers    ON failure_pattern TYPE option<array<string>>;
DEFINE FIELD created_at  ON failure_pattern TYPE datetime DEFAULT time::now();
DEFINE FIELD updated_at  ON failure_pattern TYPE datetime DEFAULT time::now();
DEFINE INDEX failure_pattern_slug     ON failure_pattern FIELDS slug, version UNIQUE;
DEFINE INDEX failure_pattern_category ON failure_pattern FIELDS category;


-- ----------------------------------------------------------------------------
-- edge_case — concrete, repo-local failure instance
-- ----------------------------------------------------------------------------

DEFINE TABLE edge_case SCHEMAFULL;
DEFINE FIELD title       ON edge_case TYPE string ASSERT $value != NONE;
DEFINE FIELD description ON edge_case TYPE string;
DEFINE FIELD repo        ON edge_case TYPE record<repo>;
DEFINE FIELD severity    ON edge_case TYPE string DEFAULT 'medium'
    ASSERT $value INSIDE ['low','medium','high','critical'];
DEFINE FIELD state       ON edge_case TYPE string DEFAULT 'open'
    ASSERT $value INSIDE ['open','triaged','fixed','wont_fix','duplicate'];
DEFINE FIELD first_seen  ON edge_case TYPE datetime DEFAULT time::now();
DEFINE FIELD last_seen   ON edge_case TYPE option<datetime>;
DEFINE FIELD source      ON edge_case TYPE option<string>;
DEFINE FIELD tags        ON edge_case TYPE option<array<string>>;
DEFINE INDEX edge_case_repo     ON edge_case FIELDS repo;
DEFINE INDEX edge_case_state    ON edge_case FIELDS state;
DEFINE INDEX edge_case_severity ON edge_case FIELDS severity;


-- ----------------------------------------------------------------------------
-- Edges — connect the failure layer to qai's existing graph
-- ----------------------------------------------------------------------------
-- All TYPE RELATION SCHEMAFULL, all with a UNIQUE index on (in, out) to keep
-- the graph idempotent under re-ingestion (same policy as qai's existing
-- emits_error / calls_api etc.).

-- An edge_case is an instance of a transferable failure_pattern.
DEFINE TABLE instance_of TYPE RELATION IN edge_case OUT failure_pattern SCHEMAFULL;
DEFINE FIELD confidence ON instance_of TYPE float DEFAULT 1.0
    ASSERT $value >= 0.0 AND $value <= 1.0;
DEFINE FIELD source     ON instance_of TYPE string
    ASSERT $value INSIDE ['human','llm','heuristic','test','log'];
DEFINE FIELD created_at ON instance_of TYPE datetime DEFAULT time::now();
DEFINE INDEX instance_of_unique ON instance_of FIELDS in, out UNIQUE;

-- Primary anchor for code-vulnerability patterns: the edge_case shows up in
-- one or more files. Joining through `out.role` powers the role-ranked
-- interceptor query.
DEFINE TABLE manifests_in_file TYPE RELATION IN edge_case OUT code_file SCHEMAFULL;
DEFINE FIELD created_at ON manifests_in_file TYPE datetime DEFAULT time::now();
DEFINE INDEX manifests_in_file_unique ON manifests_in_file FIELDS in, out UNIQUE;

-- Finer-grained variant for handler bugs (re-uses qai's existing endpoint
-- table; qai doesn't model functions so this is as fine-grained as we go).
DEFINE TABLE manifests_at_endpoint TYPE RELATION IN edge_case OUT api_endpoint SCHEMAFULL;
DEFINE FIELD created_at ON manifests_at_endpoint TYPE datetime DEFAULT time::now();
DEFINE INDEX manifests_at_endpoint_unique ON manifests_at_endpoint FIELDS in, out UNIQUE;

-- The bridge edge: a failure_pattern POINTS AT one or more error_codes. Lets
-- the blind-spot query traverse pattern → error_code → endpoints-emitting-it
-- → consumers-calling-them → consumers-NOT-handling-it.
DEFINE TABLE pattern_about_error TYPE RELATION IN failure_pattern OUT error_code SCHEMAFULL;
DEFINE FIELD created_at ON pattern_about_error TYPE datetime DEFAULT time::now();
DEFINE INDEX pattern_about_error_unique ON pattern_about_error FIELDS in, out UNIQUE;


-- ----------------------------------------------------------------------------
-- Sanity checks (run after init)
-- ----------------------------------------------------------------------------

-- Confirm the new tables exist and are SCHEMAFULL.
INFO FOR DB;

-- Coverage check — should be empty until 24-qai-failure-patterns.surql runs.
SELECT count() AS patterns FROM failure_pattern GROUP ALL;
SELECT count() AS files    FROM code_file       GROUP ALL;
SELECT count() AS cases    FROM edge_case       GROUP ALL;
