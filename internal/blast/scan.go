package blast

// Scanner: walks a polyrepo root and extracts contract facts as a
// ScanReport. This is the Go port of the Phase-5 bash/awk extractor —
// same logic, no external file deps.
//
// Heuristics are deliberately conservative regex-based parsers. The
// failure mode of a missed match is a missing edge (someone has to add
// it manually), NOT a wrong edge. Don't be tempted to widen patterns
// without thinking about false positives — every false positive is a
// CI false alarm that erodes trust in the tool.

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------
// Facts
// ---------------------------------------------------------------------

type Repo struct {
	ID       string // slugified, used as record ID
	Name     string
	Kind     string // service | sdk | app | worker | docs
	Language string
	Path     string
}

type Endpoint struct {
	ID         string
	Method     string
	Route      string
	OwnerRepo  string // repo ID
	SourceFile string // relative to owner repo
	Handler    string // handler func name (kept for emits join)
}

type ErrorCode struct {
	Wire     string // "KEY_FROZEN_BY_BUDGET"
	Category string // best-effort categorization (auth/billing/...)
	Source   string // relative path inside backend
}

type SDKStruct struct {
	ID         string
	Name       string
	Language   string
	OwnerRepo  string
	SourceFile string
}

type DependsOn struct {
	FromRepo, ToRepo, VersionConstraint, DeclaredIn string
}

type CallsAPI struct {
	FromRepo    string
	EndpointID  string
	SourceFiles []string
	ViaSDK      bool
}

type HandlesError struct {
	FromRepo    string
	CodeWire    string
	SourceFiles []string
}

type EmitsError struct {
	EndpointID string
	CodeWire   string
	SourceFile string
	CallCount  int
	StatusCode int
}

// CodeFile is a per-file row populated by ExtractCodeFiles. Owned by a
// repo; deterministic from (repo, path). The Role field is the closed
// enum from 23-qai-extend-blast.surql — see role.go for the classifier.
// language is per-file (from extension), not the repo's dominant language.
type CodeFile struct {
	ID        string // slugified: cf_<repo_id>_<slug(path)>
	RepoID    string
	Path      string // relative to repo root (forward slashes)
	Language  string // per-file language inferred from extension
	Role      string // role enum value, or "unknown" if no rule fired
	LineCount int
}

type ScanReport struct {
	Repos        []Repo
	Endpoints    []Endpoint
	ErrorCodes   []ErrorCode
	SDKStructs   []SDKStruct
	DependsOn    []DependsOn
	CallsAPI     []CallsAPI
	HandlesError []HandlesError
	EmitsError   []EmitsError
	Files        []CodeFile
}

// ---------------------------------------------------------------------
// Slug helpers
// ---------------------------------------------------------------------

var slugRe = regexp.MustCompile(`[^a-z0-9_]+`)

func slug(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "x"
	}
	return s
}

// ---------------------------------------------------------------------
// Repo discovery
// ---------------------------------------------------------------------

