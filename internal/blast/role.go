package blast

// File-role classifier. Pure function — given a path + content, returns
// the closed-enum role from 23-qai-extend-blast.surql (handler, middleware,
// sdk_client, data_access, serializer, parser, lib, ipc, i18n, job, ui,
// migration, schema, config, test, unknown).
//
// The role axis is what makes the failure-layer's headline query useful:
// "for a given failure_pattern, which file roles does it bite across
// repos?" collapses to "unknown" without it. So this lives in the scanner
// for the same reason ExtractRoutes / ExtractErrorCodes do — it's
// extracted in the same pass that builds the rest of the graph.
//
// Rule shape: kind ∈ {path, filename, content}, regex pattern, confidence
// in [0,1], reason string. For each file we score every rule; for each
// role we keep the highest matching confidence; the role with the highest
// winning rule wins overall. A file that fires nothing stays 'unknown'.
//
// Same conservative principle as scan.go: a missed role is preferable to
// a wrong role. Threshold defaults are tuned so weak signals (e.g. /lib/
// alone) lose to anything more specific.

import (
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------
// Extension policy + helpers
// ---------------------------------------------------------------------

// roleExts is the ALLOW-list of file extensions that get classified. It
// is INTENTIONALLY broader than scan.go's `scanExts`: schema (.surql,
// .sql, .prisma), config (.toml, .yaml, .json), and i18n (.po, .mo, .ftl)
// files all carry a real role even though we don't want them counted in
// the calls/handles consumer-side scan. The two extension sets serve
// different purposes — keep them separate.
var roleExts = map[string]bool{
	// source
	".go": true, ".rs": true, ".ts": true, ".tsx": true, ".js": true,
	".jsx": true, ".mjs": true, ".py": true, ".swift": true, ".zig": true,
	// UI
	".svelte": true, ".vue": true, ".astro": true,
	// schema / data definition
	".surql": true, ".sql": true, ".prisma": true, ".graphql": true,
	// i18n
	".po": true, ".mo": true, ".ftl": true,
	// config
	".toml": true, ".yaml": true, ".yml": true, ".json": true,
}

// langFromExt maps an extension to the canonical language string written
// to code_file.language. Independent of the repo's dominant language —
// a .svelte file inside a Tauri/Rust repo records as 'svelte', not 'rust'.
func langFromExt(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs":
		return "javascript"
	case ".py":
		return "python"
	case ".swift":
		return "swift"
	case ".zig":
		return "zig"
	case ".svelte":
		return "svelte"
	case ".vue":
		return "vue"
	case ".astro":
		return "astro"
	case ".surql":
		return "surrealql"
	case ".sql":
		return "sql"
	case ".prisma":
		return "prisma"
	case ".graphql":
		return "graphql"
	case ".po", ".mo", ".ftl":
		return "i18n"
	case ".toml":
		return "toml"
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	}
	return "unknown"
}

// ---------------------------------------------------------------------
// Rule table
// ---------------------------------------------------------------------

// RoleRule is a single classifier rule.
type RoleRule struct {
	Role       string         // role enum value
	Kind       string         // "path" | "filename" | "content"
	Pattern    *regexp.Regexp // pre-compiled
	Confidence float64        // 0..1
	Reason     string         // human-readable (used by ClassifyFile reasons output)
}

