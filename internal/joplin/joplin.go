// Package joplin provides a minimal HTTP client for the local Joplin
// Web Clipper Service (https://joplinapp.org/api/references/rest_api/).
//
// Only the operations the qai CLI actually calls live here — list/find/create
// folders, list/create/update notes, ping. No abstraction layer, no ORM: the
// REST API is small enough that direct fetch-and-unmarshal is cleaner than a
// typed request builder.
//
// Config + auth resolution is intentionally kept separate (see joplin.Config
// built by `project` / future `note` subcommands). This package never reads
// files or env vars itself so it stays trivially testable.
package joplin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config bundles the runtime inputs needed to talk to Joplin.
type Config struct {
	// BaseURL is Joplin's web-clipper HTTP endpoint, e.g. "http://127.0.0.1:41184".
	BaseURL string
	// Token is the Web Clipper API token shown in
	// Joplin → Tools → Options → Web Clipper.
	Token string
	// Timeout caps every request; default is 10 seconds.
	Timeout time.Duration
}

// Client is the Joplin REST client.
type Client struct {
	cfg Config
	hc  *http.Client
}

// New builds a Client. The default timeout is applied when cfg.Timeout is zero.
func New(cfg Config) *Client {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Client{
		cfg: cfg,
		hc:  &http.Client{Timeout: cfg.Timeout},
	}
}

// ── Types ───────────────────────────────────────────────────────────────────

// Folder is a Joplin notebook — we expose only the fields the CLI uses.
type Folder struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ParentID string `json:"parent_id,omitempty"`
}

// Note is a single note record.
type Note struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Body            string `json:"body,omitempty"`
	ParentID        string `json:"parent_id,omitempty"`
	SourceURL       string `json:"source_url,omitempty"`
	UserCreatedTime int64  `json:"user_created_time,omitempty"`
	UserUpdatedTime int64  `json:"user_updated_time,omitempty"`
}

// folderList is the paginated response shape for `/folders`, `/folders/{}/notes`.
type folderList struct {
	Items   []Folder `json:"items"`
	HasMore bool     `json:"has_more"`
}

type noteList struct {
	Items   []Note `json:"items"`
	HasMore bool   `json:"has_more"`
}

// ── Basic probes ────────────────────────────────────────────────────────────