// DetectRepos walks the polyrepo root (one level deep + a handful of
// known nested locations like qe-sdk-collection/{lang}_projects/*) and
// returns whatever looks like a repo. A repo is "anything with a
// manifest (Cargo.toml / package.json / go.mod) or a .git dir".
func DetectRepos(root string) ([]Repo, error) {
	var repos []Repo
	seen := map[string]bool{}

	usedIDs := map[string]bool{}
	usedNames := map[string]bool{}
	add := func(path, kind, lang string) {
		path = filepath.Clean(path)
		if seen[path] {
			return
		}
		st, err := os.Stat(path)
		if err != nil || !st.IsDir() {
			return
		}
		name := filepath.Base(path)
		id := slug(name)
		// Collision-bust: multiple `quantum-sdk` dirs under different
		// language buckets (rust_projects, go_projects, ...) would slug
		// to the same ID and clash on both the id-PK and the unique
		// `name` index. Suffix BOTH with the parent dir (e.g.
		// "rust_projects") so they remain readable and unique.
		if usedIDs[id] || usedNames[name] {
			parent := filepath.Base(filepath.Dir(path))
			suffix := parent
			// Trim "_projects" so we get "quantum-sdk-rust" not
			// "quantum-sdk-rust_projects".
			suffix = strings.TrimSuffix(suffix, "_projects")
			name = name + "-" + suffix
			id = slug(name)
			if usedIDs[id] || usedNames[name] {
				// Pathological: walk up one more level.
				gp := filepath.Base(filepath.Dir(filepath.Dir(path)))
				name = name + "-" + gp
				id = slug(name)
			}
		}
		usedIDs[id] = true
		usedNames[name] = true
		repos = append(repos, Repo{
			ID:       id,
			Name:     name,
			Kind:     kind,
			Language: lang,
			Path:     path,
		})
		seen[path] = true
	}

	// Top level — one entry per immediate child dir.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(root, e.Name())
		kind, lang := classifyRepo(path)
		add(path, kind, lang)
	}

	// Nested SDK projects under qe-sdk-collection (well-known convention).
	for _, sub := range []string{
		"qe-sdk-collection/rust_projects",
		"qe-sdk-collection/npm_projects",
		"qe-sdk-collection/go_projects",
		"qe-sdk-collection/python_projects",
		"qe-sdk-collection/sdk-example-apps",
	} {
		dir := filepath.Join(root, sub)
		children, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ch := range children {
			if !ch.IsDir() || strings.HasPrefix(ch.Name(), ".") {
				continue
			}
			path := filepath.Join(dir, ch.Name())
			kind, lang := classifyRepo(path)
			// SDK example apps are apps; bare *_projects/quantum-sdk dirs are sdks.
			if strings.Contains(sub, "sdk-example-apps") {
				kind = "app"
			} else if ch.Name() == "quantum-sdk" {
				kind = "sdk"
			}
			add(path, kind, lang)
		}
	}

	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	return repos, nil
}

// classifyRepo makes a quick kind/language guess from manifest files.
// "service" is reserved for the one repo with internal/server/server.go
// (heuristically: that's the canonical Quantum backend).
func classifyRepo(path string) (kind, lang string) {
	has := func(rel string) bool {
		_, err := os.Stat(filepath.Join(path, rel))
		return err == nil
	}
	switch {
	case has("internal/server/server.go"):
		return "service", "go"
	case has("Cargo.toml") && has("src-tauri"):
		return "app", "mixed"
	case has("Cargo.toml"):
		return "sdk", "rust" // overridden by DetectRepos for example-apps
	case has("go.mod"):
		return "sdk", "go"
	case has("requirements.txt") || has("pyproject.toml"):
		return "sdk", "python"
	case has("svelte.config.js"):
		return "app", "svelte"
	case has("astro.config.mjs") || has("astro.config.ts"):
		return "app", "astro"
	case has("package.json"):
		return "app", "typescript"
	default:
		return "docs", "mixed"
	}
}

// FindServiceRepo returns the repo with kind=service, or nil.
func FindServiceRepo(repos []Repo) *Repo {
	for i := range repos {
		if repos[i].Kind == "service" {
			return &repos[i]
		}
	}
	return nil
}

