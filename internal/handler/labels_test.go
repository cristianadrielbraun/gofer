package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestLabelActionFallsBackToLocalLabelWhenRemoteApplyFails(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
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
	if err := db.UpsertSyncMessages(ctx, []storage.SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_inbox",
		RemoteUID: 42,
		MessageID: "<label-fallback@example.com>",
		Subject:   "Label fallback",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<label-fallback@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}

	h := &Handler{db: db, syncer: mail.NewSyncOrchestrator(db, nil, nil, nil)}
	body := `{"targets":[{"id":"` + strings.TrimSpace(strconv.FormatInt(msgID, 10)) + `"}],"folder_id":"acc_inbox","label":"Invoices"}`
	req := httptest.NewRequest(http.MethodPost, "/api/messages/label", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.handleLabelMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var response map[string]int
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["messages"] != 1 || response["remote_failed"] != 1 {
		t.Fatalf("response = %#v, want one local label with one remote failure", response)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Invoices" || email.Labels[0].ProviderType != storage.LabelProviderLocal {
		t.Fatalf("labels = %#v, want local Invoices label", email.Labels)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/messages/unlabel", strings.NewReader(body))
	rec = httptest.NewRecorder()

	h.handleUnlabelMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unlabel status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	response = map[string]int{}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode unlabel response: %v", err)
	}
	if response["messages"] != 1 || response["remote_failed"] != 1 {
		t.Fatalf("unlabel response = %#v, want one local removal with one remote failure", response)
	}
	email, err = db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() after unlabel error = %v", err)
	}
	if len(email.Labels) != 0 {
		t.Fatalf("labels after unlabel = %#v, want none", email.Labels)
	}
}
