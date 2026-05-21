package blast

// Pre-canned blast-radius queries. Mirrors the TS DEFAULT_PRESETS in
// ai-conductor's frontend explorer and the SQL in queries.surql.

type Preset struct {
	ID          string
	Label       string
	Description string
	SurQL       string
}

var Presets = []Preset{
	{
		ID:          "overview",
		Label:       "Graph census",
		Description: "Counts of every node and edge type.",
		SurQL: `SELECT count() AS repos          FROM repo          GROUP ALL;
SELECT count() AS api_endpoints  FROM api_endpoint  GROUP ALL;
SELECT count() AS sdk_structs    FROM sdk_struct    GROUP ALL;
SELECT count() AS error_codes    FROM error_code    GROUP ALL;
SELECT count() AS depends_on     FROM depends_on    GROUP ALL;
SELECT count() AS calls_api      FROM calls_api     GROUP ALL;
SELECT count() AS uses_struct    FROM uses_struct   GROUP ALL;
SELECT count() AS handles_error  FROM handles_error GROUP ALL;
SELECT count() AS emits_error    FROM emits_error   GROUP ALL;`,
	},
	{
		ID:          "handler-gap",
		Label:       "Handler gap (Q7 — killer query)",
		Description: "Consumers that call an emitting endpoint but do NOT branch on the code it emits.",
		// Filter to non-empty blind_spot so the table only shows actionable
		// gaps. Caveat: handles_error currently counts any string mention
		// of the wire code (incl. docs/README). Tightening the regex to
		// branch-context only is on the v1.1 backlog.
		SurQL: `SELECT * FROM (
    SELECT
        out.code_name AS code,
        in.route      AS route,
        in.method     AS method,
        array::complement(
            (SELECT VALUE in.name FROM calls_api WHERE out = $parent.in),
            (SELECT VALUE in.name FROM handles_error WHERE out = $parent.out)
        ) AS blind_spot
    FROM emits_error
) WHERE array::len(blind_spot) > 0;`,
	},
	{
		ID:          "error-blast",
		Label:       "Error code blast (Q1)",
		Description: "Repos that branch on a given code today.",
		SurQL: `SELECT
    in.name       AS repo,
    in.kind       AS repo_kind,
    in.language   AS language,
    source_files  AS files_to_update,
    remediation   AS current_remediation
FROM handles_error
WHERE out.code_name IN ['KEY_FROZEN_BY_BUDGET','BUDGET_FROZEN'];`,
	},
	{
		ID:          "endpoint-callers",
		Label:       "Endpoint callers (Q3)",
		Description: "Who calls POST /qai/v1/chat?",
		SurQL: `SELECT in.name AS repo, in.language AS language, source_files AS files, via_sdk
FROM calls_api
WHERE out.route = '/qai/v1/chat' AND out.method = 'POST'
ORDER BY via_sdk DESC, repo;`,
	},
	{
		ID:          "sdk-consumers",
		Label:       "SDK consumers (Q2)",
		Description: "Direct downstream consumers of quantum-sdk-rs.",
		SurQL: `SELECT name AS sdk, <-depends_on<-repo.{name, kind, language, path} AS consumers
FROM repo:quantum_sdk_rs;`,
	},
	{
		ID:          "struct-consumers",
		Label:       "Struct consumers (Q4)",
		Description: "Repos that deserialize ChatUsage.",
		SurQL: `SELECT in.name AS repo, in.language AS language, source_files AS files
FROM uses_struct WHERE out.name = 'ChatUsage';`,
	},
	{
		ID:          "orphan-endpoints",
		Label:       "Orphan endpoints (Q6a)",
		Description: "Endpoints no consumer calls.",
		SurQL: `SELECT method, route, source_file FROM api_endpoint
WHERE count(<-calls_api) = 0 ORDER BY route;`,
	},
	{
		ID:          "orphan-codes",
		Label:       "Orphan error codes (Q6c)",
		Description: "Backend codes no consumer handles.",
		SurQL: `SELECT code_name, category FROM error_code
WHERE count(<-handles_error) = 0 ORDER BY category, code_name;`,
	},
	{
		ID:          "all-repos",
		Label:       "All repos",
		Description: "Every repo node in the graph.",
		SurQL:       `SELECT name, kind, language, path FROM repo ORDER BY kind, name;`,
	},
}

// FindPreset returns nil if id doesn't match.
func FindPreset(id string) *Preset {
	for i := range Presets {
		if Presets[i].ID == id {
			return &Presets[i]
		}
	}
	return nil
}