// FindSDKRustRepo returns the Rust SDK repo (heuristic: kind=sdk + Cargo.toml under src/lib.rs).
func FindSDKRustRepo(repos []Repo) *Repo {
	for i := range repos {
		if repos[i].Kind == "sdk" && repos[i].Language == "rust" {
			if _, err := os.Stat(filepath.Join(repos[i].Path, "src", "lib.rs")); err == nil {
				return &repos[i]
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Routes + handler→endpoint map (Go backend)
// ---------------------------------------------------------------------

var routeRe = regexp.MustCompile(`s\.mux\.HandleFunc\("([A-Z]+) ([^"]+)", *s\.(handle[A-Za-z0-9_]+)`)

func ExtractRoutes(backend *Repo) ([]Endpoint, error) {
	if backend == nil {
		return nil, nil
	}
	dir := filepath.Join(backend.Path, "internal/server")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []Endpoint
	seen := map[string]bool{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(backend.Path, f)
		for _, m := range routeRe.FindAllStringSubmatch(string(body), -1) {
			method, route, handler := m[1], m[2], m[3]
			id := "ep_" + slug(method+"_"+route)
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, Endpoint{
				ID:         id,
				Method:     method,
				Route:      route,
				OwnerRepo:  backend.ID,
				SourceFile: rel,
				Handler:    handler,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---------------------------------------------------------------------
// Error codes
// ---------------------------------------------------------------------

var codeConstRe = regexp.MustCompile(`(?m)^\s*Code([A-Za-z]+)\s*=\s*"([A-Z_]+)"`)

// ExtractErrorCodes returns the Go-ident → wire mapping and a slice
// of ErrorCode records, both keyed by wire string.
func ExtractErrorCodes(backend *Repo) (map[string]string, []ErrorCode, error) {
	if backend == nil {
		return nil, nil, nil
	}
	path := filepath.Join(backend.Path, "internal/server/errors.go")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	codeMap := map[string]string{}
	var codes []ErrorCode
	for _, m := range codeConstRe.FindAllStringSubmatch(string(body), -1) {
		ident, wire := "Code"+m[1], m[2]
		if _, ok := codeMap[ident]; ok {
			continue
		}
		codeMap[ident] = wire
		codes = append(codes, ErrorCode{
			Wire:     wire,
			Category: categorize(wire),
			Source:   "internal/server/errors.go",
		})
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i].Wire < codes[j].Wire })
	return codeMap, codes, nil
}

// categorize is a tiny rule-of-thumb. Mirrors the comment groupings in
// the Phase-1 errors.go const blocks.
func categorize(wire string) string {
	switch {
	case strings.HasPrefix(wire, "AUTH_") || strings.HasPrefix(wire, "KEY_") ||
		strings.HasPrefix(wire, "SESSION_") || strings.HasPrefix(wire, "EPHEMERAL_") ||
		strings.HasPrefix(wire, "BEARER_"):
		return "auth"
	case strings.HasPrefix(wire, "SCOPE_") || strings.HasPrefix(wire, "ADMIN_") ||
		strings.HasPrefix(wire, "SERVICE_ACCOUNT_"):
		return "authz"
	case strings.HasPrefix(wire, "INSUFFICIENT_") || strings.HasPrefix(wire, "TRIAL_") ||
		strings.HasPrefix(wire, "SUBSCRIPTION_") || strings.HasPrefix(wire, "SPEND_") ||
		strings.HasPrefix(wire, "BUDGET_") || strings.HasPrefix(wire, "PAYMENT_") ||
		strings.HasPrefix(wire, "BILLING_"):
		return "billing"
	case strings.HasPrefix(wire, "RATE_") || strings.HasPrefix(wire, "QUOTA_"):
		return "rate"
	case strings.HasPrefix(wire, "PROVIDER_") || wire == "CONTENT_REJECTED" || wire == "MODEL_NOT_AVAILABLE":
		return "provider"
	case strings.HasPrefix(wire, "INVALID_") || strings.HasPrefix(wire, "MISSING_") ||
		strings.HasPrefix(wire, "FIELD_") || strings.HasPrefix(wire, "ATTACHMENT_") ||
		strings.HasPrefix(wire, "UNSUPPORTED_"):
		return "validation"
	case strings.HasSuffix(wire, "_PAYWALL"):
		return "paywall"
	default:
		return "system"
	}
}

// ---------------------------------------------------------------------
// emits_error (static analysis of writeErrorWithCode)
// ---------------------------------------------------------------------

var (
	handlerDefRe = regexp.MustCompile(`^func \(s \*Server\) (handle[A-Za-z0-9_]+)`)
	emitCallRe   = regexp.MustCompile(`writeErrorWithCode\([^)]*?(http\.Status[A-Za-z]+)[^)]*?(Code[A-Z][A-Za-z0-9_]+)`)
)

// ExtractEmits scans routes_*.go files, tracks the current handler
// function, finds writeErrorWithCode call sites, resolves Code* idents
// to wire strings, and joins handler→(method,route) via the endpoints.
func ExtractEmits(backend *Repo, codeMap map[string]string, endpoints []Endpoint) ([]EmitsError, error) {
	if backend == nil {
		return nil, nil
	}
	dir := filepath.Join(backend.Path, "internal/server")
	files, err := filepath.Glob(filepath.Join(dir, "routes_*.go"))
	if err != nil {
		return nil, err
	}
	// handler → []endpointID
	handlerToEPs := map[string][]string{}
	for _, ep := range endpoints {
		handlerToEPs[ep.Handler] = append(handlerToEPs[ep.Handler], ep.ID)
	}

	type key struct{ epID, wire string }
	agg := map[key]*EmitsError{}

	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(backend.Path, f)
		current := ""
		scanner := bufio.NewScanner(fh)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if m := handlerDefRe.FindStringSubmatch(line); m != nil {
				current = m[1]
				continue
			}
			if current == "" {
				continue
			}
			for _, m := range emitCallRe.FindAllStringSubmatch(line, -1) {
				statusToken := strings.TrimPrefix(m[1], "http.Status")
				codeIdent := m[2]
				wire, ok := codeMap[codeIdent]
				if !ok {
					continue
				}
				status := statusNameToCode(statusToken)
				for _, epID := range handlerToEPs[current] {
					k := key{epID, wire}
					if e := agg[k]; e != nil {
						e.CallCount++
					} else {
						agg[k] = &EmitsError{
							EndpointID: epID,
							CodeWire:   wire,
							SourceFile: rel,
							CallCount:  1,
							StatusCode: status,
						}
					}
				}
			}
		}
		fh.Close()
	}

	out := make([]EmitsError, 0, len(agg))
	for _, e := range agg {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EndpointID != out[j].EndpointID {
			return out[i].EndpointID < out[j].EndpointID
		}
		return out[i].CodeWire < out[j].CodeWire
	})
	return out, nil
}

// Small subset of net/http status constants — covers everything the
// backend actually uses. Unknown tokens map to 0 (rendered as NULL in
// the schema's status_code field).
func statusNameToCode(name string) int {
	switch name {
	case "OK":
		return 200
	case "Created":
		return 201
	case "NoContent":
		return 204
	case "BadRequest":
		return 400
	case "Unauthorized":
		return 401
	case "PaymentRequired":
		return 402
	case "Forbidden":
		return 403
	case "NotFound":
		return 404
	case "Conflict":
		return 409
	case "UnprocessableEntity":
		return 422
	case "TooManyRequests":
		return 429
	case "InternalServerError":
		return 500
	case "BadGateway":
		return 502
	case "ServiceUnavailable":
		return 503
	case "GatewayTimeout":
		return 504
	}
	return 0
}

// ---------------------------------------------------------------------
// SDK structs (Rust)
// ---------------------------------------------------------------------

var pubStructRe = regexp.MustCompile(`(?m)^pub struct ([A-Z][A-Za-z0-9_]+)`)

func ExtractSDKStructs(sdk *Repo) ([]SDKStruct, error) {
	if sdk == nil {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(sdk.Path, "src", "*.rs"))
	if err != nil {
		return nil, err
	}
	var out []SDKStruct
	seen := map[string]bool{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(sdk.Path, f)
		for _, m := range pubStructRe.FindAllStringSubmatch(string(body), -1) {
			name := m[1]
			id := "s_" + slug(sdk.Name+"_"+name)
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, SDKStruct{
				ID:         id,
				Name:       name,
				Language:   "rust",
				OwnerRepo:  sdk.ID,
				SourceFile: rel,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---------------------------------------------------------------------
// depends_on (Cargo.toml only for v1 — package.json / go.mod TBD)
// ---------------------------------------------------------------------

var cargoQuantumSDKRe = regexp.MustCompile(`(?m)^quantum[-_]sdk\s*=\s*(.+)$`)

func ExtractDependsOn(repos []Repo, sdkRust *Repo) ([]DependsOn, error) {
	var out []DependsOn
	for _, r := range repos {
		if sdkRust != nil && r.ID == sdkRust.ID {
			continue
		}
		cargo := filepath.Join(r.Path, "Cargo.toml")
		body, err := os.ReadFile(cargo)
		if err != nil {
			continue
		}
		if m := cargoQuantumSDKRe.FindStringSubmatch(string(body)); m != nil && sdkRust != nil {
			out = append(out, DependsOn{
				FromRepo:          r.ID,
				ToRepo:            sdkRust.ID,
				VersionConstraint: strings.TrimSpace(m[1]),
				DeclaredIn:        "Cargo.toml",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromRepo != out[j].FromRepo {
			return out[i].FromRepo < out[j].FromRepo
		}
		return out[i].ToRepo < out[j].ToRepo
	})
	return out, nil
}

// ---------------------------------------------------------------------
// calls_api + handles_error (consumer-side grep)
// ---------------------------------------------------------------------

var (
	qaiRouteRe = regexp.MustCompile(`/qai/v1/[A-Za-z0-9/_{}.-]+`)
)

// scanExts is an ALLOW-list of source-code extensions worth scanning
// for /qai/v1/ URLs and canonical error-code mentions.
var scanExts = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true,
	".svelte": true, ".astro": true, ".vue": true,
	".rs": true, ".go": true, ".py": true, ".sh": true, ".html": true,
}

// skipExts is an explicit DENY-list — files matching these are never
// scanned, EVEN IF a future maintainer widens scanExts to include them.
//
// Reason: docs, configs, sample data, and SQL fixtures all routinely
// contain the literal string "KEY_FROZEN_BY_BUDGET" or "/qai/v1/chat"
// in human-readable examples. Counting those as handles_error or
// calls_api edges manufactures phantom consumers and poisons the
// handler-gap query (every code looks "handled everywhere", masking
// real blind spots).
var skipExts = map[string]bool{
	".md":   true, // README, findings.md, design docs
	".mdx":  true,
	".txt":  true, // LICENSE, NOTICE, plain-text changelogs
	".json": true, // package.json, tsconfig.json, fixture payloads
	".log":  true, // ingest output, test runs
	".csv":  true, // sample data dumps
	".surql": true, // seed scripts, migrations (would match their own RELATE strings)
	".sql":  true,
	".yaml": true, ".yml": true, // CI configs, k8s manifests with example URLs
	".toml": true, // Cargo.toml, pyproject.toml
	".lock": true, // Cargo.lock, package-lock.json
	// Note: .html is intentionally NOT denied — backend hand-writes a
	// docs page at internal/server/llms.txt that gets served with .html
	// extension in some deployments.
}

var skipDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "target": true,
	".git": true, ".svelte-kit": true, ".next": true, ".turbo": true,
	"vendor": true,
}

func ExtractCallsAndHandles(repos []Repo, endpoints []Endpoint, codes []ErrorCode, backend *Repo) ([]CallsAPI, []HandlesError, error) {
	// Index endpoints by route (longest first so we match specific over generic).
	endpointByRoute := map[string]string{} // route -> endpoint ID
	for _, ep := range endpoints {
		endpointByRoute[ep.Route] = ep.ID
	}
	routePrefixes := make([]string, 0, len(endpoints))
	for r := range endpointByRoute {
		routePrefixes = append(routePrefixes, r)
	}
	sort.Slice(routePrefixes, func(i, j int) bool { return len(routePrefixes[i]) > len(routePrefixes[j]) })

	// Codes regex (one big alternation).
	var codesRe *regexp.Regexp
	if len(codes) > 0 {
		alts := make([]string, len(codes))
		for i, c := range codes {
			alts[i] = regexp.QuoteMeta(c.Wire)
		}
		codesRe = regexp.MustCompile(`\b(` + strings.Join(alts, "|") + `)\b`)
	}

	// (consumer, endpoint) → []sourceFile
	callsAgg := map[string]map[string]map[string]bool{}
	// (consumer, code) → []sourceFile
	handlesAgg := map[string]map[string]map[string]bool{}

	addCall := func(consumer, epID, file string) {
		if callsAgg[consumer] == nil {
			callsAgg[consumer] = map[string]map[string]bool{}
		}
		if callsAgg[consumer][epID] == nil {
			callsAgg[consumer][epID] = map[string]bool{}
		}
		callsAgg[consumer][epID][file] = true
	}
	addHandle := func(consumer, wire, file string) {
		if handlesAgg[consumer] == nil {
			handlesAgg[consumer] = map[string]map[string]bool{}
		}
		if handlesAgg[consumer][wire] == nil {
			handlesAgg[consumer][wire] = map[string]bool{}
		}
		handlesAgg[consumer][wire][file] = true
	}

	for _, r := range repos {
		// Skip backend from calls_api (it OWNS endpoints, doesn't call them).
		// But DO scan it for handles_error since errors.go and the SDK mirror
		// share the codes (the SDK is its own repo).
		isBackend := backend != nil && r.ID == backend.ID

		err := filepath.WalkDir(r.Path, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(d.Name())
			// Deny-list runs FIRST so a future widening of scanExts (or
			// a fileless basename like "Cargo.lock" / "Dockerfile") can't
			// silently start scanning docs and poisoning the graph.
			if skipExts[ext] {
				return nil
			}
			if !scanExts[ext] {
				return nil
			}
			// Some manifest/lock files lack extensions or use bespoke names
			// — catch those by basename so we don't have to whack-a-mole.
			switch d.Name() {
			case "Dockerfile", "Makefile", "Gemfile", "Pipfile":
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(r.Path, path)

			if !isBackend {
				for _, m := range qaiRouteRe.FindAllString(string(body), -1) {
					route := stripURLTrailers(m)
					epID := matchRoute(route, routePrefixes, endpointByRoute)
					if epID != "" {
						addCall(r.ID, epID, rel)
					}
				}
			}

			if codesRe != nil {
				for _, m := range codesRe.FindAllStringSubmatch(string(body), -1) {
					addHandle(r.ID, m[1], rel)
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("walk %s: %w", r.Path, err)
		}
	}

	// Materialize.
	var calls []CallsAPI
	for consumer, byEP := range callsAgg {
		for epID, files := range byEP {
			calls = append(calls, CallsAPI{
				FromRepo:    consumer,
				EndpointID:  epID,
				SourceFiles: keys(files),
				ViaSDK:      false, // best-effort: TODO infer from sdk.post_raw / sdk.invoke
			})
		}
	}
	var handles []HandlesError
	for consumer, byCode := range handlesAgg {
		for wire, files := range byCode {
			handles = append(handles, HandlesError{
				FromRepo:    consumer,
				CodeWire:    wire,
				SourceFiles: keys(files),
			})
		}
	}
	sort.Slice(calls, func(i, j int) bool {
		if calls[i].FromRepo != calls[j].FromRepo {
			return calls[i].FromRepo < calls[j].FromRepo
		}
		return calls[i].EndpointID < calls[j].EndpointID
	})
	sort.Slice(handles, func(i, j int) bool {
		if handles[i].FromRepo != handles[j].FromRepo {
			return handles[i].FromRepo < handles[j].FromRepo
		}
		return handles[i].CodeWire < handles[j].CodeWire
	})
	return calls, handles, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stripURLTrailers chops off trailing punctuation that's almost
// certainly not part of the route (quote marks, backticks, commas, etc).
func stripURLTrailers(s string) string {
	return strings.TrimRight(s, `'",;:`+"`")
}

// matchRoute finds the best (longest-prefix) endpoint match for a
// concrete URL seen in code. Returns "" if nothing matches.
//
// Handles parameterised endpoints: `/qai/v1/chat/sessions/{id}` is the
// canonical form; calls in code interpolate values (e.g.
// `/qai/v1/chat/sessions/${sessionId}`) — we strip the suffix after the
// last fixed prefix the endpoint registers and call it a match.
func matchRoute(callsite string, prefixes []string, byRoute map[string]string) string {
	// Exact hit first.
	if id, ok := byRoute[callsite]; ok {
		return id
	}
	// Strip parameter values: take the longest registered route whose
	// fixed prefix (up to the first `{...}`) matches the callsite.
	for _, route := range prefixes {
		fixed := route
		if i := strings.Index(route, "{"); i >= 0 {
			fixed = route[:i]
		}
		if strings.HasPrefix(callsite, fixed) {
			return byRoute[route]
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// code_file (per-file role classification)
// ---------------------------------------------------------------------

// ExtractCodeFiles walks every repo and emits one CodeFile per source
// file (filtered by role.go's roleExts allow-list). Role assignment is
// driven by ClassifyFile in role.go; files that match no rule keep
// role="unknown" — the deliberate "missing role > wrong role" call.
//
// File-size cap: 2 MiB. Larger files are classified by path/filename
// only (content rules skipped) so the scan stays fast even when repos
// contain large generated artifacts.
//
// Walks each repo separately. Skips skipDirs (node_modules, target,
// .git, etc — re-used from ExtractCallsAndHandles).
func ExtractCodeFiles(repos []Repo) ([]CodeFile, error) {
	const maxContentBytes = 2 * 1024 * 1024
	var out []CodeFile
	for _, r := range repos {
		err := filepath.WalkDir(r.Path, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(d.Name())
			if !roleExts[ext] {
				return nil
			}
			rel, _ := filepath.Rel(r.Path, path)
			rel = filepath.ToSlash(rel) // platform-agnostic record IDs

			var content []byte
			if st, statErr := os.Stat(path); statErr == nil && st.Size() <= maxContentBytes {
				if body, readErr := os.ReadFile(path); readErr == nil {
					content = body
				}
			}
			role, _, _ := ClassifyFile(rel, content)

			lines := 0
			if content != nil {
				lines = bytes.Count(content, []byte{'\n'}) + 1
			}

			id := "cf_" + slug(r.ID+"_"+rel)
			out = append(out, CodeFile{
				ID:        id,
				RepoID:    r.ID,
				Path:      rel,
				Language:  langFromExt(ext),
				Role:      role,
				LineCount: lines,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", r.Path, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---------------------------------------------------------------------
// Top-level entry point
// ---------------------------------------------------------------------

// Scan runs the full extraction pipeline against a polyrepo root.
func Scan(root string) (*ScanReport, error) {
	repos, err := DetectRepos(root)
	if err != nil {
		return nil, err
	}
	rep := &ScanReport{Repos: repos}

	backend := FindServiceRepo(repos)
	sdkRust := FindSDKRustRepo(repos)

	if rep.Endpoints, err = ExtractRoutes(backend); err != nil {
		return nil, err
	}
	codeMap, codes, err := ExtractErrorCodes(backend)
	if err != nil {
		return nil, err
	}
	rep.ErrorCodes = codes
	if rep.SDKStructs, err = ExtractSDKStructs(sdkRust); err != nil {
		return nil, err
	}
	if rep.EmitsError, err = ExtractEmits(backend, codeMap, rep.Endpoints); err != nil {
		return nil, err
	}
	if rep.DependsOn, err = ExtractDependsOn(repos, sdkRust); err != nil {
		return nil, err
	}
	if rep.CallsAPI, rep.HandlesError, err = ExtractCallsAndHandles(repos, rep.Endpoints, rep.ErrorCodes, backend); err != nil {
		return nil, err
	}
	if rep.Files, err = ExtractCodeFiles(repos); err != nil {
		return nil, err
	}
	return rep, nil
}
