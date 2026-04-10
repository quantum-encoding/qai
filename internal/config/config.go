// Package config provides central configuration for qai-cli.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// HomeDir returns the user's home directory.
func HomeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "."
}

// Home is the resolved home directory, set once at startup.
var Home = HomeDir()

// ─── types ────────────────────────────────────────────────────────────────

type Config struct {
	Embeddings EmbeddingsConfig `yaml:"embeddings"`
	API        APIConfig        `yaml:"api"`
	Surreal    SurrealConfig    `yaml:"surreal"`
	Search     SearchConfig     `yaml:"search"`
	Vertex     VertexConfig     `yaml:"vertex"`
}

type EmbeddingsConfig struct {
	Provider   string `yaml:"provider"`
	APIKey     string `yaml:"api_key,omitempty"`
	Model      string `yaml:"model"`
	Dimensions int    `yaml:"dimensions"`
	Endpoint   string `yaml:"endpoint,omitempty"`
}

type APIConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key,omitempty"`
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
	Default string `yaml:"default"`
}

type VertexConfig struct {
	Project string `yaml:"project,omitempty"`
	Region  string `yaml:"region"`
	Bucket  string `yaml:"bucket,omitempty"`
}

// SurrealConn holds connection details for a SurrealDB instance.
type SurrealConn struct {
	URL  string
	NS   string
	DB   string
	Auth string // base64 encoded
}

// ─── paths ────────────────────────────────────────────────────────────────

func ConfigDir() string {
	if v := os.Getenv("QAI_CONFIG_HOME"); v != "" {
		return v
	}
	return filepath.Join(Home, ".qai")
}

func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.yaml")
}

// ─── defaults ─────────────────────────────────────────────────────────────

func Default() *Config {
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
		Search: SearchConfig{Default: "local"},
		Vertex: VertexConfig{Region: "europe-west4"},
	}
}

// ─── load / save ──────────────────────────────────────────────────────────

func Load() *Config {
	c := Default()
	data, err := os.ReadFile(ConfigPath())
	if err == nil {
		if err := yaml.Unmarshal(data, c); err != nil {
			fmt.Fprintf(os.Stderr, "qai: warning: bad config %s: %v\n", ConfigPath(), err)
		}
	}
	overlayEnvVars(c)
	return c
}

func Save(c *Config) error {
	os.MkdirAll(ConfigDir(), 0755)
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	header := "# qai configuration — edit or regenerate with: qai init\n#\n" +
		"# Docs: https://github.com/quantum-encoding/qai\n\n"
	return os.WriteFile(ConfigPath(), []byte(header+string(data)), 0600)
}

func overlayEnvVars(c *Config) {
	if v := os.Getenv("QAI_API_KEY"); v != "" { c.API.APIKey = v }
	if v := os.Getenv("QAI_BASE_URL"); v != "" { c.API.BaseURL = v }
	if v := os.Getenv("QAI_EMBED_PROVIDER"); v != "" { c.Embeddings.Provider = v }
	if v := os.Getenv("QAI_EMBED_API_KEY"); v != "" { c.Embeddings.APIKey = v }
	if v := os.Getenv("QAI_EMBED_MODEL"); v != "" { c.Embeddings.Model = v }
	if v := os.Getenv("QAI_EMBED_DIMENSIONS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil { c.Embeddings.Dimensions = d }
	}
	if v := os.Getenv("QAI_EMBED_ENDPOINT"); v != "" { c.Embeddings.Endpoint = v }
	if v := os.Getenv("SURREAL_CLOUD_URL"); v != "" { c.Surreal.CloudURL = v }
	if v := os.Getenv("SURREAL_CLOUD_NS"); v != "" { c.Surreal.CloudNS = v }
	if v := os.Getenv("SURREAL_CLOUD_DB"); v != "" { c.Surreal.CloudDB = v }
	if v := os.Getenv("SURREAL_CLOUD_USER"); v != "" { c.Surreal.CloudUser = v }
	if v := os.Getenv("SURREAL_CLOUD_PASS"); v != "" { c.Surreal.CloudPass = v }
	if v := os.Getenv("SURREAL_LOCAL_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil { c.Surreal.LocalPort = p }
	}
	if v := os.Getenv("GCP_PROJECT"); v != "" { c.Vertex.Project = v }
	if v := os.Getenv("GCP_REGION"); v != "" { c.Vertex.Region = v }
	if v := os.Getenv("GCS_BUCKET"); v != "" { c.Vertex.Bucket = v }
}

// ─── helpers ──────────────────────────────────────────────────────────────

func (c *Config) LocalSurrealURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", c.Surreal.LocalPort)
}

func (c *Config) LocalSurrealAuth() string {
	return base64.StdEncoding.EncodeToString([]byte(c.Surreal.LocalUser + ":" + c.Surreal.LocalPass))
}

func (c *Config) CloudSurrealAuth() string {
	if c.Surreal.CloudUser == "" { return "" }
	return base64.StdEncoding.EncodeToString([]byte(c.Surreal.CloudUser + ":" + c.Surreal.CloudPass))
}

func (c *Config) LocalConn() SurrealConn {
	return SurrealConn{URL: c.LocalSurrealURL(), NS: "qai", DB: "knowledge", Auth: c.LocalSurrealAuth()}
}

func (c *Config) CloudConn() SurrealConn {
	return SurrealConn{URL: c.Surreal.CloudURL, NS: c.Surreal.CloudNS, DB: c.Surreal.CloudDB, Auth: c.CloudSurrealAuth()}
}

func (c *Config) EmbedAPIKey() string {
	if c.Embeddings.APIKey != "" { return c.Embeddings.APIKey }
	return c.API.APIKey
}

func (c *Config) GetDBPort() int    { return c.Surreal.LocalPort }
func (c *Config) GetDBUser() string { return c.Surreal.LocalUser }
func (c *Config) GetDBPass() string { return c.Surreal.LocalPass }
