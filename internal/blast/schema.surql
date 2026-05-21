-- =====================================================================
-- Blast Radius Mapper — SurrealDB v3 schema
-- =====================================================================
-- Models the polyrepo as a directed graph: contracts (endpoints, structs,
-- error codes) ←→ consumers (repos). A git diff that touches a contract
-- node lets CI compute exactly which consumer repos/files break, instead
-- of relying on memory or grep-archaeology.
--
-- Naming: snake_case tables, kebab-case IDs (where derived from strings).
-- Edges are typed RELATION tables — modern v3 syntax — which lets us
-- attach version constraints / file lists onto the edge itself rather
-- than smearing them across the endpoints.

-- Namespace + database come from the connection (--ns / --db / QAI_SURREAL_NS / _DB).
-- Do NOT put a `USE NS ... DB ...` here — it would silently override the
-- caller's target and ingest into the wrong place.

-- ---------------------------------------------------------------------
-- Nodes
-- ---------------------------------------------------------------------

DEFINE TABLE repo SCHEMAFULL;
DEFINE FIELD name      ON repo TYPE string ASSERT $value != NONE AND $value != '';
DEFINE FIELD kind      ON repo TYPE string ASSERT $value INSIDE ['service','sdk','app','worker','docs'];
DEFINE FIELD language  ON repo TYPE string;                  -- go | rust | typescript | python | astro | svelte | mixed
DEFINE FIELD path      ON repo TYPE option<string>;          -- absolute path on dev machine (optional)
DEFINE FIELD vcs_url   ON repo TYPE option<string>;
DEFINE INDEX repo_name ON repo FIELDS name UNIQUE;

DEFINE TABLE api_endpoint SCHEMAFULL;
DEFINE FIELD route        ON api_endpoint TYPE string ASSERT string::starts_with($value,'/');
DEFINE FIELD method       ON api_endpoint TYPE string ASSERT $value INSIDE ['GET','POST','PUT','DELETE','PATCH','HEAD'];
DEFINE FIELD owner_repo   ON api_endpoint TYPE record<repo>;
DEFINE FIELD source_file  ON api_endpoint TYPE option<string>;   -- "internal/server/routes_chat.go"
DEFINE FIELD description  ON api_endpoint TYPE option<string>;
DEFINE INDEX api_endpoint_unique ON api_endpoint FIELDS method, route UNIQUE;

DEFINE TABLE sdk_struct SCHEMAFULL;
DEFINE FIELD name        ON sdk_struct TYPE string;
DEFINE FIELD language    ON sdk_struct TYPE string;          -- rust | typescript | go | python
DEFINE FIELD owner_repo  ON sdk_struct TYPE record<repo>;
DEFINE FIELD source_file ON sdk_struct TYPE option<string>;  -- "src/chat.rs"
DEFINE FIELD fields      ON sdk_struct TYPE option<array<string>>;
DEFINE INDEX sdk_struct_unique ON sdk_struct FIELDS owner_repo, name, language UNIQUE;

DEFINE TABLE error_code SCHEMAFULL;
DEFINE FIELD code_name   ON error_code TYPE string;          -- "KEY_FROZEN_BY_BUDGET"
DEFINE FIELD category    ON error_code TYPE string;          -- auth | authz | billing | rate | provider | validation | system | paywall
DEFINE FIELD source_file ON error_code TYPE option<string>;  -- "internal/server/errors.go"
DEFINE FIELD added_in    ON error_code TYPE option<string>;  -- git sha / semver, for forward-compat tracking
DEFINE INDEX error_code_unique ON error_code FIELDS code_name UNIQUE;

-- ---------------------------------------------------------------------
-- Edges (RELATION tables, v3 syntax)
-- ---------------------------------------------------------------------
-- IN/OUT constraints are enforced at insert time — RELATE statements
-- that violate them are rejected, not silently coerced.

DEFINE TABLE depends_on TYPE RELATION IN repo OUT repo SCHEMAFULL;
DEFINE FIELD version_constraint ON depends_on TYPE string;        -- "^0.7", "path:..", "workspace:*", "0.7.2"
DEFINE FIELD declared_in        ON depends_on TYPE string;        -- "Cargo.toml" | "package.json" | "go.mod" | "requirements.txt"
DEFINE INDEX depends_on_unique  ON depends_on FIELDS in, out, declared_in UNIQUE;

DEFINE TABLE calls_api TYPE RELATION IN repo OUT api_endpoint SCHEMAFULL;
DEFINE FIELD source_files ON calls_api TYPE array<string>;        -- every file in the consumer that hits this endpoint
DEFINE FIELD via_sdk      ON calls_api TYPE bool DEFAULT false;   -- true if called via SDK wrapper, false if raw HTTP
DEFINE INDEX calls_api_unique ON calls_api FIELDS in, out UNIQUE;

DEFINE TABLE uses_struct TYPE RELATION IN repo OUT sdk_struct SCHEMAFULL;
DEFINE FIELD source_files ON uses_struct TYPE array<string>;
DEFINE INDEX uses_struct_unique ON uses_struct FIELDS in, out UNIQUE;

DEFINE TABLE handles_error TYPE RELATION IN repo OUT error_code SCHEMAFULL;
DEFINE FIELD source_files ON handles_error TYPE array<string>;    -- where the consumer branches on this code
DEFINE FIELD remediation  ON handles_error TYPE option<string>;   -- short note on what the consumer does on this code
DEFINE INDEX handles_error_unique ON handles_error FIELDS in, out UNIQUE;

-- The "Missing Link" — backend emit edges. An endpoint emits an error code
-- when its handler calls writeErrorWithCode(..., CodeXxx, ...). Combined
-- with calls_api (consumer→endpoint) and handles_error (consumer→code) this
-- closes the loop: for any emitted code we can compute the *gap* between
-- "endpoints that emit it" → "consumers that call those endpoints" →
-- "consumers that handle it" → the set difference is the blind spot.
DEFINE TABLE emits_error TYPE RELATION IN api_endpoint OUT error_code SCHEMAFULL;
DEFINE FIELD source_file ON emits_error TYPE string;              -- "internal/server/routes_admin.go"
DEFINE FIELD call_count  ON emits_error TYPE int DEFAULT 1;       -- how many writeErrorWithCode sites in this handler emit this code
DEFINE FIELD status_code ON emits_error TYPE option<int>;         -- HTTP status when known (403 for ADMIN_REQUIRED etc.)
DEFINE INDEX emits_error_unique ON emits_error FIELDS in, out UNIQUE;
