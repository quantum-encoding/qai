// search_rag.go — Vertex AI RAG search (replaces rag binary).
//
// Searches all RAG corpora individually and merges results by score.
// Auth via GCP ADC (reuses token.go's gcpRefreshToken).

package search

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/strutil"
	"github.com/quantum-encoding/qai-cli/internal/token"
)

// ─── types ────────────────────────────────────────────────────────────────

type ragCorpus struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	State       string `json:"state"`
}

type ragContextResult struct {
	SourceURI  string  `json:"sourceUri"`
	SourceName string  `json:"sourceName"`
	Text       string  `json:"text"`
	Score      float64 `json:"score"`
	Distance   float64 `json:"distance"`
}

type ragRetrieveResponse struct {
	Contexts struct {
		Contexts []ragContextResult `json:"contexts"`
	} `json:"contexts"`
}

// ─── config ───────────────────────────────────────────────────────────────

func APIBase() string {
	project := Cfg.Vertex.Project
	region := Cfg.Vertex.Region
	if project == "" {
		project = os.Getenv("RAG_PROJECT")
	}
	if region == "" {
		region = os.Getenv("RAG_REGION")
	}
	if project == "" || region == "" {
		return ""
	}
	return fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1beta1/projects/%s/locations/%s",
		region, project, region)
}

// ─── search command ───────────────────────────────────────────────────────

