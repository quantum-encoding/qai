package media

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/quantum-encoding/qai-cli/internal/audit"
)

// defaultTemplate is the system-instruction profile media sessions
// pick up automatically when the user doesn't pass --system or
// --template. It tells the model to be verbose-faithful (not
// summarising) and to chunk when the content won't fit in one reply.
// Edit ~/.qai/profiles/media-narrate.yaml to customise without
// rebuilding the binary; the embedded copy in
// internal/audit/profiles/media-narrate.yaml is the fallback.
const defaultTemplate = "media-narrate"

// defaultMaxTurns caps --auto chunk pumping. 8 is the user's stated
// safety floor — beyond this the user should opt-in with --max-turns N
// rather than have a runaway eat their token budget.
const defaultMaxTurns = 8

// chatOpts is the parsed flag set for all qai media chat paths. Holds
// every per-invocation knob so chatCreateSession + chatExplicitSession
// stop accumulating positional parameters as new features land.
type chatOpts struct {
	model        string
	system       string
	mimeOverride string
	noCompress   bool
	ttl          int    // cache TTL seconds; 0 = broker default
	maxTokens    *int32 // per-turn cap
	output       string // -o path; "" = stdout only
	appendOut    bool   // -a; append instead of overwrite
	auto         bool   // --auto = plan + auto-walk
	maxTurns     int    // --max-turns; default defaultMaxTurns
}

// resolveSystemInstruction picks the system prompt to bake into a new
// session's cache. Priority:
//
//	--system "..."           explicit prompt wins
//	--template <name>        named audit-profile system field
//	(neither given)          falls back to defaultTemplate, then ""
func resolveSystemInstruction(explicit, templateName string) string {
	if explicit != "" {
		return explicit
	}
	name := templateName
	if name == "" {
		name = defaultTemplate
	}
	sys, _, ok := audit.LookupProfile(name)
	if !ok {
		if templateName != "" {
			diefatal("unknown template %q — see `qai audit -- profile names` for available profiles, or drop a YAML in ~/.qai/profiles/", templateName)
		}
		return ""
	}
	return sys
}

// cmdChat handles `qai media chat ...`. Three modes:
//
//   qai media chat <file> "first prompt"        — create session
//   qai media chat --session <id> "prompt"      — explicit session
//   qai media chat "prompt"                     — resume active session
//
// Plus orthogonal flags: --auto, -o/--output, --max-turns.
func cmdChat(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Println(helpMedia)
			return
		}
	}

	opts := chatOpts{maxTurns: defaultMaxTurns}

	args, opts.model = stripModelFlag(args)
	opts.model = resolveModel(opts.model)

	var explicitSystem, templateName, templateShort string
	args, explicitSystem, _ = stripFlag(args, "--system")
	args, templateName, _ = stripFlag(args, "--template")
	args, templateShort, _ = stripFlag(args, "-t")
	if templateName == "" {
		templateName = templateShort
	}
	opts.system = resolveSystemInstruction(explicitSystem, templateName)

	args, opts.mimeOverride, _ = stripFlag(args, "--mime")
	args, opts.noCompress = stripBoolFlag(args, "--no-compress")

	var sessionIDFlag string
	var sessionFlagPresent bool
	args, sessionIDFlag, sessionFlagPresent = stripFlag(args, "--session")

	var ttlStr, maxTokensStr, maxTurnsStr, outShort string
	args, ttlStr, _ = stripFlag(args, "--ttl")
	args, maxTokensStr, _ = stripFlag(args, "--max-tokens")
	args, maxTurnsStr, _ = stripFlag(args, "--max-turns")
	args, opts.output, _ = stripFlag(args, "--output")
	args, outShort, _ = stripFlag(args, "-o")
	if opts.output == "" {
		opts.output = outShort
	}
	args, opts.appendOut = stripBoolFlag(args, "--append")
	if !opts.appendOut {
		args, opts.appendOut = stripBoolFlag(args, "-a")
	}
	args, opts.auto = stripBoolFlag(args, "--auto")

	if ttlStr != "" {
		n, err := strconv.Atoi(ttlStr)
		if err != nil {
			diefatal("--ttl: %v", err)
		}
		opts.ttl = n
	}
	if maxTokensStr != "" {
		n, err := strconv.Atoi(maxTokensStr)
		if err != nil {
			diefatal("--max-tokens: %v", err)
		}
		v := int32(n)
		opts.maxTokens = &v
	}
	if maxTurnsStr != "" {
		n, err := strconv.Atoi(maxTurnsStr)
		if err != nil || n < 1 {
			diefatal("--max-turns: must be a positive integer")
		}
		opts.maxTurns = n
	}

	// Decide mode based on what positionals look like.
	switch {
	case sessionFlagPresent && sessionIDFlag != "":
		chatExplicitSession(sessionIDFlag, joinPrompt(args), opts)
	case len(args) >= 2 && isExistingFile(args[0]):
		chatCreateSession(args[0], joinPrompt(args[1:]), opts)
	case len(args) >= 1:
		id := getActiveSession()
		if id == "" {
			diefatal("no active session — start one with `qai media chat <file> \"first prompt\"` or pick one with --session <id>")
		}
		chatExplicitSession(id, joinPrompt(args), opts)
	default:
		diefatal("qai media chat needs a prompt")
	}
}

func isExistingFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// chatCreateSession uploads the file, creates a cached session, and
// runs the first turn (or full auto-walk when opts.auto is set).
func chatCreateSession(file, prompt string, opts chatOpts) {
	if prompt == "" {
		diefatal("first prompt cannot be empty")
	}

	upload, cleanup, err := uploadFile(file, opts.mimeOverride, opts.noCompress)
	defer cleanup()
	if err != nil {
		diefatal("%v", err)
	}

	createReq := sessionCreateRequest{
		FileURI:           upload.FileURI,
		MimeType:          upload.MimeType,
		Model:             opts.model,
		SystemInstruction: opts.system,
		DisplayName:       upload.Name,
		CacheTTLSeconds:   opts.ttl,
	}

	session, err := CreateSession(createReq)
	if err != nil {
		diefatalAPI(err)
	}

	if err := setActiveSession(session.ID); err != nil {
		fmt.Fprintf(os.Stderr,
			"qai media: warning — could not record active session pointer: %v\n", err)
	}

	fmt.Fprintf(os.Stderr,
		"qai media: session %s created (model=%s, cache expires %s)\n",
		shortID(session.ID), session.Model, formatExpiry(session.ExpiresAt))

	// For the markdown header, capture the source filename so the user
	// knows what they're reading without scrolling back to invocation.
	sourceLabel := filepath.Base(file)

	if opts.auto {
		runAutoWalk(session.ID, sourceLabel, prompt, opts)
		return
	}
	runSingleTurn(session.ID, sourceLabel, prompt, opts)
}

// chatExplicitSession is the resume / --session path — no upload, no
// session create, just a turn against an existing cached session.
func chatExplicitSession(id, prompt string, opts chatOpts) {
	if prompt == "" {
		diefatal("prompt cannot be empty")
	}
	_ = setActiveSession(id) // refresh the active pointer
	if opts.auto {
		// For resume + --auto we don't have a clean source label,
		// fall back to the session id.
		runAutoWalk(id, "session "+shortID(id), prompt, opts)
		return
	}
	runSingleTurn(id, "session "+shortID(id), prompt, opts)
}

// runSingleTurn runs ONE turn against the session, prints the answer
// to stdout, and optionally writes a small markdown file when -o is set.
func runSingleTurn(sessionID, sourceLabel, prompt string, opts chatOpts) {
	resp, err := SessionChat(sessionID, prompt, opts.maxTokens, nil)
	if err != nil {
		diefatalAPI(err)
	}
	fmt.Println(resp.Answer)

	if opts.output != "" {
		md := buildSingleTurnMarkdown(sourceLabel, sessionID, prompt, resp.Answer)
		writeOutput(opts.output, md, opts.appendOut)
	}
}

