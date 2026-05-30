package media

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/conduct"
)

// SessionRecord mirrors the broker's mediaSessionRecord JSON shape.
// JSON tags only — Firestore tags live server-side. We deliberately
// keep this small subset rather than mirroring every field, because
// fields the client doesn't read shouldn't enforce a coupling.
type SessionRecord struct {
	ID                string         `json:"id"`
	FileURI           string         `json:"file_uri"`
	MimeType          string         `json:"mime_type"`
	FileDisplayName   string         `json:"file_display_name,omitempty"`
	CacheName         string         `json:"cache_name"`
	Model             string         `json:"model"`
	SystemInstruction string         `json:"system_instruction,omitempty"`
	History           []SessionTurn  `json:"history"`
	CreatedAt         string         `json:"created_at"`
	LastUsedAt        string         `json:"last_used_at"`
	ExpiresAt         string         `json:"expires_at"`
	MessageCount      int            `json:"message_count"`
}

type SessionTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	At      string `json:"at"`
}

// sessionChatRequest is the body of POST /qai/v1/media-sessions/{id}/chat.
type sessionChatRequest struct {
	Message     string   `json:"message"`
	MaxTokens   *int32   `json:"max_tokens,omitempty"`
	Temperature *float32 `json:"temperature,omitempty"`
}

// sessionChatResponse is the JSON returned by the chat endpoint.
type sessionChatResponse struct {
	SessionID string         `json:"session_id"`
	Answer    string         `json:"answer"`
	History   []SessionTurn  `json:"history"`
	Usage     map[string]any `json:"usage,omitempty"`
}

// sessionCreateRequest mirrors the broker's mediaSessionCreateRequest.
type sessionCreateRequest struct {
	FileURI           string `json:"file_uri"`
	MimeType          string `json:"mime_type"`
	Model             string `json:"model"`
	SystemInstruction string `json:"system_instruction,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	CacheTTLSeconds   int    `json:"cache_ttl_seconds,omitempty"`
}

// CreateSession POSTs to /qai/v1/media-sessions, returning the new
// session. The broker creates the underlying Vertex cache as part of
// the same request.
func CreateSession(req sessionCreateRequest) (*SessionRecord, error) {
	resp, err := conduct.API("POST", "/qai/v1/media-sessions", req)
	if err != nil {
		return nil, err
	}
	var s SessionRecord
	if err := json.Unmarshal(resp, &s); err != nil {
		return nil, fmt.Errorf("parse session-create response: %w", err)
	}
	return &s, nil
}

// SessionChat POSTs to /qai/v1/media-sessions/{id}/chat with the next
// user turn. Returns the assistant's answer + the updated history.
func SessionChat(id, message string, maxTokens *int32, temperature *float32) (*sessionChatResponse, error) {
	path := fmt.Sprintf("/qai/v1/media-sessions/%s/chat", id)
	resp, err := conduct.API("POST", path, sessionChatRequest{
		Message: message, MaxTokens: maxTokens, Temperature: temperature,
	})
	if err != nil {
		return nil, err
	}
	var out sessionChatResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("parse session-chat response: %w", err)
	}
	return &out, nil
}

// ListSessions GETs /qai/v1/media-sessions.
func ListSessions() ([]SessionRecord, error) {
	resp, err := conduct.API("GET", "/qai/v1/media-sessions", nil)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Sessions []SessionRecord `json:"sessions"`
	}
	if err := json.Unmarshal(resp, &wrap); err != nil {
		return nil, fmt.Errorf("parse list response: %w", err)
	}
	return wrap.Sessions, nil
}

// DeleteSession DELETEs /qai/v1/media-sessions/{id}.
func DeleteSession(id string) error {
	_, err := conduct.API("DELETE", "/qai/v1/media-sessions/"+id, nil)
	return err
}

// ─── active-session pointer ──────────────────────────────────────────────
//
// Single piece of client-side state: the most recently used session id.
// Stored at ~/.qai/media-sessions/active so a no-arg `qai media chat
// "next"` knows what to resume. The session itself lives on the broker
// (Firestore) — this file is just a pointer to it.

const activePointerPath = "media-sessions/active"

func activeSessionFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".qai", activePointerPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return path, nil
}

func setActiveSession(id string) error {
	path, err := activeSessionFile()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(id+"\n"), 0o600)
}

func getActiveSession() string {
	path, err := activeSessionFile()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func clearActiveSession() {
	path, err := activeSessionFile()
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

// shortID is the display helper for session ids in list output — UUIDs
// are 36 chars and only the first 8 are useful for visual scanning.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// formatExpiry renders an ISO-8601 string as a human-friendly
// "in 47m" / "expired 12m ago" so the user can see at a glance which
// sessions still have a valid cache. Bad / empty input returns "?".
func formatExpiry(s string) string {
	if s == "" {
		return "?"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "?"
	}
	d := time.Until(t)
	if d < 0 {
		return fmt.Sprintf("expired %s ago", roundDur(-d))
	}
	return fmt.Sprintf("in %s", roundDur(d))
}

func roundDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
