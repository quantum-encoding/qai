// Package patterns — qai patterns — Failure-pattern taxonomy CLI.
//
// Subcommands:
//
//	qai patterns init                            load failure-layer schema (idempotent)
//	qai patterns import <file.surql>             apply a seed file
//	qai patterns import --seed                   apply the embedded seed taxonomy
//	qai patterns list [--category C] [--role R]  browse the taxonomy
//	qai patterns show <slug>                     full detail (detection + remediation + triggers + tags)
//	qai patterns triggers <import-or-symbol>     which patterns match this trigger?
//	qai patterns review <repo>:<file>            given a file, what to ask (role-ranked)
//	qai patterns coverage                        category × count + trigger coverage
//	qai patterns schema                          print embedded schema
//	qai patterns seed                            print embedded seed
//	qai patterns health                          ping the DB
//
// Re-uses internal/blast's HTTP client so credential policy, env vars, and
// the no-default-credentials guard apply unchanged. Targets the same
// blast_radius DB by default — the failure layer composes with qai's
// existing repo / api_endpoint / error_code graph.
package patterns

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/blast"
)

//go:embed schema.surql
var embeddedSchema string

//go:embed seed.surql
var embeddedSeed string

//go:embed affinity.surql
var embeddedAffinity string

func Cmd(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "init":
		runInit(rest)
	case "import":
		runImport(rest)
	case "list":
		runList(rest)
	case "show":
		runShow(rest)
	case "triggers":
		runTriggers(rest)
	case "review":
		runReview(rest)
	case "coverage":
		runCoverage(rest)
	case "schema":
		fmt.Print(embeddedSchema)
	case "seed":
		fmt.Print(embeddedSeed)
	case "health", "ping":
		runHealth(rest)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "qai patterns: unknown action %q\n", verb)
		usage()
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// Connection helpers — reuse blast's Options + Client unchanged
// ---------------------------------------------------------------------------

// connectFlags registers --url / --user / --pass / --ns / --db on fs with
// blast's defaults, then resolves them into an Options on Parse.
func connectFlags(fs *flag.FlagSet) func() blast.Options {
	def := blast.DefaultOptions()
	url := fs.String("url", def.URL, "SurrealDB endpoint")
	user := fs.String("user", def.User, "username (env QAI_SURREAL_USER)")
	pass := fs.String("pass", def.Pass, "password (env QAI_SURREAL_PASS)")
	ns := fs.String("ns", def.NS, "namespace")
	db := fs.String("db", def.DB, "database")
	return func() blast.Options {
		return blast.Options{URL: *url, User: *user, Pass: *pass, NS: *ns, DB: *db}
	}
}

// mustClient returns a validated client or exits with a clear message.
func mustClient(opts blast.Options) *blast.Client {
	if err := opts.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "qai patterns: "+err.Error())
		os.Exit(2)
	}
	return blast.NewClient(opts)
}

// ---------------------------------------------------------------------------
// init / import / health
// ---------------------------------------------------------------------------

func runInit(args []string) {
	fs := flag.NewFlagSet("patterns init", flag.ExitOnError)
	getOpts := connectFlags(fs)
	fs.Parse(args)
	c := mustClient(getOpts())

	results, err := c.Exec(embeddedSchema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns init: %v\n", err)
		os.Exit(1)
	}
	if err := blast.FirstError(results); err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns init: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("patterns schema applied to %s (ns=%s db=%s)\n", c.Opts().URL, c.Opts().NS, c.Opts().DB)
}

func runImport(args []string) {
	fs := flag.NewFlagSet("patterns import", flag.ExitOnError)
	useSeed := fs.Bool("seed", false, "apply the embedded seed taxonomy (24-qai-failure-patterns)")
	useAffinity := fs.Bool("affinity", false, "apply the embedded role-affinity + polarity backfill (25-qai-pattern-role-affinity)")
	getOpts := connectFlags(fs)
	fs.Parse(args)

	var body string
	switch {
	case *useSeed:
		body = embeddedSeed
	case *useAffinity:
		body = embeddedAffinity
	case fs.NArg() == 1:
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "qai patterns import: read %s: %v\n", fs.Arg(0), err)
			os.Exit(1)
		}
		body = string(raw)
	default:
		fmt.Fprintln(os.Stderr, "qai patterns import: provide a file path, --seed, or --affinity")
		os.Exit(2)
	}

	c := mustClient(getOpts())
	results, err := c.Exec(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns import: %v\n", err)
		os.Exit(1)
	}
	if err := blast.FirstError(results); err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns import: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("import OK (%d statements)\n", len(results))
}

