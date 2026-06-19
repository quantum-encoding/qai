package conduct

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"

	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
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

// apiError is a sentinel-flavoured error so dieAPI can decide whether the
// blame is on the user's environment (no key, no network) vs. the broker
// (4xx/5xx with a response body worth surfacing).
type apiError struct {
	kind    string // "auth", "network", "broker", "decode"
	status  int    // HTTP status for broker errors; 0 otherwise
	method  string
	path    string
	body    string // truncated response body for broker errors
	wrapped error  // underlying error for network/decode
}

func (e *apiError) Error() string {
	switch e.kind {
	case "auth":
		return "QAI_API_KEY not set"
	case "network":
		return fmt.Sprintf("network error talking to %s %s: %v", e.method, qaiBase()+e.path, e.wrapped)
	case "broker":
		return fmt.Sprintf("broker %d on %s %s: %s", e.status, e.method, e.path, e.body)
	case "decode":
		return fmt.Sprintf("could not read broker response from %s %s: %v", e.method, e.path, e.wrapped)
	default:
		return e.wrapped.Error()
	}
}

func qaiAPI(method, path string, body any) ([]byte, error) {
	if qaiKey() == "" {
		return nil, &apiError{kind: "auth", method: method, path: path}
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
		return nil, &apiError{kind: "network", method: method, path: path, wrapped: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &apiError{kind: "decode", method: method, path: path, wrapped: err}
	}

	if resp.StatusCode >= 400 {
		bodyStr := strings.TrimSpace(string(respBody))
		if len(bodyStr) > 800 {
			bodyStr = bodyStr[:800] + "...(truncated)"
		}
		return nil, &apiError{
			kind:   "broker",
			status: resp.StatusCode,
			method: method,
			path:   path,
			body:   bodyStr,
		}
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

// dieAPI prints a structured failure message and exits. The message
// distinguishes qai-side problems (missing API key, no network) from
// broker-side ones (HTTP 4xx/5xx with the body the broker returned), so
// the user knows whether to fix their environment or their request.
func dieAPI(err error) {
	if ae, ok := err.(*apiError); ok {
		switch ae.kind {
		case "auth":
			fmt.Fprintln(os.Stderr, "qai conduct: QAI_API_KEY not set")
			fmt.Fprintln(os.Stderr, "  → fix: export QAI_API_KEY=<key>  (get one at https://quantumencoding.ai)")
		case "network":
			fmt.Fprintf(os.Stderr, "qai conduct: cannot reach broker at %s\n", qaiBase())
			fmt.Fprintf(os.Stderr, "  detail: %v\n", ae.wrapped)
			fmt.Fprintln(os.Stderr, "  → fix: check connectivity and QAI_BASE_URL (defaults to the public broker)")
		case "decode":
			fmt.Fprintf(os.Stderr, "qai conduct: broker reply unreadable on %s %s: %v\n", ae.method, ae.path, ae.wrapped)
			fmt.Fprintln(os.Stderr, "  → fix: retry; if it persists this is a broker-side outage")
		case "broker":
			fmt.Fprintf(os.Stderr, "qai conduct: broker returned HTTP %d on %s %s\n", ae.status, ae.method, ae.path)
			if ae.body != "" {
				fmt.Fprintf(os.Stderr, "  broker said: %s\n", ae.body)
			}
			switch {
			case ae.status == 401 || ae.status == 403:
				fmt.Fprintln(os.Stderr, "  → fix: this is an auth error from the broker. Check QAI_API_KEY is valid and not revoked.")
			case ae.status == 402:
				fmt.Fprintln(os.Stderr, "  → fix: out of credits. Top up at https://quantumencoding.ai or run `qai conduct balance`.")
			case ae.status == 404:
				fmt.Fprintln(os.Stderr, "  → fix: route or model id unknown to the broker. List options with `qai conduct models`.")
			case ae.status == 429:
				fmt.Fprintln(os.Stderr, "  → fix: provider rate limit. Retry after a short delay, or lower --parallel.")
			case ae.status >= 500:
				fmt.Fprintln(os.Stderr, "  → fix: broker-side error. Retry; if it persists check provider status.")
			}
		default:
			fmt.Fprintf(os.Stderr, "qai conduct: %v\n", err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "qai conduct: %v\n", err)
	}
	os.Exit(1)
}

// saveSeq is incremented under a mutex to disambiguate concurrent
// saves that land in the same wall-clock second. Without it, parallel
// image-batch generations stomp each other's filenames.
var (
	saveMu  sync.Mutex
	saveSeq uint64
)

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

	// Resolve a non-colliding filename even under parallel calls in
	// the same wall-clock second. Lock the counter, then probe for an
	// unused name — both the second-resolution timestamp and a
	// per-process sequence number combine to avoid the
	// 20260521-163347.png + 20260521-163347.png collision that
	// `qai image --batch --parallel N` produced before.
	saveMu.Lock()
	defer saveMu.Unlock()

	base := time.Now().Format("20060102-150405")
	name := base + ext
	path := filepath.Join(outDir, name)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		saveSeq++
		name = fmt.Sprintf("%s-%d%s", base, saveSeq, ext)
		path = filepath.Join(outDir, name)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to save %s: %v\n", path, err)
		return ""
	}
	return path
}

// ── Chat ────────────────────────────────────────────────────────────────────

// Chat is the public, programmatic entry point for /qai/v1/chat. Wraps
// the same HTTP path the CLI uses so `qai chat` (and any internal caller)
// can reuse the broker round-trip + response shape without duplicating the
// auth/decode plumbing. system may be empty; maxTokens=0 means broker default.
// Returns the assistant text on success, or a *apiError on failure.
func Chat(model, system, message string, maxTokens int) (string, error) {
	if model == "" {
		return "", fmt.Errorf("model is required")
	}
	if message == "" {
		return "", fmt.Errorf("message is required")
	}
	// Broker expects OpenAI-shape `messages: [{role, content}]`. System
	// prompt goes in as the first message with role=system when provided.
	msgs := make([]map[string]string, 0, 2)
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": message})
	body := map[string]any{"model": model, "messages": msgs}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	data, err := qaiAPI("POST", "/qai/v1/chat", body)
	if err != nil {
		return "", err
	}
	// Try common response shapes in order: OpenAI choices, top-level text,
	// Anthropic content blocks. Fall back to raw bytes if none match.
	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
			if c0, ok := choices[0].(map[string]any); ok {
				if msg, ok := c0["message"].(map[string]any); ok {
					if content, ok := msg["content"].(string); ok {
						return content, nil
					}
				}
			}
		}
		if text, ok := resp["text"].(string); ok {
			return text, nil
		}
		if text, ok := resp["response"].(string); ok {
			return text, nil
		}
		// Broker's qai-shape: {cached, content: "<string>", model, usage}.
		// Order matters — check string-typed content BEFORE the Anthropic
		// content-array shape, since both use the same field name.
		if text, ok := resp["content"].(string); ok {
			return text, nil
		}
		if content, ok := resp["content"].([]any); ok && len(content) > 0 {
			if c0, ok := content[0].(map[string]any); ok {
				if text, ok := c0["text"].(string); ok {
					return text, nil
				}
			}
		}
	}
	return string(data), nil
}

