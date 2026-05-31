package joplinbridge

import (
	"github.com/quantum-encoding/qai-cli/internal/blast"
	"github.com/quantum-encoding/qai-cli/internal/joplin"
)

// joplinAPI is the Joplin-side surface the syncer needs.
//
// Defined as an interface so tests can substitute a deterministic
// fake. The methods exactly match what *joplin.Client already exposes
// (no shims) so the test fake just records calls in a map.
type joplinAPI interface {
	ListFolders() ([]joplin.Folder, error)
	ListNotesFull(folderID string, fields []string) ([]joplin.Note, error)
	ListTags() ([]joplin.Tag, error)
	GetNoteTags(noteID string) ([]joplin.Tag, error)
	GetEvents(cursor string, limit int) (*joplin.EventsResponse, error)
}

// surrealAPI is the SurrealDB-side surface the syncer needs.
//
// Same shape rationale: matches *blast.Client.Exec exactly so the test
// fake records SQL strings without further translation.
type surrealAPI interface {
	Exec(surql string) ([]blast.StatementResult, error)
}
