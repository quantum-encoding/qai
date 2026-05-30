package media

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/conduct"
)

// mimeFromExt is the small extension → MIME-type table for the file
// types the broker's /qai/v1/files allowlist accepts. We deliberately
// don't probe the file's magic bytes — it's user-supplied content with
// a stable extension, and the broker will reject mismatches anyway.
var mimeFromExt = map[string]string{
	// video
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".qt":   "video/quicktime",
	// audio
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
	// images
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".heic": "image/heic",
	".heif": "image/heif",
	// docs
	".pdf": "application/pdf",
}

// guessMime returns the inferred MIME from the file extension, or ""
// when unrecognised. Callers that get "" should error out with the
// list of accepted types — guessing is bad here because the broker
// rejects on MIME mismatch.
func guessMime(path string) string {
	return mimeFromExt[strings.ToLower(filepath.Ext(path))]
}

// uploadResponse mirrors the broker's filesUploadResponse JSON.
type uploadResponse struct {
	FileURI         string `json:"file_uri"`
	Name            string `json:"name"`
	MimeType        string `json:"mime_type"`
	SizeBytes       int64  `json:"size_bytes"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
}

// uploadFile streams the file at path to POST /qai/v1/files and returns
// the broker's response. Auto-compresses video over the threshold
// unless noCompress is true. The mimeOverride parameter wins over the
// extension-derived MIME — useful when a file has a non-standard
// extension but you know it's mp4 underneath.
//
// Returns the response + a cleanup function the caller must defer.
// Cleanup trashes the compressed sidecar (if any); the original is
// left alone.
func uploadFile(path, mimeOverride string, noCompress bool) (*uploadResponse, func(), error) {
	if _, err := os.Stat(path); err != nil {
		return nil, func() {}, fmt.Errorf("file not found: %s", path)
	}
	mime := mimeOverride
	if mime == "" {
		mime = guessMime(path)
	}
	if mime == "" {
		return nil, func() {}, fmt.Errorf(
			"cannot infer MIME for %s — pass --mime <type> (allowed: video/mp4, video/webm, video/quicktime, audio/mpeg, audio/wav, audio/ogg, audio/flac, image/{png,jpeg,webp,heic,heif}, application/pdf)",
			filepath.Ext(path),
		)
	}

	uploadPath := path
	cleanup := func() {}
	if !noCompress && shouldCompress(path, mime) {
		compressed, err := compressVideo(path, mime)
		if err != nil {
			return nil, func() {}, fmt.Errorf("compress: %w", err)
		}
		uploadPath = compressed
		// Compressed output is always mp4 — keep that mime even if
		// the source was webm/quicktime, because ffmpeg's pipeline
		// settles on H.264+AAC inside an mp4 container.
		mime = "video/mp4"
		cleanup = func() {
			if err := os.Remove(uploadPath); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "qai media: leftover compressed file at %s (cleanup failed: %v)\n", uploadPath, err)
			}
		}
	}

	content, err := os.ReadFile(uploadPath)
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("read %s: %w", uploadPath, err)
	}

	resp, err := conduct.APIMultipart("/qai/v1/files", "file", filepath.Base(uploadPath), mime, content)
	if err != nil {
		cleanup()
		conduct.DieAPI(err)
		return nil, func() {}, err // unreachable
	}

	var out uploadResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("parse upload response: %w", err)
	}
	if out.FileURI == "" {
		cleanup()
		return nil, func() {}, fmt.Errorf("broker returned empty file_uri")
	}
	return &out, cleanup, nil
}

// Avoid "imported and not used" by referencing http here — APIMultipart
// uses it internally but the package import surface needs it visible
// for go vet / lints in case the helper grows direct fetches later.
var _ = http.MethodPost
