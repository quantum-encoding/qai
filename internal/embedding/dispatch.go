// Package embedding provides text embedding via multiple providers.
package embedding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/config"
	"github.com/quantum-encoding/qai-cli/internal/strutil"
)

// Dispatch routes embedding requests based on config.
func Dispatch(cfg *config.Config, texts []string) ([][]float64, error) {
	switch cfg.Embeddings.Provider {
	case "ollama":
		return Ollama(cfg.Embeddings.Endpoint, cfg.Embeddings.Model, texts)
	case "gemini":
		return Gemini(cfg.EmbedAPIKey(), cfg.Embeddings.Model, cfg.Embeddings.Dimensions, texts)
	case "openai":
		return OpenAI(cfg.EmbedAPIKey(), cfg.Embeddings.Model, cfg.Embeddings.Dimensions, texts)
	case "qai":
		return QAI(cfg.API.APIKey, cfg.API.BaseURL, cfg.Embeddings.Model, cfg.Embeddings.Dimensions, texts)
	default:
		return nil, fmt.Errorf("unknown embedding provider: %s (run qai init)", cfg.Embeddings.Provider)
	}
}

func Ollama(endpoint, model string, texts []string) ([][]float64, error) {
	body := map[string]any{"model": model, "input": texts}
	data, _ := json.Marshal(body)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Post(
		endpoint+"/api/embed", "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("ollama: %w (is Ollama running?)", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, strutil.TruncateStr(string(respBody), 200))
	}
	var result struct{ Embeddings [][]float64 `json:"embeddings"` }
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("ollama parse: %w", err)
	}
	return result.Embeddings, nil
}

func Gemini(apiKey, model string, dimensions int, texts []string) ([][]float64, error) {
	requests := make([]map[string]any, len(texts))
	for i, t := range texts {
		requests[i] = map[string]any{
			"model":   "models/" + model,
			"content": map[string]any{"parts": []map[string]string{{"text": t}}},
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
		return nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, strutil.TruncateStr(string(respBody), 200))
	}
	var result struct {
		Embeddings []struct{ Values []float64 `json:"values"` } `json:"embeddings"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("gemini parse: %w", err)
	}
	vecs := make([][]float64, len(result.Embeddings))
	for i, e := range result.Embeddings { vecs[i] = e.Values }
	return vecs, nil
}

func OpenAI(apiKey, model string, dimensions int, texts []string) ([][]float64, error) {
	body := map[string]any{"model": model, "input": texts, "dimensions": dimensions}
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
		return nil, fmt.Errorf("openai %d: %s", resp.StatusCode, strutil.TruncateStr(string(respBody), 200))
	}
	var result struct {
		Data []struct{ Embedding []float64 `json:"embedding"` } `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("openai parse: %w", err)
	}
	vecs := make([][]float64, len(result.Data))
	for i, d := range result.Data { vecs[i] = d.Embedding }
	return vecs, nil
}

func QAI(apiKey, baseURL, model string, dimensions int, texts []string) ([][]float64, error) {
	body := map[string]any{"model": model, "input": texts}
	if dimensions > 0 { body["dimensions"] = dimensions }
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", baseURL+"/qai/v1/embeddings", bytes.NewReader(data))
	if err != nil { return nil, err }
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("qai embed %d: %s", resp.StatusCode, strutil.TruncateStr(string(respBody), 200))
	}
	var result struct{ Embeddings [][]float64 `json:"embeddings"` }
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse embeddings: %w", err)
	}
	return result.Embeddings, nil
}
