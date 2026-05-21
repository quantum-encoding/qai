// search_native.go — Native Go implementations of search commands.
//
// Replaces: node rag-search.mjs, node joplin-search.mjs, brave-cli
// Zero external dependencies — just HTTP calls.

package search

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/embedding"
	"github.com/quantum-encoding/qai-cli/internal/strutil"
)

// ─── Brave Search (replaces brave-cli) ────────────────────────────────────

// Cfg is set by main.
var Cfg *config.Config

const braveBaseURL = "https://api.search.brave.com"

func BraveSearch(query string, count int, freshness string) {
	key := os.Getenv("BRAVE_SEARCH_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "qai search: BRAVE_SEARCH_API_KEY not set")
		fmt.Fprintln(os.Stderr, "  → fix: export BRAVE_SEARCH_API_KEY=<key>  (get one at https://api.search.brave.com/)")
		os.Exit(1)
	}

	params := url.Values{"q": {query}}
	if count > 0 {
		params.Set("count", fmt.Sprintf("%d", count))
	}
	if freshness != "" {
		params.Set("freshness", freshness)
	}

	req, _ := http.NewRequest("GET", braveBaseURL+"/res/v1/web/search?"+params.Encode(), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search: brave search request failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: check network connectivity to api.search.brave.com")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "qai search: brave search returned HTTP %d: %s\n", resp.StatusCode, strutil.TruncateStr(string(body), 200))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			fmt.Fprintln(os.Stderr, "  → fix: verify BRAVE_SEARCH_API_KEY at https://api.search.brave.com/app/dashboard")
		}
		os.Exit(1)
	}

	var result struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	json.Unmarshal(body, &result)

	for i, r := range result.Web.Results {
		fmt.Printf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Description)
	}
}

func BraveSearchJSON(query string, count int) []byte {
	key := os.Getenv("BRAVE_SEARCH_API_KEY")
	if key == "" {
		return nil
	}
	params := url.Values{"q": {query}}
	if count > 0 {
		params.Set("count", fmt.Sprintf("%d", count))
	}
	req, _ := http.NewRequest("GET", braveBaseURL+"/res/v1/web/search?"+params.Encode(), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil
	}
	return body
}

// BraveAsk calls the Brave Answers API (/res/v1/chat/completions) and
// prints the grounded reply plus citations.
//
// Migrated 2026-05-20 from the deprecated Summarizer field that used
// to ride along on /res/v1/web/search?summary=1. Brave deprecated
// that endpoint on 2026-05-17 (90-day sunset). The Answers API is the
// official replacement — see https://api.search.brave.com/app/documentation/answers.
//
// BRAVE_ANSWERS_API_KEY may be set if your Answers subscription uses
// a different key than the Search subscription. Falls back to
// BRAVE_SEARCH_API_KEY when unset (Brave currently provisions a
// single key by default).
func BraveAsk(question string) {
	key := os.Getenv("BRAVE_ANSWERS_API_KEY")
	if key == "" {
		key = os.Getenv("BRAVE_SEARCH_API_KEY")
	}
	if key == "" {
		fmt.Fprintln(os.Stderr, "qai ask: BRAVE_SEARCH_API_KEY not set (or BRAVE_ANSWERS_API_KEY for a separate Answers subscription)")
		fmt.Fprintln(os.Stderr, "  → fix: export BRAVE_SEARCH_API_KEY=<key>  (get one at https://api.search.brave.com/)")
		os.Exit(1)
	}

	body, err := json.Marshal(map[string]any{
		"model":  "brave",
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": question},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai ask: marshal request: %v\n", err)
		os.Exit(1)
	}

	req, _ := http.NewRequest("POST", braveBaseURL+"/res/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai ask: brave answers request failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: check network connectivity to api.search.brave.com")
		os.Exit(1)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "qai ask: brave answers returned HTTP %d: %s\n", resp.StatusCode, strutil.TruncateStr(string(respBody), 200))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			fmt.Fprintln(os.Stderr, "  → fix: verify BRAVE_SEARCH_API_KEY (or BRAVE_ANSWERS_API_KEY) at https://api.search.brave.com/app/dashboard")
		}
		os.Exit(1)
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Citations []struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"citations"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Fprintf(os.Stderr, "qai ask: parse brave answers response: %v\n", err)
		os.Exit(1)
	}

	for _, c := range result.Choices {
		if c.Message.Content != "" {
			fmt.Println(c.Message.Content)
		}
	}
	if len(result.Citations) > 0 {
		fmt.Println()
		fmt.Println("Sources:")
		for i, c := range result.Citations {
			title := c.Title
			if title == "" {
				title = c.URL
			}
			fmt.Printf("  %d. %s\n     %s\n", i+1, title, c.URL)
		}
	}
}

