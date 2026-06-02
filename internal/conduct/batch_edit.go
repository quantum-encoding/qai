// batch_edit.go — batch image editing.
//
// Two modes:
//   1. Parallel mode (--parallel N): bounded worker pool hitting the normal
//      /qai/v1/images/edit endpoint. Fast, respects per-minute rate limits.
//   2. Vertex batch mode (default when --batch is set without --parallel):
//      builds a JSONL of Gemini inference requests, uploads to GCS, submits
//      a Vertex AI batchPredictionJob, polls until done, then writes decoded
//      images to the output directory.

package conduct

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/strutil"
	"github.com/quantum-encoding/qai-cli/internal/token"
)

// ─── entry point ──────────────────────────────────────────────────────────

// CmdEditBatch handles `qai edit --batch` (called by main dispatcher after
// the --batch sentinel is consumed).
func CmdEditBatch(args []string) {
	parallel := 0
	model := "gemini-2.5-flash-image"
	bucket := Cfg.Vertex.Bucket
	region := "us-central1" // Vertex batch for image models is us-central1 only.
	provider := ""          // parallel-mode provider override (e.g. "gemini")
	var prompt, inputDir, outputDir string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--batch":
			// sentinel already consumed by caller; tolerate if present
		case "--parallel":
			if i+1 < len(args) {
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n < 1 {
					fmt.Fprintf(os.Stderr, "qai edit --batch: --parallel got %q, want a positive integer\n", args[i+1])
					fmt.Fprintln(os.Stderr, "  → fix: e.g. --parallel 4")
					os.Exit(1)
				}
				parallel = n
				i++
			}
		case "--model":
			if i+1 < len(args) {
				model = resolveImageModel(args[i+1])
				i++
			}
		case "--gcs-bucket":
			if i+1 < len(args) {
				bucket = args[i+1]
				i++
			}
		case "--region":
			if i+1 < len(args) {
				region = args[i+1]
				i++
			}
		case "--provider":
			if i+1 < len(args) {
				provider = args[i+1]
				i++
			}
		case "help", "--help", "-h":
			editBatchUsage()
			return
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 3 {
		fmt.Fprintf(os.Stderr, "qai edit --batch: need 3 positional args (prompt, input-dir, output-dir); got %d\n", len(positional))
		editBatchUsage()
		os.Exit(1)
	}
	prompt = positional[0]
	inputDir = positional[1]
	outputDir = positional[2]

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: cannot create output dir %s: %v\n", outputDir, err)
		fmt.Fprintln(os.Stderr, "  → fix: pick a writable path or check parent dir permissions")
		os.Exit(1)
	}

	images, err := listImages(inputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: cannot read input dir %s: %v\n", inputDir, err)
		fmt.Fprintln(os.Stderr, "  → fix: pass an existing directory containing .png/.jpg/.jpeg/.webp files")
		os.Exit(1)
	}
	if len(images) == 0 {
		fmt.Fprintf(os.Stderr, "qai edit --batch: no images found in %s\n", inputDir)
		fmt.Fprintln(os.Stderr, "  → fix: only .png/.jpg/.jpeg/.webp are scanned; check the directory contents")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Found %d images in %s\n", len(images), inputDir)

	if parallel > 0 {
		runParallelEdits(prompt, images, outputDir, parallel, model, provider)
		return
	}
	runVertexBatchEdit(prompt, images, outputDir, model, bucket, region)
}

func editBatchUsage() {
	fmt.Fprintln(os.Stderr, `usage: qai edit --batch [flags] "prompt" <input-dir> <output-dir>

Flags:
  --parallel N        Parallel mode — N concurrent /qai/v1/images/edit calls.
  --model M           Model id (default gemini-2.5-flash-image).
  --gcs-bucket B      GCS bucket for Vertex batch mode (defaults to vertex.bucket).
  --region R          Vertex region (default us-central1).
  --provider P        Provider override for parallel mode (e.g. gemini).

Without --parallel: submits a Vertex AI batchPredictionJob, polls to
completion, then decodes results into <output-dir>.`)
}