// DieAPI is the exported counterpart to dieAPI so other packages that
// call Chat() can render the same structured failure message.
func DieAPI(err error) { dieAPI(err) }

// countingReader wraps an io.Reader and fires onRead(sent, total) on
// every successful read with the cumulative bytes consumed so far.
// Used by APIMultipartProgress to drive the upload progress bar.
type countingReader struct {
	r      io.Reader
	total  int64
	sent   int64
	onRead func(sent, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.sent += int64(n)
		if c.onRead != nil {
			c.onRead(c.sent, c.total)
		}
	}
	return n, err
}

// API is the exported helper for any package that needs to talk to the
// broker on a path that doesn't have its own typed wrapper. Same shape
// as the internal qaiAPI: method+path+body → body bytes, with the
// apiError sentinel so DieAPI can classify the failure. For multipart
// uploads (POST /qai/v1/files) use APIMultipart below.
func API(method, path string, body any) ([]byte, error) {
	return qaiAPI(method, path, body)
}

// APIMultipart is the multipart variant of API. Same as
// APIMultipartProgress but with a no-op progress callback — kept for
// callers that don't need a progress bar (so they don't have to pass
// a nil func).
func APIMultipart(path, partName, filename, mimeType string, content []byte) ([]byte, error) {
	return APIMultipartProgress(path, partName, filename, mimeType, content, nil)
}

