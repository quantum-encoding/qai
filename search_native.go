// search_native.go — Native Go implementations of search commands.
//
// Replaces: node rag-search.mjs, node joplin-search.mjs, brave-cli
// Zero external dependencies — just HTTP calls.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ─── Brave Search (replaces brave-cli) ────────────────────────────────────

const braveBaseURL = "https://api.search.brave.com"

func braveSearch(query string, count int, freshness string) {
	key := os.Getenv("BRAVE_SEARCH_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "BRAVE_SEARCH_API_KEY not set")
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
		fmt.Fprintf(os.Stderr, "brave search: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "brave search %d: %s\n", resp.StatusCode, truncateStr(string(body), 200))
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

func braveSearchJSON(query string, count int) []byte {
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

func braveAsk(question string) {
	key := os.Getenv("BRAVE_SEARCH_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "BRAVE_SEARCH_API_KEY not set")
		os.Exit(1)
	}

	params := url.Values{"q": {question}}
	req, _ := http.NewRequest("GET", braveBaseURL+"/res/v1/web/search?"+params.Encode(), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "brave ask: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "brave ask %d: %s\n", resp.StatusCode, truncateStr(string(body), 200))
		os.Exit(1)
	}

	// Try to extract summarizer/answer if available, otherwise show web results
	var result struct {
		Summarizer struct {
			Results []struct {
				Text string `json:"text"`
			} `json:"results"`
		} `json:"summarizer"`
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	json.Unmarshal(body, &result)

	if len(result.Summarizer.Results) > 0 {
		for _, r := range result.Summarizer.Results {
			fmt.Println(r.Text)
		}
		return
	}

	// Fallback to web results
	for i, r := range result.Web.Results {
		fmt.Printf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Description)
	}
}

func braveContext(query string) {
	key := os.Getenv("BRAVE_SEARCH_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "BRAVE_SEARCH_API_KEY not set")
		os.Exit(1)
	}

	params := url.Values{"q": {query}, "summary": {"1"}}
	req, _ := http.NewRequest("GET", braveBaseURL+"/res/v1/web/search?"+params.Encode(), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", key)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "brave context: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "brave context %d: %s\n", resp.StatusCode, truncateStr(string(body), 200))
		os.Exit(1)
	}

	// Print raw JSON for LLM consumption
	var pretty json.RawMessage
	if json.Unmarshal(body, &pretty) == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		os.Stdout.Write(out)
		fmt.Println()
	}
}

// ─── Joplin Search (replaces node joplin-search.mjs) ─────────────────────

func joplinSearch(query string, limit int) {
	token := os.Getenv("JOPLIN_TOKEN")
	joplinURL := os.Getenv("JOPLIN_URL")
	if joplinURL == "" {
		joplinURL = "http://localhost:41184"
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "JOPLIN_TOKEN not set (Joplin Web Clipper API token)")
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
		fmt.Fprintf(os.Stderr, "joplin search: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "joplin search %d: %s\n", resp.StatusCode, truncateStr(string(body), 200))
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

func searchCloudSurreal(query, provider string, limit int) {
	if cfg.Surreal.CloudURL == "" {
		fmt.Fprintln(os.Stderr, "no cloud SurrealDB configured — run: qai init")
		os.Exit(1)
	}

	fmt.Printf("\n🔍 RAG Search: %q", query)
	if provider != "" {
		fmt.Printf(" [provider: %s]", provider)
	}
	fmt.Println()

	// Embed query
	embs, err := embedTextsDispatch([]string{query})
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --surreal: embed failed: %v\n", err)
		os.Exit(1)
	}
	if len(embs) == 0 || len(embs[0]) == 0 {
		fmt.Fprintln(os.Stderr, "qai search --surreal: empty embedding")
		os.Exit(1)
	}

	// Build SQL
	embJSON, _ := json.Marshal(embs[0])
	providerClause := ""
	if provider != "" {
		providerClause = fmt.Sprintf(" AND provider = '%s'", escapeSurQL(provider))
	}
	sql := fmt.Sprintf(
		"SELECT text, provider, source_file, vector::similarity::cosine(embedding, %s) AS score FROM doc_chunk WHERE embedding != NONE%s ORDER BY score DESC LIMIT %d;",
		string(embJSON), providerClause, limit,
	)

	// Query cloud SurrealDB
	conn := cfg.CloudConn()
	req, _ := http.NewRequest("POST", conn.url+"/sql", strings.NewReader(sql))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+conn.auth)
	req.Header.Set("Surreal-NS", conn.ns)
	req.Header.Set("Surreal-DB", conn.db)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --surreal: connection failed: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "qai search --surreal: parse error\n")
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