func RAGSearch(args []string) {
	base := APIBase()
	if base == "" {
		fmt.Fprintln(os.Stderr, "qai search --rag: GCP project not configured")
		fmt.Fprintln(os.Stderr, "  Set vertex.project + vertex.region in ~/.qai/config.yaml or run: qai init")
		os.Exit(1)
	}

	var corpusFilter string
	topK := 10
	var queryParts []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "--corpus":
			if i+1 < len(args) {
				i++
				corpusFilter = args[i]
			}
		case "-k", "--top-k":
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil {
					topK = n
				}
			}
		default:
			queryParts = append(queryParts, args[i])
		}
	}

	query := strings.Join(queryParts, " ")
	if query == "" {
		fmt.Fprintln(os.Stderr, "usage: qai search --rag <query> [-c corpus] [-k topK]")
		os.Exit(1)
	}

	// Auth
	creds, err := token.LoadGCPADC()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --rag: %v\n", err)
		os.Exit(1)
	}
	tok, err := token.RefreshToken(creds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --rag: auth: %v\n", err)
		os.Exit(1)
	}
	token := tok.AccessToken

	// List corpora
	corpora, err := ragListCorpora(base, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai search --rag: %v\n", err)
		os.Exit(1)
	}

	// Filter — explicit flag, auto-detect from query, or all
	var searchCorpora []ragCorpus
	if corpusFilter != "" {
		filter := strings.ToLower(corpusFilter)
		for _, c := range corpora {
			id := ragCorpusID(c.Name)
			if id == filter || strings.ToLower(c.DisplayName) == filter ||
				strings.Contains(strings.ToLower(c.DisplayName), filter) {
				searchCorpora = append(searchCorpora, c)
			}
		}
		if len(searchCorpora) == 0 {
			fmt.Fprintf(os.Stderr, "qai search --rag: no corpus matching %q\n", corpusFilter)
			os.Exit(1)
		}
	} else {
		// Auto-detect language from query keywords to filter corpora.
		// This prevents Zig queries from being drowned by Python/Rust results.
		autoFilter := ragAutoDetectCorpus(query)
		for _, c := range corpora {
			if c.State != "ACTIVE" && c.State != "" {
				continue
			}
			if autoFilter != "" && !strings.Contains(strings.ToLower(c.DisplayName), autoFilter) {
				continue
			}
			searchCorpora = append(searchCorpora, c)
		}
		// If auto-filter matched nothing, search all
		if len(searchCorpora) == 0 {
			for _, c := range corpora {
				if c.State == "ACTIVE" || c.State == "" {
					searchCorpora = append(searchCorpora, c)
				}
			}
		}
	}

	if len(searchCorpora) == 0 {
		fmt.Fprintln(os.Stderr, "qai search --rag: no active corpora")
		os.Exit(1)
	}

	// Query each corpus individually and merge
	client := &http.Client{Timeout: 30 * time.Second}
	var allResults []ragContextResult

	for _, c := range searchCorpora {
		body := map[string]any{
			"vertexRagStore": map[string]any{
				"ragResources": []map[string]any{
					{"ragCorpus": c.Name},
				},
			},
			"query": map[string]any{
				"text":           query,
				"similarityTopK": topK * 3, // over-fetch to compensate for dedup
			},
		}
		bodyBytes, _ := json.Marshal(body)

		url := base + ":retrieveContexts"
		req, _ := http.NewRequest("POST", url, strings.NewReader(string(bodyBytes)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  search %s: %v\n", c.DisplayName, err)
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "  search %s: %d %s\n", c.DisplayName, resp.StatusCode, strutil.TruncateStr(string(respBody), 200))
			continue
		}

		var result ragRetrieveResponse
		if json.Unmarshal(respBody, &result) == nil {
			allResults = append(allResults, result.Contexts.Contexts...)
		}
	}

	// Sort by score descending
	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].Score > allResults[j].Score
	})

	// Deduplicate — keep only the highest-scoring chunk per source file.
	// Vertex AI RAG often returns the same file 5-10x with identical scores,
	// wasting all result slots on duplicates.
	seen := make(map[string]int) // source → count
	var deduped []ragContextResult
	for _, r := range allResults {
		source := r.SourceName
		if source == "" {
			source = r.SourceURI
		}
		if seen[source] >= 2 {
			continue // allow max 2 chunks per source file
		}
		seen[source]++
		deduped = append(deduped, r)
	}
	allResults = deduped

	// Cap to topK
	if len(allResults) > topK {
		allResults = allResults[:topK]
	}

	if len(allResults) == 0 {
		fmt.Printf("\nNo results for: %s\n", query)
		return
	}

	// Print
	names := make([]string, len(searchCorpora))
	for i, c := range searchCorpora {
		names[i] = c.DisplayName
	}
	fmt.Printf("\n\033[1mSearch:\033[0m %s\n", query)
	fmt.Printf("\033[2mCorpora: %s · %d results\033[0m\n", strings.Join(names, ", "), len(allResults))
	fmt.Println()

	for i, r := range allResults {
		source := r.SourceName
		if source == "" {
			source = r.SourceURI
		}
		if idx := strings.LastIndex(source, "/"); idx >= 0 {
			source = source[idx+1:]
		}

		scoreStr := ""
		if r.Score > 0 {
			scoreStr = fmt.Sprintf("  \033[2mscore: %.3f\033[0m", r.Score)
		} else if r.Distance > 0 {
			scoreStr = fmt.Sprintf("  \033[2mdist: %.3f\033[0m", r.Distance)
		}

		fmt.Printf("\033[33m[%d]\033[0m \033[1m%s\033[0m%s\n", i+1, source, scoreStr)

		text := strings.TrimSpace(r.Text)
		lines := strings.Split(text, "\n")
		maxLines := 15
		for j, line := range lines {
			if j >= maxLines {
				fmt.Printf("    \033[2m... (%d more lines)\033[0m\n", len(lines)-maxLines)
				break
			}
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}
}

// ─── corpora list ─────────────────────────────────────────────────────────

func ragCorpora(args []string) {
	base := APIBase()
	if base == "" {
		fmt.Fprintln(os.Stderr, "qai rag corpora: GCP project not configured")
		os.Exit(1)
	}

	creds, err := token.LoadGCPADC()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai rag: %v\n", err)
		os.Exit(1)
	}
	tok, err := token.RefreshToken(creds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai rag: auth: %v\n", err)
		os.Exit(1)
	}

	corpora, err := ragListCorpora(base, tok.AccessToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai rag: %v\n", err)
		os.Exit(1)
	}

	if len(corpora) == 0 {
		fmt.Println("No RAG corpora found.")
		return
	}

	maxW := 0
	for _, c := range corpora {
		if len(c.DisplayName) > maxW {
			maxW = len(c.DisplayName)
		}
	}

	fmt.Printf("\n  %-*s  %-8s  %s\n", maxW, "NAME", "STATE", "DESCRIPTION")
	fmt.Printf("  %s  %s  %s\n", strings.Repeat("─", maxW), "────────", strings.Repeat("─", 50))

	for _, c := range corpora {
		icon := "\033[32m●\033[0m" // green
		if strings.Contains(c.State, "ERROR") {
			icon = "\033[31m●\033[0m"
		}
		desc := c.Description
		if len(desc) > 50 {
			desc = desc[:47] + "..."
		}
		id := ragCorpusID(c.Name)
		fmt.Printf("  %-*s  %s %-6s  %s\n", maxW, c.DisplayName, icon, c.State, desc)
		fmt.Printf("  %*s  id: %s\n", maxW, "", id)
	}
	fmt.Println()
}

