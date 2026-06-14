package handler

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestBuildSyncSettingsUsesRemoteFolderPaths(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address) VALUES ('acc', 'default', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true, SortOrder: 1},
		{ID: "acc_sent", AccountID: "acc", RemoteID: "Sent", Name: "Sent", Role: "sent", Selectable: true, SortOrder: 2},
		{ID: "acc_gmail_sent", AccountID: "acc", RemoteID: "[Gmail]/Sent Mail", Name: "Sent", Role: "sent", Selectable: true, SortOrder: 3},
		{ID: "acc_drafts", AccountID: "acc", RemoteID: "[Gmail]/Drafts", Name: "Drafts", Role: "drafts", Selectable: true, SortOrder: 4},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	h := &Handler{db: db}
	settings := h.buildSyncSettings(ctx, []models.Account{{
		ID:               "acc",
		Name:             "User",
		Email:            "user@example.com",
		EmailSyncEnabled: true,
	}})

	if len(settings.Accounts) != 1 {
		t.Fatalf("settings.Accounts = %#v, want one account", settings.Accounts)
	}
	got := make(map[string]string)
	for _, folder := range settings.Accounts[0].Folders {
		got[folder.ID] = folder.Name
	}
	for folderID, want := range map[string]string{
		"acc_inbox":      "INBOX",
		"acc_sent":       "Sent",
		"acc_gmail_sent": "[Gmail]/Sent Mail",
		"acc_drafts":     "[Gmail]/Drafts",
	} {
		if got[folderID] != want {
			t.Fatalf("folder %s name = %q, want %q (all names: %#v)", folderID, got[folderID], want, got)
		}
	}
}
