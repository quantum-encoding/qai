// config.go — Central configuration for qai-cli.
//
// Loading chain: defaults → ~/.qai/config.yaml → environment variables.
// Later values win. Missing config file is fine (first-run uses defaults).
//
// Default config is fully local: Ollama embeddings + local SurrealDB.
// Zero API keys needed out of the box.

package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ─── types ────────────────────────────────────────────────────────────────

type Config struct {
	Embeddings EmbeddingsConfig `yaml:"embeddings"`
	API        APIConfig        `yaml:"api"`
	Surreal    SurrealConfig    `yaml:"surreal"`
	Search     SearchConfig     `yaml:"search"`
	Vertex     VertexConfig     `yaml:"vertex"`
}

type EmbeddingsConfig struct {
	Provider   string `yaml:"provider"`            // ollama, gemini, openai, qai
	APIKey     string `yaml:"api_key,omitempty"`   // gemini/openai key
	Model      string `yaml:"model"`               // e.g. nomic-embed-text, gemini-embedding-2-preview
	Dimensions int    `yaml:"dimensions"`           // vector dimensions (768)
	Endpoint   string `yaml:"endpoint,omitempty"`  // ollama: http://localhost:11434
}

type APIConfig struct {
	BaseURL string `yaml:"base_url"`           // https://api.quantumencoding.ai
	APIKey  string `yaml:"api_key,omitempty"`  // qai_k_...
}

type SurrealConfig struct {
	CloudURL  string `yaml:"cloud_url,omitempty"`
	CloudNS   string `yaml:"cloud_ns,omitempty"`
	CloudDB   string `yaml:"cloud_db,omitempty"`
	CloudUser string `yaml:"cloud_user,omitempty"`
	CloudPass string `yaml:"cloud_pass,omitempty"`
	LocalPort int    `yaml:"local_port"`
	LocalUser string `yaml:"local_user"`
	LocalPass string `yaml:"local_pass"`
}

type SearchConfig struct {
	Default string `yaml:"default"` // local, surreal, joplin, rag, all
}

type VertexConfig struct {
	Project string `yaml:"project,omitempty"`
	Region  string `yaml:"region"`
	Bucket  string `yaml:"bucket,omitempty"`
}

// ─── SurrealDB connection helper ──────────────────────────────────────────

type surrealConn struct {
	url  string
	ns   string
	db   string
	auth string // base64 encoded
}

// ─── package-level config ─────────────────────────────────────────────────

var cfg *Config

// ─── paths ────────────────────────────────────────────────────────────────

func qaiConfigDir() string {
	if v := os.Getenv("QAI_CONFIG_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".qai")
}

func qaiConfigPath() string {
	return filepath.Join(qaiConfigDir(), "config.yaml")
}

// ─── defaults ─────────────────────────────────────────────────────────────

func defaultConfig() *Config {
	return &Config{
		Embeddings: EmbeddingsConfig{
			Provider:   "ollama",
			Model:      "nomic-embed-text",
			Dimensions: 768,
			Endpoint:   "http://localhost:11434",
		},
		API: APIConfig{
			BaseURL: "https://api.quantumencoding.ai",
		},
		Surreal: SurrealConfig{
			LocalPort: 9473,
			LocalUser: "root",
			LocalPass: "root",
		},
		Search: SearchConfig{
			Default: "local",
		},
		Vertex: VertexConfig{
			Region: "europe-west4",
		},
	}
}

// ─── load / save ──────────────────────────────────────────────────────────

func loadConfig() *Config {
	c := defaultConfig()

	// Read config file (non-fatal if missing)
	data, err := os.ReadFile(qaiConfigPath())
	if err == nil {
		if err := yaml.Unmarshal(data, c); err != nil {
			fmt.Fprintf(os.Stderr, "qai: warning: bad config %s: %v\n", qaiConfigPath(), err)
		}
	}

	// Env vars override config file
	overlayEnvVars(c)

	return c
}

func saveConfig(c *Config) error {
	dir := qaiConfigDir()
	os.MkdirAll(dir, 0755)

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	header := "# qai configuration — edit or regenerate with: qai init\n#\n" +
		"# Docs: https://github.com/quantum-encoding/qai-cli\n\n"

	return os.WriteFile(qaiConfigPath(), []byte(header+string(data)), 0600)
}

func overlayEnvVars(c *Config) {
	if v := os.Getenv("QAI_API_KEY"); v != "" {
		c.API.APIKey = v
	}
	if v := os.Getenv("QAI_BASE_URL"); v != "" {
		c.API.BaseURL = v
	}
	if v := os.Getenv("QAI_EMBED_PROVIDER"); v != "" {
		c.Embeddings.Provider = v
	}
	if v := os.Getenv("QAI_EMBED_API_KEY"); v != "" {
		c.Embeddings.APIKey = v
	}
	if v := os.Getenv("QAI_EMBED_MODEL"); v != "" {
		c.Embeddings.Model = v
	}
	if v := os.Getenv("QAI_EMBED_DIMENSIONS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			c.Embeddings.Dimensions = d
		}
	}
	if v := os.Getenv("QAI_EMBED_ENDPOINT"); v != "" {
		c.Embeddings.Endpoint = v
	}
	if v := os.Getenv("SURREAL_CLOUD_URL"); v != "" {
		c.Surreal.CloudURL = v
	}
	if v := os.Getenv("SURREAL_CLOUD_NS"); v != "" {
		c.Surreal.CloudNS = v
	}
	if v := os.Getenv("SURREAL_CLOUD_DB"); v != "" {
		c.Surreal.CloudDB = v
	}
	if v := os.Getenv("SURREAL_CLOUD_USER"); v != "" {
		c.Surreal.CloudUser = v
	}
	if v := os.Getenv("SURREAL_CLOUD_PASS"); v != "" {
		c.Surreal.CloudPass = v
	}
	if v := os.Getenv("SURREAL_LOCAL_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Surreal.LocalPort = p
		}
	}
	if v := os.Getenv("GCP_PROJECT"); v != "" {
		c.Vertex.Project = v
	}
	if v := os.Getenv("GCP_REGION"); v != "" {
		c.Vertex.Region = v
	}
	if v := os.Getenv("GCS_BUCKET"); v != "" {
		c.Vertex.Bucket = v
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────

func (c *Config) LocalSurrealURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", c.Surreal.LocalPort)
}

func (c *Config) LocalSurrealAuth() string {
	return base64.StdEncoding.EncodeToString(
		[]byte(c.Surreal.LocalUser + ":" + c.Surreal.LocalPass),
	)
}

func (c *Config) CloudSurrealAuth() string {
	if c.Surreal.CloudUser == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString(
		[]byte(c.Surreal.CloudUser + ":" + c.Surreal.CloudPass),
	)
}

func (c *Config) LocalConn() surrealConn {
	return surrealConn{
		url:  c.LocalSurrealURL(),
		ns:   "qai",
		db:   "knowledge",
		auth: c.LocalSurrealAuth(),
	}
}

func (c *Config) CloudConn() surrealConn {
	return surrealConn{
		url:  c.Surreal.CloudURL,
		ns:   c.Surreal.CloudNS,
		db:   c.Surreal.CloudDB,
		auth: c.CloudSurrealAuth(),
	}
}

// EmbedAPIKey returns the appropriate API key for the current embedding provider.
func (c *Config) EmbedAPIKey() string {
	if c.Embeddings.APIKey != "" {
		return c.Embeddings.APIKey
	}
	return c.API.APIKey // fallback to QAI key for "qai" provider
}