// regex helpers — package-level pre-compiled so rule definitions stay readable.
func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// roleRules — definitions ported from scripts/classify_roles.ts.
// Listed roughly in confidence band (high→low) for readability; the
// scoring loop is order-independent.
var roleRules = []RoleRule{
	// ---- migration (deterministic) ---------------------------------
	{Role: "migration", Kind: "path", Pattern: mustRe(`(?i)/(db/)?migrations?/`), Confidence: 0.95, Reason: "path contains /migrations/"},
	{Role: "migration", Kind: "filename", Pattern: mustRe(`^(V\d+__|\d{4,14}_).+\.(sql|go|rs|ts|py)$`), Confidence: 0.92, Reason: "timestamped migration filename"},

	// ---- test ------------------------------------------------------
	{Role: "test", Kind: "filename", Pattern: mustRe(`(?i)(_test\.go|\.test\.[jt]sx?|\.spec\.[jt]sx?|test_.+\.py|.+\.test\.rs)$`), Confidence: 0.95, Reason: "test filename suffix"},
	{Role: "test", Kind: "path", Pattern: mustRe(`(?i)/(tests?|__tests__|spec|specs?)/`), Confidence: 0.85, Reason: "path contains /tests/"},
	{Role: "test", Kind: "content", Pattern: mustRe(`(?m)^#\[cfg\(test\)\]`), Confidence: 0.70, Reason: "Rust #[cfg(test)] module"},

	// ---- schema ----------------------------------------------------
	{Role: "schema", Kind: "filename", Pattern: mustRe(`(?i)(^|/)(schema\.(surql|sql|prisma|graphql|ts)|\.schema\.[jt]s)$`), Confidence: 0.92, Reason: "schema filename"},
	{Role: "schema", Kind: "content", Pattern: mustRe(`(?i)\bDEFINE\s+(TABLE|FIELD|INDEX|EVENT|ANALYZER|FUNCTION)\b`), Confidence: 0.85, Reason: "SurrealQL DDL"},
	// CREATE TABLE also fires in migrations; keep lower so migration wins.
	{Role: "schema", Kind: "content", Pattern: mustRe(`(?i)\bCREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?\w+`), Confidence: 0.55, Reason: "SQL CREATE TABLE"},

	// ---- config ----------------------------------------------------
	{Role: "config", Kind: "filename", Pattern: mustRe(`(?i)(^|/)(config|tauri\.conf|svelte\.config|vite\.config|astro\.config|wrangler|.+\.config)\.(json|toml|yaml|yml|ts|js|mjs)$|(^|/)\.env(\..+)?$`), Confidence: 0.92, Reason: "config filename"},
	{Role: "config", Kind: "path", Pattern: mustRe(`(?i)/config/`), Confidence: 0.50, Reason: "path contains /config/"},

	// ---- i18n ------------------------------------------------------
	{Role: "i18n", Kind: "path", Pattern: mustRe(`(?i)/(locales?|i18n|translations|messages)/`), Confidence: 0.95, Reason: "locale path"},
	// ISO 639-1 or BCP-47 prefix as filename: en.ts, en-GB.json, fr_FR.po
	{Role: "i18n", Kind: "filename", Pattern: mustRe(`^([a-z]{2}|[a-z]{2}[-_][A-Z]{2})\.(ts|js|json|po|mo|yaml|yml|ftl)$`), Confidence: 0.92, Reason: "locale-code filename"},
	{Role: "i18n", Kind: "content", Pattern: mustRe(`\b(i18next|svelte-i18n|next-intl|intl\.formatMessage|FormattedMessage)\b`), Confidence: 0.70, Reason: "i18n library reference"},

	// ---- ui --------------------------------------------------------
	{Role: "ui", Kind: "filename", Pattern: mustRe(`(?i)\.(svelte|tsx|jsx|vue)$`), Confidence: 0.85, Reason: "UI component extension"},
	{Role: "ui", Kind: "filename", Pattern: mustRe(`(View|Component|Screen|Page)\.swift$`), Confidence: 0.85, Reason: "Swift UI suffix"},
	{Role: "ui", Kind: "path", Pattern: mustRe(`(?i)/(components|views|pages|screens)/`), Confidence: 0.60, Reason: "UI directory"},
	{Role: "ui", Kind: "content", Pattern: mustRe(`\bimport\s+React\b|\b(useState|useEffect|useMemo)\s*\(`), Confidence: 0.55, Reason: "React hooks/import"},

	// ---- middleware (must outrank handler when both fire) ----------
	{Role: "middleware", Kind: "path", Pattern: mustRe(`(?i)/middlewares?/`), Confidence: 0.95, Reason: "middleware directory"},
	{Role: "middleware", Kind: "filename", Pattern: mustRe(`(?i)(^|/)(middleware|interceptor)\.(go|ts|rs|py)$|_(middleware|interceptor)\.(go|ts|rs|py)$`), Confidence: 0.85, Reason: "middleware filename"},
	{Role: "middleware", Kind: "content", Pattern: mustRe(`\bfunc\s+\w+\s*\(\s*next\s+http\.Handler\s*\)\s*http\.Handler\b`), Confidence: 0.92, Reason: "Go middleware signature"},
	{Role: "middleware", Kind: "content", Pattern: mustRe(`\baxum::middleware::from_fn\b|tower::layer::|\.layer\(`), Confidence: 0.78, Reason: "Rust tower/axum layer"},
	{Role: "middleware", Kind: "content", Pattern: mustRe(`\bapp\.use\s*\(\s*[a-zA-Z_$]`), Confidence: 0.65, Reason: "Express/Hono app.use"},
	{Role: "middleware", Kind: "content", Pattern: mustRe(`\bc\.Next\(\)|\bawait\s+next\(\)`), Confidence: 0.75, Reason: "middleware next() call"},

	// ---- handler ---------------------------------------------------
	// Note: no negative lookahead (RE2 doesn't support it). The simpler
	// alternation matches /api/ broadly; in practice files under /api/
	// are handlers, including under /api/v1/ etc.
	{Role: "handler", Kind: "path", Pattern: mustRe(`(?i)/(handlers?|routes?|controllers?|endpoints?|api|server/api)/`), Confidence: 0.85, Reason: "handler directory"},
	{Role: "handler", Kind: "filename", Pattern: mustRe(`(?i)(_handler\.(go|ts|rs)|_controller\.(go|ts)|^route\.(ts|js)$|^\+(server|page)\.(ts|js)$)`), Confidence: 0.85, Reason: "handler filename"},
	{Role: "handler", Kind: "content", Pattern: mustRe(`\bfunc\s+\w+\s*\(\s*w\s+http\.ResponseWriter\s*,\s*r\s+\*http\.Request\s*\)`), Confidence: 0.92, Reason: "Go HTTP handler signature"},
	{Role: "handler", Kind: "content", Pattern: mustRe(`\b(mux\.HandleFunc|chi\.NewRouter|gin\.Default|gin\.New|fiber\.New|echo\.New)\b`), Confidence: 0.85, Reason: "Go router init"},
	{Role: "handler", Kind: "content", Pattern: mustRe(`#\[tauri::command\]|tauri::generate_handler!`), Confidence: 0.95, Reason: "Tauri command"},
	{Role: "handler", Kind: "content", Pattern: mustRe(`\b(axum::Router::new|actix_web::web::resource)\b|#\[(get|post|put|delete|patch)\(`), Confidence: 0.85, Reason: "Rust handler attribute"},
	{Role: "handler", Kind: "content", Pattern: mustRe(`export\s+(async\s+)?function\s+(GET|POST|PUT|DELETE|PATCH)\s*\(`), Confidence: 0.88, Reason: "SvelteKit/Next route export"},
	{Role: "handler", Kind: "content", Pattern: mustRe(`(?m)^@(app|router)\.(get|post|put|delete|patch)\s*\(`), Confidence: 0.85, Reason: "FastAPI decorator"},
	{Role: "handler", Kind: "content", Pattern: mustRe(`export\s+default\s+\{\s*async\s+fetch\s*\(\s*request[^)]*\)\s*\{`), Confidence: 0.92, Reason: "Cloudflare Worker entrypoint"},
	{Role: "handler", Kind: "content", Pattern: mustRe("\\b(app|router)\\.(get|post|put|delete|patch)\\s*\\(\\s*['\"`]"), Confidence: 0.65, Reason: "Express/Hono route"},

	// ---- sdk_client ------------------------------------------------
	{Role: "sdk_client", Kind: "path", Pattern: mustRe(`(?i)/(clients?|integrations?|providers?|sdks?|vendors?)/`), Confidence: 0.70, Reason: "sdk_client directory"},
	{Role: "sdk_client", Kind: "filename", Pattern: mustRe(`(?i)(_client\.(go|ts|rs|py)|\.client\.(ts|js)|Client\.swift)$`), Confidence: 0.75, Reason: "client filename"},
	{Role: "sdk_client", Kind: "content", Pattern: mustRe(`\b(http\.DefaultClient|http\.NewRequest|http\.NewRequestWithContext|http\.Client\{)\b`), Confidence: 0.72, Reason: "Go HTTP client"},
	{Role: "sdk_client", Kind: "content", Pattern: mustRe(`\bnew\s+(OpenAI|Anthropic|GoogleGenerativeAI|MistralClient|GroqClient|XAI|Replicate)\s*\(`), Confidence: 0.90, Reason: "LLM SDK instantiation"},
	{Role: "sdk_client", Kind: "content", Pattern: mustRe(`\b(axios\.create|axios\.get|axios\.post|node-fetch|got\(|reqwest::Client|httpx\.AsyncClient)\b`), Confidence: 0.70, Reason: "outbound HTTP library"},
	{Role: "sdk_client", Kind: "content", Pattern: mustRe(`\b(chromedp\.Run|chromedp\.Navigate|playwright|puppeteer|page\.goto)\b`), Confidence: 0.85, Reason: "headless browser SDK"},

	// ---- data_access -----------------------------------------------
	{Role: "data_access", Kind: "path", Pattern: mustRe(`(?i)/(db|store|repositor(y|ies)|dao|persistence|queries)/`), Confidence: 0.85, Reason: "data layer directory"},
	{Role: "data_access", Kind: "filename", Pattern: mustRe(`(?i)(_repo(sitory)?\.(go|ts|rs)|\.queries\.(ts|sql)|^db\.(ts|go|rs)$)`), Confidence: 0.80, Reason: "data layer filename"},
	{Role: "data_access", Kind: "content", Pattern: mustRe(`\bsql\.Open\(|\b(db|conn|tx)\.(Query|QueryContext|Exec|ExecContext|Prepare)\s*\(`), Confidence: 0.75, Reason: "database/sql driver call"},
	{Role: "data_access", Kind: "content", Pattern: mustRe(`(?i)\bSurreal(::new|<RocksDb>|<Ws>)\b|\bsurrealdb\.(query|select|create)\b`), Confidence: 0.85, Reason: "SurrealDB client"},
	{Role: "data_access", Kind: "content", Pattern: mustRe(`\b(KVNamespace|D1Database|R2Bucket|DurableObjectNamespace)\b|\benv\.(KV|R2|D1|DB)\b`), Confidence: 0.95, Reason: "Cloudflare data binding"},
	{Role: "data_access", Kind: "content", Pattern: mustRe(`\b(rocksdb::|sqlx::|sea_orm::|diesel::|tokio_postgres::)`), Confidence: 0.85, Reason: "Rust data-access crate"},
	{Role: "data_access", Kind: "content", Pattern: mustRe(`\b(prisma|drizzle-orm|TypeORM)\b`), Confidence: 0.80, Reason: "TS ORM"},

	// ---- serializer ------------------------------------------------
	{Role: "serializer", Kind: "filename", Pattern: mustRe(`(?i)(^|/)(serde|de|ser)\.rs$|\.(codec|serde|serializer|deserializer)\.(ts|rs)$`), Confidence: 0.85, Reason: "serde filename"},
	// serde derive is everywhere in Rust — keep low so it loses to other roles.
	{Role: "serializer", Kind: "content", Pattern: mustRe(`#\[derive\([^)]*\b(Serialize|Deserialize)\b`), Confidence: 0.45, Reason: "serde derive (weak)"},
	{Role: "serializer", Kind: "content", Pattern: mustRe(`\bimpl\s+(Serialize|Deserialize|Serializer|Deserializer)\s+for\b`), Confidence: 0.85, Reason: "manual serde impl"},
	{Role: "serializer", Kind: "content", Pattern: mustRe(`\b(json\.Marshal|json\.Unmarshal|proto\.Marshal|bincode::|serde_json::to_string)\b`), Confidence: 0.55, Reason: "(de)serialization call"},

	// ---- parser ----------------------------------------------------
	{Role: "parser", Kind: "path", Pattern: mustRe(`(?i)/(parsers?|lexer|tokenizer)/`), Confidence: 0.90, Reason: "parser directory"},
	{Role: "parser", Kind: "content", Pattern: mustRe(`\b(gzip|flate|zstd)\.NewReader\b|\bpako\.(inflate|ungzip)\b|\bfflate\.(gunzip|inflate)\b`), Confidence: 0.88, Reason: "decompression library"},
	{Role: "parser", Kind: "content", Pattern: mustRe(`\b(tar\.NewReader|tar-stream|node-tar|tarfile\.open)\b`), Confidence: 0.85, Reason: "tar parser"},
	{Role: "parser", Kind: "content", Pattern: mustRe(`\basn1\.Unmarshal\b|\bencoding/asn1\b|\bx509\.ParseCertificate\b`), Confidence: 0.85, Reason: "ASN.1 / X.509 parser"},
	{Role: "parser", Kind: "content", Pattern: mustRe(`\btree_sitter\b|\bnom::|\bpest::`), Confidence: 0.85, Reason: "parser combinator/PEG"},

	// ---- ipc -------------------------------------------------------
	{Role: "ipc", Kind: "path", Pattern: mustRe(`(?i)/ipc/`), Confidence: 0.95, Reason: "ipc directory"},
	{Role: "ipc", Kind: "content", Pattern: mustRe(`\bnet\.Listen\s*\(\s*"unix"|\bUnixListener\b|\bUnixStream\b|\btokio::net::UnixListener\b|\bSO_PEERCRED\b`), Confidence: 0.90, Reason: "UDS / IPC primitive"},

	// ---- job -------------------------------------------------------
	{Role: "job", Kind: "path", Pattern: mustRe(`(?i)/(jobs?|workers?|queues?|cron|scheduler|tasks)/`), Confidence: 0.85, Reason: "job directory"},
	{Role: "job", Kind: "filename", Pattern: mustRe(`(?i)(_job|_worker|_task)\.(go|ts|rs|py)$|^cron\.(go|ts)$`), Confidence: 0.80, Reason: "job filename"},
	{Role: "job", Kind: "content", Pattern: mustRe(`\b(cron\.New|robfig/cron|asynq\.Server|BullMQ|new\s+Worker|Bree)\b`), Confidence: 0.80, Reason: "job library"},

	// ---- lib (catch-all, low confidence) ---------------------------
	{Role: "lib", Kind: "path", Pattern: mustRe(`(?i)/(lib|util|utils|helpers?|core|internal/[^/]+)/`), Confidence: 0.40, Reason: "lib/util directory (weak)"},
	{Role: "lib", Kind: "content", Pattern: mustRe(`\b(crypto/rsa|crypto/ed25519|crypto/aes|std\.crypto)\b|\bsecureZero\b|\bzeroize::Zeroize\b`), Confidence: 0.78, Reason: "crypto library code"},
}

