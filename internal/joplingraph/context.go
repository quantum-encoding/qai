// Package joplingraph implements `qai joplin graph context` — the
// agent-memory READ verb that consumes the Stage 3 Health API and
// returns a structured payload of relevant notes plus their graph
// neighbourhood for a session-start hook to hand to an agent.
//
// Architecturally: bridge = write/maintain (sync/tail/status), graph =
// read. This package imports joplinbridge for Health, Thresholds,
// surrealAPI/joplinAPI interfaces, and the quote()/recordID() helpers.
// It does not write to bridge_state, edges, or any other table.
//
// The freshness gate is policy-by-design — serve-stale-with-label by
// default, --strict refuses on !OK. See the Stage 4 spec for the
// rationale: an agent-memory verb that commonly returns nothing
// defeats the whole subsystem; labelled-degraded beats fail-closed.
package joplingraph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
	"github.com/quantum-encoding/qai-cli/internal/joplinbridge"
	"github.com/quantum-encoding/qai-cli/internal/projectid"
)

// Exit code constants — same numbers as `bridge status --check`. The
// graph context verb adds exitRefused on the --strict refuse branch.
const (
	exitOK         = 0
	exitRefused    = 1
	exitBootstrap  = 2
	exitInvocation = 3
)

// schemaVersion stamps every payload so a consumer keying off it can
// refuse a newer shape it doesn't understand. Bump on any payload
// field rename or removal.
const schemaVersion = 1

// hardLimit caps --limit. The spec refuses larger values with
// exitInvocation; document the cap in --include-bodies help text.
const hardLimit = 200

// bodyFetchWorkers is the parallelism cap for the --include-bodies
// path. Joplin's local Web Clipper happily serves concurrent GETs;
// eight is the spec-suggested ceiling that doesn't hammer it.
const bodyFetchWorkers = 8

// LookupKind classifies which arm of the lookup resolution table ran.
type LookupKind string

const (
	KindFullText LookupKind = "fulltext"
	KindProject  LookupKind = "project"
	KindTag      LookupKind = "tag"
)

// Args is the parsed CLI invocation — the entry point for tests that
// don't want to drive an os.Args slice through the parser.
type Args struct {
	Query        string
	Project      string // populated; empty means resolver should run
	ProjectFlag  bool   // --project was passed (with or without value)
	Tag          string
	With         string
	Hops         int
	Limit        int
	IncludeBody  bool
	MaxLag       time.Duration
	Strict       bool
	Explain      bool
}

// CmdGraph is the dispatcher wired into joplinops.go.
func CmdGraph(args []string) {
	if len(args) == 0 || args[0] == "context" {
		rest := args
		if len(args) > 0 && args[0] == "context" {
			rest = args[1:]
		}
		cmdContext(rest)
		return
	}
	if isHelp(args[0]) {
		fmt.Println(helpGraph)
		return
	}
	fmt.Fprintf(os.Stderr, "qai joplin graph: unknown subcommand %q\n", args[0])
	fmt.Fprintln(os.Stderr, "Run `qai joplin graph --help` for the list.")
	os.Exit(exitInvocation)
}

func isHelp(s string) bool { return s == "--help" || s == "-h" || s == "help" }

