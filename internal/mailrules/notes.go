package mailrules

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// A Note is one thing a rule's activity entry says, such as "moved to
// “Receipts”". The activity stores notes as codes with their details and
// writes them in the reader's language when shown, so an entry reads in
// whatever language the page is in.
type Note struct {
	// Key names the message, as rules.activity.note.<Key>.
	Key string `json:"k"`
	// Args fill the message's placeholders. A "folder" with a "role" is a
	// special folder, named in the reader's language while it keeps the
	// name sync gave it.
	Args map[string]string `json:"a,omitempty"`
	// Action is, for a failure, the action that failed.
	Action *Note `json:"x,omitempty"`
	// Error is, for a failure, the error as it came, from a mail server
	// for instance; it is shown as it is.
	Error string `json:"e,omitempty"`
	// Messages counts messages, for a note that says how many changed.
	Messages *int `json:"m,omitempty"`
}

// NewNote is a note with its arguments in name, value pairs.
func NewNote(key string, pairs ...string) Note {
	note := Note{Key: key}
	if len(pairs) > 1 {
		note.Args = map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			note.Args[pairs[i]] = pairs[i+1]
		}
	}
	return note
}

// Failed is a note for an action that failed with err.
func Failed(action Note, err error) Note {
	return Note{Key: "failed", Action: &action, Error: err.Error()}
}

// notesPrefix starts a detail stored as notes. Entries recorded before notes
// hold plain English text, which is shown as it is.
const notesPrefix = `{"notes":`

// EncodeNotes stores notes as an activity entry's detail.
func EncodeNotes(notes ...Note) string {
	if len(notes) == 0 {
		return ""
	}
	data, err := json.Marshal(struct {
		Notes []Note `json:"notes"`
	}{notes})
	if err != nil {
		return ""
	}
	return string(data)
}

// DecodeNotes reads the notes of a stored detail, or false for text stored
// before notes.
func DecodeNotes(detail string) ([]Note, bool) {
	if !strings.HasPrefix(detail, notesPrefix) {
		return nil, false
	}
	var stored struct {
		Notes []Note `json:"notes"`
	}
	if err := json.Unmarshal([]byte(detail), &stored); err != nil {
		return nil, false
	}
	return stored.Notes, true
}

// DetailText writes a stored detail in ctx's language: its notes joined with
// "; ", or text stored before notes as it is.
func DetailText(ctx context.Context, detail string) string {
	notes, ok := DecodeNotes(detail)
	if !ok {
		return detail
	}
	parts := make([]string, 0, len(notes))
	for _, note := range notes {
		if text := NoteText(ctx, note); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "; ")
}

// NoteText writes one note in ctx's language.
func NoteText(ctx context.Context, note Note) string {
	if note.Key == "failed" {
		action := ""
		if note.Action != nil {
			action = NoteText(ctx, *note.Action)
		}
		return i18n.T(ctx, "rules.activity.note.failed", "action", action, "error", note.Error)
	}
	args := make([]any, 0, 2*len(note.Args)+4)
	for name, value := range note.Args {
		if name == "folder" && note.Args["role"] != "" {
			value = i18n.FolderName(ctx, note.Args["role"], value)
		}
		args = append(args, name, value)
	}
	if note.Messages != nil {
		n := *note.Messages
		args = append(args, "messages", i18n.N(ctx, "rules.messages", n, "count", i18n.Number(ctx, n)))
	}
	return i18n.T(ctx, "rules.activity.note."+note.Key, args...)
}

// CountNote is a note that says how many messages changed.
func CountNote(key string, messages int, pairs ...string) Note {
	note := NewNote(key, pairs...)
	note.Messages = &messages
	return note
}