// ─── parallel mode ────────────────────────────────────────────────────────

type editResult struct {
	input string
	out   string
	err   error
}

func runParallelEdits(prompt string, images []string, outDir string, workers int, model, provider string) {
	fmt.Fprintf(os.Stderr, "Parallel mode · workers=%d · model=%s\n", workers, model)

	jobs := make(chan string, len(images))
	results := make(chan editResult, len(images))

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for img := range jobs {
				out, err := editOne(prompt, img, outDir, model, provider)
				results <- editResult{input: img, out: out, err: err}
			}
		}()
	}

	for _, img := range images {
		jobs <- img
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	done, failed := 0, 0
	total := len(images)
	for r := range results {
		if r.err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  [FAIL] %s: %v\n", filepath.Base(r.input), r.err)
		} else {
			done++
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s → %s\n", done+failed, total, filepath.Base(r.input), r.out)
		}
	}
	fmt.Fprintf(os.Stderr, "Done. %d succeeded, %d failed.\n", done, failed)
	if failed > 0 {
		fmt.Fprintln(os.Stderr, "qai edit --batch: one or more edits failed (see [FAIL] lines above)")
		fmt.Fprintln(os.Stderr, "  → fix: retry failed inputs individually with `qai edit <file> \"prompt\"`")
		os.Exit(1)
	}
}