// cmdContext parses, gates on Health, executes the lookup + expansion,
// and writes the JSON payload. Built so the io.Writer + clock can be
// fed in for tests; the public surface is the os.Args-driven CmdGraph
// above.
func cmdContext(rawArgs []string) {
	if hasFlag(rawArgs, "--help", "-h") {
		fmt.Println(helpContext)
		return
	}
	parsed, err := parseArgs(rawArgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin graph context: %v\n", err)
		os.Exit(exitInvocation)
	}

	// Surreal client — read-only consumer of notes_graph.
	sOpts := blast.DefaultOptions()
	if os.Getenv("QAI_SURREAL_DB") == "" {
		sOpts.DB = "notes_graph"
	}
	if err := sOpts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "qai joplin graph context: %v\n", err)
		os.Exit(exitInvocation)
	}
	sc := blast.NewClient(sOpts)

	// Joplin client only needed when --include-bodies is set OR for
	// the Health backlog probe. Soft-fail when missing.
	jc := tryLoadJoplin()

	// Resolve --project bare via the Stage 0 resolver. Done after
	// argparse so a malformed project: tag from the resolver surfaces
	// as the same exit-3 path as any other invocation error.
	if parsed.ProjectFlag && parsed.Project == "" {
		cwd, _ := os.Getwd()
		r, err := projectid.Resolve(cwd)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"qai joplin graph context: --project resolver: %v\n", err)
			fmt.Fprintln(os.Stderr,
				"  → fix: pass --project <name> explicitly, or write .qai/project")
			os.Exit(exitInvocation)
		}
		parsed.Project = r.Project
	}
	// No positional + no --project + no --tag → default to --project bare.
	if parsed.Query == "" && !parsed.ProjectFlag && parsed.Tag == "" {
		cwd, _ := os.Getwd()
		r, err := projectid.Resolve(cwd)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"qai joplin graph context: no query and project resolver failed: %v\n", err)
			fmt.Fprintln(os.Stderr,
				"  → fix: pass a [query], --project <name>, or --tag <name>")
			os.Exit(exitInvocation)
		}
		parsed.Project = r.Project
		parsed.ProjectFlag = true
	}

	runCtx := &runContext{
		Surreal: sc,
		Joplin:  jc,
		Args:    parsed,
		Now:     time.Now(),
		Out:     os.Stdout,
		Err:     os.Stderr,
	}
	exit := runContext_run(runCtx)
	if exit != exitOK {
		os.Exit(exit)
	}
}

// runContext bundles the io seams the orchestration body uses so tests
// can drive cmdContext's logic without globals. Fields are public so
// the test fakes can populate them directly.
type runContext struct {
	Surreal surrealAPI
	Joplin  joplinAPI
	Args    Args
	Now     time.Time
	Out     io.Writer
	Err     io.Writer
}

// surrealAPI matches joplinbridge.surrealAPI exactly so the same fakes
// satisfy both packages. Redeclared here (not aliased) because the
// joplinbridge surface is unexported.
type surrealAPI interface {
	Exec(surql string) ([]blast.StatementResult, error)
}

// joplinAPI is the read-side subset graph context calls — only GetNote
// for --include-bodies, plus the methods joplinbridge.Health needs
// (GetEvents for the backlog probe). The full bridge interface is too
// wide for this stage.
type joplinAPI interface {
	GetNote(id string, fields ...string) (*joplin.Note, error)
	GetEvents(cursor string, limit int) (*joplin.EventsResponse, error)
}

// runContext_run is the testable body: parse + gate + execute. Returns
// the would-be exit code; the dispatcher os.Exits with it. Pure-ish:
// writes JSON / stderr lines through rc.Out/rc.Err.
func runContext_run(rc *runContext) int {
	// --explain runs the gate (so health context still appears) but
	// dumps the constructed query to stderr instead of executing it.
	// Document on a fake's Exec the zero-call assertion for the
	// post-gate query path.
	if rc.Args.Explain {
		return runExplain(rc)
	}

	// Single Health() call — used both to drive the gate AND to
	// populate the payload's health summary. Do NOT call twice.
	hc, err := joplinbridge.Health(adaptSurrealForBridge(rc.Surreal), adaptJoplinForBridge(rc.Joplin), rc.Now, joplinbridge.Thresholds{
		MaxLag: rc.Args.MaxLag,
	})
	if err != nil {
		fmt.Fprintf(rc.Err, "qai joplin graph context: health probe: %v\n", err)
		return exitInvocation
	}

	stale := !hc.OK
	warning := hc.Reason

	if !hc.OK && rc.Args.Strict {
		fmt.Fprintf(rc.Err, "unhealthy: %s — %s\n", hc.Status, hc.Reason)
		if hc.Status == joplinbridge.HealthBootstrap {
			return exitBootstrap
		}
		return exitRefused
	}
	if !hc.OK {
		fmt.Fprintf(rc.Err,
			"warning: serving stale graph — %s (use --strict to refuse)\n",
			hc.Reason)
	}
	if hc.OK && hc.Reason != "" {
		// Degraded: data still fresh but tail stopped. Caller MUST
		// surface this — Stage 3 contract.
		fmt.Fprintf(rc.Err,
			"warning: %s\n",
			hc.Reason)
	}

	primaryRows, kind, err := runPrimaryLookup(rc.Surreal, rc.Args)
	if err != nil {
		fmt.Fprintf(rc.Err, "qai joplin graph context: lookup: %v\n", err)
		return exitInvocation
	}

	// Neighbourhood expansion + path resolution + (optional) body fetch.
	notes, err := assemble(rc, primaryRows, kind)
	if err != nil {
		fmt.Fprintf(rc.Err, "qai joplin graph context: assemble: %v\n", err)
		return exitInvocation
	}

	payload := buildContext(notes, rc.Args, kind, hc, stale, warning)
	enc := json.NewEncoder(rc.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintf(rc.Err, "qai joplin graph context: encode: %v\n", err)
		return exitInvocation
	}
	return exitOK
}

