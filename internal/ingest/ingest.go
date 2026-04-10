package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/db"
	"github.com/quantum-encoding/qai-cli/internal/embedding"
	"github.com/quantum-encoding/qai-cli/internal/strutil"
)

// Cfg is set by main.
var Cfg *config.Config

const embedBatchSize = 20

func CmdIngest(args []string) {
	// Parse flags.
	target := ""
	local := false
	precomputed := false
	var positional []string
	gcsBucket := Cfg.Vertex.Bucket
	gcpProject := Cfg.Vertex.Project
	gcpRegion := Cfg.Vertex.Region

	for _, a := range args {
		switch a {
		case "--surreal":
			target = "surreal"
		case "--local":
			target = "surreal"
			local = true
		case "--vertex":
			target = "vertex"
		case "--precomputed":
			precomputed = true
		default:
			positional = append(positional, a)
		}
	}

	// Precomputed mode: load raw archive directly (skip chunk+embed).
	if precomputed {
		if target == "" {
			target = "surreal"
			local = true
		}
		if len(positional) < 2 {
			fmt.Fprintln(os.Stderr, `usage: qai ingest --precomputed [--local|--surreal] <provider> <archive-dir>

  Loads pre-computed embeddings from a MetalEmbeddings raw archive into SurrealDB.
  Skips chunking and embedding — reads metadata.jsonl + results.jsonl + bodies/ directly.

  provider:    label (e.g. "zig-std-0.16", "chronos", "zig-forge")
  archive-dir: directory containing metadata.jsonl, results.jsonl (or *.telemetry.jsonl), and bodies/

Examples:
  qai ingest --precomputed --local zig-std-0.16 data/raw-embeddings/zig-std-0.16/
  qai ingest --precomputed --local chronos data/raw-embeddings/chronos/`)
			os.Exit(1)
		}
		provider := positional[0]
		archiveDir := positional[1]

		var conn config.SurrealConn
		if local {
			conn = Cfg.LocalConn()
			fmt.Printf("Target: local SurrealDB (%s)\n", Cfg.LocalSurrealURL())
		} else {
			conn = Cfg.CloudConn()
			fmt.Println("Target: SurrealDB cloud")
		}
		ingestPrecomputed(conn, provider, archiveDir)
		return
	}

	if len(positional) < 2 || target == "" {
		fmt.Fprintln(os.Stderr, `usage: qai ingest --surreal <provider> <path>   # → SurrealDB cloud
       qai ingest --local   <provider> <path>   # → SurrealDB local (127.0.0.1:9473)
       qai ingest --vertex  <provider> <path>   # → GCS + Vertex Vector Search
       qai ingest --precomputed [--local|--surreal] <provider> <archive-dir>

  provider: label (e.g. "heygen", "rust", "quantum-ai")
  path:     file or directory of .md/.txt/.pdf files

Examples:
  qai ingest --local   heygen ~/Documents/HeyGen_Docs/
  qai ingest --surreal heygen ~/Documents/HeyGen_Docs/
  qai ingest --vertex  heygen ~/Documents/HeyGen_Docs/
  qai ingest --precomputed --local zig-std-0.16 data/raw-embeddings/zig-std-0.16/

Env vars:
  QAI_API_KEY    API key for embeddings (default: test key)
  GCS_BUCKET     GCS bucket for Vertex vectors
  GCP_PROJECT    GCP project
  GCP_REGION     GCP region (default: europe-west4)`)
		os.Exit(1)
	}

	provider := positional[0]
	path := positional[1]

	// 1. Chunk via axiom.
	chunks := chunkDocs(path)
	if len(chunks) == 0 {
		fmt.Fprintln(os.Stderr, "qai ingest: no chunks produced")
		os.Exit(1)
	}
	fmt.Printf("Got %d chunks\n", len(chunks))

	// 2. Embed using configured provider.
	fmt.Printf("Embedding via %s (%s, %d-dim)...\n", Cfg.Embeddings.Provider, Cfg.Embeddings.Model, Cfg.Embeddings.Dimensions)
	embeddings := embedAllChunks(chunks)

	// 3. Store based on target.
	switch target {
	case "surreal":
		var conn config.SurrealConn
		if local {
			conn = Cfg.LocalConn()
			fmt.Printf("Target: local SurrealDB (%s)\n", Cfg.LocalSurrealURL())
		} else {
			if Cfg.Surreal.CloudURL == "" {
				fmt.Fprintln(os.Stderr, "qai ingest: no cloud SurrealDB configured — run: qai init")
				os.Exit(1)
			}
			conn = Cfg.CloudConn()
			fmt.Println("Target: SurrealDB cloud")
		}
		StoreSurreal(conn, provider, chunks, embeddings)
	case "vertex":
		storeVertex(provider, chunks, embeddings, gcsBucket, gcpProject, gcpRegion)
	}
}

