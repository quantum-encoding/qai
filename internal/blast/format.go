package blast

// format.go — terminal output rendering.
//
// Uses Go's stdlib text/tabwriter so column alignment is consistent
// across statements and ranks/wraps correctly with multi-byte chars.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// tabwriterStdout is a tiny convenience for short tables (e.g. `blast list`)
// where we just want aligned columns on stdout without rebuilding the
// boilerplate at every call site.
func tabwriterStdout() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

// RenderTable writes a per-statement aligned table to w. Each statement's
// header / rows are flushed independently because their column sets
// differ. Non-row results (scalars, errors, NULL) fall through to a
// labeled line so the developer still sees something useful.
func RenderTable(w io.Writer, results []StatementResult) {
	for i, r := range results {
		fmt.Fprintf(w, "── statement %d", i+1)
		if r.Time != "" {
			fmt.Fprintf(w, " · %s", r.Time)
		}
		fmt.Fprintln(w)

		if r.Status != "OK" {
			detail := r.Detail
			if detail == "" {
				detail = "(no detail)"
			}
			fmt.Fprintf(w, "ERR: %s\n\n", detail)
			continue
		}

		var rows []map[string]any
		if err := json.Unmarshal(r.Result, &rows); err != nil || rows == nil {
			// Scalar / array of non-objects / null — print compact JSON.
			pretty := string(r.Result)
			if len(pretty) == 0 || pretty == "null" {
				fmt.Fprintln(w, "(no result)")
			} else {
				fmt.Fprintln(w, pretty)
			}
			fmt.Fprintln(w)
			continue
		}
		if len(rows) == 0 {
			fmt.Fprintln(w, "(no rows)")
			fmt.Fprintln(w)
			continue
		}

		cols := discoverColumns(rows)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

		// Header line
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
		// Separator line — dashes scaled per header label length so the
		// rule reads as a divider rather than a fixed-width banner.
		dividers := make([]string, len(cols))
		for j, c := range cols {
			dividers[j] = strings.Repeat("-", maxInt(len(c), 3))
		}
		fmt.Fprintln(tw, strings.Join(dividers, "\t"))

		for _, row := range rows {
			cells := make([]string, len(cols))
			for j, c := range cols {
				cells[j] = renderCell(row[c])
			}
			fmt.Fprintln(tw, strings.Join(cells, "\t"))
		}
		tw.Flush()
		fmt.Fprintf(w, "(%d row%s)\n\n", len(rows), pluralS(len(rows)))
	}
}

// discoverColumns picks the column order: descending frequency first
// (so important columns stay leftmost on sparse rows), then alphabetical
// for stable layout across re-runs.
func discoverColumns(rows []map[string]any) []string {
	freq := map[string]int{}
	for _, row := range rows {
		for k := range row {
			freq[k]++
		}
	}
	cols := make([]string, 0, len(freq))
	for k := range freq {
		cols = append(cols, k)
	}
	sort.SliceStable(cols, func(i, j int) bool {
		if freq[cols[i]] != freq[cols[j]] {
			return freq[cols[i]] > freq[cols[j]]
		}
		return cols[i] < cols[j]
	})
	return cols
}

// renderCell turns a JSON value into a single-line terminal cell.
// Multi-line content (e.g. file-list arrays) collapses to comma-
// separated; tabwriter cannot honor embedded newlines mid-row.
func renderCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case string:
		return sanitize(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	case []any:
		// All-strings: join with commas (file lists, blind_spots).
		strs := make([]string, 0, len(x))
		allStrings := true
		for _, el := range x {
			if s, ok := el.(string); ok {
				strs = append(strs, s)
			} else {
				allStrings = false
				break
			}
		}
		if allStrings {
			if len(strs) == 0 {
				return "[]"
			}
			return strings.Join(strs, ", ")
		}
		b, _ := json.Marshal(x)
		return sanitize(string(b))
	default:
		b, _ := json.Marshal(x)
		return sanitize(string(b))
	}
}

func sanitize(s string) string {
	// Strip newlines + tabs so they don't break tabwriter alignment.
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