// Ping checks that Joplin's web clipper service is reachable. It returns a
// descriptive error when the endpoint isn't the expected JoplinClipperServer,
// which usually means the port is taken by something else.
func (c *Client) Ping() error {
	resp, err := c.hc.Get(c.cfg.BaseURL + "/ping")
	if err != nil {
		return fmt.Errorf("joplin ping: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("joplin ping: HTTP %d: %s", resp.StatusCode, truncate(string(body), 120))
	}
	if !strings.Contains(string(body), "JoplinClipperServer") {
		return fmt.Errorf("joplin ping: unexpected response %q (is something else on this port?)", truncate(string(body), 80))
	}
	return nil
}

// ── Folders ─────────────────────────────────────────────────────────────────

// ListFolders paginates over `/folders` and returns every notebook.
//
// Joplin caps `limit` at 100 and rejects multi-field `fields=` lists, so we
// accept the default schema and rely on the JSON parser to drop fields we
// don't model.
func (c *Client) ListFolders() ([]Folder, error) {
	var all []Folder
	page := 1
	for {
		u, err := c.urlWithToken("/folders", map[string]string{
			"limit": "100",
			"page":  fmt.Sprintf("%d", page),
		})
		if err != nil {
			return nil, err
		}
		var resp folderList
		if err := c.getJSON(u, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Items...)
		if !resp.HasMore {
			break
		}
		page++
		if page > 50 {
			return all, fmt.Errorf("joplin: folder pagination runaway (>5000 folders)")
		}
	}
	return all, nil
}

// FindFolderByTitle does a trimmed, case-sensitive match against every notebook
// title. Whitespace is stripped on both sides so notebooks whose title carries
// a stray `\n` from older hook versions still match cleanly.
func (c *Client) FindFolderByTitle(title string) (*Folder, error) {
	folders, err := c.ListFolders()
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(title)
	for i := range folders {
		if strings.TrimSpace(folders[i].Title) == want {
			return &folders[i], nil
		}
	}
	return nil, nil
}

// CreateFolder POSTs `/folders` with the given title (and optional parent id).
func (c *Client) CreateFolder(title string, parentID string) (*Folder, error) {
	body := map[string]any{"title": title}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	u, err := c.urlWithToken("/folders", nil)
	if err != nil {
		return nil, err
	}
	var out Folder
	if err := c.postJSON(u, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FindOrCreateFolder is the idempotent helper most callers actually want.
// If a match exists with dirty whitespace, it renames it in place so the
// notebook self-heals.
func (c *Client) FindOrCreateFolder(title string) (*Folder, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("joplin: folder title required")
	}
	found, err := c.FindFolderByTitle(title)
	if err != nil {
		return nil, err
	}
	if found != nil {
		if strings.TrimSpace(found.Title) != found.Title {
			// Best-effort rename to strip whitespace; non-fatal.
			_ = c.UpdateFolderTitle(found.ID, title)
			found.Title = title
		}
		return found, nil
	}
	return c.CreateFolder(title, "")
}

// FindOrCreateFolderPath walks a `/`-delimited folder path, creating any
// missing levels, and returns the leaf folder.
//
// Examples:
//
//	"qai"                       → folder titled "qai" at root
//	"qai/scans"                 → "qai" at root, "scans" under "qai"
//	"qai/scans/dag-cli"         → three-level chain
//	"  qai / scans / dag-cli "  → same as above (segments are trimmed)
//
// Empty segments (leading/trailing/double slashes) are skipped, so
// `/qai/scans/` and `qai/scans` resolve identically. An entirely empty
// path is rejected.
//
// The lookup uses a single ListFolders call: callers paying for many
// path lookups in a row should cache the client.
func (c *Client) FindOrCreateFolderPath(path string) (*Folder, error) {
	segments := SplitFolderPath(path)
	if len(segments) == 0 {
		return nil, fmt.Errorf("joplin: folder path is empty")
	}

	all, err := c.ListFolders()
	if err != nil {
		return nil, err
	}
	// Index folders by (parent, trimmed-title) for fast walk.
	type key struct{ parent, title string }
	index := make(map[key]*Folder, len(all))
	for i := range all {
		k := key{parent: all[i].ParentID, title: strings.TrimSpace(all[i].Title)}
		index[k] = &all[i]
	}

	parentID := ""
	var leaf *Folder
	for _, segment := range segments {
		if found, ok := index[key{parent: parentID, title: segment}]; ok {
			leaf = found
			parentID = found.ID
			continue
		}
		created, err := c.CreateFolder(segment, parentID)
		if err != nil {
			return nil, fmt.Errorf("joplin: create %q under %q: %w", segment, parentID, err)
		}
		// Track the new folder for any later segments under the same parent.
		index[key{parent: parentID, title: segment}] = created
		leaf = created
		parentID = created.ID
	}
	return leaf, nil
}

// SplitFolderPath splits a `/`-delimited folder path into trimmed,
// non-empty segments. Exposed because callers occasionally want to
// validate a path without performing the lookup.
func SplitFolderPath(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// UpdateFolderTitle PUTs a new title onto an existing folder.
func (c *Client) UpdateFolderTitle(id, title string) error {
	u, err := c.urlWithToken("/folders/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.putJSON(u, map[string]any{"title": title}, nil)
}

// DeleteFolder removes a folder (and cascade-deletes its notes).
func (c *Client) DeleteFolder(id string) error {
	u, err := c.urlWithToken("/folders/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("joplin delete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("joplin delete: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return nil
}

// ── Notes ───────────────────────────────────────────────────────────────────

// ListNotes paginates notes in the given folder. Pass "" to list every note.
func (c *Client) ListNotes(folderID string) ([]Note, error) {
	var all []Note
	page := 1
	for {
		path := "/notes"
		if folderID != "" {
			path = "/folders/" + url.PathEscape(folderID) + "/notes"
		}
		u, err := c.urlWithToken(path, map[string]string{
			"limit": "100",
			"page":  fmt.Sprintf("%d", page),
			// Joplin's default listing fields omit user_{created,updated}_time
			// which we need for sorting/display. Comma-joined fields are
			// accepted on /notes endpoints (unlike /folders).
			"fields": "id,parent_id,title,user_created_time,user_updated_time",
		})
		if err != nil {
			return nil, err
		}
		var resp noteList
		if err := c.getJSON(u, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Items...)
		if !resp.HasMore {
			break
		}
		page++
		if page > 50 {
			return all, fmt.Errorf("joplin: note pagination runaway")
		}
	}
	return all, nil
}

// CreateNote writes a new note.
func (c *Client) CreateNote(n Note) (*Note, error) {
	u, err := c.urlWithToken("/notes", nil)
	if err != nil {
		return nil, err
	}
	var out Note
	if err := c.postJSON(u, n, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateNoteBody patches just the body of an existing note — for append flows.
func (c *Client) UpdateNoteBody(id, body string) error {
	u, err := c.urlWithToken("/notes/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return c.putJSON(u, map[string]any{"body": body}, nil)
}

// GetNote fetches a single note with a subset of fields (default = everything).
func (c *Client) GetNote(id string, fields ...string) (*Note, error) {
	params := map[string]string{}
	if len(fields) > 0 {
		// Joplin rejects comma-joined field lists on /folders, but /notes/{id}
		// accepts them fine. Send as a single joined param.
		params["fields"] = strings.Join(fields, ",")
	}
	u, err := c.urlWithToken("/notes/"+url.PathEscape(id), params)
	if err != nil {
		return nil, err
	}
	var n Note
	if err := c.getJSON(u, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// ── HTTP helpers ────────────────────────────────────────────────────────────

func (c *Client) urlWithToken(path string, params map[string]string) (string, error) {
	if c.cfg.Token == "" {
		return "", fmt.Errorf("joplin: no token configured")
	}
	u, err := url.Parse(c.cfg.BaseURL + path)
	if err != nil {
		return "", fmt.Errorf("joplin: bad base URL %q: %w", c.cfg.BaseURL, err)
	}
	q := u.Query()
	q.Set("token", c.cfg.Token)
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) getJSON(u string, out any) error {
	resp, err := c.hc.Get(u)
	if err != nil {
		return fmt.Errorf("joplin GET: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("joplin GET: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) postJSON(u string, body, out any) error {
	return c.writeJSON(http.MethodPost, u, body, out)
}

func (c *Client) putJSON(u string, body, out any) error {
	return c.writeJSON(http.MethodPut, u, body, out)
}

func (c *Client) writeJSON(method, u string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("joplin marshal: %w", err)
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("joplin %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		text, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("joplin %s: HTTP %d: %s", method, resp.StatusCode, truncate(string(text), 200))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
