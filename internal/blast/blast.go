// Package blast — qai blast — Blast Radius Mapper CLI.
//
// Subcommands:
//
//	qai blast init                          load schema (idempotent)
//	qai blast ingest <polyrepo-path>        scan + populate graph
//	qai blast list                          print available query presets
//	qai blast run    <preset-id>            execute a preset → terminal table
//	qai blast query  "<raw-surql>"          execute custom SurrealQL → terminal table
//	qai blast schema                        print embedded schema to stdout
//	qai blast health                        ping the DB
//
// Security policy: USER + PASS have NO compiled-in defaults. The CLI
// fails fast if QAI_SURREAL_USER / QAI_SURREAL_PASS aren't set (or
// --user / --pass flags supplied). URL / NS / DB do have safe defaults
// because they're not credentials.

package blast

import (
	_ "embed"
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

//go:embed schema.surql
var embeddedSchema string

func Cmd(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	verb := args[0]
	rest := args[1:]

	switch verb {
	case "init":
		runInit(rest)
	case "ingest":
		runIngest(rest)
	case "list", "presets":
		runList(rest)
	case "run":
		runPreset(rest)
	case "query", "q":
		runQuery(rest)
	case "schema":
		fmt.Print(embeddedSchema)
	case "health", "ping":
		runHealth(rest)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "qai blast: unknown subcommand %q\n\n", verb)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: qai blast <subcommand> [args]

Blast Radius Mapper — track contract changes (API endpoints, SDK structs,
error codes) across a polyrepo. Backed by SurrealDB v3.

Subcommands:
  init                          Load the schema. Idempotent.
  ingest <polyrepo-path>        Scan the polyrepo and populate the graph.
  list                          List built-in query presets.
  run <preset-id>               Run a preset → terminal table.
  query "<raw-surql>"           Run custom SurrealQL → terminal table.
  schema                        Print the embedded schema to stdout.
  health                        Ping the SurrealDB instance.

Output (run / query):
  --format table   Aligned terminal table via text/tabwriter (default)
  --format json    Indented JSON of each statement's result array
  --format jsonl   One JSON object per row, one row per line
  --format raw     The whole SurrealDB /sql response unchanged

Connection (all subcommands):
  --url <url>      SurrealDB URL          (default http://127.0.0.1:8000)
  --user <user>    Username               (REQUIRED — no default)
  --pass <pass>    Password               (REQUIRED — no default)
  --ns <ns>        Namespace              (default quantumencoding)
  --db <db>        Database               (default blast_radius)

Env vars (override flag defaults; credentials are env-or-flag, no default):
  QAI_SURREAL_URL / QAI_SURREAL_USER / QAI_SURREAL_PASS
  QAI_SURREAL_NS  / QAI_SURREAL_DB

Examples:
  export QAI_SURREAL_USER=root QAI_SURREAL_PASS=root
  qai blast init
  qai blast ingest /Users/director/work/poly-repo/quantum-ai-polyrepo
  qai blast list
  qai blast run handler-gap
  qai blast query "SELECT name, kind FROM repo ORDER BY name;"
`)
}

// addConnFlags wires connection flags. Defaults come from DefaultOptions
// (env-aware). Credentials still need to be present at Validate() time
// — flag defaults are empty strings, env-derived or user-supplied.
func addConnFlags(fs *flag.FlagSet) *Options {
	def := DefaultOptions()
	o := &Options{}
	fs.StringVar(&o.URL, "url", def.URL, "SurrealDB URL")
	fs.StringVar(&o.User, "user", def.User, "Username (required)")
	fs.StringVar(&o.Pass, "pass", def.Pass, "Password (required)")
	fs.StringVar(&o.NS, "ns", def.NS, "Namespace")
	fs.StringVar(&o.DB, "db", def.DB, "Database")
	return o
}

// mustClient builds a Client after enforcing the credential policy.
// Health() is intentionally skipped here — runInit / runIngest call it
// themselves so they can give context-specific error messages.
func mustClient(opts Options) *Client {
	if err := opts.Validate(); err != nil {
		fail("%v", err)
	}
	return NewClient(opts)
}

// ---------------------------------------------------------------------
// init
// ---------------------------------------------------------------------

func runInit(args []string) {
	fs := flag.NewFlagSet("blast init", flag.ExitOnError)
	opts := addConnFlags(fs)
	_ = fs.Parse(args)

	client := mustClient(*opts)
	if v, err := client.Health(); err != nil {
		fail("connect to %s: %v", opts.URL, err)
	} else {
		fmt.Fprintf(os.Stderr, "connected to %s (%s)\n", opts.URL, v)
	}

	// Bootstrap: ensure ns + db exist BEFORE running schema DEFINEs. The
	// embedded schema deliberately omits `USE NS / DB` (so flags govern
	// the target); that means an uninitialized ns/db would 404 on the
	// first DEFINE TABLE without these guards.
	bootstrap := []string{
		fmt.Sprintf("DEFINE NAMESPACE IF NOT EXISTS %s", opts.NS),
		fmt.Sprintf("USE NS %s", opts.NS),
		fmt.Sprintf("DEFINE DATABASE IF NOT EXISTS %s", opts.DB),
	}
	if _, err := client.Exec(strings.Join(bootstrap, ";\n") + ";"); err != nil {
		fail("bootstrap ns/db: %v", err)
	}

	stmts := splitTopLevelStatements(embeddedSchema)
	fmt.Fprintf(os.Stderr, "loading schema (%d statements) into %s/%s ...\n", len(stmts), opts.NS, opts.DB)
	results, err := client.ExecMany(stmts, 50)
	if err != nil {
		fail("schema load: %v", err)
	}
	if err := FirstError(results); err != nil {
		fail("schema load: %v", err)
	}
	fmt.Fprintln(os.Stderr, "schema loaded.")
}

// splitTopLevelStatements breaks a script into individual statements at
// top-level `;` boundaries, stripping comments and blank lines.
func splitTopLevelStatements(script string) []string {
	var out []string
	var buf strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(script))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "--") {
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if strings.HasSuffix(trim, ";") {
			s := strings.TrimSpace(buf.String())
			s = strings.TrimSuffix(s, ";")
			s = strings.TrimSpace(s)
			if s != "" {
				out = append(out, s)
			}
			buf.Reset()
		}
	}
	if s := strings.TrimSpace(buf.String()); s != "" {
		out = append(out, strings.TrimSuffix(s, ";"))
	}
	return out
}

// ---------------------------------------------------------------------
// ingest
// ---------------------------------------------------------------------

func runIngest(args []string) {
	fs := flag.NewFlagSet("blast ingest", flag.ExitOnError)
	opts := addConnFlags(fs)
	noReset := fs.Bool("no-reset", false, "Don't wipe rows before re-inserting (default: wipe)")
	dryRun := fs.Bool("dry-run", false, "Print the generated SurrealQL instead of executing it")
	batchSize := fs.Int("batch", 200, "Statements per HTTP round-trip")
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fail("ingest: missing <polyrepo-path>")
	}
	root := fs.Arg(0)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		fail("ingest: %s is not a directory", root)
	}

	fmt.Fprintf(os.Stderr, "scanning %s ...\n", root)
	rep, err := Scan(root)
	if err != nil {
		fail("scan: %v", err)
	}
	fmt.Fprintf(os.Stderr, "  repos:%d  endpoints:%d  codes:%d  structs:%d  deps:%d  calls:%d  handles:%d  emits:%d\n",
		len(rep.Repos), len(rep.Endpoints), len(rep.ErrorCodes), len(rep.SDKStructs),
		len(rep.DependsOn), len(rep.CallsAPI), len(rep.HandlesError), len(rep.EmitsError))

	stmts := Statements(rep, IngestOpts{Reset: !*noReset, BatchSize: *batchSize})

	if *dryRun {
		for _, s := range stmts {
			fmt.Println(s + ";")
		}
		return
	}

	client := mustClient(*opts)
	if _, err := client.Health(); err != nil {
		fail("connect to %s: %v", opts.URL, err)
	}

	fmt.Fprintf(os.Stderr, "loading %d statements into %s/%s (batch=%d) ...\n", len(stmts), opts.NS, opts.DB, *batchSize)
	results, err := client.ExecMany(stmts, *batchSize)
	if err != nil {
		fail("ingest: %v", err)
	}
	failed := 0
	for _, r := range results {
		if r.Status != "OK" {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "ingest: %d/%d statements failed\n", failed, len(results))
		if err := FirstError(results); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "ingest complete.")
}

// ---------------------------------------------------------------------
// list / run / query
// ---------------------------------------------------------------------

func runList(args []string) {
	w := tabwriterStdout()
	defer w.Flush()
	fmt.Fprintln(w, "ID\tLABEL\tDESCRIPTION")
	fmt.Fprintln(w, "---\t-----\t-----------")
	for _, p := range Presets {
		fmt.Fprintf(w, "%s\t%s\t%s\n", p.ID, p.Label, p.Description)
	}
}

func runPreset(args []string) {
	fs := flag.NewFlagSet("blast run", flag.ExitOnError)
	opts := addConnFlags(fs)
	format := fs.String("format", "table", "Output format: table | json | jsonl | raw")
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fail("run: missing <preset-id>. Run `qai blast list` to see options.")
	}
	preset := FindPreset(fs.Arg(0))
	if preset == nil {
		fail("run: unknown preset %q. Run `qai blast list` to see options.", fs.Arg(0))
	}

	executeAndRender(*opts, preset.SurQL, *format)
}

func runQuery(args []string) {
	fs := flag.NewFlagSet("blast query", flag.ExitOnError)
	opts := addConnFlags(fs)
	format := fs.String("format", "table", "Output format: table | json | jsonl | raw")
	_ = fs.Parse(args)

	if fs.NArg() < 1 {
		fail("query: missing \"<surql>\"")
	}
	surql := strings.Join(fs.Args(), " ")

	executeAndRender(*opts, surql, *format)
}

func executeAndRender(opts Options, surql, format string) {
	client := mustClient(opts)
	results, err := client.Exec(surql)
	if err != nil {
		fail("%v", err)
	}

	switch format {
	case "raw":
		b, _ := json.MarshalIndent(results, "", "  ")
		fmt.Println(string(b))
	case "json":
		all := []json.RawMessage{}
		for _, r := range results {
			if r.Status == "OK" && len(r.Result) > 0 {
				all = append(all, r.Result)
			}
		}
		b, _ := json.MarshalIndent(all, "", "  ")
		fmt.Println(string(b))
	case "jsonl":
		for _, r := range results {
			if r.Status != "OK" {
				continue
			}
			var rows []map[string]any
			if err := json.Unmarshal(r.Result, &rows); err != nil {
				fmt.Println(string(r.Result))
				continue
			}
			for _, row := range rows {
				b, _ := json.Marshal(row)
				fmt.Println(string(b))
			}
		}
	case "table", "":
		RenderTable(os.Stdout, results)
	default:
		fail("unknown --format %q (table | json | jsonl | raw)", format)
	}

	// Non-zero exit if any statement failed, so CI catches it. The
	// per-statement ERR detail was already rendered by the format
	// dispatcher above; this stderr note just makes the exit visible.
	for _, r := range results {
		if r.Status != "OK" {
			fmt.Fprintln(os.Stderr, "qai blast: one or more statements failed (see ERR rows above); exiting non-zero")
			os.Exit(1)
		}
	}
}

// ---------------------------------------------------------------------
// health
// ---------------------------------------------------------------------

func runHealth(args []string) {
	fs := flag.NewFlagSet("blast health", flag.ExitOnError)
	opts := addConnFlags(fs)
	_ = fs.Parse(args)

	// Health is auth-free: we DON'T enforce credentials here. The point
	// of `qai blast health` is to answer "is the server reachable?" —
	// useful pre-flight even before the operator has set credentials.
	client := NewClient(*opts)
	v, err := client.Health()
	if err != nil {
		fail("%v", err)
	}
	fmt.Printf("%s\n  ns=%s db=%s\n  %s\n", opts.URL, opts.NS, opts.DB, v)

	if errors.Is(opts.Validate(), ErrCredentialsMissing) {
		fmt.Fprintln(os.Stderr, "\n⚠ credentials NOT set — `qai blast init / ingest / run / query` will fail until you set QAI_SURREAL_USER and QAI_SURREAL_PASS.")
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "qai blast: "+format+"\n", a...)
	os.Exit(1)
}
