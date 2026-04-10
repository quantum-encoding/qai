// init_cmd.go — Interactive setup wizard and embedding provider dispatch.
//
// qai init walks users through first-time configuration:
//   - Choose embedding provider (Ollama / Gemini / OpenAI / QAI managed)
//   - Validate API keys
//   - Start local SurrealDB
//   - Write ~/.qai/config.yaml

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ─── qai init ─────────────────────────────────────────────────────────────

func cmdInit(args []string) {
	fmt.Println(`
  ╔══════════════════════════════════════════╗
  ║           qai — first-time setup        ║
  ╚══════════════════════════════════════════╝`)

	c := defaultConfig()

	// Step 1: Embedding provider
	fmt.Println(`
  Choose your embedding provider:

    1. Ollama        — fully local, no API key, zero cost
                       requires: ollama + nomic-embed-text model
    2. Google Gemini — free tier available (1500 req/day)
                       requires: API key from aistudio.google.com
    3. OpenAI        — text-embedding-3-small
                       requires: API key from platform.openai.com
    4. QAI Managed   — everything through api.quantumencoding.ai
                       requires: QAI API key`)

	choice := promptChoice("\n  Your choice", 4)

	switch choice {
	case 1: // Ollama
		c.Embeddings.Provider = "ollama"
		c.Embeddings.Endpoint = promptString("  Ollama endpoint", "http://localhost:11434")
		c.Embeddings.Model = promptString("  Embedding model", "nomic-embed-text")
		c.Embeddings.Dimensions = 768

		// Validate
		fmt.Fprintf(os.Stderr, "  checking Ollama...")
		resp, err := http.Get(c.Embeddings.Endpoint + "/api/tags")
		if err != nil {
			fmt.Fprintf(os.Stderr, " not reachable\n")
			fmt.Println("\n  Ollama not detected. Install from https://ollama.ai then run:")
			fmt.Printf("    ollama pull %s\n", c.Embeddings.Model)
			fmt.Println("  You can still save this config and set up Ollama later.")
		} else {
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, " connected!\n")
		}

	case 2: // Gemini
		c.Embeddings.Provider = "gemini"
		c.Embeddings.Model = "gemini-embedding-2-preview"
		c.Embeddings.Dimensions = 768
		c.Embeddings.APIKey = promptString("  Gemini API key (from aistudio.google.com)", "")
		if c.Embeddings.APIKey != "" {
			fmt.Fprintf(os.Stderr, "  validating key...")
			_, err := embedGemini(c.Embeddings.APIKey, c.Embeddings.Model, c.Embeddings.Dimensions, []string{"test"})
			if err != nil {
				fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, " valid!\n")
			}
		}

	case 3: // OpenAI
		c.Embeddings.Provider = "openai"
		c.Embeddings.Model = "text-embedding-3-small"
		c.Embeddings.Dimensions = 768
		c.Embeddings.APIKey = promptString("  OpenAI API key (from platform.openai.com)", "")
		if c.Embeddings.APIKey != "" {
			fmt.Fprintf(os.Stderr, "  validating key...")
			_, err := embedOpenAI(c.Embeddings.APIKey, c.Embeddings.Model, c.Embeddings.Dimensions, []string{"test"})
			if err != nil {
				fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, " valid!\n")
			}
		}

	case 4: // QAI Managed
		c.Embeddings.Provider = "qai"
		c.Embeddings.Model = "gemini-embedding-2-preview"
		c.Embeddings.Dimensions = 768
		c.API.APIKey = promptString("  QAI API key (qai_k_...)", "")
		if c.API.APIKey != "" {
			fmt.Fprintf(os.Stderr, "  checking balance...")
			_, err := embedQAI(c.API.APIKey, c.API.BaseURL, c.Embeddings.Model, c.Embeddings.Dimensions, []string{"test"})
			if err != nil {
				fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, " valid!\n")
			}
		}
	}

	// Step 2: Local SurrealDB
	fmt.Println()
	if promptYN("  Start local SurrealDB for knowledge storage?", true) {
		if !dbIsRunning() {
			fmt.Println()
			dbStart()
		} else {
			fmt.Printf("  SurrealDB already running on port %d\n", c.Surreal.LocalPort)
		}
	}

	// Step 3: Save config
	fmt.Println()
	if err := saveConfig(c); err != nil {
		fmt.Fprintf(os.Stderr, "  save config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Configuration saved to %s\n", qaiConfigPath())

	// Reload global config
	cfg = c

	// Step 4: Try-it commands
	fmt.Println(`
  You're all set! Try:

    qai db status                             # check knowledge base
    qai ingest --local my-docs ~/Documents/   # embed + store documents
    qai search --local "how does X work"      # vector similarity search`)

	if c.API.APIKey != "" {
		fmt.Println(`    qai conduct models                        # list available AI models
    qai audit . --dry-run                     # preview code audit`)
	}
	fmt.Println()
}