// runExplain runs the gate but prints the would-be primary lookup SQL
// to stderr instead of executing it. Exits 0 unless --strict refuses
// at the gate. Used as a debugging aid; do NOT route a Joplin response
// through it.
func runExplain(rc *runContext) int {
	hc, err := joplinbridge.Health(adaptSurrealForBridge(rc.Surreal), adaptJoplinForBridge(rc.Joplin), rc.Now, joplinbridge.Thresholds{
		MaxLag: rc.Args.MaxLag,
	})
	if err != nil {
		fmt.Fprintf(rc.Err, "qai joplin graph context: health probe: %v\n", err)
		return exitInvocation
	}
	if !hc.OK && rc.Args.Strict {
		fmt.Fprintf(rc.Err, "unhealthy: %s — %s\n", hc.Status, hc.Reason)
		if hc.Status == joplinbridge.HealthBootstrap {
			return exitBootstrap
		}
		return exitRefused
	}
	sql, kind := buildPrimarySQL(rc.Args)
	fmt.Fprintf(rc.Err, "qai joplin graph context --explain (kind=%s):\n%s\n", kind, sql)
	return exitOK
}

// runPrimaryLookup builds and runs the primary SurrealQL per the
// lookup-resolution table, returning the decoded note rows.
func runPrimaryLookup(s surrealAPI, args Args) ([]noteRow, LookupKind, error) {
	sql, kind := buildPrimarySQL(args)
	results, err := s.Exec(sql)
	if err != nil {
		return nil, kind, err
	}
	if err := blast.FirstError(results); err != nil {
		return nil, kind, err
	}
	if len(results) == 0 || len(results[0].Result) == 0 {
		return nil, kind, nil
	}
	// The last statement's result is the note rows (the tag-intersection
	// path emits LET prefixes; the final SELECT is the last result).
	rows, err := decodeNoteRows(results[len(results)-1].Result)
	if err != nil {
		return nil, kind, fmt.Errorf("decode: %w", err)
	}
	return rows, kind, nil
}

