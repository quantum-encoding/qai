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
	args, explicitSystem, _ := stripFlag(args, "--system")
	args, templateName, _ := stripFlag(args, "--template")
	args, templateNameShort, _ := stripFlag(args, "-t")
	if templateName == "" {
		templateName = templateNameShort
	}
	args, mimeOverride, _ := stripFlag(args, "--mime")
	args, noCompress := stripBoolFlag(args, "--no-compress")
	args, maxTokensStr, _ := stripFlag(args, "--max-tokens")
	// Document-vision flags. --transcribe / --translate select a built-in
	// faithful handwriting→Markdown prompt (see prompts.go) so the user
	// doesn't pass a prompt string. --to sets the translation target.
	args, transcribe := stripBoolFlag(args, "--transcribe")
	args, translate := stripBoolFlag(args, "--translate")
	args, targetLang, _ := stripFlag(args, "--to")
	// Output-to-file (parity with `qai media chat -o`).
	args, outPath, _ := stripFlag(args, "--output")
	args, outShort, _ := stripFlag(args, "-o")
	if outPath == "" {
		outPath = outShort
	}
	args, appendOut := stripBoolFlag(args, "--append")

	docMode := transcribe || translate
	// --to implies translation even if --translate was omitted.
	if targetLang != "" {
		translate = true
		docMode = true
	}

	// Default model: document-vision modes want the strong 3.5 Flash;
	// everything else keeps the cheap flash-lite default.
	if model == "" && docMode {
		model = "gemini-3.5-flash"
	}
	model = resolveModel(model)

	// One-shot supports system instructions via prepending a system
	// message — it's a single turn so we don't need the cache for it.
	// In doc mode the built-in transcription system prompt applies unless
	// the user passed an explicit --system/--template override.
	system := resolveSystemInstruction(explicitSystem, templateName)
	if docMode && explicitSystem == "" && templateName == "" {
		system = transcribeSystem
	}

	var filePath, prompt string
	if docMode {
		// Doc mode: only <file> is required — the prompt is built-in.
		if len(args) < 1 {
			diefatal("--transcribe/--translate needs <file>. Example: qai media --transcribe ~/page.heic")
		}
		filePath = args[0]
		// Any trailing positional text becomes an extra instruction
		// appended to the built-in prompt (e.g. "ignore the margin notes").
		extra := joinPrompt(args[1:])
		prompt = buildDocPrompt(transcribe, translate, targetLang)
		if extra != "" {
			prompt += "\n\nAdditional instruction: " + extra
		}
	} else {
		if len(args) < 2 {
			diefatal("one-shot needs <file> <prompt>; got %d args. Example: qai media ~/lecture.mp4 \"summarise\"", len(args))
		}
		filePath = args[0]
		prompt = joinPrompt(args[1:])
		if prompt == "" {
			diefatal("prompt cannot be empty")
		}
	}

	upload, cleanup, err := uploadFile(filePath, mimeOverride, noCompress)
	defer cleanup()
	if err != nil {
		diefatal("%v", err)
	}

	fmt.Fprintf(os.Stderr, "qai media: uploaded → %s (%s, %ds)\n",
		upload.FileURI, upload.MimeType, upload.DurationSeconds)

	// Build a chat request with the file_uri as a content block.
	// One-shot only — no caching, no session. System prompt (if any)
	// goes in as the first message; no cached_content to conflict with.
	messages := []map[string]any{}
	if system != "" {
		messages = append(messages, map[string]any{
			"role": "system", "content": system,
		})
	}
	messages = append(messages, map[string]any{
		"role": "user",
		"content_blocks": []map[string]any{
			{"type": "file_uri", "file_uri": upload.FileURI, "mime_type": upload.MimeType},
			{"type": "text", "text": prompt},
		},
	})
	body := map[string]any{
		"model":    model,
		"messages": messages,
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
	out := sb.String()
	if outPath != "" {
		writeOutput(outPath, out, appendOut)
		return
	}
	fmt.Println(out)
}