func editOne(prompt, imagePath, outDir, model, provider string) (string, error) {
	data, _, err := readImageUpright(imagePath)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	body := map[string]any{
		"prompt":       prompt,
		"image_base64": b64,
		"input_images": []string{b64},
		"model":        model,
	}
	if provider != "" {
		body["provider"] = provider
	}

	respBytes, err := qaiAPI("POST", "/qai/v1/images/edit", body)
	if err != nil {
		return "", err
	}

	var resp map[string]any
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	outB64 := extractEditedImage(resp)
	if outB64 == "" {
		keys := make([]string, 0, len(resp))
		for k, v := range resp {
			switch val := v.(type) {
			case string:
				if len(val) > 80 {
					val = val[:80] + "...(" + strconv.Itoa(len(v.(string))) + ")"
				}
				keys = append(keys, fmt.Sprintf("%s=%q", k, val))
			default:
				b, _ := json.Marshal(v)
				s := string(b)
				if len(s) > 200 {
					s = s[:200] + "..."
				}
				keys = append(keys, fmt.Sprintf("%s=%s", k, s))
			}
		}
		return "", fmt.Errorf("no image in response: {%s}", strings.Join(keys, ", "))
	}

	outBytes, err := base64.StdEncoding.DecodeString(outB64)
	if err != nil {
		if outBytes, err = base64.URLEncoding.DecodeString(outB64); err != nil {
			return "", fmt.Errorf("decode base64: %w", err)
		}
	}

	outName := editedFilename(imagePath)
	outPath := filepath.Join(outDir, outName)
	if err := os.WriteFile(outPath, outBytes, 0644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return outPath, nil
}

// ─── Vertex batch mode ────────────────────────────────────────────────────

func runVertexBatchEdit(prompt string, images []string, outDir, model, bucket, region string) {
	project := Cfg.Vertex.Project
	if project == "" {
		fmt.Fprintln(os.Stderr, "qai edit --batch: vertex.project not configured")
		fmt.Fprintln(os.Stderr, "  → fix: run `qai init` to set vertex.project, or pass --parallel N for the live-API path instead")
		os.Exit(1)
	}
	if bucket == "" {
		fmt.Fprintln(os.Stderr, "qai edit --batch: GCS bucket not set")
		fmt.Fprintln(os.Stderr, "  → fix: pass --gcs-bucket <name>, or set vertex.bucket in ~/.config/qai/config.toml")
		os.Exit(1)
	}

	tok, err := getVertexToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: GCP auth failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: run `gcloud auth application-default login`, or check that ADC is valid with `qai token --check`")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Vertex batch · project=%s · region=%s · bucket=%s · model=%s\n",
		project, region, bucket, model)

	ts := time.Now().Format("20060102-150405")
	jobPrefix := fmt.Sprintf("qai-batch/%s", ts)
	inputObj := jobPrefix + "/input.jsonl"
	outputPrefix := jobPrefix + "/output/"

	jsonlBuf, keys, err := buildBatchJSONL(prompt, images)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: build JSONL request body: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: one of the input images is unreadable; check the [FAIL] line above for which one")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Built JSONL: %d requests, %d bytes\n", len(images), jsonlBuf.Len())

	if err := gcsUpload(tok, bucket, inputObj, "application/jsonl", jsonlBuf.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: upload JSONL to gs://%s/%s: %v\n", bucket, inputObj, err)
		fmt.Fprintln(os.Stderr, "  → fix: ensure the bucket exists in the right region and your ADC has storage.objects.create on it")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Uploaded gs://%s/%s\n", bucket, inputObj)

	jobName, err := submitVertexBatchJob(tok, project, region, model, bucket, inputObj, outputPrefix, ts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: submit Vertex batch job: %v\n", err)
		fmt.Fprintln(os.Stderr, "  → fix: confirm the model id is valid for batchPredictionJobs in this region, and ADC has aiplatform.batchPredictionJobs.create")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Submitted job: %s\n", jobName)

	state, outDir2, err := pollVertexBatchJob(tok, jobName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: poll Vertex job %s: %v\n", jobName, err)
		fmt.Fprintf(os.Stderr, "  → fix: inspect the job in the console or with `gcloud ai batch-prediction-jobs describe %s`\n", jobName)
		os.Exit(1)
	}
	if state != "JOB_STATE_SUCCEEDED" {
		fmt.Fprintf(os.Stderr, "qai edit --batch: Vertex job %s ended in state %s\n", jobName, state)
		fmt.Fprintln(os.Stderr, "  → fix: see the `error:` line above for the broker's reason; re-run with corrected inputs or model")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Job succeeded. Output: %s\n", outDir2)

	if outDir2 == "" {
		outDir2 = fmt.Sprintf("gs://%s/%s", bucket, outputPrefix)
	}
	if err := downloadBatchResults(tok, outDir2, outDir, keys); err != nil {
		fmt.Fprintf(os.Stderr, "qai edit --batch: download results from %s: %v\n", outDir2, err)
		fmt.Fprintln(os.Stderr, "  → fix: the job succeeded but pulling predictions failed — re-run download with the gs:// path above")
		os.Exit(1)
	}
}

// buildBatchJSONL creates a JSONL body. Returns the buffer and an ordered
// list of input basenames (without extension) keyed to each line so we can
// map responses back even if the batch service reorders them.
func buildBatchJSONL(prompt string, images []string) (*bytes.Buffer, []string, error) {
	buf := &bytes.Buffer{}
	keys := make([]string, 0, len(images))
	for i, img := range images {
		data, rotated, err := readImageUpright(img)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", img, err)
		}
		mime := mimeFromExt(img)
		if rotated {
			mime = "image/png" // rotation re-encodes as PNG
		}
		key := fmt.Sprintf("%04d_%s", i, strings.TrimSuffix(filepath.Base(img), filepath.Ext(img)))
		keys = append(keys, key)

		request := map[string]any{
			"contents": []map[string]any{
				{
					"role": "user",
					"parts": []map[string]any{
						{"text": prompt},
						{
							"inlineData": map[string]any{
								"mimeType": mime,
								"data":     base64.StdEncoding.EncodeToString(data),
							},
						},
					},
				},
			},
			"generationConfig": map[string]any{
				"responseModalities": []string{"IMAGE"},
			},
		}
		line := map[string]any{
			"request": request,
			"labels":  map[string]string{"qai_key": key},
		}
		b, err := json.Marshal(line)
		if err != nil {
			return nil, nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf, keys, nil
}

// ─── Vertex API ───────────────────────────────────────────────────────────

func getVertexToken() (string, error) {
	creds, err := token.LoadGCPADC()
	if err != nil {
		return "", err
	}
	tok, err := token.RefreshToken(creds)
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

func submitVertexBatchJob(tok, project, region, model, bucket, inputObj, outputPrefix, ts string) (string, error) {
	body := map[string]any{
		"displayName": "qai-edit-batch-" + ts,
		"model":       "publishers/google/models/" + model,
		"inputConfig": map[string]any{
			"instancesFormat": "jsonl",
			"gcsSource": map[string]any{
				"uris": []string{fmt.Sprintf("gs://%s/%s", bucket, inputObj)},
			},
		},
		"outputConfig": map[string]any{
			"predictionsFormat": "jsonl",
			"gcsDestination": map[string]any{
				"outputUriPrefix": fmt.Sprintf("gs://%s/%s", bucket, outputPrefix),
			},
		},
	}
	data, _ := json.Marshal(body)

	endpoint := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/batchPredictionJobs",
		region, project, region)
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("submit %d: %s", resp.StatusCode, strutil.TruncateStr(string(respBody), 500))
	}

	var out struct {
		Name  string `json:"name"`
		State string `json:"state"`
	}
	json.Unmarshal(respBody, &out)
	return out.Name, nil
}

func pollVertexBatchJob(tok, jobName string) (string, string, error) {
	// jobName is the full resource: projects/{p}/locations/{r}/batchPredictionJobs/{id}
	endpoint := "https://" + regionFromJobName(jobName) + "-aiplatform.googleapis.com/v1/" + jobName
	client := &http.Client{Timeout: 60 * time.Second}

	lastState := ""
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("poll timeout")
		default:
		}

		req, _ := http.NewRequest("GET", endpoint, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		if err != nil {
			return "", "", err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return "", "", fmt.Errorf("poll %d: %s", resp.StatusCode, strutil.TruncateStr(string(body), 300))
		}

		var out struct {
			State      string `json:"state"`
			Error      any    `json:"error"`
			OutputInfo struct {
				GcsOutputDirectory string `json:"gcsOutputDirectory"`
			} `json:"outputInfo"`
		}
		json.Unmarshal(body, &out)

		if out.State != lastState {
			fmt.Fprintf(os.Stderr, "  state: %s\n", out.State)
			lastState = out.State
		}

		switch out.State {
		case "JOB_STATE_SUCCEEDED":
			return out.State, out.OutputInfo.GcsOutputDirectory, nil
		case "JOB_STATE_FAILED", "JOB_STATE_CANCELLED", "JOB_STATE_EXPIRED":
			if b, _ := json.Marshal(out.Error); len(b) > 2 {
				fmt.Fprintf(os.Stderr, "  error: %s\n", string(b))
			}
			return out.State, "", nil
		}

		time.Sleep(15 * time.Second)
	}
}

func regionFromJobName(jobName string) string {
	parts := strings.Split(jobName, "/")
	for i, p := range parts {
		if p == "locations" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return "us-central1"
}

// ─── GCS helpers (REST, no gsutil) ────────────────────────────────────────

func gcsUpload(tok, bucket, object, contentType string, data []byte) error {
	u := fmt.Sprintf("https://storage.googleapis.com/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		url.PathEscape(bucket), url.QueryEscape(object))
	req, _ := http.NewRequest("POST", u, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", contentType)

	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("upload %d: %s", resp.StatusCode, strutil.TruncateStr(string(body), 300))
	}
	return nil
}

func gcsListPrefix(tok, bucket, prefix string) ([]string, error) {
	var names []string
	pageToken := ""
	client := &http.Client{Timeout: 60 * time.Second}
	for {
		u := fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o?prefix=%s",
			url.PathEscape(bucket), url.QueryEscape(prefix))
		if pageToken != "" {
			u += "&pageToken=" + url.QueryEscape(pageToken)
		}
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("list %d: %s", resp.StatusCode, strutil.TruncateStr(string(body), 300))
		}
		var out struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		json.Unmarshal(body, &out)
		for _, it := range out.Items {
			names = append(names, it.Name)
		}
		if out.NextPageToken == "" {
			break
		}
		pageToken = out.NextPageToken
	}
	return names, nil
}

func gcsDownload(tok, bucket, object string) ([]byte, error) {
	u := fmt.Sprintf("https://storage.googleapis.com/storage/v1/b/%s/o/%s?alt=media",
		url.PathEscape(bucket), url.PathEscape(object))
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download %d: %s", resp.StatusCode, strutil.TruncateStr(string(body), 300))
	}
	return body, nil
}