// assemble fans out the per-note enrichment work and stitches the
// final []Note for buildContext. kind is unused today but kept on the
// signature so a future per-kind tweak (e.g. score-aware ordering on
// fulltext) doesn't require ripping the call site apart.
func assemble(rc *runContext, primary []noteRow, kind LookupKind) ([]Note, error) {
	_ = kind
	if len(primary) == 0 {
		return nil, nil
	}
	primaryIDs := make([]string, 0, len(primary))
	for _, r := range primary {
		primaryIDs = append(primaryIDs, stripTable(r.ID))
	}

	// Neighbourhood expansion: notebook + tags + links_to (the last is
	// no-op-safe today; lights up once Stage 5 populates the edge).
	nbh := neighbourhood{}
	if rc.Args.Hops >= 1 {
		var err error
		nbh, err = expandNeighbourhood(rc.Surreal, primaryIDs)
		if err != nil {
			return nil, fmt.Errorf("expand: %w", err)
		}
	}

	// Notebook path resolver caches across rows so N notes in one parent
	// walk the chain once. Reused for both primary + neighbour notes.
	pathCache := map[string]string{}

	notes := make([]Note, 0, len(primary)+len(nbh.LinkTargetNotes))
	for _, r := range primary {
		n := convertNoteRow(r, "primary")
		attachNotebookFromMap(&n, nbh.NotebookByNoteID, pathCache, rc.Surreal)
		attachTagsFromMap(&n, nbh.TagsByNoteID)
		attachLinksFromMap(&n, nbh.LinksOut, nbh.LinksIn)
		notes = append(notes, n)
	}

	// --hops 2: pull neighbour notes' own notebook+tags (one extra layer,
	// no further link recursion). Today no-op until Stage 5 emits
	// link_targets; we still issue the query so the path is exercised
	// when those rows appear.
	if rc.Args.Hops >= 2 && len(nbh.LinkTargetNoteIDs) > 0 {
		neighRows, err := selectNotesByID(rc.Surreal, nbh.LinkTargetNoteIDs)
		if err != nil {
			return nil, fmt.Errorf("hop2: %w", err)
		}
		// Recursive neighbourhood — single pass; no further link layer.
		neighIDs := make([]string, 0, len(neighRows))
		for _, r := range neighRows {
			neighIDs = append(neighIDs, r.ID)
		}
		nbh2, err := expandNeighbourhoodNoLinks(rc.Surreal, neighIDs)
		if err != nil {
			return nil, fmt.Errorf("hop2 expand: %w", err)
		}
		for _, r := range neighRows {
			n := convertNoteRow(r, "neighbour")
			attachNotebookFromMap(&n, nbh2.NotebookByNoteID, pathCache, rc.Surreal)
			attachTagsFromMap(&n, nbh2.TagsByNoteID)
			notes = append(notes, n)
		}
	}

	// --include-bodies: parallel GET /notes/:id?fields=body across an
	// 8-worker pool. Per-note failure → null body + stderr line.
	if rc.Args.IncludeBody && rc.Joplin != nil {
		fetchBodies(rc, notes)
	}

	return notes, nil
}

// fetchBodies pulls the full body for every note in `notes` via an
// 8-worker pool. Failures land as null body + a single stderr line
// naming the ID; the rest of the payload still ships.
func fetchBodies(rc *runContext, notes []Note) {
	type job struct {
		idx int
		id  string
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for w := 0; w < bodyFetchWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				note, err := rc.Joplin.GetNote(j.id, "id", "body")
				if err != nil {
					mu.Lock()
					fmt.Fprintf(rc.Err, "qai joplin graph context: fetch body %s: %v\n", j.id, err)
					mu.Unlock()
					continue
				}
				mu.Lock()
				body := note.Body
				notes[j.idx].Body = &body
				mu.Unlock()
			}
		}()
	}
	for i := range notes {
		jobs <- job{idx: i, id: notes[i].ID}
	}
	close(jobs)
	wg.Wait()
	_ = context.Background // reserved for future cancellation plumbing
}

// buildContext assembles the final payload. Pure: no I/O. Same shape
// as Stage 3's classify()/renderHuman() split — the testable seam.
func buildContext(notes []Note, args Args, kind LookupKind, hc joplinbridge.HealthCheck, stale bool, warning string) ContextPayload {
	primary := 0
	for _, n := range notes {
		if n.Match == "primary" {
			primary++
		}
	}
	queryValue := args.Query
	if kind == KindProject {
		queryValue = args.Project
	}
	if kind == KindTag {
		queryValue = args.Tag
		if args.With != "" {
			queryValue = args.Tag + "+" + args.With
		}
	}
	if notes == nil {
		// Empty payload still emits []. A jq pipeline reading
		// .notes[] should never hit a JSON null.
		notes = []Note{}
	}
	return ContextPayload{
		SchemaVersion: schemaVersion,
		Query: QueryDesc{
			Kind:  kind,
			Value: queryValue,
			Hops:  args.Hops,
		},
		Stale:            stale,
		FreshnessWarning: warning,
		Health: HealthSummary{
			Status:    string(hc.Status),
			LagNS:     hc.Lag,
			DataAgeNS: hc.DataAge,
			Backlog:   hc.Backlog,
		},
		Notes: notes,
		Counts: Counts{
			Primary:    primary,
			Neighbours: len(notes) - primary,
			Total:      len(notes),
		},
	}
}

