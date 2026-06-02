package blast

// Materializes a ScanReport into SurrealQL CREATE / RELATE statements
// and ships them to a SurrealDB instance.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// quote turns a Go string into a SurrealQL string literal.
// We round-trip through JSON so escaping of quotes / backslashes /
// non-ASCII is correct without us writing an escaper by hand.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func quoteArr(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = quote(x)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// optStr returns NONE for empty strings, quoted literal otherwise.
// SurrealQL's option<T> fields accept NONE explicitly.
func optStr(s string) string {
	if s == "" {
		return "NONE"
	}
	return quote(s)
}

// optInt returns NONE for zero, the integer otherwise.
func optInt(n int) string {
	if n == 0 {
		return "NONE"
	}
	return fmt.Sprintf("%d", n)
}

// Statements turns the report into per-statement SurrealQL strings.
// Caller can ship them via Client.ExecMany.
func Statements(rep *ScanReport, opts IngestOpts) []string {
	var s []string

	if opts.Reset {
		// Wipe all rows but keep schema. ORDER MATTERS: edges first so
		// schemafull IN/OUT constraints don't trip on dangling references
		// while the wipe is in flight.
		for _, t := range []string{
			// Edges first so schemafull IN/OUT constraints don't trip
			// on dangling references while the wipe is in flight.
			"depends_on", "calls_api", "uses_struct", "handles_error", "emits_error",
			// NOTE: code_file is deliberately NOT in the wipe list.
			// Once edge_cases exist with `manifests_in_file` edges
			// pointing at code_file rows, wiping would orphan them on
			// every re-ingest. The CREATE below is UPSERT-shaped on a
			// deterministic ID (cf_<repo>_<slug(path)>) so unchanged
			// files refresh in place and edges survive a re-scan.
			// Stale rows for renamed/deleted files persist (tiny cost);
			// reconcile them with a separate `qai patterns reconcile`
			// pass once that's built.
			"repo", "api_endpoint", "sdk_struct", "error_code",
		} {
			s = append(s, "DELETE "+t)
		}
	}

	for _, r := range rep.Repos {
		s = append(s, fmt.Sprintf(
			"CREATE repo:%s CONTENT { name: %s, kind: %s, language: %s, path: %s }",
			r.ID, quote(r.Name), quote(r.Kind), quote(r.Language), quote(r.Path),
		))
	}

	for _, ep := range rep.Endpoints {
		s = append(s, fmt.Sprintf(
			"CREATE api_endpoint:%s CONTENT { method: %s, route: %s, owner_repo: repo:%s, source_file: %s, description: NONE }",
			ep.ID, quote(ep.Method), quote(ep.Route), ep.OwnerRepo, optStr(ep.SourceFile),
		))
	}

	for _, c := range rep.ErrorCodes {
		s = append(s, fmt.Sprintf(
			"CREATE error_code:%s CONTENT { code_name: %s, category: %s, source_file: %s }",
			c.Wire, quote(c.Wire), quote(c.Category), optStr(c.Source),
		))
	}

	for _, st := range rep.SDKStructs {
		s = append(s, fmt.Sprintf(
			"CREATE sdk_struct:%s CONTENT { name: %s, language: %s, owner_repo: repo:%s, source_file: %s }",
			st.ID, quote(st.Name), quote(st.Language), st.OwnerRepo, optStr(st.SourceFile),
		))
	}

	// code_file rows come from ExtractCodeFiles (role.go drives Role).
	// UPSERT (not CREATE) on the deterministic ID so unchanged files
	// refresh in place — this is the durability guarantee for
	// `manifests_in_file` edges that hang off code_file once edge_cases
	// land. Wiping code_file on Reset is intentionally disabled above
	// for the same reason.
	for _, f := range rep.Files {
		s = append(s, fmt.Sprintf(
			"UPSERT code_file:%s CONTENT { repo: repo:%s, path: %s, language: %s, role: %s, line_count: %s }",
			f.ID, f.RepoID, quote(f.Path), quote(f.Language), quote(f.Role), optInt(f.LineCount),
		))
	}

	for _, d := range rep.DependsOn {
		s = append(s, fmt.Sprintf(
			"RELATE repo:%s->depends_on->repo:%s SET version_constraint = %s, declared_in = %s",
			d.FromRepo, d.ToRepo, quote(d.VersionConstraint), quote(d.DeclaredIn),
		))
	}

	for _, c := range rep.CallsAPI {
		s = append(s, fmt.Sprintf(
			"RELATE repo:%s->calls_api->api_endpoint:%s SET source_files = %s, via_sdk = %t",
			c.FromRepo, c.EndpointID, quoteArr(c.SourceFiles), c.ViaSDK,
		))
	}

	for _, h := range rep.HandlesError {
		s = append(s, fmt.Sprintf(
			"RELATE repo:%s->handles_error->error_code:%s SET source_files = %s, remediation = NONE",
			h.FromRepo, h.CodeWire, quoteArr(h.SourceFiles),
		))
	}

	for _, e := range rep.EmitsError {
		s = append(s, fmt.Sprintf(
			"RELATE api_endpoint:%s->emits_error->error_code:%s SET source_file = %s, call_count = %d, status_code = %s",
			e.EndpointID, e.CodeWire, quote(e.SourceFile), e.CallCount, optInt(e.StatusCode),
		))
	}
	return s
}

// IngestOpts controls reset behavior and batch sizes.
type IngestOpts struct {
	Reset     bool // wipe all rows before re-inserting (default true)
	BatchSize int  // statements per HTTP round-trip (default 200)
}