// ─── download & decode batch results ──────────────────────────────────────

// downloadBatchResults enumerates the output prefix, parses prediction JSONL,
// and writes each embedded image to outDir.
func downloadBatchResults(tok, gcsOutputDir, outDir string, keys []string) error {
	bucket, prefix, err := parseGCSURI(gcsOutputDir)
	if err != nil {
		return err
	}
	objects, err := gcsListPrefix(tok, bucket, prefix)
	if err != nil {
		return err
	}

	var jsonlObjects []string
	for _, o := range objects {
		if strings.HasSuffix(o, ".jsonl") || strings.Contains(o, "predictions") {
			jsonlObjects = append(jsonlObjects, o)
		}
	}
	if len(jsonlObjects) == 0 {
		return fmt.Errorf("no prediction jsonl under %s", gcsOutputDir)
	}

	idx := 0
	saved, failed := 0, 0
	for _, obj := range jsonlObjects {
		data, err := gcsDownload(tok, bucket, obj)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  download %s: %v\n", obj, err)
			continue
		}
		scanner := bytesLineScanner(data)
		for scanner.next() {
			line := scanner.line()
			if len(line) == 0 {
				continue
			}
			var rec struct {
				Status   string            `json:"status"`
				Labels   map[string]string `json:"labels"`
				Response json.RawMessage   `json:"response"`
				Error    json.RawMessage   `json:"error"`
			}
			if err := json.Unmarshal(line, &rec); err != nil {
				fmt.Fprintf(os.Stderr, "  parse line: %v\n", err)
				failed++
				continue
			}
			if len(rec.Error) > 0 && string(rec.Error) != "null" {
				fmt.Fprintf(os.Stderr, "  error: %s\n", strutil.TruncateStr(string(rec.Error), 200))
				failed++
				idx++
				continue
			}

			imgB64, mime := extractInlineImage(rec.Response)
			if imgB64 == "" {
				fmt.Fprintf(os.Stderr, "  no image in prediction %d\n", idx)
				failed++
				idx++
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(imgB64)
			if err != nil {
				raw, err = base64.URLEncoding.DecodeString(imgB64)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  decode %d: %v\n", idx, err)
					failed++
					idx++
					continue
				}
			}

			key := ""
			if rec.Labels != nil {
				key = rec.Labels["qai_key"]
			}
			if key == "" && idx < len(keys) {
				key = keys[idx]
			}
			ext := extFromMime(mime)
			outName := key + ext
			if outName == ext {
				outName = fmt.Sprintf("result_%04d%s", idx, ext)
			}
			outPath := filepath.Join(outDir, outName)
			if err := os.WriteFile(outPath, raw, 0644); err != nil {
				fmt.Fprintf(os.Stderr, "  write %s: %v\n", outPath, err)
				failed++
				idx++
				continue
			}
			fmt.Fprintf(os.Stderr, "  saved %s\n", outPath)
			saved++
			idx++
		}
	}
	fmt.Fprintf(os.Stderr, "Done. %d saved, %d failed.\n", saved, failed)
	if failed > 0 && saved == 0 {
		return fmt.Errorf("all predictions failed")
	}
	return nil
}

