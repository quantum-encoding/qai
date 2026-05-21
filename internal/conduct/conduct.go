package conduct

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"path/filepath"
	"strings"
	"time"
)

// Cfg is set by main before any command runs.
var Cfg *config.Config

// qaiBase() and qaiKey() are read from cfg (set in main).
// These accessors keep the rest of conduct.go unchanged.
func qaiBase() string { return Cfg.API.BaseURL }
func qaiKey() string  { return Cfg.API.APIKey }

func CmdConduct(args []string) {
	if len(args) == 0 {
		conductUsage()
		os.Exit(1)
	}

	action := args[0]
	rest := args[1:]

	switch action {
	case "help", "--help", "-h":
		conductUsage()
		return
	case "chat":
		conductChat(rest)
	case "image":
		conductImage(rest)
	case "image-edit":
		conductImageEdit(rest)
	case "video":
		conductVideo(rest)
	case "tts":
		conductTTS(rest)
	case "transcribe":
		conductTranscribe(rest)
	case "music":
		conductMusic(rest)
	case "sfx":
		conductSFX(rest)
	case "clone-voice":
		conductCloneVoice(rest)
	case "search":
		conductSearch(rest)
	case "web":
		conductWeb(rest)
	case "context":
		conductContext(rest)
	case "screenshot":
		conductScreenshot(rest)
	case "scrape":
		conductScrape(rest)
	case "scan":
		conductScan(rest)
	case "models":
		conductModels()
	case "balance":
		conductBalance()
	case "job":
		conductJob(rest)
	case "gpu":
		conductGPU(rest)
	default:
		fmt.Fprintf(os.Stderr, "qai conduct: unknown action %q\n", action)
		conductUsage()
		os.Exit(1)
	}
}

// ── HTTP helper ─────────────────────────────────────────────────────────────

func qaiAPI(method, path string, body any) ([]byte, error) {
	if qaiKey() == "" {
		return nil, fmt.Errorf("QAI_API_KEY not set")
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, qaiBase()+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+qaiKey())
	req.Header.Set("X-API-Key", qaiKey())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func printJSON(data []byte) {
	var v any
	if json.Unmarshal(data, &v) == nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(v)
	} else {
		os.Stdout.Write(data)
	}
}

func dieAPI(err error) {
	fmt.Fprintf(os.Stderr, "qai conduct: %v\n", err)
	os.Exit(1)
}

func saveBase64(b64, dir, ext string) string {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// Try URL-safe encoding
		data, err = base64.URLEncoding.DecodeString(b64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to decode base64\n")
			return ""
		}
	}
	outDir := filepath.Join(config.Home, dir)
	os.MkdirAll(outDir, 0755)
	name := time.Now().Format("20060102-150405") + ext
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save %s: %v\n", path, err)
		return ""
	}
	return path
}

// ── Chat ────────────────────────────────────────────────────────────────────

func conductChat(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct chat <model> \"message\" [--system \"prompt\"] [--max-tokens N] [--temperature F]")
		os.Exit(1)
	}

	model := args[0]
	message := args[1]
	body := map[string]any{"model": model, "message": message}

	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--system":
			if i+1 < len(args) {
				body["system_prompt"] = args[i+1]
				i++
			}
		case "--max-tokens":
			if i+1 < len(args) {
				body["max_tokens"] = parseIntArg(args[i+1])
				i++
			}
		case "--temperature":
			if i+1 < len(args) {
				body["temperature"] = parseFloatArg(args[i+1])
				i++
			}
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/chat", body)
	if err != nil {
		dieAPI(err)
	}

	// Try to extract just the text response
	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if text, ok := resp["text"].(string); ok {
			fmt.Println(text)
			return
		}
		if text, ok := resp["response"].(string); ok {
			fmt.Println(text)
			return
		}
	}
	os.Stdout.Write(data)
	fmt.Println()
}

// ── Image ───────────────────────────────────────────────────────────────────

// imageModelAliases maps human-friendly names (and shorthand) to the
// canonical API model id. Lookup is case-insensitive on the alias side.
// Whichever route the user takes — positional, --model flag, or the
// registry id verbatim — resolves to the same canonical id sent to the
// broker.
var imageModelAliases = map[string]string{
	// Gemini 3 Pro Image — "Nano Banana Pro". Default since 2026-05-02.
	"nano-banana-pro":          "gemini-3-pro-image-preview",
	"nano-banana":              "gemini-3-pro-image-preview",
	"gemini-pro":               "gemini-3-pro-image-preview",
	"gemini":                   "gemini-3-pro-image-preview",
	"gemini-3-pro-image-preview": "gemini-3-pro-image-preview",
	// Gemini 2.5 Flash Image — "Nano Banana" (the original)
	"nano-banana-flash":      "gemini-2.5-flash-image",
	"gemini-flash":           "gemini-2.5-flash-image",
	"gemini-2.5-flash-image": "gemini-2.5-flash-image",
	// xAI Grok Imagine
	"grok":               "grok-imagine-image",
	"grok-imagine":       "grok-imagine-image",
	"grok-imagine-image": "grok-imagine-image",
	// OpenAI GPT-Image
	"gpt":         "gpt-image-1",
	"openai":      "gpt-image-1",
	"gpt-image-1": "gpt-image-1",
}

