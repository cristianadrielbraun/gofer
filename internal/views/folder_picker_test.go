package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestFolderPickerEntriesNameThePath(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), "es")
	account := models.Account{ID: "acct", Email: "robin@example.test", Folders: []models.Folder{
		{ID: "in", Name: "Inbox", Role: "inbox", Children: []models.Folder{
			{ID: "trips", Name: "Trips", Children: []models.Folder{{ID: "lisbon", Name: "Lisbon"}}},
		}},
		{ID: "star", Name: "Starred", Role: "starred", Children: []models.Folder{{ID: "kept", Name: "Kept"}}},
	}}

	entries := folderPickerEntries(ctx, account, false)
	var ids []string
	for _, entry := range entries {
		ids = append(ids, entry.Folder.ID)
	}
	if got := strings.Join(ids, ","); got != "in,trips,lisbon,kept" {
		t.Fatalf("entries = %s, want Starred left out but the folders inside it listed", got)
	}
	lisbon := entries[2]
	if lisbon.Depth != 2 || lisbon.ParentID != "trips" || lisbon.Parent != "Recibidos / Trips" || lisbon.Path != "Recibidos / Trips / Lisbon" {
		t.Fatalf("lisbon = %+v, want its translated path", lisbon)
	}
	for _, text := range []string{"robin@example.test", "recibidos / trips / lisbon"} {
		if !strings.Contains(lisbon.Search, text) {
			t.Fatalf("search text %q lacks %q", lisbon.Search, text)
		}
	}
	if !strings.Contains(entries[0].Search, "inbox") {
		t.Fatalf("search text %q lacks the folder's own name", entries[0].Search)
	}
	if got := len(folderPickerEntries(ctx, account, true)); got != 5 {
		t.Fatalf("with Starred: %d entries, want 5", got)
	}
}

func TestMailMoveDialogUsesTheFolderPicker(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), "es")
	email := &models.Email{ID: "1", AccountID: "acct", FolderID: "in"}
	accounts := []models.Account{{ID: "acct", Email: "robin@example.test", Provider: "imap", Folders: []models.Folder{
		{ID: "in", Name: "Inbox", Role: "inbox", Children: []models.Folder{{ID: "trips", Name: "Trips"}}},
	}}}
	var out bytes.Buffer
	if err := MailMoveDialog(email, accounts).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"data-folder-picker-search", "data-folder-picker-account", `data-search-text="robin@example.test recibidos / trips"`, ">Recibidos<", `data-mail-move-create-parent="trips"`, "data-folder-picker-keep"} {
		if !strings.Contains(html, want) {
			t.Fatalf("Move dialog lacks %s", want)
		}
	}
}