func extractInlineImage(response json.RawMessage) (string, string) {
	if len(response) == 0 {
		return "", ""
	}
	var r struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(response, &r); err != nil {
		return "", ""
	}
	for _, c := range r.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData.Data != "" {
				return p.InlineData.Data, p.InlineData.MimeType
			}
		}
	}
	return "", ""
}

// ─── small utilities ──────────────────────────────────────────────────────

// extractEditedImage searches a qai /images/edit response for a base64 image.
// Covers xai (base64), gemini (images[0].base64 / output.base64), and
// common OpenAI-style (data[0].b64_json) shapes.
func extractEditedImage(resp map[string]any) string {
	if s, _ := resp["base64"].(string); s != "" {
		return s
	}
	if s, _ := resp["b64_json"].(string); s != "" {
		return s
	}
	if s, _ := resp["image_base64"].(string); s != "" {
		return s
	}
	if arr, ok := resp["images"].([]any); ok {
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				if s, _ := m["base64"].(string); s != "" {
					return s
				}
				if s, _ := m["b64_json"].(string); s != "" {
					return s
				}
				if s, _ := m["data"].(string); s != "" {
					return s
				}
			}
			if s, ok := it.(string); ok && len(s) > 200 {
				return s
			}
		}
	}
	if arr, ok := resp["data"].([]any); ok {
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				if s, _ := m["b64_json"].(string); s != "" {
					return s
				}
				if s, _ := m["base64"].(string); s != "" {
					return s
				}
			}
		}
	}
	if m, ok := resp["output"].(map[string]any); ok {
		if s, _ := m["base64"].(string); s != "" {
			return s
		}
	}
	return ""
}

