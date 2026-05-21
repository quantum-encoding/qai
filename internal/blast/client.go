package blast

// Tiny SurrealDB HTTP client.
//
// Security policy (enforced):
//
//   • URL / NS / DB can have safe defaults — they're not credentials.
//   • USER and PASS have NO compiled-in defaults. The CLI fails fast if
//     they're not set via flags or QAI_SURREAL_USER / QAI_SURREAL_PASS.
//
// This prevents the "developer points the CLI at a hosted prod cluster
// without flags, ships root:root over the wire" failure mode.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Options struct {
	URL  string
	User string
	Pass string
	NS   string
	DB   string
}

// DefaultOptions resolves env vars only for non-credential fields.
// USER and PASS stay empty unless QAI_SURREAL_USER / _PASS are set,
// forcing Validate() to surface a clear error if neither flags nor env
// supplied them.
func DefaultOptions() Options {
	return Options{
		URL:  envOr("QAI_SURREAL_URL", "http://127.0.0.1:8000"),
		User: os.Getenv("QAI_SURREAL_USER"),
		Pass: os.Getenv("QAI_SURREAL_PASS"),
		NS:   envOr("QAI_SURREAL_NS", "quantumencoding"),
		DB:   envOr("QAI_SURREAL_DB", "blast_radius"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ErrCredentialsMissing is returned by Validate when no auth is configured.
var ErrCredentialsMissing = errors.New(
	"SurrealDB credentials required.\n" +
		"  Provide either:\n" +
		"    QAI_SURREAL_USER=... QAI_SURREAL_PASS=...   (env)\n" +
		"  or:\n" +
		"    --user <name> --pass <secret>                (flags)\n" +
		"There are no compiled-in defaults so this CLI cannot silently leak\n" +
		"dev credentials to a misconfigured production target.",
)

// Validate enforces the security policy. Call before any network op.
func (o Options) Validate() error {
	if strings.TrimSpace(o.User) == "" || strings.TrimSpace(o.Pass) == "" {
		return ErrCredentialsMissing
	}
	if strings.TrimSpace(o.URL) == "" {
		return errors.New("SurrealDB URL is empty (set --url or QAI_SURREAL_URL)")
	}
	if strings.TrimSpace(o.NS) == "" || strings.TrimSpace(o.DB) == "" {
		return errors.New("namespace and database are required (--ns / --db)")
	}
	return nil
}

// StatementResult mirrors one entry from SurrealDB /sql's response.
type StatementResult struct {
	Status string          `json:"status"`
	Time   string          `json:"time,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Detail string          `json:"detail,omitempty"`
}

type Client struct {
	opts Options
	http *http.Client
}

func NewClient(opts Options) *Client {
	return &Client{
		opts: opts,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Opts() Options { return c.opts }

// Health pings /version. Returns "" + err on failure. Does NOT require
// auth — /version is unauthenticated — so this is the right pre-flight
// to detect "server up?" vs. "credentials wrong?".
func (c *Client) Health() (string, error) {
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(c.opts.URL, "/")+"/version", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), nil
}

// Exec runs one or more SurrealQL statements separated by `;`.
func (c *Client) Exec(surql string) ([]StatementResult, error) {
	endpoint := strings.TrimRight(c.opts.URL, "/") + "/sql"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBufferString(surql))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("surreal-ns", c.opts.NS)
	req.Header.Set("surreal-db", c.opts.DB)
	auth := base64.StdEncoding.EncodeToString([]byte(c.opts.User + ":" + c.opts.Pass))
	req.Header.Set("Authorization", "Basic "+auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("surreal: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("surreal %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []StatementResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("surreal: decode response: %w (body: %s)", err, string(body))
	}
	return out, nil
}

// ExecMany batches statements across HTTP round-trips.
func (c *Client) ExecMany(statements []string, batchSize int) ([]StatementResult, error) {
	if batchSize <= 0 {
		batchSize = 200
	}
	var all []StatementResult
	for i := 0; i < len(statements); i += batchSize {
		end := i + batchSize
		if end > len(statements) {
			end = len(statements)
		}
		chunk := strings.Join(statements[i:end], ";\n") + ";"
		res, err := c.Exec(chunk)
		if err != nil {
			return all, fmt.Errorf("batch %d-%d: %w", i, end, err)
		}
		all = append(all, res...)
	}
	return all, nil
}

// FirstError returns the first non-OK StatementResult as a Go error.
func FirstError(results []StatementResult) error {
	for i, r := range results {
		if r.Status != "OK" {
			detail := r.Detail
			if detail == "" {
				detail = string(r.Result)
			}
			return fmt.Errorf("statement %d: %s: %s", i+1, r.Status, detail)
		}
	}
	return nil
}
