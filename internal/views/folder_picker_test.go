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
	render := func(f folderField) string {
		var out bytes.Buffer
		if err := folderFieldView(f).Render(ctx, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	cond := mailRuleConditionFolders(ctx, 0, &models.MailRuleFolderTarget{FolderID: "w-trips"}, accounts)
	if cond.Value != "id:w-trips" || cond.Chosen.Name != "Trips" || cond.Chosen.Parent != "Inbox" || cond.Chosen.Account != "robin@example.test" || cond.Chosen.Full() != "robin@example.test · Inbox / Trips" {
		t.Fatalf("condition chosen = %q %+v", cond.Value, cond.Chosen)
	}
	html := render(cond)
	if strings.Contains(html, `data-folder-field-choice="id:l-keep"`) || !strings.Contains(html, `data-folder-field-choice="role:sent"`) || strings.Contains(html, "data-folder-field-create") {
		t.Fatal("a condition tests mail accounts' folders, Sent included, and makes no folders")
	}
	if !regexp.MustCompile(`aria-selected="true"[^>]*data-folder-field-choice="id:w-trips"`).MatchString(html) {
		t.Fatal("the chosen folder isn't selected in the picker")
	}

	move := mailRuleMoveFolders(ctx, 0, &models.MailRuleFolderTarget{Role: "archive"}, accounts)
	html = render(move)
	if move.Value != "role:archive" || !strings.Contains(html, `data-folder-field-choice="id:l-keep"`) || strings.Contains(html, `data-folder-field-choice="id:o-notes"`) {
		t.Fatalf("move offers Local folders but not a hidden account: %q", move.Value)
	}
	if !regexp.MustCompile(`disabled[^>]*data-folder-field-choice="id:w-sent"`).MatchString(html) || !strings.Contains(html, "data-mail-rule-rerender") || !strings.Contains(html, `data-folder-picker-create-parent="w-trips"`) {
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
	if run.Chosen.Name != "Inbox" || !strings.Contains(html, `name="scope"`) || strings.Contains(html, `data-folder-field-choice="id:l-keep"`) {
		t.Fatalf("run picker = %+v", run.Chosen)
	}
}

func TestFolderFieldsForImportAndRoles(t *testing.T) {
	ctx := context.Background()
	account := models.Account{ID: "work", Email: "robin@example.test", Provider: "imap", Folders: []models.Folder{
		{ID: "in", Name: "Inbox", Role: "inbox", Children: []models.Folder{{ID: "trips", Name: "Trips"}}},
		{ID: "drafts", Name: "Drafts", Role: "drafts"},
	}}
	offered := []MailImportFolder{{ID: "in", Path: "Inbox"}, {ID: "trips", Path: "Inbox/Trips"}}

	target := mailFolderTargetField(ctx, "mail-import-target-0", "target:0", "Folder for Trips", account, offered, "trips", true)
	if target.Value != "trips" || target.Chosen.Account != "" || target.Chosen.Full() != "Inbox / Trips" {
		t.Fatalf("one account's field = %q %+v", target.Value, target.Chosen)
	}
	var out bytes.Buffer
	if err := folderFieldView(target).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{`data-folder-field-choice="new"`, `data-folder-field-choice="skip"`, "data-mail-import-target", `name="target:0" value="trips"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("import field lacks %s", want)
		}
	}
	if !regexp.MustCompile(`disabled [^>]*data-folder-field-choice="drafts"`).MatchString(html) {
		t.Fatal("a folder that isn't offered can be picked")
	}
	if got := mailFolderTargetField(ctx, "t", "target:1", "x", account, offered, "new", false); got.Value != "new" || got.Chosen.Name != "New folder…" || len(got.Extras) != 1 {
		t.Fatalf("transfer field = %q %+v", got.Value, got.Chosen)
	}

	roles := models.FolderRolesData{Account: account, Folders: []models.FolderOption{{ID: "drafts", Name: "Drafts"}, {ID: "trips", Name: "INBOX.Trips"}}}
	automatic := folderRoleField(ctx, roles, models.FolderRole{Role: "drafts"})
	if automatic.Value != "" || automatic.Chosen.Name != "Automatic" || automatic.Name != "role_drafts" {
		t.Fatalf("role left to the server = %q %+v", automatic.Value, automatic.Chosen)
	}
	if got := folderRoleField(ctx, roles, models.FolderRole{Role: "drafts", Chosen: "in"}); got.Value != "" {
		t.Fatalf("the Inbox was taken as a role's folder: %q", got.Value)
	}
}