func resolveImageModel(alias string) string {
	if canonical, ok := imageModelAliases[strings.ToLower(strings.TrimSpace(alias))]; ok {
		return canonical
	}
	// Unknown id — pass through verbatim so the broker can reject it
	// with a useful error. Avoids silently swallowing new model ids.
	return alias
}

func conductImage(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct image \"prompt\" [model] [--count N] [--aspect 16:9] [--size WxH]")
		os.Exit(1)
	}

	// Default: Gemini 3 Pro Image (Nano Banana Pro). Strongest realistic
	// output of the three providers wired here.
	body := map[string]any{"prompt": args[0], "model": "gemini-3-pro-image-preview", "count": 1}

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--model":
			if i+1 < len(args) { body["model"] = resolveImageModel(args[i+1]); i++ }
		case "--count":
			if i+1 < len(args) { body["count"] = parseIntArg(args[i+1]); i++ }
		case "--aspect":
			if i+1 < len(args) { body["aspect_ratio"] = args[i+1]; i++ }
		case "--size":
			if i+1 < len(args) { body["size"] = args[i+1]; i++ }
		default:
			// Positional model: first non-flag, non-flag-value arg after
			// the prompt. Previously silently dropped — now resolved
			// through the alias map so "nano-banana-pro" and the like
			// route to the canonical id.
			if !strings.HasPrefix(args[i], "-") {
				body["model"] = resolveImageModel(args[i])
			}
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/images/generate", body)
	if err != nil {
		dieAPI(err)
	}

	// Save base64 images
	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if images, ok := resp["images"].([]any); ok {
			for _, img := range images {
				if imgMap, ok := img.(map[string]any); ok {
					if b64, ok := imgMap["base64"].(string); ok {
						path := saveBase64(b64, "Pictures/generated", ".png")
						if path != "" {
							fmt.Println(path)
						}
					}
				}
			}
			return
		}
		if b64, ok := resp["base64"].(string); ok {
			path := saveBase64(b64, "Pictures/generated", ".png")
			if path != "" {
				fmt.Println(path)
			}
			return
		}
	}
	printJSON(data)
}

func conductImageEdit(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct image-edit <input.png> \"prompt\" [--model M]")
		os.Exit(1)
	}

	imgData, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai conduct: cannot read %s: %v\n", args[0], err)
		os.Exit(1)
	}

	body := map[string]any{
		"prompt":       args[1],
		"image_base64": base64.StdEncoding.EncodeToString(imgData),
		"model":        "gpt-image-1",
	}

	for i := 2; i < len(args); i++ {
		if args[i] == "--model" && i+1 < len(args) {
			body["model"] = args[i+1]
			i++
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/images/edit", body)
	if err != nil {
		dieAPI(err)
	}

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if b64, ok := resp["base64"].(string); ok {
			path := saveBase64(b64, "Pictures/generated", ".png")
			if path != "" {
				fmt.Println(path)
			}
			return
		}
	}
	printJSON(data)
}

// ── Video ───────────────────────────────────────────────────────────────────

func conductVideo(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct video \"prompt\" [--model M] [--duration N]")
		os.Exit(1)
	}

	body := map[string]any{
		"job_type": "video/generate",
		"params": map[string]any{
			"prompt":           args[0],
			"model":            "grok-imagine-video",
			"duration_seconds": 8,
		},
	}
	params := body["params"].(map[string]any)

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--model":
			if i+1 < len(args) { params["model"] = args[i+1]; i++ }
		case "--duration":
			if i+1 < len(args) { params["duration_seconds"] = parseIntArg(args[i+1]); i++ }
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/jobs", body)
	if err != nil {
		dieAPI(err)
	}

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if id, ok := resp["job_id"].(string); ok {
			fmt.Fprintf(os.Stderr, "Job queued. Poll with: qai conduct job %s\n", id)
			fmt.Println(id)
			return
		}
	}
	printJSON(data)
}

// ── Audio ───────────────────────────────────────────────────────────────────