// ─── Chunking ───────────────────────────────────────────────────────────────

type chunk struct {
	text       string
	sourceFile string
}

func chunkDocs(path string) []chunk {
	// Try axiom first (better quality, handles PDFs), fall back to native chunker.
	if _, err := exec.LookPath("axiom"); err == nil {
		return chunkDocsAxiom(path)
	}

	fmt.Printf("Chunking %s (native Go chunker)...\n", path)
	return chunkDocsNative(path)
}

func chunkDocsAxiom(path string) []chunk {
	tmpChunks, err := os.MkdirTemp("", "qai-ingest-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai ingest: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpChunks)

	fmt.Printf("Chunking %s via axiom...\n", path)

	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai ingest: %v\n", err)
		os.Exit(1)
	}

	if info.IsDir() {
		filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(p))
			if ext == ".md" || ext == ".txt" || ext == ".pdf" || ext == ".mdx" || ext == ".rst" {
				chunkFileAxiom(p, tmpChunks)
			}
			return nil
		})
	} else {
		chunkFileAxiom(path, tmpChunks)
	}

	var chunks []chunk
	filepath.Walk(tmpChunks, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return nil
		}
		rel, _ := filepath.Rel(tmpChunks, p)
		chunks = append(chunks, chunk{text: text, sourceFile: rel})
		return nil
	})
	return chunks
}

func chunkFileAxiom(path, outDir string) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".pdf" {
		cmd := exec.Command("axiom", "process", path, "-o", outDir, "--chunk-size", "500", "--min-size", "100")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Run()
	} else {
		cmd := exec.Command("axiom", "chunk", path, "-o", outDir, "--chunk-size", "500", "--min-size", "100")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Run()
	}
}

// ─── Embedding ──────────────────────────────────────────────────────────────

func embedAllChunks(chunks []chunk) [][]float64 {
	embeddings := make([][]float64, len(chunks))

	for i := 0; i < len(chunks); i += embedBatchSize {
		end := i + embedBatchSize
		if end > len(chunks) {
			end = len(chunks)
		}

		texts := make([]string, end-i)
		for j := i; j < end; j++ {
			texts[j-i] = chunks[j].text
		}

		embs, err := embedding.Dispatch(Cfg, texts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "embed batch %d failed: %v\n", i/embedBatchSize+1, err)
			os.Exit(1)
		}

		for j, emb := range embs {
			embeddings[i+j] = emb
		}

		batchNum := i/embedBatchSize + 1
		totalBatches := (len(chunks) + embedBatchSize - 1) / embedBatchSize
		fmt.Printf("  batch %d/%d (%d chunks)\n", batchNum, totalBatches, len(embs))
	}
	return embeddings
}

// embedTexts removed — replaced by embedQAI/embedGemini/embedOllama/embedOpenAI in init.go
// All embedding calls now go through embedding.Dispatch().

// ─── SurrealDB storage ──────────────────────────────────────────────────────

func StoreSurreal(conn config.SurrealConn, provider string, chunks []chunk, embeddings [][]float64) {
	fmt.Printf("Clearing existing %q in SurrealDB...\n", provider)
	db.Exec(conn, fmt.Sprintf("DELETE FROM doc_chunk WHERE provider = '%s';", strutil.EscapeSurQL(provider)))

	dim := 0
	if len(embeddings) > 0 {
		dim = len(embeddings[0])
	}

	fmt.Printf("Inserting into SurrealDB (%d-dim)...\n", dim)
	for i, c := range chunks {
		record := map[string]any{
			"provider":    provider,
			"source_file": c.sourceFile,
			"text":        c.text,
			"embedding":   embeddings[i],
			"dimension":   len(embeddings[i]),
		}
		contentJSON, _ := json.Marshal(record)
		sql := "CREATE doc_chunk CONTENT " + string(contentJSON) + ";"
		db.Exec(conn, sql)

		if (i+1)%20 == 0 || i == len(chunks)-1 {
			fmt.Printf("  %d/%d inserted\n", i+1, len(chunks))
		}
	}
	fmt.Printf("Done! %d chunks → SurrealDB (provider=%q, %d-dim)\n", len(chunks), provider, dim)
}