// ────────────────────────────────────────────────────────────────────────
// payload shape
// ────────────────────────────────────────────────────────────────────────

type ContextPayload struct {
	SchemaVersion    int           `json:"schema_version"`
	Query            QueryDesc     `json:"query"`
	Stale            bool          `json:"stale"`
	FreshnessWarning string        `json:"freshness_warning"`
	Health           HealthSummary `json:"health"`
	Notes            []Note        `json:"notes"`
	Counts           Counts        `json:"counts"`
}

type QueryDesc struct {
	Kind  LookupKind `json:"kind"`
	Value string     `json:"value"`
	Hops  int        `json:"hops"`
}

type HealthSummary struct {
	Status    string        `json:"status"`
	LagNS     time.Duration `json:"lag_ns"`
	DataAgeNS time.Duration `json:"data_age_ns"`
	Backlog   int64         `json:"backlog"`
}

type Note struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	Excerpt  string      `json:"excerpt"`
	Body     *string     `json:"body"`
	Notebook NotebookRef `json:"notebook"`
	Tags     []TagRef    `json:"tags"`
	LinksOut []NoteRef   `json:"links_out"`
	LinksIn  []NoteRef   `json:"links_in"`
	Score    float64     `json:"score"`
	Match    string      `json:"match"`
}

type NotebookRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
}

type TagRef struct {
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

type NoteRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type Counts struct {
	Primary    int `json:"primary"`
	Neighbours int `json:"neighbours"`
	Total      int `json:"total"`
}

// convertNoteRow turns a wire-shape noteRow into the public Note,
// stamped with the match kind (primary | neighbour).
func convertNoteRow(r noteRow, match string) Note {
	excerpt := ""
	if r.Excerpt != nil {
		excerpt = *r.Excerpt
	}
	return Note{
		ID:       stripTable(r.ID),
		Title:    r.Title,
		Excerpt:  excerpt,
		Body:     nil,
		Notebook: NotebookRef{},
		Tags:     []TagRef{},
		LinksOut: []NoteRef{},
		LinksIn:  []NoteRef{},
		Score:    r.Score,
		Match:    match,
	}
}

func attachNotebookFromMap(n *Note, byNote map[string]NotebookRef, cache map[string]string, s surrealAPI) {
	if nb, ok := byNote[n.ID]; ok {
		nb.Path = resolveNotebookPath(s, nb.ID, cache)
		n.Notebook = nb
	}
}

func attachTagsFromMap(n *Note, byNote map[string][]TagRef) {
	if ts, ok := byNote[n.ID]; ok {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Title < ts[j].Title })
		n.Tags = ts
	}
}

func attachLinksFromMap(n *Note, out, in map[string][]NoteRef) {
	if rs, ok := out[n.ID]; ok {
		n.LinksOut = rs
	}
	if rs, ok := in[n.ID]; ok {
		n.LinksIn = rs
	}
}

// ────────────────────────────────────────────────────────────────────────
// argparse
// ────────────────────────────────────────────────────────────────────────

