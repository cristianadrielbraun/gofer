package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type labelSyncTestTokens struct{}

func (labelSyncTestTokens) GetOAuthTokenForAccount(context.Context, string) (string, error) {
	return "gmail-token", nil
}

func (labelSyncTestTokens) GetMicrosoftGraphMailTokenForAccount(context.Context, string) (string, error) {
	return "graph-token", nil
}

func newLabelSyncTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users (id, email, name, is_admin) VALUES ('default', 'local@example.com', 'Local', 1)`); err != nil {
		t.Fatalf("insert default user: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedLabelSyncMessage(t *testing.T, db *storage.DB, provider, messageID, remoteMessageID string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, provider); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []storage.SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_inbox",
		RemoteUID: 7,
		MessageID: messageID,
		Subject:   "Provider labels",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", messageID)
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	if remoteMessageID != "" {
		if err := db.SetMessageProviderMessageID(ctx, msgID, remoteMessageID); err != nil {
			t.Fatalf("SetMessageProviderMessageID() error = %v", err)
		}
	}
	return msgID
}

func TestSyncGmailLabelsImportsRemoteMessageLabels(t *testing.T) {
	db := newLabelSyncTestDB(t)
	msgID := seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-label@example.com>", "gmail-msg-1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{
				{"id": "Label_1", "name": "Projects", "type": "user"},
				{"id": "INBOX", "name": "INBOX", "type": "system"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-msg-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gmail-msg-1", "labelIds": []string{"INBOX", "Label_1"}})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncGmailLabels(context.Background(), "acc"); err != nil {
		t.Fatalf("syncGmailLabels() error = %v", err)
	}

	email, err := db.GetEmailByID(context.Background(), strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Projects" || email.Labels[0].ProviderID != "Label_1" || email.Labels[0].ProviderType != storage.LabelProviderGmail {
		t.Fatalf("labels = %#v, want Projects Gmail label only", email.Labels)
	}
}

func TestSyncOutlookCategoriesImportsRemoteMessageCategories(t *testing.T) {
	db := newLabelSyncTestDB(t)
	msgID := seedLabelSyncMessage(t, db, providers.ProviderOutlook, "<outlook-label@example.com>", "outlook-msg-1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Prefer"); !strings.Contains(got, "ImmutableId") {
			t.Fatalf("Prefer = %q, want immutable ids", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/outlook/masterCategories":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{
				{"displayName": "Invoices", "color": "preset7"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/outlook-msg-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "outlook-msg-1", "categories": []string{"Invoices"}})
		default:
			t.Fatalf("unexpected Outlook request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookCategories(context.Background(), "acc"); err != nil {
		t.Fatalf("syncOutlookCategories() error = %v", err)
	}

	email, err := db.GetEmailByID(context.Background(), strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Invoices" || email.Labels[0].ProviderType != storage.LabelProviderOutlook {
		t.Fatalf("labels = %#v, want Invoices Outlook category", email.Labels)
	}
}

func TestReplayGmailLabelMutationQueueAppliesQueuedAdd(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	msgID := seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-queued-label@example.com>", "gmail-msg-queued")
	if _, err := db.AddMessageLabel(ctx, msgID, "acc", storage.LabelInput{
		AccountID:    "acc",
		Name:         "Projects",
		ProviderType: storage.LabelProviderLocal,
	}); err != nil {
		t.Fatalf("AddMessageLabel() error = %v", err)
	}
	if err := db.EnqueueLabelMutation(ctx, "acc", msgID, "acc_inbox", storage.LabelProviderGmail, storage.LabelMutationAdd, "Projects", errors.New("temporary provider failure")); err != nil {
		t.Fatalf("EnqueueLabelMutation() error = %v", err)
	}

	modified := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{
				{"id": "Label_Projects", "name": "Projects", "type": "user"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/users/me/messages/gmail-msg-queued/modify":
			var payload map[string][]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode modify payload: %v", err)
			}
			if got := payload["addLabelIds"]; len(got) != 1 || got[0] != "Label_Projects" {
				t.Fatalf("addLabelIds = %#v, want Label_Projects", got)
			}
			modified = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gmail-msg-queued"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	orchestrator.replayGmailLabelMutationQueue(ctx, "acc", "gmail-token")

	if !modified {
		t.Fatalf("queued Gmail mutation was not applied")
	}
	entries, err := db.ListDueLabelMutations(ctx, "acc", storage.LabelProviderGmail, 10)
	if err != nil {
		t.Fatalf("ListDueLabelMutations() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("queued entries after replay = %#v, want none", entries)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].ProviderType != storage.LabelProviderGmail || email.Labels[0].ProviderID != "Label_Projects" {
		t.Fatalf("labels = %#v, want provider-backed Gmail label", email.Labels)
	}
}

func TestReplayOutlookLabelMutationQueueAppliesQueuedAdd(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	msgID := seedLabelSyncMessage(t, db, providers.ProviderOutlook, "<outlook-queued-label@example.com>", "outlook-msg-queued")
	if _, err := db.AddMessageLabel(ctx, msgID, "acc", storage.LabelInput{
		AccountID:    "acc",
		Name:         "Invoices",
		ProviderType: storage.LabelProviderLocal,
	}); err != nil {
		t.Fatalf("AddMessageLabel() error = %v", err)
	}
	if err := db.EnqueueLabelMutation(ctx, "acc", msgID, "acc_inbox", storage.LabelProviderOutlook, storage.LabelMutationAdd, "Invoices", errors.New("temporary provider failure")); err != nil {
		t.Fatalf("EnqueueLabelMutation() error = %v", err)
	}

	categoryCreated := false
	messagePatched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Prefer"); !strings.Contains(got, "ImmutableId") {
			t.Fatalf("Prefer = %q, want immutable ids", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/outlook-msg-queued":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "outlook-msg-queued", "categories": []string{}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/outlook/masterCategories":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{}})
		case r.Method == http.MethodPost && r.URL.Path == "/me/outlook/masterCategories":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode category payload: %v", err)
			}
			if payload["displayName"] != "Invoices" {
				t.Fatalf("displayName = %q, want Invoices", payload["displayName"])
			}
			categoryCreated = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"displayName": "Invoices", "color": "preset0"})
		case r.Method == http.MethodPatch && r.URL.Path == "/me/messages/outlook-msg-queued":
			var payload map[string][]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode patch payload: %v", err)
			}
			if got := payload["categories"]; len(got) != 1 || got[0] != "Invoices" {
				t.Fatalf("categories = %#v, want Invoices", got)
			}
			messagePatched = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "outlook-msg-queued"})
		default:
			t.Fatalf("unexpected Outlook request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	orchestrator.replayOutlookLabelMutationQueue(ctx, "acc", "graph-token")

	if !categoryCreated || !messagePatched {
		t.Fatalf("queued Outlook mutation was not fully applied: category=%v patch=%v", categoryCreated, messagePatched)
	}
	entries, err := db.ListDueLabelMutations(ctx, "acc", storage.LabelProviderOutlook, 10)
	if err != nil {
		t.Fatalf("ListDueLabelMutations() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("queued entries after replay = %#v, want none", entries)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].ProviderType != storage.LabelProviderOutlook {
		t.Fatalf("labels = %#v, want provider-backed Outlook label", email.Labels)
	}
}
