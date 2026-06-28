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

	"github.com/cristianadrielbraun/gofer/internal/models"
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

func (labelSyncTestTokens) GetMicrosoftLegacyOutlookMailTokenForAccount(context.Context, string) (string, error) {
	return "legacy-outlook-token", nil
}

type refreshingLabelSyncTestTokens struct {
	initial   string
	refreshed string
	refreshes *int
}

func (t refreshingLabelSyncTestTokens) GetOAuthTokenForAccount(context.Context, string) (string, error) {
	if strings.TrimSpace(t.initial) != "" {
		return t.initial, nil
	}
	return "gmail-token", nil
}

func (t refreshingLabelSyncTestTokens) RefreshOAuthTokenForAccount(context.Context, string) (string, error) {
	if t.refreshes != nil {
		(*t.refreshes)++
	}
	if strings.TrimSpace(t.refreshed) != "" {
		return t.refreshed, nil
	}
	return "gmail-token-refreshed", nil
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
	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, provider); err != nil {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gmail-msg-1", "historyId": "101", "labelIds": []string{"INBOX", "Label_1"}})
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
	state, err := db.GetLabelSyncState(context.Background(), "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.LastTotalMessages != 1 || state.LastSyncedMessages != 1 || state.LastWithLabels != 1 || state.LastWithoutLabels != 0 || state.LastFailedMessages != 0 {
		t.Fatalf("sync stats = %#v, want one synced labeled Gmail message", state)
	}
	if state.Cursor != "105" {
		t.Fatalf("sync cursor = %q, want profile history cursor", state.Cursor)
	}
}