func conductTTS(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct tts \"text\" [--voice alloy]")
		os.Exit(1)
	}

	body := map[string]any{"text": args[0], "voice": "alloy", "model": "tts-1"}
	for i := 1; i < len(args); i++ {
		if args[i] == "--voice" && i+1 < len(args) { body["voice"] = args[i+1]; i++ }
	}

	data, err := qaiAPI("POST", "/qai/v1/audio/tts", body)
	if err != nil { dieAPI(err) }

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if b64, ok := resp["audio_base64"].(string); ok {
			path := saveBase64(b64, "Music/generated", ".mp3")
			if path != "" { fmt.Println(path) }
			return
		}
	}
	printJSON(data)
}

func conductTranscribe(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct transcribe <audio.mp3> [--language en]")
		os.Exit(1)
	}

	audioData, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai conduct: cannot read %s: %v\n", args[0], err)
		os.Exit(1)
	}

	body := map[string]any{
		"audio_base64": base64.StdEncoding.EncodeToString(audioData),
		"model":        "whisper-1",
	}
	for i := 1; i < len(args); i++ {
		if args[i] == "--language" && i+1 < len(args) { body["language"] = args[i+1]; i++ }
	}

	data, err := qaiAPI("POST", "/qai/v1/audio/stt", body)
	if err != nil { dieAPI(err) }

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if text, ok := resp["text"].(string); ok {
			fmt.Println(text)
			return
		}
	}
	printJSON(data)
}

func conductMusic(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct music \"prompt\" [--duration 30]")
		os.Exit(1)
	}

	body := map[string]any{"prompt": args[0], "duration_seconds": 30}
	for i := 1; i < len(args); i++ {
		if args[i] == "--duration" && i+1 < len(args) { body["duration_seconds"] = parseIntArg(args[i+1]); i++ }
	}

	data, err := qaiAPI("POST", "/qai/v1/audio/music", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

func conductSFX(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct sfx \"prompt\" [--duration N]")
		os.Exit(1)
	}

	body := map[string]any{"prompt": args[0]}
	for i := 1; i < len(args); i++ {
		if args[i] == "--duration" && i+1 < len(args) { body["duration_seconds"] = parseIntArg(args[i+1]); i++ }
	}

	data, err := qaiAPI("POST", "/qai/v1/audio/sound-effects", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

func conductCloneVoice(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct clone-voice \"name\" <audio.mp3>")
		os.Exit(1)
	}

	audioData, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai conduct: cannot read %s: %v\n", args[1], err)
		os.Exit(1)
	}

	body := map[string]any{
		"name":         args[0],
		"audio_base64": base64.StdEncoding.EncodeToString(audioData),
	}

	data, err := qaiAPI("POST", "/qai/v1/voices/clone", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

// ── Search ──────────────────────────────────────────────────────────────────

func conductSearch(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct search \"query\" [--provider xai|claude|openai|surrealdb|deepseek|gemini]")
		os.Exit(1)
	}

	body := map[string]any{"query": args[0]}
	for i := 1; i < len(args); i++ {
		if args[i] == "--provider" && i+1 < len(args) { body["provider"] = args[i+1]; i++ }
	}

	data, err := qaiAPI("POST", "/qai/v1/rag/surreal/search", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

func conductWeb(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct web \"query\" [--count 5] [--freshness pd|pw|pm]")
		os.Exit(1)
	}

	body := map[string]any{"query": args[0], "count": 5}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--count":
			if i+1 < len(args) { body["count"] = parseIntArg(args[i+1]); i++ }
		case "--freshness":
			if i+1 < len(args) { body["freshness"] = args[i+1]; i++ }
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/search/web", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

func conductContext(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct context \"query\" [--count 3]")
		os.Exit(1)
	}

	body := map[string]any{"query": args[0], "count": 3}
	for i := 1; i < len(args); i++ {
		if args[i] == "--count" && i+1 < len(args) { body["count"] = parseIntArg(args[i+1]); i++ }
	}

	data, err := qaiAPI("POST", "/qai/v1/search/context", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

// ── Scraping ────────────────────────────────────────────────────────────────

func conductScreenshot(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct screenshot <url> [urls...] [--full-page] [--width N] [--height N]")
		os.Exit(1)
	}

	var urls []string
	body := map[string]any{"width": 1280, "height": 800, "full_page": false}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--full-page":
			body["full_page"] = true
		case "--width":
			if i+1 < len(args) { body["width"] = parseIntArg(args[i+1]); i++ }
		case "--height":
			if i+1 < len(args) { body["height"] = parseIntArg(args[i+1]); i++ }
		default:
			urls = append(urls, args[i])
		}
	}
	body["urls"] = urls

	data, err := qaiAPI("POST", "/qai/v1/scraper/screenshot", body)
	if err != nil { dieAPI(err) }

	// Save screenshots
	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if images, ok := resp["images"].([]any); ok {
			for _, img := range images {
				if imgMap, ok := img.(map[string]any); ok {
					if b64, ok := imgMap["base64"].(string); ok {
						path := saveBase64(b64, "Pictures/screenshots", ".png")
						if path != "" { fmt.Println(path) }
					}
				}
			}
			return
		}
	}
	printJSON(data)
}

