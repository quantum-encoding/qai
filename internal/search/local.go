// local.go — Local SurrealDB search (vector, text, read, list, similar).
//
// Uses the local SurrealDB instance managed by `qai db`.
// Shares package-level Cfg with native.go and rag.go.

package search

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/db"
	"github.com/quantum-encoding/qai-cli/internal/embedding"
	"github.com/quantum-encoding/qai-cli/internal/strutil"
)

// LocalSurreal embeds the query and runs vector similarity search against
// the local SurrealDB instance. Filters by dimension to match the query
// embedding size so mixed-provider corpora don't collide.
func LocalSurreal(query, provider string, limit int) {
	fmt.Printf("\n🔍 Local RAG Search: %q", query)
	if provider != "" {
		fmt.Printf(" [provider: %s]", provider)
	}
	fmt.Println()

	embs, err := embedding.Dispatch(Cfg, []string{query})
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --local: embed failed: %v\n", err)
		os.Exit(1)
	}
	if len(embs) == 0 || len(embs[0]) == 0 {
		fmt.Fprintln(os.Stderr, "qai search --local: empty embedding")
		os.Exit(1)
	}

	queryDim := len(embs[0])
	fmt.Printf("  query dimension: %d (%s/%s)\n", queryDim, Cfg.Embeddings.Provider, Cfg.Embeddings.Model)

	embJSON, _ := json.Marshal(embs[0])
	whereClauses := []string{
		"embedding != NONE",
		fmt.Sprintf("dimension = %d", queryDim),
	}
	if provider != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("provider = '%s'", strutil.EscapeSurQL(provider)))
	}
	sql := fmt.Sprintf(
		"SELECT text, provider, source_file, title, clearance, vector::similarity::cosine(embedding, %s) AS score FROM doc_chunk WHERE %s ORDER BY score DESC LIMIT %d;",
		string(embJSON), strings.Join(whereClauses, " AND "), limit,
	)

	body := postLocalSQL(sql, 120*time.Second, "qai search --local")

	var stmts []struct {
		Status string `json:"status"`
		Result []struct {
			Text       string  `json:"text"`
			Provider   string  `json:"provider"`
			SourceFile string  `json:"source_file"`
			Title      string  `json:"title"`
			Clearance  *int    `json:"clearance"`
			Score      float64 `json:"score"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &stmts); err != nil || len(stmts) == 0 {
		fmt.Fprintf(os.Stderr, "qai search --local: parse response: %v\n", err)
		os.Exit(1)
	}
	if stmts[0].Status == "ERR" {
		fmt.Fprintf(os.Stderr, "qai search --local: query error: %s\n", string(body))
		os.Exit(1)
	}

	results := stmts[0].Result
	if len(results) == 0 {
		fmt.Printf("\nNo results found (searched %d-dim vectors", queryDim)
		if provider != "" {
			fmt.Printf(", provider=%s", provider)
		}
		fmt.Println(").")

		dimResult := db.Query("SELECT dimension, count() AS n FROM doc_chunk GROUP BY dimension;")
		if dimResult != "" {
			fmt.Println("Available dimensions in DB:")
			var dimStmts []struct {
				Result []struct {
					Dimension *int `json:"dimension"`
					N         int  `json:"n"`
				} `json:"result"`
			}
			if json.Unmarshal([]byte(dimResult), &dimStmts) == nil && len(dimStmts) > 0 {
				for _, d := range dimStmts[0].Result {
					dim := "nil"
					if d.Dimension != nil {
						dim = fmt.Sprintf("%d", *d.Dimension)
					}
					fmt.Printf("  %s-dim: %d chunks\n", dim, d.N)
				}
			}
		}
		return
	}

	fmt.Println()
	for i, r := range results {
		preview := r.Text
		if len(preview) > 300 {
			preview = preview[:300]
		}
		preview = strings.ReplaceAll(preview, "\n", "\n  ")
		label := r.SourceFile
		if r.Title != "" {
			label = r.Title
		}
		fmt.Printf("── %d. [%s] %s (score: %.4f) ──\n", i+1, r.Provider, label, r.Score)
		fmt.Printf("  %s\n", strings.TrimSpace(preview))
		if len(r.Text) > 300 {
			fmt.Println("  ...")
		}
		fmt.Println()
	}
	fmt.Printf("📊 %d results from local SurrealDB (%d-dim)\n", len(results), queryDim)
}

// LocalText does a text-based search against local SurrealDB — no embedding
// needed. Splits the query into words; any word matching text/title/source_file
// is a hit.
func LocalText(query, provider string, limit int) {
	fmt.Printf("\n🔍 Local Text Search: %q", query)
	if provider != "" {
		fmt.Printf(" [provider: %s]", provider)
	}
	fmt.Println()

	words := strings.Fields(query)
	var wordClauses []string
	for _, w := range words {
		ew := strings.ToLower(strutil.EscapeSurQL(w))
		wordClauses = append(wordClauses, fmt.Sprintf(
			"(string::lowercase(text) CONTAINS '%s' OR string::lowercase(title) CONTAINS '%s' OR string::lowercase(source_file) CONTAINS '%s')",
			ew, ew, ew))
	}
	whereClauses := []string{"(" + strings.Join(wordClauses, " OR ") + ")"}
	if provider != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("provider = '%s'", strutil.EscapeSurQL(provider)))
	}
	sql := fmt.Sprintf(
		"SELECT text, provider, source_file, title, clearance FROM doc_chunk WHERE %s LIMIT %d;",
		strings.Join(whereClauses, " AND "), limit,
	)

	body := postLocalSQL(sql, 30*time.Second, "qai search --text")

	var stmts []struct {
		Status string `json:"status"`
		Result []struct {
			Text       string `json:"text"`
			Provider   string `json:"provider"`
			SourceFile string `json:"source_file"`
			Title      string `json:"title"`
			Clearance  *int   `json:"clearance"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &stmts); err != nil || len(stmts) == 0 {
		fmt.Fprintf(os.Stderr, "qai search --text: parse response: %v\n", err)
		os.Exit(1)
	}
	if stmts[0].Status == "ERR" {
		fmt.Fprintf(os.Stderr, "qai search --text: query error: %s\n", string(body))
		os.Exit(1)
	}

	results := stmts[0].Result
	if len(results) == 0 {
		fmt.Println("\nNo results found.")
		return
	}

	fmt.Println()
	for i, r := range results {
		preview := r.Text
		if len(preview) > 300 {
			preview = preview[:300]
		}
		preview = strings.ReplaceAll(preview, "\n", "\n  ")
		label := r.SourceFile
		if r.Title != "" {
			label = r.Title
		}
		fmt.Printf("── %d. [%s] %s ──\n", i+1, r.Provider, label)
		fmt.Printf("  %s\n", strings.TrimSpace(preview))
		if len(r.Text) > 300 {
			fmt.Println("  ...")
		}
		fmt.Println()
	}
	fmt.Printf("📊 %d results from local SurrealDB (text search)\n", len(results))
}