// BraveContext fetches LLM-optimized content chunks for a query via
// the /res/v1/llm/context endpoint and prints the raw JSON for
// downstream LLM consumption.
//
// Migrated 2026-05-20 from /res/v1/web/search?summary=1, which is on
// Brave's 90-day deprecation window (sunset 2026-08-15). The
// llm/context endpoint is purpose-built for this use case and is
// not affected by the Summarizer deprecation.
func BraveContext(query string) {
	key := os.Getenv("BRAVE_SEARCH_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "qai context: BRAVE_SEARCH_API_KEY not set")
		fmt.Fprintln(os.Stderr, "  → fix: export BRAVE_SEARCH_API_KEY=<key>  (get one at https://api.search.brave.com/)")
		os.Exit(1)
	}

	params := url.Values{"q": {query}}
	req, _ := http.NewRequest("GET", braveBaseURL+"/res/v1/llm/context?"+params.Encode(), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai context: brave llm/context request failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: check network connectivity to api.search.brave.com")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "qai context: brave llm/context returned HTTP %d: %s\n", resp.StatusCode, strutil.TruncateStr(string(body), 200))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			fmt.Fprintln(os.Stderr, "  → fix: verify BRAVE_SEARCH_API_KEY at https://api.search.brave.com/app/dashboard")
		}
		os.Exit(1)
	}

	// Print raw JSON for LLM consumption.
	var pretty json.RawMessage
	if json.Unmarshal(body, &pretty) == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		os.Stdout.Write(out)
		fmt.Println()
	}
}

// ─── Joplin Search (replaces node joplin-search.mjs) ─────────────────────

