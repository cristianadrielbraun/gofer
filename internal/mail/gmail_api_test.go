package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestShouldUseGmailAPIMailIsAlwaysUsedForGmailAccounts(t *testing.T) {
	orchestrator := NewSyncOrchestrator(nil, nil, nil, labelSyncTestTokens{})
	cfg := &models.AccountConfig{Provider: providers.ProviderGmail}

	if !orchestrator.shouldUseGmailAPIMail(cfg) {
		t.Fatal("shouldUseGmailAPIMail() = false for Gmail account")
	}

	t.Setenv("GOFER_GMAIL_API_SYNC", "0")
	if !orchestrator.shouldUseGmailAPIMail(cfg) {
		t.Fatal("shouldUseGmailAPIMail() followed deprecated disable env")
	}
}

func TestSyncGmailAPIAccountImportsLabelsMessagesAndCursor(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_sent", AccountID: "acc", RemoteID: "[Gmail]/Sent Mail", ProviderRemoteID: "SENT", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "acc_projects", AccountID: "acc", RemoteID: "Projects", ProviderRemoteID: "Label_Projects", Name: "Projects", Role: "custom", Selectable: true},
		{ID: "acc_important", AccountID: "acc", RemoteID: "[Gmail]/Important", ProviderRemoteID: "IMPORTANT", Name: "[Gmail]/Important", Role: "custom", Selectable: true},
		{ID: "acc_category_forums", AccountID: "acc", RemoteID: "CATEGORY_FORUMS", ProviderRemoteID: "CATEGORY_FORUMS", Name: "CATEGORY_FORUMS", Role: "custom", Selectable: true},
		{ID: "acc_yellow_star", AccountID: "acc", RemoteID: "YELLOW_STAR", ProviderRemoteID: "YELLOW_STAR", Name: "YELLOW_STAR", Role: "custom", Selectable: true},
		{ID: "acc_imap_trash", AccountID: "acc", RemoteID: "[Imap]/Trash", ProviderRemoteID: "Label_ImapTrash", Name: "[Imap]/Trash", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders(seed) error = %v", err)
	}
	requests := map[string]int{}
	requestSeq := 0
	messageGetSeq := 0
	sentListSeq := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		requestSeq++
		key := r.Method + " " + r.URL.Path
		requests[key]++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]any{
				{"id": "INBOX", "name": "INBOX", "type": "system"},
				{"id": "SENT", "name": "SENT", "type": "system"},
				{"id": "IMPORTANT", "name": "IMPORTANT", "type": "system"},
				{"id": "CATEGORY_FORUMS", "name": "CATEGORY_FORUMS", "type": "system"},
				{"id": "YELLOW_STAR", "name": "YELLOW_STAR", "type": "system"},
				{"id": "Label_Projects", "name": "Projects", "type": "user"},
				{"id": "Label_ImapTrash", "name": "[Imap]/Trash", "type": "user"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "gmail-msg-1", "threadId": "thread-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "SENT":
			sentListSeq = requestSeq
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "IMPORTANT":
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "Label_Projects":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "gmail-msg-1", "threadId": "thread-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-msg-1":
			messageGetSeq = requestSeq
			if got := r.URL.Query().Get("format"); got != "metadata" {
				t.Fatalf("message format = %q, want metadata during baseline sync", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "gmail-msg-1",
				"threadId":     "thread-1",
				"labelIds":     []string{"INBOX", "UNREAD", "STARRED", "Label_Projects"},
				"snippet":      "Project update",
				"historyId":    "101",
				"internalDate": "1760000000000",
				"payload": map[string]any{
					"mimeType": "multipart/alternative",
					"headers": []map[string]string{
						{"name": "Message-ID", "value": "<gmail-api@example.com>"},
						{"name": "Subject", "value": "Gmail API subject"},
						{"name": "From", "value": "Sender <sender@example.com>"},
						{"name": "To", "value": "Recipient <recipient@example.com>"},
						{"name": "Date", "value": "Fri, 26 Jun 2026 12:00:00 +0000"},
					},
					"parts": []map[string]any{{
						"mimeType": "text/plain",
						"body":     map[string]string{"data": "R21haWwgQVBJIGJvZHk"},
					}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "105"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), labelSyncTestTokens{})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v", err)
	}
	if requests["GET /users/me/messages/gmail-msg-1"] != 1 {
		t.Fatalf("message get requests = %d, want one deduped provider fetch", requests["GET /users/me/messages/gmail-msg-1"])
	}
	if messageGetSeq == 0 || sentListSeq == 0 || messageGetSeq > sentListSeq {
		t.Fatalf("request order message=%d sent=%d, want first Inbox message imported before later targets", messageGetSeq, sentListSeq)
	}

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<gmail-api@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if email.Subject != "Gmail API subject" || email.From.Email != "sender@example.com" || email.IsRead || !email.IsStarred {
		t.Fatalf("email = %#v, want Gmail API subject/from/unread/starred", email)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Projects" || email.Labels[0].ProviderID != "Label_Projects" {
		t.Fatalf("labels = %#v, want Projects Gmail label", email.Labels)
	}
	if db.IsBodyFetched(ctx, msgID) {
		t.Fatal("Gmail API baseline sync fetched the body; want metadata-only import")
	}
	body, err := db.GetEmailBody(ctx, strconv.FormatInt(msgID, 10))
	if err != nil || body != nil {
		t.Fatalf("GetEmailBody() = %q, %v; want no stored body before lazy fetch", string(body), err)
	}
	var staleSent int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'acc_sent' AND is_deleted = 0`, msgID).Scan(&staleSent); err != nil {
		t.Fatalf("query stale sent state: %v", err)
	}
	if staleSent != 0 {
		t.Fatalf("stale sent states = %d, want Gmail label reconciliation to remove stale folder membership", staleSent)
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	byProviderID := map[string]storage.FolderSyncInfo{}
	for _, folder := range folders {
		byProviderID[folder.ProviderRemoteID] = folder
	}
	if byProviderID["INBOX"].Role != "inbox" || byProviderID["ARCHIVE"].Role != "archive" {
		t.Fatalf("folders by provider = %#v, want Gmail mailbox views plus archive view", byProviderID)
	}
	for _, providerID := range []string{"IMPORTANT", "CATEGORY_FORUMS", "YELLOW_STAR", "Label_Projects", "Label_ImapTrash"} {
		if _, ok := byProviderID[providerID]; ok {
			t.Fatalf("%s rendered as selectable folder: %#v", providerID, byProviderID[providerID])
		}
	}
	var hiddenLabelFolders int
	if err := db.Read().QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM folders
		WHERE account_id = 'acc'
		  AND provider_remote_id IN ('IMPORTANT', 'CATEGORY_FORUMS', 'YELLOW_STAR', 'Label_Projects', 'Label_ImapTrash')
		  AND COALESCE(selectable, 1) = 1`).Scan(&hiddenLabelFolders); err != nil {
		t.Fatalf("query hidden Gmail labels: %v", err)
	}
	if hiddenLabelFolders != 0 {
		t.Fatalf("hidden Gmail label folders selectable = %d, want 0", hiddenLabelFolders)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "105" || !state.LastFullSyncAt.Valid || state.LastSyncedMessages != 1 {
		t.Fatalf("sync state = %#v, want full cursor 105 with one synced message", state)
	}
}

func TestSyncGmailAPIAccountRefreshesTokenAfterUnauthorized(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}

	messageGets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "gmail-msg-1", "threadId": "thread-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-msg-1":
			messageGets++
			if r.Header.Get("Authorization") == "Bearer stale-token" {
				http.Error(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer fresh-token" {
				t.Fatalf("Authorization = %q, want refreshed token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "gmail-msg-1",
				"threadId":     "thread-1",
				"labelIds":     []string{"INBOX"},
				"historyId":    "101",
				"internalDate": "1760000000000",
				"payload": map[string]any{"headers": []map[string]string{
					{"name": "Message-ID", "value": "<gmail-refresh@example.com>"},
					{"name": "Subject", "value": "Token refresh"},
					{"name": "From", "value": "Sender <sender@example.com>"},
					{"name": "Date", "value": "Fri, 26 Jun 2026 12:00:00 +0000"},
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "105"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	refreshes := 0
	orchestrator := NewSyncOrchestrator(db, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), refreshingLabelSyncTestTokens{
		initial:   "stale-token",
		refreshed: "fresh-token",
		refreshes: &refreshes,
	})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v", err)
	}
	if messageGets != 2 {
		t.Fatalf("message get requests = %d, want stale attempt plus refreshed retry", messageGets)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "105" || !state.LastFullSyncAt.Valid || state.LastError != "" {
		t.Fatalf("sync state = %#v, want successful full sync with refreshed token", state)
	}
}

func TestSyncGmailAPIHistoricalImportRefreshesTokenForMessageList(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	messageListRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "105"})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			messageListRequests++
			if r.Header.Get("Authorization") == "Bearer stale-token" {
				http.Error(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer fresh-token" {
				t.Fatalf("Authorization = %q, want refreshed token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "gmail-msg-1", "threadId": "thread-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			if got := r.Header.Get("Authorization"); got != "Bearer fresh-token" {
				t.Fatalf("archive Authorization = %q, want refreshed token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-msg-1":
			if got := r.Header.Get("Authorization"); got != "Bearer fresh-token" {
				t.Fatalf("metadata Authorization = %q, want refreshed token", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "gmail-msg-1",
				"threadId":     "thread-1",
				"labelIds":     []string{"INBOX"},
				"historyId":    "101",
				"internalDate": "1760000000000",
				"payload": map[string]any{"headers": []map[string]string{
					{"name": "Message-ID", "value": "<gmail-list-refresh@example.com>"},
					{"name": "Subject", "value": "List refresh"},
					{"name": "From", "value": "Sender <sender@example.com>"},
					{"name": "Date", "value": "Fri, 26 Jun 2026 12:00:00 +0000"},
				}},
			})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	refreshes := 0
	orchestrator := NewSyncOrchestrator(db, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), refreshingLabelSyncTestTokens{
		initial:   "stale-token",
		refreshed: "fresh-token",
		refreshes: &refreshes,
	})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", false); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v", err)
	}
	if messageListRequests != 2 {
		t.Fatalf("message list requests = %d, want stale attempt plus refreshed retry", messageListRequests)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
}

func TestSyncGmailAPIAccountSeedsCursorWithoutHistoricalImportForExistingProviderMessages(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	idsByProvider, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "gmail-msg-1",
		InternetMessageID: "<known@gmail.example>",
		Subject:           "Known Gmail message",
		FromEmail:         "sender@example.com",
		DateSent:          mustParseGmailAPITestTime(t),
		DateReceived:      mustParseGmailAPITestTime(t),
		IsRead:            true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if idsByProvider["gmail-msg-1"] == 0 {
		t.Fatalf("idsByProvider = %#v, want gmail-msg-1 local id", idsByProvider)
	}

	messageListRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "105"})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			messageListRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "gmail-msg-1", "threadId": "thread-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case strings.HasPrefix(r.URL.Path, "/users/me/messages/"):
			t.Fatalf("existing provider-backed Gmail catch-up should not fetch known message metadata: %s", r.URL.String())
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v", err)
	}
	if messageListRequests != 1 {
		t.Fatalf("message list requests = %d, want one bounded recent catch-up page", messageListRequests)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "105" || !state.LastSuccessAt.Valid || state.LastFullSyncAt.Valid {
		t.Fatalf("sync state = %#v, want seeded live cursor without historical import", state)
	}
}

func TestSyncGmailAPIAccountRecentCatchupImportsGapMessageBeforeHistory(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	baseline := mustParseGmailAPITestTime(t)
	if _, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "gmail-known",
		InternetMessageID: "<known@gmail.example>",
		Subject:           "Known Gmail message",
		FromEmail:         "sender@example.com",
		DateSent:          baseline,
		DateReceived:      baseline,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID:      "acc",
		ProviderType:   storage.LabelProviderGmail,
		Scope:          "messages",
		StartedAt:      baseline.Add(-time.Minute),
		FinishedAt:     baseline,
		Full:           false,
		Cursor:         "200",
		TotalMessages:  0,
		SyncedMessages: 0,
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	messageGets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]any{
				{"id": "INBOX", "name": "INBOX", "type": "system"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{
				{"id": "gmail-gap", "threadId": "thread-gap"},
				{"id": "gmail-known", "threadId": "thread-known"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-gap":
			messageGets++
			if got := r.URL.Query().Get("format"); got != "metadata" {
				t.Fatalf("message format = %q, want metadata for recent catch-up", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "gmail-gap",
				"threadId":     "thread-gap",
				"labelIds":     []string{"INBOX", "UNREAD"},
				"snippet":      "Reddit gap",
				"historyId":    "201",
				"internalDate": "1760000600000",
				"payload": map[string]any{
					"headers": []map[string]string{
						{"name": "Message-ID", "value": "<gap@gmail.example>"},
						{"name": "Subject", "value": "Reddit gap"},
						{"name": "From", "value": "Reddit <noreply@redditmail.com>"},
						{"name": "Date", "value": "Fri, 26 Jun 2026 12:10:00 +0000"},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-known":
			t.Fatalf("recent catch-up fetched known message metadata: %s", r.URL.String())
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/history":
			if got := r.URL.Query().Get("startHistoryId"); got != "200" {
				t.Fatalf("startHistoryId = %q, want seeded cursor 200", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "210"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), labelSyncTestTokens{})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v", err)
	}
	if messageGets != 1 {
		t.Fatalf("message metadata gets = %d, want only missing gap message", messageGets)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<gap@gmail.example>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "210" || state.LastFullSyncAt.Valid {
		t.Fatalf("sync state = %#v, want history cursor advanced without full baseline", state)
	}
}

func TestRepairGmailAPIAccountRunsHistoricalImportForExistingProviderMessages(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address, auth_method) VALUES ('acc', 'default', ?, 'user@example.com', 'oauth2')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	baseline := mustParseGmailAPITestTime(t)
	if _, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "gmail-known",
		InternetMessageID: "<known@gmail.example>",
		Subject:           "Known Gmail message",
		FromEmail:         "sender@example.com",
		DateSent:          baseline,
		DateReceived:      baseline,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID:    "acc",
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		StartedAt:    baseline.Add(-time.Minute),
		FinishedAt:   baseline,
		Full:         false,
		Cursor:       "200",
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	messageGets := map[string]int{}
	var messageGetsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]any{
				{"id": "INBOX", "name": "INBOX", "type": "system"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{
				{"id": "gmail-new", "threadId": "thread-new"},
				{"id": "gmail-known", "threadId": "thread-known"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/me/messages/"):
			providerID := strings.TrimPrefix(r.URL.Path, "/users/me/messages/")
			messageGetsMu.Lock()
			messageGets[providerID]++
			messageGetsMu.Unlock()
			subject := "Repaired known"
			internetID := "<known@gmail.example>"
			historyID := "201"
			if providerID == "gmail-new" {
				subject = "Repaired new"
				internetID = "<new@gmail.example>"
				historyID = "202"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           providerID,
				"threadId":     "thread-" + providerID,
				"labelIds":     []string{"INBOX"},
				"snippet":      subject,
				"historyId":    historyID,
				"internalDate": "1760000600000",
				"payload": map[string]any{
					"headers": []map[string]string{
						{"name": "Message-ID", "value": internetID},
						{"name": "Subject", "value": subject},
						{"name": "From", "value": "Sender <sender@example.com>"},
						{"name": "Date", "value": "Fri, 26 Jun 2026 12:10:00 +0000"},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "250"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	orchestrator := NewSyncOrchestrator(db, accountStore, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), labelSyncTestTokens{})
	if err := orchestrator.repairGmailAPIAccount(ctx, "acc"); err != nil {
		t.Fatalf("repairGmailAPIAccount() error = %v", err)
	}
	messageGetsMu.Lock()
	defer messageGetsMu.Unlock()
	if messageGets["gmail-known"] != 1 || messageGets["gmail-new"] != 1 {
		t.Fatalf("message metadata gets = %#v, want known and new fetched during repair", messageGets)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<new@gmail.example>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(new) = %d, %v", msgID, err)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "250" || !state.LastFullSyncAt.Valid || !state.LastSuccessAt.Valid {
		t.Fatalf("sync state = %#v, want full repair cursor 250", state)
	}
}

func TestSyncGmailAPIHistoricalImportFetchesMetadataInParallel(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	var activeMetadata int32
	var maxMetadata int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]any{
				{"id": "INBOX", "name": "INBOX", "type": "system"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{
				{"id": "gmail-parallel-1", "threadId": "thread-1"},
				{"id": "gmail-parallel-2", "threadId": "thread-2"},
				{"id": "gmail-parallel-3", "threadId": "thread-3"},
				{"id": "gmail-parallel-4", "threadId": "thread-4"},
				{"id": "gmail-parallel-5", "threadId": "thread-5"},
				{"id": "gmail-parallel-6", "threadId": "thread-6"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/me/messages/gmail-parallel-"):
			current := atomic.AddInt32(&activeMetadata, 1)
			for {
				previous := atomic.LoadInt32(&maxMetadata)
				if current <= previous || atomic.CompareAndSwapInt32(&maxMetadata, previous, current) {
					break
				}
			}
			time.Sleep(75 * time.Millisecond)
			atomic.AddInt32(&activeMetadata, -1)

			providerID := strings.TrimPrefix(r.URL.Path, "/users/me/messages/")
			sequence := strings.TrimPrefix(providerID, "gmail-parallel-")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           providerID,
				"threadId":     "thread-" + sequence,
				"labelIds":     []string{"INBOX"},
				"snippet":      "Parallel import " + sequence,
				"historyId":    "30" + sequence,
				"internalDate": "1760000600000",
				"payload": map[string]any{
					"headers": []map[string]string{
						{"name": "Message-ID", "value": "<parallel-" + sequence + "@gmail.example>"},
						{"name": "Subject", "value": "Parallel import " + sequence},
						{"name": "From", "value": "Sender <sender@example.com>"},
						{"name": "Date", "value": "Fri, 26 Jun 2026 12:10:00 +0000"},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "350"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), labelSyncTestTokens{})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v", err)
	}
	if max := atomic.LoadInt32(&maxMetadata); max < 2 {
		t.Fatalf("max concurrent metadata fetches = %d, want parallel fetches", max)
	}
}

func TestSyncGmailAPIAccountUsesHistoryAfterLiveCursorWithoutFullBaseline(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	baseline := mustParseGmailAPITestTime(t)
	if _, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "gmail-msg-1",
		InternetMessageID: "<known@gmail.example>",
		Subject:           "Known Gmail message",
		FromEmail:         "sender@example.com",
		DateSent:          baseline,
		DateReceived:      baseline,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID:      "acc",
		ProviderType:   storage.LabelProviderGmail,
		Scope:          "messages",
		StartedAt:      baseline.Add(-time.Minute),
		FinishedAt:     baseline,
		Full:           false,
		Cursor:         "105",
		TotalMessages:  1,
		SyncedMessages: 1,
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	var sawHistory bool
	recentListRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]any{
				{"id": "INBOX", "name": "INBOX", "type": "system"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
			recentListRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{{"id": "gmail-msg-1", "threadId": "thread-1"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/history":
			if got := r.URL.Query().Get("startHistoryId"); got != "105" {
				t.Fatalf("startHistoryId = %q, want baseline cursor 105", got)
			}
			sawHistory = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"historyId": "110",
				"history": []map[string]any{{
					"labelsAdded": []map[string]any{{
						"message":  map[string]string{"id": "gmail-msg-2"},
						"labelIds": []string{"INBOX"},
					}},
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-msg-2":
			if got := r.URL.Query().Get("format"); got != "metadata" {
				t.Fatalf("message format = %q, want metadata for history delta sync", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "gmail-msg-2",
				"threadId":     "thread-2",
				"labelIds":     []string{"INBOX"},
				"snippet":      "Scheduled delta",
				"historyId":    "111",
				"internalDate": "1760000000000",
				"payload": map[string]any{
					"headers": []map[string]string{
						{"name": "Message-ID", "value": "<gmail-history@example.com>"},
						{"name": "Subject", "value": "Gmail history"},
						{"name": "From", "value": "Sender <sender@example.com>"},
						{"name": "Date", "value": "Fri, 26 Jun 2026 12:00:00 +0000"},
					},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages":
			t.Fatalf("unexpected Gmail message list request: %s", r.URL.String())
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), labelSyncTestTokens{})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v", err)
	}
	if !sawHistory {
		t.Fatal("Gmail history API was not used")
	}
	if recentListRequests != 1 {
		t.Fatalf("recent list requests = %d, want one bounded catch-up before history", recentListRequests)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<gmail-history@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "111" || state.LastSyncedMessages != 1 {
		t.Fatalf("sync state = %#v, want history cursor 111 with one synced message", state)
	}
	if db.IsBodyFetched(ctx, msgID) {
		t.Fatal("Gmail history sync fetched the body; want metadata-only import before lazy open")
	}
}

func mustParseGmailAPITestTime(t *testing.T) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, "2026-06-26T12:00:00Z")
	if err != nil {
		t.Fatalf("parse test time: %v", err)
	}
	return parsed
}