func TestSyncGmailLabelsMirrorsInboxSystemLabelFromImportantMailbox(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'gmail', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "acc_important", AccountID: "acc", RemoteID: "[Gmail]/Important", Name: "Important", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []storage.SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_important",
		RemoteUID: 44,
		MessageID: "<important-inbox@example.com>",
		Subject:   "Important arrival",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    false,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages() error = %v", err)
	}
	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<important-inbox@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	if err := db.SetMessageProviderMessageID(ctx, msgID, "gmail-important-1"); err != nil {
		t.Fatalf("SetMessageProviderMessageID() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{
				{"id": "INBOX", "name": "INBOX", "type": "system"},
				{"id": "IMPORTANT", "name": "IMPORTANT", "type": "system"},
				{"id": "UNREAD", "name": "UNREAD", "type": "system"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-important-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gmail-important-1", "historyId": "201", "labelIds": []string{"INBOX", "IMPORTANT", "UNREAD"}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "205"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncGmailLabels(ctx, "acc"); err != nil {
		t.Fatalf("syncGmailLabels() error = %v", err)
	}

	page, err := db.GetEmailsRangeFilteredForUser(ctx, "default", "inbox", 0, 50, models.EmailFilters{})
	if err != nil {
		t.Fatalf("GetEmailsRangeFilteredForUser(inbox) error = %v", err)
	}
	if page.TotalCount != 1 || len(page.Emails) != 1 || page.Emails[0].Subject != "Important arrival" {
		t.Fatalf("inbox page total=%d emails=%#v, want Important arrival", page.TotalCount, page.Emails)
	}
	if page.Emails[0].IsRead {
		t.Fatalf("important inbox email IsRead = true, want unread from Gmail UNREAD label")
	}

	var inboxRows, nullUIDs int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*), SUM(CASE WHEN remote_uid IS NULL THEN 1 ELSE 0 END) FROM message_folder_state WHERE message_id = ? AND folder_id = 'acc_inbox'`, msgID).Scan(&inboxRows, &nullUIDs); err != nil {
		t.Fatalf("query inbox state: %v", err)
	}
	if inboxRows != 1 || nullUIDs != 1 {
		t.Fatalf("inbox state rows=%d nullUIDs=%d, want one synthetic inbox row", inboxRows, nullUIDs)
	}
}

func TestSyncGmailLabelChangesUsesHistoryCursor(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	msgID := seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-history@example.com>", "gmail-msg-history")
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID:    "acc",
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		Cursor:       "10",
		Full:         true,
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	historyRequests := 0
	messageRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{
				{"id": "Label_1", "name": "Projects", "type": "user"},
				{"id": "INBOX", "name": "INBOX", "type": "system"},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/history":
			historyRequests++
			if got := r.URL.Query().Get("startHistoryId"); got != "10" {
				t.Fatalf("startHistoryId = %q, want 10", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"historyId": "12",
				"history": []map[string]any{{
					"labelsAdded": []map[string]any{{
						"message":  map[string]string{"id": "gmail-msg-history"},
						"labelIds": []string{"Label_1"},
					}},
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages/gmail-msg-history":
			messageRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "gmail-msg-history",
				"historyId": "12",
				"labelIds":  []string{"INBOX", "Label_1"},
				"payload": map[string]any{"headers": []map[string]string{
					{"name": "Message-ID", "value": "<gmail-history@example.com>"},
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

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncGmailLabelChanges(ctx, "acc"); err != nil {
		t.Fatalf("syncGmailLabelChanges() error = %v", err)
	}
	if historyRequests != 1 || messageRequests != 1 {
		t.Fatalf("requests history=%d message=%d, want 1 and 1", historyRequests, messageRequests)
	}

	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Projects" || email.Labels[0].ProviderID != "Label_1" {
		t.Fatalf("labels = %#v, want Projects from history delta", email.Labels)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "12" || state.LastTotalMessages != 1 || state.LastSyncedMessages != 1 || state.LastWithLabels != 1 {
		t.Fatalf("sync state = %#v, want cursor 12 and one synced labeled message", state)
	}
}

func TestSyncGmailLabelChangesSkipsCatalogWhenHistoryIsEmpty(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'gmail', 'user@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID:    "acc",
		ProviderType: storage.LabelProviderGmail,
		Scope:        "messages",
		Cursor:       "20",
		Full:         true,
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}

	historyRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/history":
			historyRequests++
			if got := r.URL.Query().Get("startHistoryId"); got != "20" {
				t.Fatalf("startHistoryId = %q, want 20", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "21"})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncGmailLabelChanges(ctx, "acc"); err != nil {
		t.Fatalf("syncGmailLabelChanges() error = %v", err)
	}
	if historyRequests != 1 {
		t.Fatalf("history requests = %d, want 1", historyRequests)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "21" || state.LastTotalMessages != 0 || state.LastSyncedMessages != 0 {
		t.Fatalf("sync state = %#v, want cursor 21 and no message work", state)
	}
}

func TestSyncProviderLabelsStopsGmailMessageLoopOnAuthFailure(t *testing.T) {
	db := newLabelSyncTestDB(t)
	seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-label-1@example.com>", "gmail-msg-1")
	seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-label-2@example.com>", "gmail-msg-2")
	seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-label-3@example.com>", "gmail-msg-3")

	messageRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{
				{"id": "Label_1", "name": "Projects", "type": "user"},
			}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/me/messages/"):
			messageRequests++
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    401,
					"message": "Invalid Credentials",
					"status":  "UNAUTHENTICATED",
				},
			})
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	err := orchestrator.syncProviderLabels(context.Background(), "acc", providers.ProviderGmail)
	if err == nil {
		t.Fatalf("syncProviderLabels() error = nil, want auth failure")
	}
	if !providerLabelSyncShouldStop(err) {
		t.Fatalf("providerLabelSyncShouldStop(%v) = false, want true", err)
	}
	if !strings.Contains(err.Error(), "provider api returned 401") {
		t.Fatalf("error = %v, want provider 401", err)
	}
	if messageRequests != 1 {
		t.Fatalf("message requests = %d, want 1", messageRequests)
	}

	state, err := db.GetLabelSyncState(context.Background(), "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if !strings.Contains(state.LastError, "provider api returned 401") {
		t.Fatalf("label sync error = %q, want provider 401", state.LastError)
	}
}

func TestSyncGmailLabelsStopsOnContextCancellation(t *testing.T) {
	db := newLabelSyncTestDB(t)
	seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-cancel-1@example.com>", "gmail-msg-1")
	seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-cancel-2@example.com>", "gmail-msg-2")
	seedLabelSyncMessage(t, db, providers.ProviderGmail, "<gmail-cancel-3@example.com>", "gmail-msg-3")

	ctx, cancel := context.WithCancel(context.Background())
	messageRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{
				{"id": "Label_1", "name": "Projects", "type": "user"},
			}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/me/messages/"):
			messageRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "gmail-msg-1",
				"historyId": "101",
				"labelIds":  []string{"Label_1"},
			})
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			cancel()
		default:
			t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	err := orchestrator.syncGmailLabels(ctx, "acc")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("syncGmailLabels() error = %v, want context canceled", err)
	}
	if messageRequests != 1 {
		t.Fatalf("message requests = %d, want cancellation after first request", messageRequests)
	}
}

func TestSyncOutlookCategoriesImportsRemoteMessageCategories(t *testing.T) {
	db := newLabelSyncTestDB(t)
	msgID := seedLabelSyncMessage(t, db, providers.ProviderOutlook, "<outlook-label@example.com>", "outlook-msg-1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/outlook/masterCategories":
			if got := r.Header.Get("Prefer"); !strings.Contains(got, "ImmutableId") {
				t.Fatalf("Prefer = %q, want immutable ids", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{
				{"displayName": "Invoices", "color": "preset7"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/$batch":
			var payload struct {
				Requests []providerBatchRequest `json:"requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode batch payload: %v", err)
			}
			if len(payload.Requests) != 1 {
				t.Fatalf("batch requests = %#v, want one request", payload.Requests)
			}
			if payload.Requests[0].Method != http.MethodGet || payload.Requests[0].URL != "/me/messages/outlook-msg-1?$select=id,categories" {
				t.Fatalf("batch request = %#v, want message category lookup", payload.Requests[0])
			}
			if got := payload.Requests[0].Headers["Prefer"]; !strings.Contains(got, "ImmutableId") {
				t.Fatalf("batch Prefer = %q, want immutable ids", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"responses": []map[string]any{
				{"id": payload.Requests[0].ID, "status": http.StatusOK, "body": map[string]any{"id": "outlook-msg-1", "categories": []string{"Invoices"}}},
			}})
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