// runAutoWalk runs the structured-output plan turn + N continuation
// turns. Streams each chunk to stdout as it arrives so the user sees
// progress; writes the full assembled document to opts.output when set.
func runAutoWalk(sessionID, sourceLabel, firstPrompt string, opts chatOpts) {
	// Turn 1: force the plan schema. Model returns
	//   { total_chunks_estimated, outline, content }
	// where content is the prose for chunk 1.
	planResp, err := SessionChatWithSchema(sessionID, firstPrompt, opts.maxTokens, autoPlanSchema)
	if err != nil {
		diefatalAPI(err)
	}

	var plan struct {
		TotalChunks int      `json:"total_chunks_estimated"`
		Outline     []string `json:"outline"`
		Content     string   `json:"content"`
	}
	if err := json.Unmarshal([]byte(planResp.Answer), &plan); err != nil {
		// Schema enforcement should make this near-impossible, but
		// degrade gracefully: print whatever came back and stop.
		fmt.Fprintf(os.Stderr,
			"qai media: --auto plan parse failed (%v) — printing raw output and stopping\n", err)
		fmt.Println(planResp.Answer)
		return
	}

	// Cap the walk to opts.maxTurns. If the model estimated more than
	// we'll execute, tell the user how to resume.
	executable := plan.TotalChunks
	capped := false
	if executable > opts.maxTurns {
		executable = opts.maxTurns
		capped = true
	}

	fmt.Fprintf(os.Stderr,
		"qai media: --auto plan — %d chunk(s) estimated, will run %d:\n",
		plan.TotalChunks, executable)
	for i, sec := range plan.Outline {
		marker := " "
		if i+1 > executable {
			marker = "·" // skipped due to cap
		}
		fmt.Fprintf(os.Stderr, "  %s %d. %s\n", marker, i+1, sec)
	}

	// Print chunk 1 (already in the plan response).
	printChunkHeader(1, plan.Outline)
	fmt.Println(plan.Content)
	chunks := []string{plan.Content}

	for i := 2; i <= executable; i++ {
		contPrompt := fmt.Sprintf(
			"Continue with chunk %d of %d as plain text. Pick up where chunk %d left off — do not summarise, do not restate the plan, do not re-greet the reader. Just deliver the next chunk's content.",
			i, plan.TotalChunks, i-1)
		resp, err := SessionChat(sessionID, contPrompt, opts.maxTokens, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"qai media: chunk %d failed (%v) — stopping early with %d/%d chunks\n",
				i, err, len(chunks), plan.TotalChunks)
			break
		}
		printChunkHeader(i, plan.Outline)
		fmt.Println(resp.Answer)
		chunks = append(chunks, resp.Answer)
	}

	if capped && len(chunks) == opts.maxTurns {
		fmt.Fprintf(os.Stderr,
			"qai media: stopped at chunk %d/%d due to --max-turns. Run `qai media chat \"continue with chunk %d\"` to resume.\n",
			opts.maxTurns, plan.TotalChunks, opts.maxTurns+1)
	}

	if opts.output != "" {
		md := buildAutoWalkMarkdown(sourceLabel, sessionID, opts.model, firstPrompt, plan.TotalChunks, plan.Outline, chunks)
		writeOutput(opts.output, md, opts.appendOut)
	}
}

// printChunkHeader writes a separator + title between chunks so the
// stdout stream stays scan-able. Uses stderr so a stdout-capture
// pipeline (`qai media chat ... > file.txt`) still gets clean content.
func printChunkHeader(i int, outline []string) {
	title := ""
	if i-1 < len(outline) {
		title = outline[i-1]
	}
	if title == "" {
		fmt.Fprintf(os.Stderr, "\n── chunk %d ──\n", i)
	} else {
		fmt.Fprintf(os.Stderr, "\n── chunk %d: %s ──\n", i, title)
	}
}