func conductScrape(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct scrape \"name\" <url> [--selector \"nav a\"] [--content \"article\"] [--max-pages 50] [--recursive]")
		os.Exit(1)
	}

	body := map[string]any{
		"name":      args[0],
		"url":       args[1],
		"selector":  "nav a[href]",
		"content":   "article, main",
		"recursive": false,
		"max_pages": 50,
	}

	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--selector":
			if i+1 < len(args) { body["selector"] = args[i+1]; i++ }
		case "--content":
			if i+1 < len(args) { body["content"] = args[i+1]; i++ }
		case "--max-pages":
			if i+1 < len(args) { body["max_pages"] = parseIntArg(args[i+1]); i++ }
		case "--recursive":
			body["recursive"] = true
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/scraper/scrape", body)
	if err != nil { dieAPI(err) }

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if id, ok := resp["job_id"].(string); ok {
			fmt.Fprintf(os.Stderr, "Scrape job queued. Poll with: qai conduct job %s\n", id)
			fmt.Println(id)
			return
		}
	}
	printJSON(data)
}

func conductScan(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct scan <source> [--name N] [--languages rust,go,ts]")
		os.Exit(1)
	}

	body := map[string]any{"source": args[0]}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) { body["name"] = args[i+1]; i++ }
		case "--languages":
			if i+1 < len(args) { body["languages"] = strings.Split(args[i+1], ","); i++ }
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/scanner/scan", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

// ── Info ────────────────────────────────────────────────────────────────────

func conductModels() {
	data, err := qaiAPI("GET", "/qai/v1/models", nil)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

func conductBalance() {
	data, err := qaiAPI("GET", "/qai/v1/account/balance", nil)
	if err != nil { dieAPI(err) }

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if display, ok := resp["balance_display"].(string); ok {
			fmt.Println(display)
			return
		}
		if bal, ok := resp["balance"].(string); ok {
			fmt.Println(bal)
			return
		}
	}
	printJSON(data)
}

func conductJob(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct job <job-id>")
		os.Exit(1)
	}

	data, err := qaiAPI("GET", "/qai/v1/jobs/"+args[0], nil)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

// ── Compute ─────────────────────────────────────────────────────────────────

func conductGPU(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct gpu <template> [--zone us-central1-a] [--spot] [--ssh-key \"key\"] [--teardown N]")
		os.Exit(1)
	}

	body := map[string]any{"template": args[0], "zone": "us-central1-a"}

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--zone":
			if i+1 < len(args) { body["zone"] = args[i+1]; i++ }
		case "--spot":
			body["spot"] = true
		case "--ssh-key":
			if i+1 < len(args) { body["ssh_public_key"] = args[i+1]; i++ }
		case "--teardown":
			if i+1 < len(args) { body["auto_teardown_minutes"] = parseIntArg(args[i+1]); i++ }
		}
	}

	data, err := qaiAPI("POST", "/qai/v1/compute/provision", body)
	if err != nil { dieAPI(err) }
	printJSON(data)
}

// ── Arg helpers ─────────────────────────────────────────────────────────────

func parseIntArg(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

func parseFloatArg(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// ── Usage ───────────────────────────────────────────────────────────────────

func conductUsage() {
	fmt.Fprint(os.Stderr, `qai conduct — AI Conductor API (fire-and-forget, no MCP server)

Chat:
  qai conduct chat <model> "message" [--system "prompt"] [--max-tokens N]

Media:
  qai conduct image "prompt" [--model M] [--count N] [--aspect 16:9]
  qai conduct image-edit <input.png> "prompt" [--model M]
  qai conduct video "prompt" [--model M] [--duration N]
  qai conduct tts "text" [--voice alloy|echo|fable|onyx|nova|shimmer]
  qai conduct transcribe <audio.mp3> [--language en]
  qai conduct music "prompt" [--duration 30]
  qai conduct sfx "prompt" [--duration N]
  qai conduct clone-voice "name" <audio.mp3>

Search:
  qai conduct search "query" [--provider xai|claude|openai|surrealdb]
  qai conduct web "query" [--count 5] [--freshness pd|pw|pm]
  qai conduct context "query" [--count 3]

Scraping:
  qai conduct screenshot <url> [urls...] [--full-page]
  qai conduct scrape "name" <url> [--selector "nav a"] [--max-pages 50]
  qai conduct scan <source> [--languages rust,go,ts]

Info:
  qai conduct models                          List all models + pricing
  qai conduct balance                         Check credit balance
  qai conduct job <job-id>                    Poll async job status

Compute:
  qai conduct gpu <template> [--zone Z] [--spot]
`)
}