// parseArgs walks rawArgs once, accepting value-bearing flags as
// `--name value` and bare flags as standalone tokens. The first
// non-flag is the positional query; subsequent positionals are
// rejected. Returns a populated Args with defaults applied for unset
// numeric fields. --strict and --max-lag compose.
func parseArgs(raw []string) (Args, error) {
	a := Args{
		Hops:   1,
		Limit:  20,
		MaxLag: 0, // 0 means "use joplinbridge.DefaultMaxLag"
	}
	for i := 0; i < len(raw); i++ {
		tok := raw[i]
		switch tok {
		case "--project":
			a.ProjectFlag = true
			if i+1 < len(raw) && !looksLikeFlag(raw[i+1]) {
				a.Project = raw[i+1]
				i++
			}
		case "--tag":
			if i+1 >= len(raw) {
				return a, fmt.Errorf("--tag requires a value")
			}
			a.Tag = raw[i+1]
			i++
		case "--with":
			if i+1 >= len(raw) {
				return a, fmt.Errorf("--with requires a value")
			}
			a.With = raw[i+1]
			i++
		case "--hops":
			if i+1 >= len(raw) {
				return a, fmt.Errorf("--hops requires a value")
			}
			n, err := strconv.Atoi(raw[i+1])
			if err != nil || n < 0 || n > 2 {
				return a, fmt.Errorf("--hops must be 0, 1, or 2 (got %q)", raw[i+1])
			}
			a.Hops = n
			i++
		case "--limit":
			if i+1 >= len(raw) {
				return a, fmt.Errorf("--limit requires a value")
			}
			n, err := strconv.Atoi(raw[i+1])
			if err != nil || n <= 0 {
				return a, fmt.Errorf("--limit must be a positive integer (got %q)", raw[i+1])
			}
			if n > hardLimit {
				return a, fmt.Errorf("--limit %d exceeds hard cap %d", n, hardLimit)
			}
			a.Limit = n
			i++
		case "--include-bodies":
			a.IncludeBody = true
		case "--max-lag":
			if i+1 >= len(raw) {
				return a, fmt.Errorf("--max-lag requires a duration")
			}
			d, err := time.ParseDuration(raw[i+1])
			if err != nil || d <= 0 {
				return a, fmt.Errorf("--max-lag invalid duration: %q", raw[i+1])
			}
			a.MaxLag = d
			i++
		case "--strict":
			a.Strict = true
		case "--explain":
			a.Explain = true
		case "--json":
			// implied; accepted for symmetry with `bridge status --json`
		default:
			if looksLikeFlag(tok) {
				return a, fmt.Errorf("unknown flag: %s", tok)
			}
			if a.Query != "" {
				return a, fmt.Errorf("only one positional query supported (got %q after %q)", tok, a.Query)
			}
			a.Query = tok
		}
	}
	// --with without --tag is meaningless.
	if a.With != "" && a.Tag == "" {
		return a, fmt.Errorf("--with requires --tag")
	}
	return a, nil
}

func looksLikeFlag(s string) bool {
	return len(s) > 1 && s[0] == '-'
}

func hasFlag(args []string, names ...string) bool {
	for _, n := range names {
		for _, a := range args {
			if a == n {
				return true
			}
		}
	}
	return false
}

// ────────────────────────────────────────────────────────────────────────
// Joplin client + bridge-API adapters
// ────────────────────────────────────────────────────────────────────────

// tryLoadJoplin mirrors Stage 3's helper — returns nil on any failure so
// the Health backlog probe degrades to BacklogUnknown rather than
// blocking the read.
func tryLoadJoplin() joplinAPI {
	token, err := joplin.LoadDefaultToken()
	if err != nil {
		return nil
	}
	base := os.Getenv("JOPLIN_URL")
	if base == "" {
		base = "http://127.0.0.1:41184"
	}
	c := joplin.New(joplin.Config{BaseURL: base, Token: token})
	if err := c.Ping(); err != nil {
		return nil
	}
	return c
}

// bridgeSurreal / bridgeJoplin adapters let us hand our narrower
// graph-context interfaces to joplinbridge.Health without exposing
// every method joplinbridge needs. The adapters embed our value and
// satisfy the bridge surface by delegating GetNote/GetEvents (the only
// methods Health() actually exercises today via the backlog probe).
type bridgeJoplin struct {
	j joplinAPI
}