// ---------------------------------------------------------------------
// ClassifyFile
// ---------------------------------------------------------------------

// ClassifyFile scores all rules against (relPath, content) and returns the
// best-fit role. relPath is the file's path RELATIVE to its repo root
// (leading slash is added internally for path-kind regex matching).
// content may be nil — path/filename rules still fire.
//
// Returns ("unknown", 0, nil) if nothing matched.
func ClassifyFile(relPath string, content []byte) (role string, confidence float64, reasons []string) {
	pathWithSlash := "/" + strings.TrimPrefix(relPath, "/")
	basename := filepath.Base(relPath)

	type winner struct {
		conf    float64
		reasons []string
	}
	scoreByRole := map[string]*winner{}

	for _, r := range roleRules {
		var hit bool
		switch r.Kind {
		case "path":
			hit = r.Pattern.MatchString(pathWithSlash)
		case "filename":
			hit = r.Pattern.MatchString(basename)
		case "content":
			if content == nil {
				continue
			}
			hit = r.Pattern.Match(content)
		}
		if !hit {
			continue
		}
		w := scoreByRole[r.Role]
		if w == nil {
			w = &winner{}
			scoreByRole[r.Role] = w
		}
		if r.Confidence > w.conf {
			w.conf = r.Confidence
		}
		w.reasons = append(w.reasons, r.Kind+": "+r.Reason)
	}

	if len(scoreByRole) == 0 {
		return "unknown", 0, nil
	}

	// Pick the role with the highest winning confidence. Ties broken by
	// role name (deterministic) — should be rare since confidences are
	// hand-tuned to discriminate.
	var bestRole string
	var best *winner
	for role, w := range scoreByRole {
		if best == nil || w.conf > best.conf || (w.conf == best.conf && role < bestRole) {
			best = w
			bestRole = role
		}
	}
	return bestRole, best.conf, best.reasons
}
