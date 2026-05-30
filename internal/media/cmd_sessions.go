package media

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// cmdSessions handles `qai media sessions` and `qai media sessions
// --rm <id>`. Read-only list + delete; nothing else, so this stays
// short.
func cmdSessions(args []string) {
	args, rmID, rmPresent := stripFlag(args, "--rm")
	if rmPresent {
		if rmID == "" {
			diefatal("--rm needs a session id (use `qai media sessions` to list)")
		}
		// Match a short id prefix against the full list — same UX as
		// docker rm so the user doesn't have to copy-paste a 36-char UUID.
		full := resolveSessionPrefix(rmID)
		if err := DeleteSession(full); err != nil {
			diefatal("%v", err)
		}
		// If the deleted session was the active one, clear the pointer.
		if getActiveSession() == full {
			clearActiveSession()
		}
		fmt.Fprintf(os.Stderr, "qai media: deleted session %s\n", shortID(full))
		return
	}
	if len(args) > 0 {
		diefatal("unknown args: %v (try `qai media sessions` or `qai media sessions --rm <id>`)", args)
	}

	sessions, err := ListSessions()
	if err != nil {
		diefatal("%v", err)
	}
	if len(sessions) == 0 {
		fmt.Println("(no sessions — start one with `qai media chat <file> \"first prompt\"`)")
		return
	}

	active := getActiveSession()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODEL\tFILE\tTURNS\tEXPIRES\tACTIVE")
	for _, s := range sessions {
		mark := ""
		if s.ID == active {
			mark = "*"
		}
		name := s.FileDisplayName
		if name == "" {
			name = s.FileURI
		}
		if len(name) > 40 {
			name = name[:37] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n",
			shortID(s.ID), s.Model, name, s.MessageCount, formatExpiry(s.ExpiresAt), mark)
	}
	tw.Flush()
}

// resolveSessionPrefix accepts a short id prefix (8+ chars) and looks
// up the full UUID by listing sessions. Avoids having to copy-paste a
// full 36-char UUID for the common case. Errors if the prefix is
// ambiguous (matches >1 session) — that's the docker-rm behaviour.
func resolveSessionPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		diefatal("session id cannot be empty")
	}
	// Long enough to be a full uuid? Just trust it.
	if len(prefix) >= 36 {
		return prefix
	}
	sessions, err := ListSessions()
	if err != nil {
		// If we can't list, the prefix might still be a full id —
		// the API call will 404 cleanly if not.
		return prefix
	}
	var matches []string
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, prefix) {
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 0:
		diefatal("no session matches prefix %q (run `qai media sessions` to list)", prefix)
	case 1:
		return matches[0]
	default:
		diefatal("prefix %q is ambiguous — matches %d sessions; pass a longer prefix", prefix, len(matches))
	}
	return prefix // unreachable
}