func runHealth(args []string) {
	fs := flag.NewFlagSet("patterns health", flag.ExitOnError)
	getOpts := connectFlags(fs)
	fs.Parse(args)
	c := blast.NewClient(getOpts())
	v, err := c.Health()
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ unreachable: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ %s\n", v)
}

// ---------------------------------------------------------------------------
// Query result shapes
// ---------------------------------------------------------------------------

type patternRow struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Version     int      `json:"version"`
	Category    string   `json:"category"`
	Summary     string   `json:"summary"`
	Detection   string   `json:"detection"`
	Remediation string   `json:"remediation"`
	Tags        []string `json:"tags"`
	Triggers    []string `json:"triggers"`
}

// firstResult unmarshals the first statement's result array into v.
func firstResult(results []blast.StatementResult, v any) error {
	if len(results) == 0 {
		return errors.New("no statements returned")
	}
	if results[0].Status != "OK" {
		return fmt.Errorf("%s: %s", results[0].Status, results[0].Detail)
	}
	return json.Unmarshal(results[0].Result, v)
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func runList(args []string) {
	fs := flag.NewFlagSet("patterns list", flag.ExitOnError)
	category := fs.String("category", "", "filter by category (e.g. ssrf, billing)")
	role := fs.String("role", "", "filter by file role this pattern has bitten (requires edge_case rows)")
	getOpts := connectFlags(fs)
	fs.Parse(args)
	c := mustClient(getOpts())

	var sql string
	switch {
	case *role != "":
		// Patterns that have a historical edge_case manifests_in_file with this role.
		sql = fmt.Sprintf(
			`SELECT VALUE out.* FROM instance_of WHERE in IN (SELECT VALUE in FROM manifests_in_file WHERE out.role = %s);`,
			quote(*role),
		)
	case *category != "":
		sql = fmt.Sprintf(
			`SELECT id, slug, name, category, array::len(triggers ?? []) AS trig FROM failure_pattern WHERE category = %s ORDER BY slug;`,
			quote(*category),
		)
	default:
		sql = `SELECT id, slug, name, category, array::len(triggers ?? []) AS trig FROM failure_pattern ORDER BY category, slug;`
	}

	results, err := c.Exec(sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns list: %v\n", err)
		os.Exit(1)
	}
	var rows []struct {
		patternRow
		Trig int `json:"trig"`
	}
	if err := firstResult(results, &rows); err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns list: %v\n", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Println("(no patterns match)")
		return
	}
	for _, r := range rows {
		trigMark := " "
		if r.Trig > 0 {
			trigMark = "*"
		}
		fmt.Printf("%s %-14s  %-44s  %s\n", trigMark, r.Category, r.Slug, r.Name)
	}
	fmt.Printf("\n%d patterns (* = has triggers)\n", len(rows))
}

// ---------------------------------------------------------------------------
// show
// ---------------------------------------------------------------------------

func runShow(args []string) {
	fs := flag.NewFlagSet("patterns show", flag.ExitOnError)
	getOpts := connectFlags(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "qai patterns show: pass exactly one slug")
		os.Exit(2)
	}
	c := mustClient(getOpts())

	sql := fmt.Sprintf(
		`SELECT id, slug, name, category, version, summary, detection, remediation, tags, triggers
         FROM failure_pattern WHERE slug = %s LIMIT 1;`,
		quote(fs.Arg(0)),
	)
	results, err := c.Exec(sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns show: %v\n", err)
		os.Exit(1)
	}
	var rows []patternRow
	if err := firstResult(results, &rows); err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns show: %v\n", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "no pattern with slug %q\n", fs.Arg(0))
		os.Exit(1)
	}
	p := rows[0]
	fmt.Printf("%s  (%s v%d)\n", p.Name, p.Category, p.Version)
	fmt.Printf("slug:        %s\n", p.Slug)
	fmt.Printf("\nsummary:\n  %s\n", p.Summary)
	if p.Detection != "" {
		fmt.Printf("\ndetection:\n  %s\n", p.Detection)
	}
	if p.Remediation != "" {
		fmt.Printf("\nremediation:\n  %s\n", p.Remediation)
	}
	if len(p.Tags) > 0 {
		fmt.Printf("\ntags:        %s\n", strings.Join(p.Tags, ", "))
	}
	if len(p.Triggers) > 0 {
		fmt.Printf("triggers:    %s\n", strings.Join(p.Triggers, ", "))
	}
}

