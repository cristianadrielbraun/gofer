package views

import (
	"bytes"
	"context"
	"regexp"
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
	for _, want := range []string{"data-folder-picker-search", "data-folder-picker-account", `data-search-text="robin@example.test recibidos / trips"`, ">Recibidos<", `data-folder-picker-create-parent="trips"`, "data-folder-picker-keep"} {
		if !strings.Contains(html, want) {
			t.Fatalf("Move dialog lacks %s", want)
		}
	}
}

func TestFolderMoveDialogUsesTheFolderPicker(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), "es")
	data := models.FolderMoveData{
		Account:    models.Account{ID: "acct", Email: "robin@example.test", Name: "Robin", Provider: "imap"},
		FolderID:   "trips",
		FolderName: "Trips",
		ParentID:   "in",
		Folders: []models.Folder{
			{ID: "in", Name: "Inbox", Role: "inbox", Children: []models.Folder{{ID: "trips", Name: "Trips", Children: []models.Folder{{ID: "lisbon", Name: "Lisbon"}}}}},
			{ID: "notes", Name: "Notes"},
		},
	}
	var out bytes.Buffer
	if err := FolderMoveDialog(data).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"data-folder-picker-search",
		`data-search-text="robin@example.test recibidos / trips / lisbon"`,
		"Primer nivel de la cuenta",
		`data-folder-move-parent="" data-folder-move-place="Robin"`,
		`data-folder-move-parent="in" data-folder-move-place="Recibidos"`,
		`data-folder-picker-keep`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Move folder dialog lacks %s", want)
		}
	}
}

func TestMailRuleFolderPickers(t *testing.T) {
	ctx := context.Background()
	accounts := []models.MailRuleAccount{
		{ID: "work", Email: "robin@example.test", Provider: "imap", Tree: []models.Folder{
			{ID: "w-in", Name: "Inbox", Role: "inbox", Children: []models.Folder{{ID: "w-trips", Name: "Trips"}}},
			{ID: "w-sent", Name: "Sent", Role: "sent"},
		}},
		{ID: "old", Email: "sam@example.test", Provider: "imap", Hidden: true, Tree: []models.Folder{{ID: "o-notes", Name: "Notes"}}},
		{ID: "local", Name: "Local folders", Provider: "local", Tree: []models.Folder{{ID: "l-keep", Name: "Keep"}}},
	}
	render := func(p mailRuleFolderPicker) string {
		var out bytes.Buffer
		if err := mailRuleFolderField(p).Render(ctx, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	cond := mailRuleConditionFolders(ctx, 0, &models.MailRuleFolderTarget{FolderID: "w-trips"}, accounts)
	if cond.Value != "id:w-trips" || cond.Chosen.Name != "Trips" || cond.Chosen.Parent != "Inbox" || cond.Chosen.Account != "robin@example.test" || cond.Chosen.Full() != "robin@example.test · Inbox / Trips" {
		t.Fatalf("condition chosen = %q %+v", cond.Value, cond.Chosen)
	}
	html := render(cond)
	if strings.Contains(html, `data-mail-rule-folder-choice="id:l-keep"`) || !strings.Contains(html, `data-mail-rule-folder-choice="role:sent"`) || strings.Contains(html, "data-mail-rule-folder-create") {
		t.Fatal("a condition tests mail accounts' folders, Sent included, and makes no folders")
	}
	if !regexp.MustCompile(`aria-selected="true"[^>]*data-mail-rule-folder-choice="id:w-trips"`).MatchString(html) {
		t.Fatal("the chosen folder isn't selected in the picker")
	}

	move := mailRuleMoveFolders(ctx, 0, &models.MailRuleFolderTarget{Role: "archive"}, accounts)
	html = render(move)
	if move.Value != "role:archive" || !strings.Contains(html, `data-mail-rule-folder-choice="id:l-keep"`) || strings.Contains(html, `data-mail-rule-folder-choice="id:o-notes"`) {
		t.Fatalf("move offers Local folders but not a hidden account: %q", move.Value)
	}
	if !regexp.MustCompile(`disabled[^>]*data-mail-rule-folder-choice="id:w-sent"`).MatchString(html) || !strings.Contains(html, "data-mail-rule-rerender") || !strings.Contains(html, `data-folder-picker-create-parent="w-trips"`) {
		t.Fatal("move can't pick Sent, draws the form again and makes folders")
	}
	if got := mailRuleMoveFolders(ctx, 0, &models.MailRuleFolderTarget{FolderID: "o-notes", AccountID: "old"}, accounts); got.Value != "id:o-notes" {
		t.Fatalf("a hidden account the rule moves to = %q", got.Value)
	}
	if got := mailRuleMoveFolders(ctx, 0, &models.MailRuleFolderTarget{FolderID: "gone"}, accounts); got.Value != "" || got.Chosen.Name != "" {
		t.Fatalf("a folder no longer there = %q %+v", got.Value, got.Chosen)
	}

	run := mailRuleRunFolders(ctx, &models.MailRuleRunData{Rule: models.MailRule{}, Scope: "id:w-in"}, accounts)
	html = render(run)
	if run.Chosen.Name != "Inbox" || !strings.Contains(html, `name="scope"`) || strings.Contains(html, `data-mail-rule-folder-choice="id:l-keep"`) {
		t.Fatalf("run picker = %+v", run.Chosen)
	}
}