func (b bridgeJoplin) ListFolders() ([]joplin.Folder, error)            { return nil, nil }
func (b bridgeJoplin) ListNotesFull(string, []string) ([]joplin.Note, error) {
	return nil, nil
}
func (b bridgeJoplin) ListTags() ([]joplin.Tag, error)                    { return nil, nil }
func (b bridgeJoplin) GetNoteTags(string) ([]joplin.Tag, error)          { return nil, nil }
func (b bridgeJoplin) GetNote(id string, fields ...string) (*joplin.Note, error) {
	return b.j.GetNote(id, fields...)
}
func (b bridgeJoplin) GetFolder(string) (*joplin.Folder, error) { return nil, nil }
func (b bridgeJoplin) GetEvents(cursor string, limit int) (*joplin.EventsResponse, error) {
	return b.j.GetEvents(cursor, limit)
}

func adaptSurrealForBridge(s surrealAPI) bridgeSurrealAPI { return bridgeSurreal{s: s} }
func adaptJoplinForBridge(j joplinAPI) bridgeJoplinAPI {
	if j == nil {
		return nil
	}
	return bridgeJoplin{j: j}
}

// bridgeSurrealAPI / bridgeJoplinAPI are the interface aliases of the
// joplinbridge unexported types. Go doesn't let us name them directly,
// but the Health() signature is structural — anything that satisfies
// the method set works. We pass concrete types.
type bridgeSurrealAPI = interface {
	Exec(string) ([]blast.StatementResult, error)
}
type bridgeJoplinAPI = interface {
	ListFolders() ([]joplin.Folder, error)
	ListNotesFull(string, []string) ([]joplin.Note, error)
	ListTags() ([]joplin.Tag, error)
	GetNoteTags(string) ([]joplin.Tag, error)
	GetEvents(string, int) (*joplin.EventsResponse, error)
	GetNote(string, ...string) (*joplin.Note, error)
	GetFolder(string) (*joplin.Folder, error)
}

type bridgeSurreal struct {
	s surrealAPI
}

func (b bridgeSurreal) Exec(sql string) ([]blast.StatementResult, error) {
	return b.s.Exec(sql)
}

// ────────────────────────────────────────────────────────────────────────
// help text
// ────────────────────────────────────────────────────────────────────────

const helpGraph = `qai joplin graph — read the notes_graph

The READ side of the joplin-bridge subsystem. 'bridge' writes /
maintains; 'graph' queries.

USAGE
  qai joplin graph context [query] [flags]     Agent-memory bundle as JSON

Run 'qai joplin graph context --help' for the full flag list.`

const helpContext = `qai joplin graph context — agent-memory READ verb

USAGE
  qai joplin graph context                     Resolve --project from cwd
  qai joplin graph context "query"             Full-text search title + excerpt
  qai joplin graph context --project NAME      Notes tagged project:NAME
  qai joplin graph context --tag NAME          Notes with tag NAME
  qai joplin graph context --tag A --with B    Notes carrying BOTH tags

FLAGS
  --hops N            Neighbourhood depth (0|1|2, default 1)
  --limit N           Primary hits cap (default 20, hard cap 200)
  --include-bodies    Fetch full bodies from Joplin (off by default)
  --max-lag DUR       Freshness threshold for the gate (default 10m)
  --strict            Refuse on !OK instead of serving-with-label
  --explain           Print emitted SurrealQL and exit (no execution)

OUTPUT
  One JSON object per invocation. See the Stage 4 spec for the schema.

FRESHNESS GATE
  Default behaviour serves stale data with a 'stale: true' label and
  populates 'freshness_warning'. Use --strict for cron-style consumers
  that prefer empty-and-fail. The asymmetry with 'bridge status --check'
  is intentional: that verb's job is operational health; this verb's
  job is feeding an agent its memory, where labelled-degraded beats
  fail-closed.

EXIT CODES
  0  served (healthy / degraded / stale-non-strict)
  1  --strict refused (stale / error / no-tail / never-run)
  2  --strict refused (bootstrap)
  3  invocation error (bad flag / Surreal unreachable / etc.)`
