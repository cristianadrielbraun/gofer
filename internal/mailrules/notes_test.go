package mailrules

import (
	"context"
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

func TestDetailTextWritesNotesInTheReadersLanguage(t *testing.T) {
	detail := EncodeNotes(
		Failed(NewNote("add_label", "label", "Bills"), errors.New("NO [LIMIT] too many labels")),
		NewNote("moved_to", "folder", "Archive", "role", "archive"),
		CountNote("run_by_hand", 2, "scope", "Inbox"),
	)
	en := DetailText(context.Background(), detail)
	if want := "add label “Bills”: NO [LIMIT] too many labels; moved to “Archive”; Run by hand on Inbox: changed 2 messages"; en != want {
		t.Fatalf("English = %q, want %q", en, want)
	}
	es := DetailText(i18n.WithLocale(context.Background(), "es"), detail)
	if want := "añadir la etiqueta «Bills»: NO [LIMIT] too many labels; movido a «Archivo»; Ejecutada a mano en Inbox: cambió 2 mensajes"; es != want {
		t.Fatalf("Spanish = %q, want %q", es, want)
	}
}

func TestDetailTextShowsEntriesStoredBeforeNotesAsTheyAre(t *testing.T) {
	for _, old := range []string{"", "marked as read; moved to “Receipts”", "Run by hand on Inbox: changed 1 message"} {
		if got := DetailText(context.Background(), old); got != old {
			t.Fatalf("DetailText(%q) = %q", old, got)
		}
	}
}
