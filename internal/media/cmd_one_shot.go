package media

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/quantum-encoding/qai-cli/internal/conduct"
)

// cmdOneShot handles `qai media <file> "prompt"`. Upload, chat once
// referencing the file_uri inline (no server-side session, no cache —
// a single turn doesn't benefit from caching), print the answer.
//
// For multi-turn or anything you'll come back to, use `qai media chat`
// which creates a server-side cache + session for ~10× cheaper
// follow-ups.
func cmdOneShot(args []string) {
	args, model := stripModelFlag(args)
	model = resolveModel(model)
	args, system, _ := stripFlag(args, "--system")
	args, mimeOverride, _ := stripFlag(args, "--mime")
	args, noCompress := stripBoolFlag(args, "--no-compress")
	args, maxTokensStr, _ := stripFlag(args, "--max-tokens")
	_ = system // one-shot doesn't ship the system instruction separately — bake into prompt or use chat mode

	if len(args) < 2 {
		diefatal("one-shot needs <file> <prompt>; got %d args. Example: qai media ~/lecture.mp4 \"summarise\"", len(args))
	}
	filePath := args[0]
	prompt := joinPrompt(args[1:])
	if prompt == "" {
		diefatal("prompt cannot be empty")
	}

	upload, cleanup, err := uploadFile(filePath, mimeOverride, noCompress)
	defer cleanup()
	if err != nil {
		diefatal("%v", err)
	}

	fmt.Fprintf(os.Stderr, "qai media: uploaded → %s (%s, %ds)\n",
		upload.FileURI, upload.MimeType, upload.DurationSeconds)

	// Build a chat request with the file_uri as a content block.
	// One-shot only — no caching, no session.
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{
				"role": "user",
				"content_blocks": []map[string]any{
					{"type": "file_uri", "file_uri": upload.FileURI, "mime_type": upload.MimeType},
					{"type": "text", "text": prompt},
				},
			},
		},
	}
	if maxTokensStr != "" {
		n, err := strconv.Atoi(maxTokensStr)
		if err != nil {
			diefatal("--max-tokens: %v", err)
		}
		body["max_tokens"] = n
	}

	respBytes, err := conduct.API("POST", "/qai/v1/chat", body)
	if err != nil {
		conduct.DieAPI(err)
	}

	// Parse and print the assistant text. The chat response wraps text
	// inside content blocks; flatten to a single string for one-shot
	// CLI output.
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		// Fall back to raw — better to show the user something than swallow.
		os.Stdout.Write(respBytes)
		fmt.Println()
		return
	}
	var sb strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		}
	}
	fmt.Println(sb.String())
}