// ─── embedding dispatch ───────────────────────────────────────────────────

// embedTextsDispatch routes embedding requests based on config.
func embedTextsDispatch(texts []string) ([][]float64, error) {
	switch cfg.Embeddings.Provider {
	case "ollama":
		return embedOllama(cfg.Embeddings.Endpoint, cfg.Embeddings.Model, texts)
	case "gemini":
		return embedGemini(cfg.EmbedAPIKey(), cfg.Embeddings.Model, cfg.Embeddings.Dimensions, texts)
	case "openai":
		return embedOpenAI(cfg.EmbedAPIKey(), cfg.Embeddings.Model, cfg.Embeddings.Dimensions, texts)
	case "qai":
		return embedQAI(cfg.API.APIKey, cfg.API.BaseURL, cfg.Embeddings.Model, cfg.Embeddings.Dimensions, texts)
	default:
		return nil, fmt.Errorf("unknown embedding provider: %s (run qai init)", cfg.Embeddings.Provider)
	}
}

// ─── Ollama embeddings ────────────────────────────────────────────────────

func embedOllama(endpoint, model string, texts []string) ([][]float64, error) {
	body := map[string]any{
		"model": model,
		"input": texts,
	}
	data, _ := json.Marshal(body)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Post(
		endpoint+"/api/embed", "application/json", bytes.NewReader(data),
	)
	if err != nil {
		return nil, fmt.Errorf("ollama: %w (is Ollama running?)", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("ollama parse: %w", err)
	}
	return result.Embeddings, nil
}

// ─── Gemini embeddings (direct, no QAI proxy) ────────────────────────────

func embedGemini(apiKey, model string, dimensions int, texts []string) ([][]float64, error) {
	// Build batch request per Gemini API spec
	requests := make([]map[string]any, len(texts))
	for i, t := range texts {
		requests[i] = map[string]any{
			"model": "models/" + model,
			"content": map[string]any{
				"parts": []map[string]string{{"text": t}},
			},
			"outputDimensionality": dimensions,
		}
	}
	body := map[string]any{"requests": requests}
	data, _ := json.Marshal(body)

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:batchEmbedContents?key=%s", model, apiKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result struct {
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("gemini parse: %w", err)
	}

	vecs := make([][]float64, len(result.Embeddings))
	for i, e := range result.Embeddings {
		vecs[i] = e.Values
	}
	return vecs, nil
}

// ─── OpenAI embeddings ───────────────────────────────────────────────────

func embedOpenAI(apiKey, model string, dimensions int, texts []string) ([][]float64, error) {
	body := map[string]any{
		"model":      model,
		"input":      texts,
		"dimensions": dimensions,
	}
	data, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/embeddings", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("openai parse: %w", err)
	}

	vecs := make([][]float64, len(result.Data))
	for i, d := range result.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// ─── QAI embeddings (through quantum-ai API) ─────────────────────────────

func embedQAI(apiKey, baseURL, model string, dimensions int, texts []string) ([][]float64, error) {
	body := map[string]any{
		"model": model,
		"input": texts,
	}
	if dimensions > 0 {
		body["dimensions"] = dimensions
	}
	data, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", baseURL+"/qai/v1/embeddings", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("qai embed %d: %s", resp.StatusCode, truncateStr(string(respBody), 200))
	}

	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse embeddings: %w", err)
	}
	return result.Embeddings, nil
}

// ─── input helpers ────────────────────────────────────────────────────────

var stdinReader = bufio.NewReader(os.Stdin)

func promptString(prompt, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := stdinReader.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal
	}
	return line
}

func promptChoice(prompt string, max int) int {
	for {
		fmt.Printf("%s [1-%d]: ", prompt, max)
		line, _ := stdinReader.ReadString('\n')
		line = strings.TrimSpace(line)
		n := 0
		if _, err := fmt.Sscanf(line, "%d", &n); err == nil && n >= 1 && n <= max {
			return n
		}
		fmt.Printf("  please enter 1-%d\n", max)
	}
}

func promptYN(prompt string, defaultYes bool) bool {
	hint := "[Y/n]"
	if !defaultYes {
		hint = "[y/N]"
	}
	fmt.Printf("%s %s: ", prompt, hint)
	line, _ := stdinReader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}