// autoPlanSchema is the JSON Schema enforced on the first turn of an
// --auto run. Gemini's response_schema forces the model's output to
// fill this shape — no model drift, no "I forgot to emit the plan."
var autoPlanSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"total_chunks_estimated": map[string]any{
			"type":        "integer",
			"description": "Number of replies required to fully narrate this video at the requested detail level. Use 1 if it fits in one reply.",
		},
		"outline": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Short section title for each estimated chunk, in order.",
		},
		"content": map[string]any{
			"type":        "string",
			"description": "The detailed narration of CHUNK 1 only (subsequent chunks come back in plain-text continuation turns).",
		},
	},
	"required":             []string{"total_chunks_estimated", "outline", "content"},
	"propertyOrdering":     []string{"total_chunks_estimated", "outline", "content"},
	"additionalProperties": false,
}

// ─── markdown builders ──────────────────────────────────────────────────

func buildSingleTurnMarkdown(source, sessionID, prompt, answer string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", source)
	fmt.Fprintf(&b, "> Session: `%s`  \n", sessionID)
	fmt.Fprintf(&b, "> Date: %s  \n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "> Prompt: %s\n\n", prompt)
	fmt.Fprintf(&b, "---\n\n")
	b.WriteString(answer)
	if !strings.HasSuffix(answer, "\n") {
		b.WriteByte('\n')
	}
	return b.String()
}

func buildAutoWalkMarkdown(source, sessionID, model, prompt string, planned int, outline, chunks []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", source)
	fmt.Fprintf(&b, "> Session: `%s`  \n", sessionID)
	fmt.Fprintf(&b, "> Model: `%s`  \n", model)
	fmt.Fprintf(&b, "> Date: %s  \n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "> Prompt: %s  \n", prompt)
	fmt.Fprintf(&b, "> Estimated chunks: %d  \n", planned)
	fmt.Fprintf(&b, "> Captured chunks: %d\n\n", len(chunks))

	if len(outline) > 0 {
		b.WriteString("## Outline\n\n")
		for i, sec := range outline {
			fmt.Fprintf(&b, "%d. %s\n", i+1, sec)
		}
		b.WriteByte('\n')
	}

	b.WriteString("---\n\n")

	for i, ch := range chunks {
		title := ""
		if i < len(outline) {
			title = outline[i]
		}
		if title != "" {
			fmt.Fprintf(&b, "## Part %d — %s\n\n", i+1, title)
		} else {
			fmt.Fprintf(&b, "## Part %d\n\n", i+1)
		}
		b.WriteString(ch)
		if !strings.HasSuffix(ch, "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	if len(chunks) < planned {
		fmt.Fprintf(&b, "---\n\n*Note: %d chunks captured of %d estimated. Resume with `qai media chat --session %s \"continue with chunk %d\"`.*\n",
			len(chunks), planned, sessionID, len(chunks)+1)
	}
	return b.String()
}

// writeOutput writes content to path. Append vs overwrite per the
// flag. Prints a one-line stderr note so the user knows the file is
// there.
func writeOutput(path, content string, appendMode bool) {
	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appendMode {
		flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qai media: write %s: %v\n", path, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		fmt.Fprintf(os.Stderr, "qai media: write %s: %v\n", path, err)
		return
	}
	verb := "wrote"
	if appendMode {
		verb = "appended"
	}
	fmt.Fprintf(os.Stderr, "qai media: %s %d bytes to %s\n", verb, len(content), path)
}

// diefatalAPI surfaces the structured broker error via diefatal. Used
// by every chat path so the user always sees the same "what failed +
// how to fix" shape.
func diefatalAPI(err error) {
	defer func() { os.Exit(1) }()
	diefatal("%v", err)
}