// APIMultipartProgress is APIMultipart with a progress callback fired
// on every wire-write as the body uploads. callback(sent, total) is
// called from the HTTP client goroutine; total = body size including
// multipart framing (the few hundred bytes of headers + boundary
// markers around the file part). Pass nil for no progress.
//
// Used by `qai media` to POST a file to /qai/v1/files. partName is the
// form field name ("file"); filename is the user-visible name on the
// part header; content is the raw bytes; mimeType is the Content-Type
// baked into the part header (the broker uses this for its MIME
// allowlist).
//
// Reads the file into memory before sending — that's fine because the
// broker caps uploads at 100 MiB and the media path auto-compresses
// before this gets called. For arbitrary streaming we'd plumb an
// io.Reader; not worth the complication today.
func APIMultipartProgress(path, partName, filename, mimeType string, content []byte, callback func(sent, total int64)) ([]byte, error) {
	if qaiKey() == "" {
		return nil, &apiError{kind: "auth", method: "POST", path: path}
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	partHeaders := textproto.MIMEHeader{}
	partHeaders.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name=%q; filename=%q`, partName, filename))
	partHeaders.Set("Content-Type", mimeType)
	part, err := mw.CreatePart(partHeaders)
	if err != nil {
		return nil, &apiError{kind: "encode", method: "POST", path: path, wrapped: err}
	}
	if _, err := part.Write(content); err != nil {
		return nil, &apiError{kind: "encode", method: "POST", path: path, wrapped: err}
	}
	if err := mw.Close(); err != nil {
		return nil, &apiError{kind: "encode", method: "POST", path: path, wrapped: err}
	}

	totalBytes := int64(buf.Len())
	var body io.Reader = &buf
	if callback != nil {
		// Wrap the body so each Read fires the callback with cumulative
		// bytes sent. http.Client.Do reads the body to send it; the
		// counts here = bytes pushed onto the TCP socket, which is the
		// number we want to show in the progress bar.
		body = &countingReader{r: &buf, total: totalBytes, onRead: callback}
	}

	req, err := http.NewRequest("POST", qaiBase()+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+qaiKey())
	req.Header.Set("X-API-Key", qaiKey())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = totalBytes // helps the server bound the upload

	// Bumped timeout — uploading even 20 MB over a flaky residential
	// link can take longer than the default qaiAPI 120s. Caller controls
	// timeout via context once we plumb context.WithTimeout through
	// here; for now, 10 minutes is the ceiling.
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &apiError{kind: "network", method: "POST", path: path, wrapped: err}
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &apiError{kind: "decode", method: "POST", path: path, wrapped: err}
	}
	if resp.StatusCode >= 400 {
		bodyStr := strings.TrimSpace(string(respBody))
		if len(bodyStr) > 800 {
			bodyStr = bodyStr[:800] + "...(truncated)"
		}
		return nil, &apiError{
			kind:   "broker",
			status: resp.StatusCode,
			method: "POST",
			path:   path,
			body:   bodyStr,
		}
	}
	return respBody, nil
}

func conductChat(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct chat <model> \"message\" [--system \"prompt\"] [--max-tokens N] [--temperature F]")
		os.Exit(1)
	}

	model := args[0]
	message := args[1]
	system := ""
	maxTokens := 0
	var temperature float64
	hasTemp := false

	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--system":
			if i+1 < len(args) {
				system = args[i+1]
				i++
			}
		case "--max-tokens":
			if i+1 < len(args) {
				maxTokens = parseIntArg(args[i+1])
				i++
			}
		case "--temperature":
			if i+1 < len(args) {
				temperature = parseFloatArg(args[i+1])
				hasTemp = true
				i++
			}
		}
	}

	// Delegate the common path to Chat() so all chat callers share the
	// same broker shape; --temperature is handled inline because Chat()
	// doesn't take it (low-volume use, didn't want to widen the signature).
	if !hasTemp {
		text, err := Chat(model, system, message, maxTokens)
		if err != nil {
			dieAPI(err)
		}
		fmt.Println(text)
		return
	}
	// Temperature path: build the body locally with the extra field.
	msgs := make([]map[string]string, 0, 2)
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": message})
	body := map[string]any{"model": model, "messages": msgs, "temperature": temperature}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	data, err := qaiAPI("POST", "/qai/v1/chat", body)
	if err != nil {
		dieAPI(err)
	}
	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
			if c0, ok := choices[0].(map[string]any); ok {
				if msg, ok := c0["message"].(map[string]any); ok {
					if content, ok := msg["content"].(string); ok {
						fmt.Println(content)
						return
					}
				}
			}
		}
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
//
// The Nano Banana naming maps to the three Gemini image generations:
//
//	"nano banana"      = gemini-2.5-flash-image       (original)
//	"nano banana 2"    = gemini-3.1-flash-image-preview (faster successor)
//	"nano banana pro"  = gemini-3-pro-image-preview   (strongest realism)
//
// The xAI side has a standard and a quality variant:
//
//	"grok imagine"         = grok-imagine-image          ($0.02/img)
//	"grok imagine quality" = grok-imagine-image-quality  ($0.05–0.07/img)
var imageModelAliases = map[string]string{
	// ── Gemini 3 Pro Image — "Nano Banana Pro" (DEFAULT) ─────────────
	"nano-banana-pro":            "gemini-3-pro-image-preview",
	"gemini-pro":                 "gemini-3-pro-image-preview",
	"gemini":                     "gemini-3-pro-image-preview",
	"gemini-3-pro-image-preview": "gemini-3-pro-image-preview",

	// ── Gemini 3.1 Flash Image Preview — "Nano Banana 2" ─────────────
	"nano-banana-2":                  "gemini-3.1-flash-image-preview",
	"gemini-flash-2":                 "gemini-3.1-flash-image-preview",
	"gemini-3.1-flash-image-preview": "gemini-3.1-flash-image-preview",

	// ── Gemini 2.5 Flash Image — "Nano Banana" (the original) ────────
	"nano-banana":            "gemini-2.5-flash-image",
	"nano-banana-flash":      "gemini-2.5-flash-image",
	"gemini-flash":           "gemini-2.5-flash-image",
	"gemini-2.5-flash-image": "gemini-2.5-flash-image",

	// ── xAI Grok Imagine — standard ──────────────────────────────────
	"grok":               "grok-imagine-image",
	"grok-imagine":       "grok-imagine-image",
	"grok-imagine-image": "grok-imagine-image",

	// ── xAI Grok Imagine Quality — higher-res / better quality ───────
	// Official aliases (all four hit the same model server-side):
	//   grok-imagine-image-quality, grok-imagine-image-quality-latest,
	//   grok-imagine-image-quality-20260403, grok-imagine-image-pro
	"grok-quality":                      "grok-imagine-image-quality",
	"grok-imagine-quality":              "grok-imagine-image-quality",
	"grok-pro":                          "grok-imagine-image-quality",
	"grok-imagine-pro":                  "grok-imagine-image-quality",
	"grok-imagine-image-pro":            "grok-imagine-image-quality",
	"grok-imagine-image-quality":        "grok-imagine-image-quality",
	"grok-imagine-image-quality-latest": "grok-imagine-image-quality",

	// ── OpenAI GPT-Image ─────────────────────────────────────────────
	// Default `gpt` / `openai` shorthand resolves to gpt-image-2 — the
	// newest in the family. Older variants stay reachable via their
	// explicit ids (or shorthand). When OpenAI ships a successor, just
	// bump the two default-aliases below; per-variant aliases keep
	// existing scripts pinned.
	"gpt":              "gpt-image-2",
	"openai":           "gpt-image-2",
	"gpt-image-2":      "gpt-image-2",
	"gpt-2":            "gpt-image-2",
	"gpt-image-1.5":    "gpt-image-1.5",
	"gpt-1.5":          "gpt-image-1.5",
	"gpt-image-1":      "gpt-image-1",
	"gpt-1":            "gpt-image-1",
	"gpt-image-1-mini": "gpt-image-1-mini",
	"gpt-mini":         "gpt-image-1-mini",
	"chatgpt":          "chatgpt-image-latest",
	"chatgpt-image":    "chatgpt-image-latest",
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
		fmt.Fprintln(os.Stderr, "usage: qai conduct image \"prompt\" [model] [flags]")
		fmt.Fprintln(os.Stderr, "       qai conduct image --batch <file> [--parallel N] [model] [flags]")
		fmt.Fprintln(os.Stderr, "Flags: --count N --aspect 16:9 --size {1K|2K|1024x1024|...} --quality low|medium|high|auto --background transparent|opaque|auto --format png|jpeg|webp")
		os.Exit(1)
	}

	// Default: Gemini 3 Pro Image (Nano Banana Pro). Strongest realistic
	// output of the three providers wired here.
	flags := imageFlags{
		model: "gemini-3-pro-image-preview",
		count: 1,
	}

	// Two-pass parse: detect --batch first (changes the prompt source),
	// then process all other flags. The first positional that isn't a
	// flag value becomes the prompt in non-batch mode, or is treated as
	// a model alias if it doesn't look like a prompt sentence.
	var batchPath string
	parallel := 4
	for i := 0; i < len(args); i++ {
		if args[i] == "--batch" && i+1 < len(args) {
			batchPath = args[i+1]
		}
	}

	gotPrompt := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--batch":
			i++ // consume value (already captured above)
		case "--parallel":
			if i+1 < len(args) {
				parallel = parseIntArg(args[i+1])
				i++
			}
		case "--model":
			if i+1 < len(args) { flags.model = resolveImageModel(args[i+1]); i++ }
		case "--count":
			if i+1 < len(args) { flags.count = parseIntArg(args[i+1]); i++ }
		case "--aspect":
			if i+1 < len(args) { flags.aspect = args[i+1]; i++ }
		case "--size":
			if i+1 < len(args) { flags.size = args[i+1]; i++ }
		case "--quality":
			if i+1 < len(args) { flags.quality = args[i+1]; i++ }
		case "--background":
			if i+1 < len(args) { flags.background = args[i+1]; i++ }
		case "--format":
			if i+1 < len(args) { flags.format = args[i+1]; i++ }
		default:
			if strings.HasPrefix(args[i], "-") {
				continue // unknown flag — pass through silently
			}
			// First positional: prompt (only when not in batch mode).
			// Subsequent positionals: model alias.
			if batchPath == "" && !gotPrompt {
				flags.prompt = args[i]
				gotPrompt = true
			} else {
				flags.model = resolveImageModel(args[i])
			}
		}
	}

	// Translate user-neutral flags into the broker shape for THIS
	// model's family (see image_params.go). Unsupported flags get a
	// stderr warning instead of being silently dropped or hard-erroring.
	body := buildImageBody(flags)

	// Batch mode: prompts come from a file, body is the template.
	if batchPath != "" {
		runImageBatch(batchPath, parallel, body)
		return
	}

	if !gotPrompt {
		fmt.Fprintln(os.Stderr, "qai image: prompt required (or pass --batch <file>)")
		fmt.Fprintln(os.Stderr, "  → fix: qai image \"your prompt here\"  or  qai image --batch prompts.txt")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "sending request to %s...\n", flags.model)
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
		fmt.Fprintf(os.Stderr, "qai conduct image-edit: cannot read input image %s: %v\n", args[0], err)
		fmt.Fprintln(os.Stderr, "  → fix: pass a path to a real PNG/JPG file as the first argument")
		os.Exit(1)
	}

	b64 := base64.StdEncoding.EncodeToString(imgData)
	body := map[string]any{
		"prompt":       args[1],
		"image_base64": b64,
		"input_images": []string{b64},
		"model":        "gpt-image-1",
	}

	for i := 2; i < len(args); i++ {
		if args[i] == "--model" && i+1 < len(args) {
			body["model"] = resolveImageModel(args[i+1])
			i++
		}
	}

	fmt.Fprintf(os.Stderr, "sending request to %v...\n", body["model"])
	data, err := qaiAPI("POST", "/qai/v1/images/edit", body)
	if err != nil {
		dieAPI(err)
	}

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if b64 := extractEditedImage(resp); b64 != "" {
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

	fmt.Fprintf(os.Stderr, "sending request to %v...\n", params["model"])
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

// ttsModelAliases maps friendly provider names to concrete TTS model
// ids. The backend selects the provider by model-id prefix, so these let
// `--model xai` / `grok` reach the (excellent) xAI generator, `gemini`
// the Vertex TTS, etc., without the user memorising exact ids.
var ttsModelAliases = map[string]string{
	"xai": "grok-tts", "grok": "grok-tts", "grok-tts": "grok-tts",
	"gemini": "gemini-2.5-flash-preview-tts", "flash": "gemini-2.5-flash-preview-tts",
	"openai": "tts-1", "tts": "tts-1",
	"eleven": "eleven_multilingual_v2", "elevenlabs": "eleven_multilingual_v2",
}

// sttModelAliases — same idea for speech-to-text.
var sttModelAliases = map[string]string{
	"xai": "grok-stt", "grok": "grok-stt", "grok-stt": "grok-stt",
	"gemini": "gemini-2.5-flash", "flash": "gemini-2.5-flash",
	"openai": "whisper-1", "whisper": "whisper-1",
	"eleven": "scribe_v2", "elevenlabs": "scribe_v2",
}

// resolveAudioModel applies an alias table, falling through to the raw
// value so exact ids still work.
func resolveAudioModel(raw string, aliases map[string]string) string {
	if id, ok := aliases[strings.ToLower(raw)]; ok {
		return id
	}
	return raw
}

// defaultTTSVoice returns the provider-appropriate default voice when the
// user didn't pass one. Each provider needs a voice it actually accepts —
// Gemini TTS in particular hard-fails (502) without a prebuilt voice
// name, so we must supply one rather than leaving it empty. ElevenLabs
// falls through to its own server-side default.
func defaultTTSVoice(model string) string {
	switch {
	case strings.HasPrefix(model, "grok"):
		return "eve" // xAI's signature voice
	case strings.HasPrefix(model, "tts-") || strings.HasPrefix(model, "gpt-"):
		return "alloy" // OpenAI default
	case strings.HasPrefix(model, "gemini"):
		return "Kore" // a Gemini prebuilt voice — Gemini TTS 502s with no voice
	}
	return ""
}

// audioExtFor turns the provider-reported format into a clean file
// extension. Most providers return a simple token (mp3/wav/opus); Gemini
// returns a MIME-ish param string like "L16;codec=pcm;rate=24000" which
// must NOT be used verbatim (it produces a filename full of semicolons).
func audioExtFor(format string) string {
	f := strings.ToLower(strings.TrimSpace(format))
	switch {
	case f == "":
		return ".mp3"
	case strings.HasPrefix(f, "l16") || strings.Contains(f, "pcm"):
		return ".pcm"
	case f == "mp3" || f == "wav" || f == "opus" || f == "aac" || f == "flac":
		return "." + f
	default:
		return ".mp3"
	}
}

func conductTTS(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct tts \"text\" [voice] [--model xai|gemini|openai] [--voice eve] [--format mp3]")
		os.Exit(1)
	}

	text := args[0]
	model := "tts-1"
	voice := ""
	format := ""
	language := ""
	sampleRate := 0
	bitRate := 0
	speed := 0.0
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--voice":
			if i+1 < len(args) { voice = args[i+1]; i++ }
		case "--model", "-m":
			if i+1 < len(args) { model = resolveAudioModel(args[i+1], ttsModelAliases); i++ }
		case "--format":
			if i+1 < len(args) { format = args[i+1]; i++ }
		case "--language", "--lang":
			if i+1 < len(args) { language = args[i+1]; i++ }
		case "--sample-rate":
			if i+1 < len(args) { sampleRate = parseIntArg(args[i+1]); i++ }
		case "--bit-rate":
			if i+1 < len(args) { bitRate = parseIntArg(args[i+1]); i++ }
		case "--speed":
			if i+1 < len(args) { speed = parseFloatArg(args[i+1]); i++ }
		default:
			// Bare positional after the text is a voice (qai tts "hi" nova).
			if !strings.HasPrefix(args[i], "-") && voice == "" { voice = args[i] }
		}
	}
	if voice == "" { voice = defaultTTSVoice(model) }
	// xAI sounds robotic when it synthesises non-English text under the
	// default "en" pronunciation. When the caller targets grok-tts and
	// gives no language, lift the quality with CD-rate audio so at least
	// the codec isn't the bottleneck — but language is the big lever, so
	// surface it via --language.
	if sampleRate == 0 && strings.HasPrefix(model, "grok") { sampleRate = 44100 }

	body := map[string]any{"text": text, "model": model}
	if voice != "" { body["voice"] = voice }
	if format != "" { body["format"] = format }
	if language != "" { body["language"] = language }
	if sampleRate > 0 { body["sample_rate"] = sampleRate }
	if bitRate > 0 { body["bit_rate"] = bitRate }
	if speed > 0 { body["speed"] = speed }

	fmt.Fprintf(os.Stderr, "sending request to %v...\n", model)
	data, err := qaiAPI("POST", "/qai/v1/audio/tts", body)
	if err != nil { dieAPI(err) }

	var resp map[string]any
	if json.Unmarshal(data, &resp) == nil {
		if b64, ok := resp["audio_base64"].(string); ok {
			ext := ".mp3"
			if f, ok := resp["format"].(string); ok { ext = audioExtFor(f) }
			path := saveBase64(b64, "Music/generated", ext)
			if path != "" { fmt.Println(path) }
			return
		}
	}
	printJSON(data)
}

// sttDefaultModelFor picks a sensible default STT model from the file
// extension. OpenAI whisper-1 rejects Opus (the WhatsApp voice-note
// codec), so `.opus`/`.ogg` route to xAI's grok-stt which accepts them;
// everything else defaults to whisper-1. An explicit --model always wins.
func sttDefaultModelFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".opus", ".ogg", ".oga":
		return "grok-stt"
	}
	return "whisper-1"
}

func conductTranscribe(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: qai conduct transcribe <audio> [--model xai|gemini|whisper] [--language en]")
		os.Exit(1)
	}

	path := args[0]
	audioData, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai conduct transcribe: cannot read audio file %s: %v\n", path, err)
		fmt.Fprintln(os.Stderr, "  → fix: pass a path to a real audio file (mp3, wav, m4a, opus) as the first argument")
		os.Exit(1)
	}

	model := sttDefaultModelFor(path)
	body := map[string]any{
		"audio_base64": base64.StdEncoding.EncodeToString(audioData),
		"filename":     filepath.Base(path), // extension drives provider MIME detection
	}
	var keyterms []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--language", "--lang":
			if i+1 < len(args) { body["language"] = args[i+1]; i++ }
		case "--model", "-m":
			if i+1 < len(args) { model = resolveAudioModel(args[i+1], sttModelAliases); i++ }
		case "--keyterm":
			// Repeatable bias term (proper nouns, product names). xAI only.
			if i+1 < len(args) { keyterms = append(keyterms, args[i+1]); i++ }
		case "--diarize":
			// Speaker labels per word (xAI). Bool flag, no value.
			body["diarize"] = true
		}
	}
	// The gateway takes keyterm as a single newline-separated string.
	if len(keyterms) > 0 { body["keyterm"] = strings.Join(keyterms, "\n") }
	body["model"] = model

	fmt.Fprintf(os.Stderr, "sending request to %v...\n", model)
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
		fmt.Fprintf(os.Stderr, "qai conduct clone-voice: cannot read voice sample %s: %v\n", args[1], err)
		fmt.Fprintln(os.Stderr, "  → fix: pass a real audio file (mp3/wav, ~30s of clean speech) as the second argument")
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