// searchLocalSurreal embeds the query and runs vector similarity search
// against the local SurrealDB instance. Pure Go, no Node dependency.
// Automatically filters by dimension to match the query embedding size.

// ─── Precomputed ingest ─────────────────────────────────────────────────────
// Reads a MetalEmbeddings raw archive (metadata.jsonl + results.jsonl + bodies/)
// and inserts directly into SurrealDB without re-embedding.

func ingestPrecomputed(conn config.SurrealConn, provider, archiveDir string) {
	// Locate files.
	metadataPath := filepath.Join(archiveDir, "metadata.jsonl")
	if _, err := os.Stat(metadataPath); err != nil {
		fmt.Fprintf(os.Stderr, "qai ingest --precomputed: metadata.jsonl not found in %s\n", archiveDir)
		os.Exit(1)
	}

	// Find results file: results.jsonl or merged telemetry files.
	resultsPath := findResultsFile(archiveDir)
	if resultsPath == "" {
		fmt.Fprintf(os.Stderr, "qai ingest --precomputed: no results.jsonl or *.telemetry.jsonl found in %s\n", archiveDir)
		os.Exit(1)
	}
	fmt.Printf("Archive: %s\n", archiveDir)
	fmt.Printf("  metadata: %s\n", metadataPath)
	fmt.Printf("  results:  %s\n", resultsPath)

	// Phase 1: Build batch→body_path index from results.
	type batchInfo struct {
		BodyPath string
		Status   int
	}
	batchIndex := make(map[string]batchInfo)

	resultsFile, err := os.Open(resultsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open results: %v\n", err)
		os.Exit(1)
	}
	scanner := bufio.NewScanner(resultsFile)
	scanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024) // 10 MB line buffer
	for scanner.Scan() {
		var row struct {
			ID       string `json:"id"`
			Status   int    `json:"status"`
			BodyPath string `json:"body_path"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil && row.Status == 200 {
			batchIndex[row.ID] = batchInfo{BodyPath: row.BodyPath, Status: row.Status}
		}
	}
	resultsFile.Close()
	fmt.Printf("  batches:  %d successful\n", len(batchIndex))

	// Phase 2: Read metadata and join with vectors.
	type metaRow struct {
		ChunkID    string `json:"chunkId"`
		Source     string `json:"source"`
		Title      string `json:"title"`
		Preview    string `json:"preview"`
		Clearance  int    `json:"clearance"`
		BatchID    string `json:"batchId"`
		BatchIndex int    `json:"batchIndex"`
	}

	metaFile, err := os.Open(metadataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open metadata: %v\n", err)
		os.Exit(1)
	}
	defer metaFile.Close()

	var rows []metaRow
	metaScanner := bufio.NewScanner(metaFile)
	metaScanner.Buffer(make([]byte, 10*1024*1024), 10*1024*1024)
	for metaScanner.Scan() {
		var row metaRow
		if json.Unmarshal(metaScanner.Bytes(), &row) == nil {
			rows = append(rows, row)
		}
	}
	fmt.Printf("  metadata: %d chunks\n", len(rows))

	// Phase 3: Parse bodies on demand, extract vectors, insert.
	// Cache parsed body files so siblings in the same batch don't re-read.
	bodyCache := make(map[string][][][]float64) // batchId → predictions
	dimension := 0
	inserted := 0
	skipped := 0

	// Detect dimension from first valid vector.
	for _, row := range rows {
		bi, ok := batchIndex[row.BatchID]
		if !ok {
			continue
		}
		preds := loadPredictions(bi.BodyPath, bodyCache, row.BatchID)
		if preds == nil || row.BatchIndex >= len(preds) {
			continue
		}
		vec := preds[row.BatchIndex]
		if len(vec) > 0 {
			dimension = len(vec[0])
			break
		}
	}
	if dimension == 0 {
		fmt.Fprintln(os.Stderr, "qai ingest --precomputed: could not detect vector dimension")
		os.Exit(1)
	}
	fmt.Printf("  dimension: %d\n\n", dimension)

	// Ensure schema supports precomputed fields.
	ensurePrecomputedSchema(conn)

	// Clear existing provider data.
	fmt.Printf("Clearing existing %q in SurrealDB...\n", provider)
	db.Exec(conn, fmt.Sprintf("DELETE FROM doc_chunk WHERE provider = '%s';", strutil.EscapeSurQL(provider)))

	// Insert one record at a time — 4096-dim vectors are ~25KB each in JSON,
	// and SurrealDB's HTTP handler chokes on large batch payloads.
	for _, row := range rows {
		bi, ok := batchIndex[row.BatchID]
		if !ok {
			skipped++
			continue
		}
		preds := loadPredictions(bi.BodyPath, bodyCache, row.BatchID)
		if preds == nil || row.BatchIndex >= len(preds) {
			skipped++
			continue
		}
		innerVec := preds[row.BatchIndex]
		if len(innerVec) == 0 {
			skipped++
			continue
		}
		rawVec := innerVec[0] // predictions[i][0] = the 4096-dim vector

		// L2 normalize.
		vec := l2Normalize(rawVec)

		// Build record JSON.
		record := map[string]any{
			"provider":    provider,
			"source_file": row.Source,
			"text":        row.Preview,
			"title":       row.Title,
			"chunk_id":    row.ChunkID,
			"clearance":   row.Clearance,
			"dimension":   len(vec),
			"embedding":   vec,
		}
		recJSON, _ := json.Marshal(record)
		sql := "CREATE doc_chunk CONTENT " + string(recJSON) + ";"
		db.Exec(conn, sql)
		inserted++

		if inserted%100 == 0 {
			fmt.Printf("  %d/%d inserted\n", inserted, len(rows))
		}
	}

	fmt.Printf("\nDone! %d chunks → SurrealDB (provider=%q, %d-dim)\n", inserted, provider, dimension)
	if skipped > 0 {
		fmt.Printf("  skipped: %d (missing batch or vector)\n", skipped)
	}
}

// findResultsFile locates the results/telemetry file in an archive directory.
// Prefers results.jsonl, falls back to merging *.telemetry.jsonl files.
func findResultsFile(dir string) string {
	p := filepath.Join(dir, "results.jsonl")
	if _, err := os.Stat(p); err == nil {
		return p
	}

	// For chronos: merge pass1.telemetry.jsonl + retry.telemetry.jsonl into a temp file.
	entries, _ := filepath.Glob(filepath.Join(dir, "*.telemetry.jsonl"))
	if len(entries) == 0 {
		return ""
	}
	if len(entries) == 1 {
		return entries[0]
	}

	// Merge multiple telemetry files (later entries win on duplicate batchId).
	tmp, err := os.CreateTemp("", "qai-merged-telemetry-*.jsonl")
	if err != nil {
		return entries[0]
	}
	for _, e := range entries {
		data, err := os.ReadFile(e)
		if err == nil {
			tmp.Write(data)
			if !bytes.HasSuffix(data, []byte("\n")) {
				tmp.WriteString("\n")
			}
		}
	}
	tmp.Close()
	fmt.Printf("  merged %d telemetry files → %s\n", len(entries), tmp.Name())
	return tmp.Name()
}

// loadPredictions loads and caches the TEI predictions from a body file.
func loadPredictions(bodyPath string, cache map[string][][][]float64, batchID string) [][][]float64 {
	if preds, ok := cache[batchID]; ok {
		return preds
	}

	data, err := os.ReadFile(bodyPath)
	if err != nil {
		return nil
	}

	var body struct {
		Predictions [][][]float64 `json:"predictions"`
	}
	if json.Unmarshal(data, &body) != nil {
		return nil
	}

	cache[batchID] = body.Predictions

	// Evict old entries to bound memory (keep last 20 batches).
	if len(cache) > 20 {
		for k := range cache {
			if k != batchID {
				delete(cache, k)
				break
			}
		}
	}

	return body.Predictions
}

// l2Normalize returns a unit-length copy of the vector.
func l2Normalize(v []float64) []float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return v
	}
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// ensurePrecomputedSchema ensures the doc_chunk table has all fields needed
// for precomputed embeddings. Removes any fixed-dimension HNSW index since
// we support multiple dimensions via brute-force KNN.
func ensurePrecomputedSchema(conn config.SurrealConn) {
	db.Exec(conn, `
DEFINE FIELD IF NOT EXISTS title ON doc_chunk TYPE option<string>;
DEFINE FIELD IF NOT EXISTS chunk_id ON doc_chunk TYPE option<string>;
DEFINE FIELD IF NOT EXISTS clearance ON doc_chunk TYPE option<int>;
DEFINE FIELD IF NOT EXISTS dimension ON doc_chunk TYPE option<int>;
DEFINE INDEX IF NOT EXISTS idx_clearance ON doc_chunk FIELDS clearance;
DEFINE INDEX IF NOT EXISTS idx_dimension ON doc_chunk FIELDS dimension;
REMOVE INDEX IF EXISTS idx_embedding ON doc_chunk;
`)
}


// ─── Vertex Vector Search storage ───────────────────────────────────────────

func storeVertex(provider string, chunks []chunk, embeddings [][]float64, bucket, project, region string) {
	// 1. Write JSONL.
	jsonlPath := filepath.Join(os.TempDir(), provider+"-vectors.json")
	f, err := os.Create(jsonlPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create jsonl: %v\n", err)
		os.Exit(1)
	}
	for i, c := range chunks {
		record := map[string]any{
			"id":        fmt.Sprintf("%s_%04d", provider, i),
			"embedding": embeddings[i],
		}
		// Store text as restricts for metadata filtering.
		_ = c // text stored in metadata if needed later
		line, _ := json.Marshal(record)
		f.Write(line)
		f.WriteString("\n")
	}
	f.Close()
	fmt.Printf("JSONL: %s (%d records)\n", jsonlPath, len(chunks))

	// 2. Upload to GCS.
	gcsPath := fmt.Sprintf("gs://%s/vector-search/%s/", bucket, provider)
	gcsFile := gcsPath + provider + "-vectors.json"
	fmt.Printf("Uploading to %s...\n", gcsFile)
	cmd := exec.Command("gsutil", "cp", jsonlPath, gcsFile)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "gsutil cp failed: %v\n", err)
		os.Exit(1)
	}

	// 3. Create Vector Search index.
	fmt.Println("Creating Vertex AI Vector Search index...")
	token := getGCPToken()
	indexName := fmt.Sprintf("%s (Gemini Embedding 2)", provider)

	reqBody := map[string]any{
		"displayName": indexName,
		"metadata": map[string]any{
			"config": map[string]any{
				"dimensions":               Cfg.Embeddings.Dimensions,
				"approximateNeighborsCount": 10,
				"shardSize":                "SHARD_SIZE_SMALL",
				"algorithmConfig":          map[string]any{"treeAhConfig": map[string]any{}},
				"distanceMeasureType":      "COSINE_DISTANCE",
			},
			"contentsDeltaUri": gcsPath,
		},
		"indexUpdateMethod": "BATCH_UPDATE",
	}
	data, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/indexes", region, project, region)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create index failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var opResp struct {
		Name string `json:"name"`
	}
	json.Unmarshal(respBody, &opResp)
	parts := strings.Split(opResp.Name, "/")
	indexID := ""
	if len(parts) > 5 {
		indexID = parts[5]
	}

	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "create index %d: %s\n", resp.StatusCode, string(respBody)[:300])
		os.Exit(1)
	}

	fmt.Printf("Done! %d vectors → Vertex Vector Search\n", len(chunks))
	fmt.Printf("  Index: %s (id: %s)\n", indexName, indexID)
	fmt.Printf("  GCS: %s\n", gcsPath)
	fmt.Printf("  Check: qai vertex-index status %s\n", indexID)
}

func getGCPToken() string {
	cmd := exec.Command("gcp-token-refresh")
	out, err := cmd.Output()
	if err != nil {
		cmd2 := exec.Command("gcloud", "auth", "print-access-token")
		out, _ = cmd2.Output()
	}
	return strings.TrimSpace(string(out))
}