// readImageUpright returns image bytes with landscape images rotated 90° CCW
// to portrait. For document scans this prevents the model from reading
// sideways text. When rotated, output is re-encoded as PNG regardless of
// input format. Returns (bytes, wasRotated, err).
func readImageUpright(path string) ([]byte, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return raw, false, nil
	}
	if cfg.Width <= cfg.Height {
		return raw, false, nil
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, false, nil
	}
	rotated := rotate90CCW(img)

	var buf bytes.Buffer
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".jpg" || ext == ".jpeg" {
		if err := jpeg.Encode(&buf, rotated, &jpeg.Options{Quality: 92}); err != nil {
			return raw, false, nil
		}
	} else {
		if err := png.Encode(&buf, rotated); err != nil {
			return raw, false, nil
		}
	}
	return buf.Bytes(), true, nil
}

func rotate90CCW(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dst.Set(y, w-1-x, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func listImages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".webp":
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

func editedFilename(in string) string {
	base := filepath.Base(in)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return stem + "_edited" + ext
}

func mimeFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func extFromMime(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func parseGCSURI(uri string) (string, string, error) {
	if !strings.HasPrefix(uri, "gs://") {
		return "", "", fmt.Errorf("not a gs:// URI: %s", uri)
	}
	rest := strings.TrimPrefix(uri, "gs://")
	slash := strings.Index(rest, "/")
	if slash == -1 {
		return rest, "", nil
	}
	return rest[:slash], rest[slash+1:], nil
}

// bytesLineScan is a tiny newline scanner that avoids bufio's token-size
// limits — prediction lines with base64 images can easily exceed 1 MB.
type bytesLineScan struct {
	data []byte
	pos  int
	cur  []byte
}

func bytesLineScanner(data []byte) *bytesLineScan {
	return &bytesLineScan{data: data}
}

func (b *bytesLineScan) next() bool {
	if b.pos >= len(b.data) {
		return false
	}
	start := b.pos
	for b.pos < len(b.data) && b.data[b.pos] != '\n' {
		b.pos++
	}
	b.cur = b.data[start:b.pos]
	if b.pos < len(b.data) {
		b.pos++ // consume \n
	}
	if n := len(b.cur); n > 0 && b.cur[n-1] == '\r' {
		b.cur = b.cur[:n-1]
	}
	return true
}

func (b *bytesLineScan) line() []byte { return b.cur }