// ─── API helpers ──────────────────────────────────────────────────────────

func ragListCorpora(base, token string) ([]ragCorpus, error) {
	var all []ragCorpus
	pageToken := ""
	client := &http.Client{Timeout: 30 * time.Second}

	for {
		url := base + "/ragCorpora?pageSize=100"
		if pageToken != "" {
			url += "&pageToken=" + pageToken
		}

		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list corpora: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("list corpora %d: %s", resp.StatusCode, strutil.TruncateStr(string(body), 200))
		}

		var data struct {
			RagCorpora    []ragCorpus `json:"ragCorpora"`
			NextPageToken string      `json:"nextPageToken"`
		}
		json.Unmarshal(body, &data)
		all = append(all, data.RagCorpora...)

		if data.NextPageToken == "" {
			break
		}
		pageToken = data.NextPageToken
	}
	return all, nil
}

// ragAutoDetectCorpus checks if the query contains language-specific keywords
// and returns a corpus filter string. Returns "" for generic queries.
func ragAutoDetectCorpus(query string) string {
	q := strings.ToLower(query)

	// Zig patterns
	zigKeywords := []string{"zig", "std.mem", "std.time", "std.http", "std.fs", "std.debug",
		"std.heap", "std.io", "std.net", "std.os", "std.crypto", "std.math",
		"comptime", "errdefer", "@import", "@intCast", "@as", "allocator"}
	for _, kw := range zigKeywords {
		if strings.Contains(q, kw) {
			return "zig"
		}
	}

	// Rust patterns
	rustKeywords := []string{"rust", "cargo", "impl ", "trait ", "fn ", "pub fn",
		"lifetime", "borrow checker", "&mut", "Option<", "Result<", "Vec<",
		"tokio", "async fn", "#[derive"}
	for _, kw := range rustKeywords {
		if strings.Contains(q, kw) {
			return "rust"
		}
	}

	// Go patterns
	goKeywords := []string{"golang", "go spec", "goroutine", "go/ast", "go/types",
		"func (", "interface{}", "chan ", "select {", "defer ", "go func"}
	for _, kw := range goKeywords {
		if strings.Contains(q, kw) {
			return "go"
		}
	}

	// TypeScript patterns
	tsKeywords := []string{"typescript", "tsc", "type Record<", "interface ", "keyof",
		"extends ", "readonly ", "Partial<", "Omit<", "Pick<"}
	for _, kw := range tsKeywords {
		if strings.Contains(q, kw) {
			return "typescript"
		}
	}

	// Python patterns
	pyKeywords := []string{"python", "def ", "class ", "__init__", "asyncio",
		"dataclass", "typing.", "pep ", "cpython"}
	for _, kw := range pyKeywords {
		if strings.Contains(q, kw) {
			return "python"
		}
	}

	// ARM patterns
	armKeywords := []string{"arm ", "neoverse", "aarch64", "sve ", "neon ",
		"armv8", "armv9", "a-profile"}
	for _, kw := range armKeywords {
		if strings.Contains(q, kw) {
			return "arm"
		}
	}

	// Blender patterns
	blenderKeywords := []string{"blender", "bpy.", "bmesh", "bpy.types", "bpy.ops"}
	for _, kw := range blenderKeywords {
		if strings.Contains(q, kw) {
			return "blender"
		}
	}

	return "" // no auto-detection — search all
}

func ragCorpusID(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return name
}