// ReadLocalFile retrieves the full stored content of a file from SurrealDB.
func ReadLocalFile(sourceFile, provider string) {
	escapedFile := strutil.EscapeSurQL(sourceFile)
	whereClauses := []string{
		fmt.Sprintf("string::lowercase(source_file) CONTAINS '%s'", strings.ToLower(escapedFile)),
	}
	if provider != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("provider = '%s'", strutil.EscapeSurQL(provider)))
	}
	sql := fmt.Sprintf(
		"SELECT text, source_file, provider, title FROM doc_chunk WHERE %s LIMIT 1;",
		strings.Join(whereClauses, " AND "),
	)

	body := postLocalSQL(sql, 30*time.Second, "qai search --read")

	var stmts []struct {
		Status string `json:"status"`
		Result []struct {
			Text       string `json:"text"`
			SourceFile string `json:"source_file"`
			Provider   string `json:"provider"`
			Title      string `json:"title"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &stmts) != nil || len(stmts) == 0 || len(stmts[0].Result) == 0 {
		fmt.Fprintf(os.Stderr, "No file found matching %q\n", sourceFile)
		os.Exit(1)
	}

	r := stmts[0].Result[0]
	fmt.Fprintf(os.Stderr, "── [%s] %s (%d chars) ──\n", r.Provider, r.SourceFile, len(r.Text))
	fmt.Print(r.Text)
}

// ListLocalFiles lists all source files in a provider (or across all providers
// when provider is empty).
func ListLocalFiles(provider string) {
	whereClauses := []string{}
	if provider != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("provider = '%s'", strutil.EscapeSurQL(provider)))
	}
	where := ""
	if len(whereClauses) > 0 {
		where = " WHERE " + strings.Join(whereClauses, " AND ")
	}
	sql := fmt.Sprintf("SELECT source_file, provider, string::len(text) AS size FROM doc_chunk%s ORDER BY provider, source_file;", where)

	body := postLocalSQL(sql, 30*time.Second, "qai search --list")

	var stmts []struct {
		Status string `json:"status"`
		Result []struct {
			SourceFile string `json:"source_file"`
			Provider   string `json:"provider"`
			Size       int    `json:"size"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &stmts) != nil || len(stmts) == 0 {
		fmt.Fprintln(os.Stderr, "No files found.")
		os.Exit(1)
	}

	results := stmts[0].Result
	lastProvider := ""
	for _, r := range results {
		if r.Provider != lastProvider {
			if lastProvider != "" {
				fmt.Println()
			}
			fmt.Printf("── %s ──\n", r.Provider)
			lastProvider = r.Provider
		}
		fmt.Printf("  %-60s %6d chars\n", r.SourceFile, r.Size)
	}
	fmt.Fprintf(os.Stderr, "\n%d files total\n", len(results))
}

