package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestMailItemIDsListsEveryMatchingRowInOrder(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderIMAP); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	now := time.Now()
	messages := []storage.SyncMessage{
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 1, MessageID: "<oldest@example.com>", Subject: "Garden plans", FromEmail: "ada@example.com", DateSent: now.Add(-3 * time.Hour), IsRead: true},
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 2, MessageID: "<middle@example.com>", Subject: "Book club", FromEmail: "ben@example.com", DateSent: now.Add(-2 * time.Hour)},
		{AccountID: "acc", FolderID: "acc_inbox", RemoteUID: 3, MessageID: "<newest@example.com>", Subject: "Bike repair", FromEmail: "cleo@example.com", DateSent: now.Add(-1 * time.Hour), IsRead: true},
	}
	if err := db.UpsertSyncMessages(ctx, messages); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	ids := map[string]string{}
	for _, message := range messages {
		id, err := db.GetMessageLocalIDByInternetIDInternal(ctx, "acc", message.MessageID)
		if err != nil || id == 0 {
			t.Fatalf("GetMessageLocalIDByInternetIDInternal(%s) = %d, %v", message.MessageID, id, err)
		}
		ids[message.MessageID] = strconv.FormatInt(id, 10)
	}

	h := &Handler{db: db}
	get := func(query string) mailItemIDsResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/mail/folder/acc_inbox/ids"+query, nil)
		req.SetPathValue("id", "acc_inbox")
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "default"}))
		rec := httptest.NewRecorder()
		h.handleMailItemIDs(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var response mailItemIDsResponse
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return response
	}

	all := get("")
	want := []string{ids["<newest@example.com>"], ids["<middle@example.com>"], ids["<oldest@example.com>"]}
	if all.Total != 3 || all.Truncated || len(all.IDs) != len(want) {
		t.Fatalf("response = %#v, want the 3 inbox messages", all)
	}
	for i, id := range want {
		if all.IDs[i].ID != id || all.IDs[i].Thread {
			t.Fatalf("ids[%d] = %#v, want single message %s (newest first)", i, all.IDs[i], id)
		}
	}

	unread := get("?unread=1")
	if unread.Total != 1 || len(unread.IDs) != 1 || unread.IDs[0].ID != ids["<middle@example.com>"] {
		t.Fatalf("unread response = %#v, want only the unread message", unread)
	}
}
