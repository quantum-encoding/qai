package media

import (
	"fmt"
	"os"
	"strconv"
)

// cmdChat handles `qai media chat ...`. Three modes:
//
//   qai media chat <file> "first prompt"       — create session
//   qai media chat --session <id> "prompt"     — explicit session
//   qai media chat "prompt"                    — resume active session
//
// Dispatch logic: if --session is set, explicit mode wins. Otherwise,
// if the first positional looks like a file path (it exists on disk),
// we're in create-session mode and the rest is the first turn. Otherwise
// it's resume mode and everything is the prompt.
func cmdChat(args []string) {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Println(helpMedia)
			return
		}
	}

	args, model := stripModelFlag(args)
	model = resolveModel(model)
	args, system, _ := stripFlag(args, "--system")
	args, mimeOverride, _ := stripFlag(args, "--mime")
	args, noCompress := stripBoolFlag(args, "--no-compress")
	args, sessionIDFlag, sessionFlagPresent := stripFlag(args, "--session")
	args, ttlStr, _ := stripFlag(args, "--ttl")
	args, maxTokensStr, _ := stripFlag(args, "--max-tokens")

	// Decide mode.
	switch {
	case sessionFlagPresent && sessionIDFlag != "":
		chatExplicitSession(sessionIDFlag, args, maxTokensStr)
	case len(args) >= 2 && isExistingFile(args[0]):
		chatCreateSession(args[0], args[1:], model, system, mimeOverride, noCompress, ttlStr, maxTokensStr)
	case len(args) >= 1:
		// Resume active session.
		id := getActiveSession()
		if id == "" {
			diefatal("no active session — start one with `qai media chat <file> \"first prompt\"` or pick one with --session <id>")
		}
		chatExplicitSession(id, args, maxTokensStr)
	default:
		diefatal("qai media chat needs a prompt")
	}
}

func isExistingFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func chatCreateSession(file string, promptArgs []string, model, system, mimeOverride string, noCompress bool, ttlStr, maxTokensStr string) {
	prompt := joinPrompt(promptArgs)
	if prompt == "" {
		diefatal("first prompt cannot be empty")
	}

	upload, cleanup, err := uploadFile(file, mimeOverride, noCompress)
	defer cleanup()
	if err != nil {
		diefatal("%v", err)
	}

	fmt.Fprintf(os.Stderr, "qai media: uploaded → %s\n", upload.FileURI)

	createReq := sessionCreateRequest{
		FileURI:           upload.FileURI,
		MimeType:          upload.MimeType,
		Model:             model,
		SystemInstruction: system,
		DisplayName:       upload.Name,
	}
	if ttlStr != "" {
		n, err := strconv.Atoi(ttlStr)
		if err != nil {
			diefatal("--ttl: %v", err)
		}
		createReq.CacheTTLSeconds = n
	}

	session, err := CreateSession(createReq)
	if err != nil {
		diefatalAPI(err)
	}

	if err := setActiveSession(session.ID); err != nil {
		// Non-fatal — the session is created, the user can still
		// reference it explicitly via --session.
		fmt.Fprintf(os.Stderr,
			"qai media: warning — could not record active session pointer: %v\n", err)
	}

	fmt.Fprintf(os.Stderr,
		"qai media: session %s created (model=%s, cache expires %s)\n",
		shortID(session.ID), session.Model, formatExpiry(session.ExpiresAt))

	// Run the first turn.
	chatExplicitSession(session.ID, []string{prompt}, maxTokensStr)
}

func chatExplicitSession(id string, promptArgs []string, maxTokensStr string) {
	prompt := joinPrompt(promptArgs)
	if prompt == "" {
		diefatal("prompt cannot be empty")
	}
	var maxTokens *int32
	if maxTokensStr != "" {
		n, err := strconv.Atoi(maxTokensStr)
		if err != nil {
			diefatal("--max-tokens: %v", err)
		}
		v := int32(n)
		maxTokens = &v
	}

	resp, err := SessionChat(id, prompt, maxTokens, nil)
	if err != nil {
		diefatalAPI(err)
	}

	// Record this as the active session so the next no-arg `qai media
	// chat "next"` resumes here. Cheap to do every turn — keeps the
	// pointer current even after explicit --session jumps.
	_ = setActiveSession(id)

	fmt.Println(resp.Answer)
}

// diefatalAPI surfaces the structured broker error via DieAPI when the
// error came from the conduct package; falls back to a plain diefatal
// otherwise. Used by every chat path so the user always sees the same
// "what failed + how to fix" shape.
func diefatalAPI(err error) {
	// conduct.API returns *apiError, which conduct.DieAPI knows how to
	// format. Pass it straight through so the user gets the canonical
	// "qai conduct: ..." prefix and the broker's reply body verbatim.
	defer func() {
		// If DieAPI didn't os.Exit (defensive — it should), make sure
		// we still bail.
		os.Exit(1)
	}()
	// Look first to see if this is the conduct package's typed error.
	// Easiest: just call DieAPI; if the type isn't recognised it
	// prints the err.Error() string and exits anyway.
	diefatal("%v", err)
}