// LocalSimilar uses an existing vector from SurrealDB to find similar chunks.
// No embedding model needed — uses the stored vectors directly.
func LocalSimilar(sourceFile, provider string, limit int) {
	fmt.Printf("\n🔍 Similar to: %q", sourceFile)
	if provider != "" {
		fmt.Printf(" [provider: %s]", provider)
	}
	fmt.Println()

	escapedFile := strutil.EscapeSurQL(sourceFile)
	lookupSQL := fmt.Sprintf(
		"SELECT embedding, dimension, provider FROM doc_chunk WHERE source_file CONTAINS '%s' LIMIT 1;",
		escapedFile,
	)

	body := postLocalSQL(lookupSQL, 30*time.Second, "qai search --similar")

	var lookup []struct {
		Status string `json:"status"`
		Result []struct {
			Embedding []float64 `json:"embedding"`
			Dimension int       `json:"dimension"`
			Provider  string    `json:"provider"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &lookup) != nil || len(lookup) == 0 || len(lookup[0].Result) == 0 {
		fmt.Fprintf(os.Stderr, "qai search --similar: no chunk found matching %q\n", sourceFile)
		fmt.Fprintln(os.Stderr, "Tip: use a partial path, e.g. 'Allocator.zig' or 'hash/mod.rs'")
		os.Exit(1)
	}

	vec := lookup[0].Result[0].Embedding
	dim := lookup[0].Result[0].Dimension
	srcProvider := lookup[0].Result[0].Provider
	fmt.Printf("  found in provider %q (%d-dim)\n", srcProvider, dim)

	embJSON, _ := json.Marshal(vec)
	whereClauses := []string{
		"embedding != NONE",
		fmt.Sprintf("dimension = %d", dim),
	}
	if provider != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("provider = '%s'", strutil.EscapeSurQL(provider)))
	}
	sql := fmt.Sprintf(
		"SELECT source_file, provider, title, vector::similarity::cosine(embedding, %s) AS score FROM doc_chunk WHERE %s ORDER BY score DESC LIMIT %d;",
		string(embJSON), strings.Join(whereClauses, " AND "), limit+1, // +1 to skip self
	)

	body2 := postLocalSQL(sql, 120*time.Second, "qai search --similar")

	var stmts []struct {
		Status string `json:"status"`
		Result []struct {
			SourceFile string  `json:"source_file"`
			Provider   string  `json:"provider"`
			Title      string  `json:"title"`
			Score      float64 `json:"score"`
		} `json:"result"`
	}
	if json.Unmarshal(body2, &stmts) != nil || len(stmts) == 0 {
		fmt.Fprintln(os.Stderr, "qai search --similar: parse error")
		os.Exit(1)
	}

	results := stmts[0].Result
	fmt.Println()
	shown := 0
	for _, r := range results {
		if r.Score > 0.999 {
			continue // skip self
		}
		shown++
		label := r.SourceFile
		if r.Title != "" {
			label = r.Title
		}
		fmt.Printf("  %.4f  [%s]  %s\n", r.Score, r.Provider, label)
		if shown >= limit {
			break
		}
	}
	fmt.Printf("\n📊 %d similar chunks (cosine similarity)\n", shown)
}

// postLocalSQL POSTs a SurrealQL statement to the local SurrealDB instance
// and returns the response body. Exits the process on connection failure —
// callers are CLI commands that fatal on error.
func postLocalSQL(sql string, timeout time.Duration, label string) []byte {
	conn := Cfg.LocalConn()
	req, _ := http.NewRequest("POST", conn.URL+"/sql", strings.NewReader(sql))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+conn.Auth)
	req.Header.Set("Surreal-NS", conn.NS)
	req.Header.Set("Surreal-DB", conn.DB)

	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: SurrealDB connection failed: %v\n", label, err)
		fmt.Fprintln(os.Stderr, "Is SurrealDB running? Start with: qai db start")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}