func TestSyncOutlookCategoriesBatchesInternetMessageLookupsAndSkipsMissing(t *testing.T) {
	db := newLabelSyncTestDB(t)
	foundID := seedLabelSyncMessage(t, db, providers.ProviderOutlook, "<outlook-found@example.com>", "")
	missingID := seedLabelSyncMessage(t, db, providers.ProviderOutlook, "<outlook-missing@example.com>", "")
	syntheticID := seedLabelSyncMessage(t, db, providers.ProviderOutlook, "<acc_inbox-42@sync.gofer>", "")
	batchCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/outlook/masterCategories":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{
				{"displayName": "Projects", "color": "preset7"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/$batch":
			batchCalls++
			var payload struct {
				Requests []providerBatchRequest `json:"requests"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode batch payload: %v", err)
			}
			if len(payload.Requests) != 2 {
				t.Fatalf("batch requests = %#v, want found and missing lookups only", payload.Requests)
			}
			responses := make([]map[string]any, 0, len(payload.Requests))
			for _, req := range payload.Requests {
				switch {
				case strings.Contains(req.URL, "outlook-found%40example.com"):
					responses = append(responses, map[string]any{
						"id":     req.ID,
						"status": http.StatusOK,
						"body": map[string]any{"value": []map[string]any{
							{"id": "outlook-found-provider-id", "categories": []string{"Projects"}},
						}},
					})
				case strings.Contains(req.URL, "outlook-missing%40example.com"):
					responses = append(responses, map[string]any{
						"id":     req.ID,
						"status": http.StatusOK,
						"body":   map[string]any{"value": []map[string]any{}},
					})
				default:
					t.Fatalf("unexpected batch lookup URL %q", req.URL)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"responses": responses})
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
	if batchCalls != 1 {
		t.Fatalf("batch calls = %d, want 1", batchCalls)
	}

	found, err := db.GetEmailByID(context.Background(), strconv.FormatInt(foundID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID(found) error = %v", err)
	}
	if len(found.Labels) != 1 || found.Labels[0].Name != "Projects" {
		t.Fatalf("found labels = %#v, want Projects", found.Labels)
	}
	missing, err := db.GetEmailByID(context.Background(), strconv.FormatInt(missingID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID(missing) error = %v", err)
	}
	if len(missing.Labels) != 0 {
		t.Fatalf("missing labels = %#v, want none", missing.Labels)
	}
	synthetic, err := db.GetEmailByID(context.Background(), strconv.FormatInt(syntheticID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID(synthetic) error = %v", err)
	}
	if len(synthetic.Labels) != 0 {
		t.Fatalf("synthetic labels = %#v, want none", synthetic.Labels)
	}
	state, err := db.GetLabelSyncState(context.Background(), "acc", storage.LabelProviderOutlook, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.LastTotalMessages != 3 ||
		state.LastSyncedMessages != 1 ||
		state.LastWithLabels != 1 ||
		state.LastWithoutLabels != 0 ||
		state.LastMissingProviderMessages != 1 ||
		state.LastSkippedMessages != 1 ||
		state.LastFailedMessages != 0 {
		t.Fatalf("sync stats = %#v, want found/missing/skipped Outlook batch counts", state)
	}
}

func TestSyncProviderLabelsRecordsOutlookGraphDeniedWithoutFailingAccountSync(t *testing.T) {
	db := newLabelSyncTestDB(t)
	seedLabelSyncMessage(t, db, providers.ProviderOutlook, "<outlook-denied@example.com>", "outlook-msg-denied")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if r.Method != http.MethodGet || r.URL.Path != "/me/outlook/masterCategories" {
			t.Fatalf("unexpected Outlook request %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "ErrorAccessDenied",
				"message": "Access is denied. Check credentials and try again.",
			},
		})
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncProviderLabels(context.Background(), "acc", providers.ProviderOutlook); err != nil {
		t.Fatalf("syncProviderLabels() error = %v, want nil", err)
	}

	state, err := db.GetLabelSyncState(context.Background(), "acc", storage.LabelProviderOutlook, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if !strings.Contains(state.LastError, "provider api returned 403") {
		t.Fatalf("label sync error = %q, want provider 403", state.LastError)
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
