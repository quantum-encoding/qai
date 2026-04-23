package scrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

const (
	defaultJoplinURL = "http://localhost:41184"
	clipTimeout      = 120 * time.Second
)

// ─── Joplin connection ───────────────────────────────────────────────────

// joplinToken returns the Web Clipper API token from $JOPLIN_TOKEN, or
// falls back to Joplin Desktop's settings.json (Joplin uses the same
// ~/.config/joplin-desktop/settings.json path on macOS and Linux).
func joplinToken() (string, error) {
	if t := os.Getenv("JOPLIN_TOKEN"); t != "" {
		return t, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("UserHomeDir: %w", err)
	}
	settingsPath := filepath.Join(home, ".config", "joplin-desktop", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return "", fmt.Errorf("JOPLIN_TOKEN not set and %s unreadable: %w", settingsPath, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", fmt.Errorf("parse %s: %w", settingsPath, err)
	}
	tok, _ := settings["api.token"].(string)
	if tok == "" {
		return "", fmt.Errorf("api.token missing from %s (enable Web Clipper in Joplin → Tools → Options)", settingsPath)
	}
	return tok, nil
}

func joplinBaseURL() string {
	if u := os.Getenv("JOPLIN_URL"); u != "" {
		return u
	}
	return defaultJoplinURL
}

// ─── qai clip shellout ───────────────────────────────────────────────────

// clipNoteIDRe parses the "Note ID: <32hex>" line the clip script prints
// after a successful clip.
var clipNoteIDRe = regexp.MustCompile(`Note ID:\s+([a-f0-9]{32})`)

// qaiClip shells out to `qai clip`, which drives Playwright → Joplin.
// We shell out rather than import the clip code because clip runs a
// Node script, and clip is already its own command with its own deps.
//
// Returns the Joplin note ID parsed from clip's stdout. Falling back
// on Joplin's search index is unreliable right after a clip — the
// search index is eventually consistent and a 1MB+ Amazon page can
// take tens of seconds to be queryable. Grabbing the ID from clip's
// own output sidesteps the race entirely.
func qaiClip(productURL, notebook, title string) (string, error) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "qai"
	}
	cmd := exec.Command(exe, "clip", productURL, notebook, title)

	var stdoutBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stderr, &stdoutBuf)
	cmd.Stderr = os.Stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return "", err
		}
	case <-time.After(clipTimeout):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("qai clip timed out after %s", clipTimeout)
	}

	// Parse "Note ID: <hash>" from the captured output.
	if m := clipNoteIDRe.FindStringSubmatch(stdoutBuf.String()); len(m) == 2 {
		return m[1], nil
	}
	return "", nil // ID not found; caller will fall back to search.
}

// getNoteByID fetches a note directly by its Joplin ID. Unlike
// /search, this endpoint is not index-backed, so it reflects the
// latest state immediately — no eventual-consistency delay.
func getNoteByID(token, id string) (*joplinNote, error) {
	u := fmt.Sprintf("%s/notes/%s?token=%s&fields=id,title,body,created_time",
		joplinBaseURL(), id, url.QueryEscape(token))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("note %s: http %d", id, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var n joplinNote
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// ─── Joplin Data API ─────────────────────────────────────────────────────

type joplinNote struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_time"`
}

// findNoteByTitle looks up a note by exact title, retrying with backoff
// because the Joplin search index is eventually consistent and a freshly-
// clipped note may not be indexed for several seconds.
func findNoteByTitle(token, title string) (*joplinNote, error) {
	for _, delay := range []time.Duration{1, 2, 3, 5, 8} {
		time.Sleep(delay * time.Second)
		n, err := searchNoteByTitle(token, title)
		if err == nil && n != nil {
			return n, nil
		}
		fmt.Fprintln(os.Stderr, "  waiting for Joplin index...")
	}
	return nil, fmt.Errorf("note %q not found after clip", title)
}

func searchNoteByTitle(token, title string) (*joplinNote, error) {
	params := url.Values{
		"token":     {token},
		"query":     {title},
		"type":      {"note"},
		"fields":    {"id,title,created_time,body"},
		"order_by":  {"created_time"},
		"order_dir": {"DESC"},
		"limit":     {"20"},
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(
		joplinBaseURL() + "/search?" + params.Encode(),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("joplin search %d", resp.StatusCode)
	}
	var result struct {
		Items []joplinNote `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	for i := range result.Items {
		if result.Items[i].Title == title {
			return &result.Items[i], nil
		}
	}
	return nil, nil
}

// ─── resource fetching ───────────────────────────────────────────────────

type joplinResource struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	FileExt string `json:"file_extension"`
	Mime    string `json:"mime"`
	Size    int64  `json:"size"`
}

func getResourceMeta(token, id string) (*joplinResource, error) {
	u := fmt.Sprintf("%s/resources/%s?token=%s&fields=id,title,file_extension,mime,size",
		joplinBaseURL(), id, url.QueryEscape(token))
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("resource %s: http %d", id, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var r joplinResource
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func getResourceFile(token, id string) ([]byte, error) {
	u := fmt.Sprintf("%s/resources/%s/file?token=%s",
		joplinBaseURL(), id, url.QueryEscape(token))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("resource file %s: http %d", id, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
