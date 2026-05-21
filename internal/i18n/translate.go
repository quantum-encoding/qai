package i18n

// translate.go — single-shot LLM translation.
//
// Builds one prompt asking for all target locales in a single JSON
// response, sends it to the QAI chat endpoint, parses the JSON out
// of the response, and returns {locale: translation}.
//
// One HTTP round trip per translated key — the savings over a
// per-locale loop are real (10× fewer requests, no rate-limit risk,
// the model can also keep locale consistency in one context).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LocaleName returns a human-readable language name for the common
// locale codes used in Kitchen Share. The LLM understands codes alone,
// but pairing "es (Spanish)" gives better results on edge cases (e.g.
// "zh" vs Chinese variants).
func LocaleName(code string) string {
	switch code {
	case "en":
		return "English"
	case "es":
		return "Spanish"
	case "fr":
		return "French"
	case "de":
		return "German"
	case "it":
		return "Italian"
	case "pt":
		return "Portuguese"
	case "nl":
		return "Dutch"
	case "ja":
		return "Japanese"
	case "ko":
		return "Korean"
	case "zh":
		return "Chinese (Simplified)"
	case "ar":
		return "Arabic"
	default:
		return code
	}
}

type TranslateRequest struct {
	English   string   // the English source string
	KeyPath   string   // dot-path; included for prompt context only
	AppName   string   // optional app name for tone context (e.g. "Kitchen Share")
	Targets   []string // locale codes (e.g. ["es", "fr", "de"])
	Model     string   // chat model id; default claude-sonnet-4-6
}

type TranslateResult struct {
	Translations map[string]string `json:"translations"` // locale → translation
	Model        string            `json:"model"`
	Notes        string            `json:"notes,omitempty"` // model-emitted caveats, if any
}

// Translate calls the QAI /qai/v1/chat endpoint with a translation prompt.
//
// Requires QAI_API_KEY in the environment. Returns an error if any
// target locale is missing from the model's response — partial results
// are returned alongside the error so the caller can apply what it got.
func Translate(req TranslateRequest) (*TranslateResult, error) {
	if strings.TrimSpace(req.English) == "" {
		return nil, fmt.Errorf("translate: english is empty")
	}
	if len(req.Targets) == 0 {
		return nil, fmt.Errorf("translate: no target locales")
	}

	apiKey := os.Getenv("QAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("translate: QAI_API_KEY not set")
	}
	baseURL := os.Getenv("QAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.quantumencoding.ai"
	}
	model := req.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	// Build the prompt. We're explicit about:
	//   • placeholder preservation ({name}, {count}, etc.)
	//   • brand names staying English
	//   • output format being ONLY JSON (no markdown fences, no commentary)
	// The brittle part of LLM-as-API is response shape; the prompt fights
	// for it hard and the parser then also strips common deviations.
	var targets strings.Builder
	for i, code := range req.Targets {
		if i > 0 {
			targets.WriteString(", ")
		}
		fmt.Fprintf(&targets, "%s (%s)", code, LocaleName(code))
	}

	app := req.AppName
	if app == "" {
		app = "the app"
	}

	prompt := fmt.Sprintf(`You are a localization translator for %s. Translate the following English UI string into every requested target locale.

Rules:
- Preserve placeholders like {name}, {count}, {0} VERBATIM. Do not translate placeholder names.
- Keep brand names (e.g. "Kitchen Share", "Apple", "Google", "WhatsApp") in English even in non-English locales.
- Match the register: UI strings should be concise and natural, not literal.
- For right-to-left languages (Arabic), translate normally; the UI handles RTL layout.

English source string: %q
Key path (for context): %q

Target locales: %s

Respond with ONLY a JSON object. No markdown fences, no prose. Format:
{"es": "translated string", "fr": "translated string", ...}
Include exactly one entry per requested locale code.`,
		app, req.English, req.KeyPath, targets.String())

	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	data, _ := json.Marshal(body)

	httpReq, _ := http.NewRequest(http.MethodPost, baseURL+"/qai/v1/chat", bytes.NewReader(data))
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("X-API-Key", apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("chat: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chat: %d %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// /qai/v1/chat returns Anthropic-style:
	//   { "content": [ {"type": "text", "text": "..."} ], "model": ..., "usage": ... }
	var apiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("chat: decode response: %w (body: %s)", err, truncStr(string(respBody), 200))
	}
	var rawText string
	for _, b := range apiResp.Content {
		if b.Type == "text" {
			rawText += b.Text
		}
	}
	if rawText == "" {
		return nil, fmt.Errorf("chat: empty text in response (body: %s)", truncStr(string(respBody), 200))
	}

	// Strip common LLM artefacts: markdown fences, "json" language tag,
	// leading/trailing prose. Find the first { and last } and trust the
	// model returned valid JSON between them.
	text := stripJSONFences(rawText)
	first := strings.Index(text, "{")
	last := strings.LastIndex(text, "}")
	if first < 0 || last < 0 || last < first {
		return nil, fmt.Errorf("chat: no JSON object in response: %s", truncStr(text, 200))
	}
	jsonBlob := text[first : last+1]

	var translations map[string]string
	if err := json.Unmarshal([]byte(jsonBlob), &translations); err != nil {
		return nil, fmt.Errorf("chat: parse JSON: %w (got: %s)", err, truncStr(jsonBlob, 200))
	}

	// Validate we got every requested locale; collect what's missing for
	// the caller's awareness but still return the partial result.
	result := &TranslateResult{
		Translations: translations,
		Model:        model,
	}
	var missing []string
	for _, code := range req.Targets {
		if _, ok := translations[code]; !ok {
			missing = append(missing, code)
		}
	}
	if len(missing) > 0 {
		return result, fmt.Errorf("chat: missing locales in response: %s", strings.Join(missing, ", "))
	}
	return result, nil
}

func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