// ---------------------------------------------------------------------------
// triggers — import-keyed interceptor lookup
// ---------------------------------------------------------------------------

func runTriggers(args []string) {
	fs := flag.NewFlagSet("patterns triggers", flag.ExitOnError)
	getOpts := connectFlags(fs)
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "qai patterns triggers: pass one or more imports/symbols")
		os.Exit(2)
	}

	symbols := fs.Args()
	parts := make([]string, len(symbols))
	for i, s := range symbols {
		parts[i] = quote(s)
	}
	c := mustClient(getOpts())

	sql := fmt.Sprintf(
		`SELECT slug, name, category, detection, remediation, tags
         FROM failure_pattern
         WHERE triggers CONTAINSANY [%s]
         ORDER BY category, slug;`,
		strings.Join(parts, ", "),
	)
	results, err := c.Exec(sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns triggers: %v\n", err)
		os.Exit(1)
	}
	var rows []patternRow
	if err := firstResult(results, &rows); err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns triggers: %v\n", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Printf("(no patterns matched %s)\n", strings.Join(symbols, ", "))
		return
	}
	for _, r := range rows {
		fmt.Printf("[%s] %s\n", r.Category, r.Name)
		if r.Detection != "" {
			fmt.Printf("    Q: %s\n", r.Detection)
		}
		if r.Remediation != "" {
			fmt.Printf("    →  %s\n", r.Remediation)
		}
		fmt.Println()
	}
}

// review implementation lives in review.go (three-query union + --diff).

// ---------------------------------------------------------------------------
// coverage — sanity / observability
// ---------------------------------------------------------------------------

func runCoverage(args []string) {
	fs := flag.NewFlagSet("patterns coverage", flag.ExitOnError)
	getOpts := connectFlags(fs)
	fs.Parse(args)
	c := mustClient(getOpts())

	sql := `SELECT category,
                   count() AS total,
                   count(IF array::len(triggers ?? []) > 0 THEN 1 ELSE NONE END) AS with_triggers
            FROM failure_pattern GROUP BY category ORDER BY category;`
	results, err := c.Exec(sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns coverage: %v\n", err)
		os.Exit(1)
	}
	var rows []struct {
		Category      string `json:"category"`
		Total         int    `json:"total"`
		WithTriggers  int    `json:"with_triggers"`
	}
	if err := firstResult(results, &rows); err != nil {
		fmt.Fprintf(os.Stderr, "qai patterns coverage: %v\n", err)
		os.Exit(1)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Total > rows[j].Total })

	fmt.Printf("%-22s %8s %14s\n", "category", "patterns", "with_triggers")
	fmt.Println(strings.Repeat("─", 46))
	var tot, twt int
	for _, r := range rows {
		fmt.Printf("%-22s %8d %14d\n", r.Category, r.Total, r.WithTriggers)
		tot += r.Total
		twt += r.WithTriggers
	}
	fmt.Println(strings.Repeat("─", 46))
	fmt.Printf("%-22s %8d %14d\n", "TOTAL", tot, twt)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// quote produces a SurrealQL string literal via JSON round-trip — same trick
// as internal/blast/ingest.go:quote, copied (not imported) to keep the
// surface area of the blast dependency to types + client only.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func splitCSV(s string) []string {
	out := strings.Split(s, ",")
	for i := range out {
		out[i] = strings.TrimSpace(out[i])
	}
	return out
}

func usage() {
	fmt.Fprint(os.Stderr, `qai patterns — failure-pattern taxonomy

Commands:
  qai patterns init                            load failure-layer schema (idempotent)
  qai patterns import <file.surql>             apply a seed file
  qai patterns import --seed                   apply the embedded seed taxonomy
  qai patterns list [--category C] [--role R]  browse the taxonomy
  qai patterns show <slug>                     full detail for one pattern
  qai patterns triggers <imp1> [<imp2>...]     patterns whose triggers match these imports/symbols
  qai patterns review <repo>:<file>            role-ranked review (add --imports a,b,c to combine)
  qai patterns coverage                        category × count + trigger coverage
  qai patterns schema                          print embedded schema (23-qai-extend-blast)
  qai patterns seed                            print embedded seed (24-qai-failure-patterns)
  qai patterns health                          ping the DB

Connection (same env as qai blast):
  QAI_SURREAL_URL  (default http://127.0.0.1:8000)
  QAI_SURREAL_USER (required — no default)
  QAI_SURREAL_PASS (required — no default)
  QAI_SURREAL_NS   (default quantumencoding)
  QAI_SURREAL_DB   (default blast_radius)
`)
}