func JoplinSearch(query string, limit int) {
	token := os.Getenv("JOPLIN_TOKEN")
	joplinURL := os.Getenv("JOPLIN_URL")
	if joplinURL == "" {
		joplinURL = "http://localhost:41184"
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "qai search --joplin: JOPLIN_TOKEN not set")
		fmt.Fprintln(os.Stderr, "  → fix: enable the Joplin Web Clipper (Tools → Options → Web Clipper) and export JOPLIN_TOKEN=<token>")
		os.Exit(1)
	}
	if limit <= 0 {
		limit = 10
	}

	// Get folders for notebook name resolution
	folders := joplinFolders(joplinURL, token)

	// Search
	params := url.Values{
		"token":    {token},
		"query":    {query},
		"limit":    {fmt.Sprintf("%d", limit)},
		"fields":   {"id,title,body,parent_id,source_url,updated_time"},
		"order_by": {"relevance"},
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(joplinURL + "/search?" + params.Encode())
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --joplin: cannot reach Joplin at %s: %v\n", joplinURL, err)
		fmt.Fprintln(os.Stderr, "  → fix: launch Joplin and enable the Web Clipper service (Tools → Options → Web Clipper)")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "qai search --joplin: Joplin returned HTTP %d: %s\n", resp.StatusCode, strutil.TruncateStr(string(body), 200))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			fmt.Fprintln(os.Stderr, "  → fix: verify JOPLIN_TOKEN matches the Web Clipper token shown in Tools → Options → Web Clipper")
		}
		os.Exit(1)
	}

	var result struct {
		Items []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Body        string `json:"body"`
			ParentID    string `json:"parent_id"`
			SourceURL   string `json:"source_url"`
			UpdatedTime int64  `json:"updated_time"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) != nil {
		// Try raw array format
		json.Unmarshal(body, &result.Items)
	}

	if len(result.Items) == 0 {
		fmt.Println("No results found.")
		return
	}

	for i, item := range result.Items {
		notebook := folders[item.ParentID]
		if notebook == "" {
			notebook = "unknown"
		}
		updated := time.UnixMilli(item.UpdatedTime).Format("2006-01-02")
		preview := strings.ReplaceAll(item.Body, "\n", " ")
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}

		fmt.Printf("── %d. %s [%s] (%s) ──\n", i+1, item.Title, notebook, updated)
		fmt.Printf("  %s\n\n", strings.TrimSpace(preview))
	}
	fmt.Printf("📊 %d results from Joplin\n", len(result.Items))
}

func joplinFolders(baseURL, token string) map[string]string {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(
		baseURL + "/folders?token=" + token + "&limit=100",
	)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Items []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &result) != nil {
		json.Unmarshal(body, &result.Items)
	}

	m := make(map[string]string)
	for _, f := range result.Items {
		m[f.ID] = f.Title
	}
	return m
}

// ─── SurrealDB Cloud Search (replaces node rag-search.mjs) ───────────────

func CloudSurreal(query, provider string, limit int) {
	if Cfg.Surreal.CloudURL == "" {
		fmt.Fprintln(os.Stderr, "qai search --surreal: no cloud SurrealDB configured")
		fmt.Fprintln(os.Stderr, "  → fix: run `qai init` or set SURREAL_CLOUD_URL / SURREAL_CLOUD_USER / SURREAL_CLOUD_PASS")
		os.Exit(1)
	}

	fmt.Printf("\n🔍 RAG Search: %q", query)
	if provider != "" {
		fmt.Printf(" [provider: %s]", provider)
	}
	fmt.Println()

	// Embed query
	embs, err := embedding.Dispatch(Cfg, []string{query})
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --surreal: embed failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "  → fix: verify embeddings provider %q is reachable and configured (check QAI_API_KEY for qai-broker, or `gcloud auth application-default login` for Vertex)\n", Cfg.Embeddings.Provider)
		os.Exit(1)
	}
	if len(embs) == 0 || len(embs[0]) == 0 {
		fmt.Fprintln(os.Stderr, "qai search --surreal: empty embedding returned from provider")
		fmt.Fprintf(os.Stderr, "  → fix: check embeddings provider %q (%s) returned a non-empty vector\n", Cfg.Embeddings.Provider, Cfg.Embeddings.Model)
		os.Exit(1)
	}

	// Build SQL
	embJSON, _ := json.Marshal(embs[0])
	providerClause := ""
	if provider != "" {
		providerClause = fmt.Sprintf(" AND provider = '%s'", strutil.EscapeSurQL(provider))
	}
	sql := fmt.Sprintf(
		"SELECT text, provider, source_file, vector::similarity::cosine(embedding, %s) AS score FROM doc_chunk WHERE embedding != NONE%s ORDER BY score DESC LIMIT %d;",
		string(embJSON), providerClause, limit,
	)

	// Query cloud SurrealDB
	conn := Cfg.CloudConn()
	req, _ := http.NewRequest("POST", conn.URL+"/sql", strings.NewReader(sql))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+conn.Auth)
	req.Header.Set("Surreal-NS", conn.NS)
	req.Header.Set("Surreal-DB", conn.DB)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --surreal: cloud SurrealDB unreachable at %s: %v\n", conn.URL, err)
		fmt.Fprintln(os.Stderr, "  → fix: verify SURREAL_CLOUD_URL / SURREAL_CLOUD_USER / SURREAL_CLOUD_PASS, or re-run `qai init`")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var stmts []struct {
		Status string `json:"status"`
		Result []struct {
			Text       string  `json:"text"`
			Provider   string  `json:"provider"`
			SourceFile string  `json:"source_file"`
			Score      float64 `json:"score"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &stmts); err != nil || len(stmts) == 0 {
		fmt.Fprintf(os.Stderr, "qai search --surreal: parse error from cloud SurrealDB at %s: %v\n", conn.URL, err)
		fmt.Fprintf(os.Stderr, "  raw response: %s\n", strutil.TruncateStr(string(body), 200))
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
		fmt.Printf("── %d. [%s] %s (score: %.4f) ──\n", i+1, r.Provider, r.SourceFile, r.Score)
		fmt.Printf("  %s\n", strings.TrimSpace(preview))
		if len(r.Text) > 300 {
			fmt.Println("  ...")
		}
		fmt.Println()
	}
	fmt.Printf("📊 %d results from SurrealDB Cloud\n", len(results))
}
