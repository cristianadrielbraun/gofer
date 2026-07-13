package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestTrashDeleteActionsLookDestructiveAndSayPermanent(t *testing.T) {
	accounts := []models.Account{{
		ID:      "acc",
		Folders: []models.Folder{{ID: "acc-trash", Name: "Trash", Role: "trash"}},
	}}
	var list bytes.Buffer
	if err := MailListToolbar(accounts, "acc-trash", "cards", models.EmailFilters{}).Render(context.Background(), &list); err != nil {
		t.Fatalf("MailListToolbar.Render() error = %v", err)
	}
	for _, want := range []string{"Permanently delete", "Permanently delete selected messages", "data-mail-delete-permanent=\"true\"", "data-mail-delete-tooltip", "border-red-500/35", "bg-red-500/12", "text-red-700"} {
		if !strings.Contains(list.String(), want) {
			t.Fatalf("Trash toolbar missing %q: %s", want, list.String())
		}
	}

	var message bytes.Buffer
	email := &models.Email{ID: "message", FolderID: "acc-trash", FolderRole: "trash", ThreadCount: 2}
	if err := MailViewHeader(email).Render(context.Background(), &message); err != nil {
		t.Fatalf("MailViewHeader.Render() error = %v", err)
	}
	for _, want := range []string{"Permanently delete thread", "deleteThread", "border-red-500/35", "bg-red-500/12", "text-red-700"} {
		if !strings.Contains(message.String(), want) {
			t.Fatalf("Trash message action missing %q: %s", want, message.String())
		}
	}
}

func TestMailViewLinksKeepTheirFolderContext(t *testing.T) {
	if got := string(mailViewPartialURL("42", "acc-trash", false)); got != "/email/42?folder_id=acc-trash" {
		t.Fatalf("mailViewPartialURL() = %q", got)
	}
	if got := string(mailViewPartialURL("42", "acc-trash", true)); got != "/email/42?folder_id=acc-trash&single=1" {
		t.Fatalf("mailViewPartialURL(single) = %q", got)
	}
}

func TestRegularDeleteActionsKeepNeutralTreatment(t *testing.T) {
	accounts := []models.Account{{ID: "acc", Folders: []models.Folder{{ID: "acc-inbox", Name: "Inbox", Role: "inbox"}}}}
	var out bytes.Buffer
	if err := MailListToolbar(accounts, "acc-inbox", "cards", models.EmailFilters{}).Render(context.Background(), &out); err != nil {
		t.Fatalf("MailListToolbar.Render() error = %v", err)
	}
	html := out.String()
	if !strings.Contains(html, "Delete selected messages") || strings.Contains(html, "Permanently delete") || strings.Contains(html, "border-red-500/35") {
		t.Fatalf("regular delete action changed unexpectedly: %s", html)
	}
}
