package mail

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

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestShouldUseOutlookGraphMailDefaultsOnAndCanBeDisabled(t *testing.T) {
	orchestrator := NewSyncOrchestrator(nil, nil, nil, labelSyncTestTokens{})
	cfg := &models.AccountConfig{Provider: providers.ProviderOutlook}

	t.Setenv("GOFER_OUTLOOK_GRAPH_SYNC", "")
	if !orchestrator.shouldUseOutlookGraphMail(cfg) {
		t.Fatal("shouldUseOutlookGraphMail() = false by default")
	}

	t.Setenv("GOFER_OUTLOOK_GRAPH_SYNC", "0")
	if orchestrator.shouldUseOutlookGraphMail(cfg) {
		t.Fatal("shouldUseOutlookGraphMail() = true when disabled")
	}
}

func TestSyncOutlookGraphFoldersImportsNestedFolders(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	var sawChildFolders bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id":               "folder-projects",
				"displayName":      "Projects",
				"childFolderCount": 1,
				"totalItemCount":   2,
				"unreadItemCount":  1,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-projects/childFolders":
			sawChildFolders = true
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id":               "folder-client-a",
				"displayName":      "Client A",
				"parentFolderId":   "folder-projects",
				"childFolderCount": 0,
				"totalItemCount":   1,
				"unreadItemCount":  0,
			}}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	targets, err := orchestrator.syncOutlookGraphFolders(ctx, "acc", "graph-token")
	if err != nil {
		t.Fatalf("syncOutlookGraphFolders() error = %v", err)
	}
	if !sawChildFolders {
		t.Fatal("child folder endpoint was not observed")
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	byProviderID := map[string]storage.FolderSyncInfo{}
	for _, folder := range folders {
		byProviderID[folder.ProviderRemoteID] = folder
	}
	parent := byProviderID["folder-projects"]
	child := byProviderID["folder-client-a"]
	if parent.ID == "" || parent.RemoteID != "Projects" {
		t.Fatalf("parent folder = %#v, want Projects graph folder", parent)
	}
	var childParentID string
	if child.ID != "" {
		if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(parent_id, '') FROM folders WHERE id = ?`, child.ID).Scan(&childParentID); err != nil {
			t.Fatalf("query child parent_id: %v", err)
		}
	}
	if child.ID == "" || child.RemoteID != "Projects/Client A" || childParentID != parent.ID {
		t.Fatalf("child folder = %#v parent_id=%q parent=%#v, want nested Graph folder", child, childParentID, parent)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %#v, want parent and child targets", targets)
	}
}

func TestSyncOutlookGraphAccountDoesNotFailOnCategoryCatalogError(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id":               "folder-inbox",
				"displayName":      "Inbox",
				"childFolderCount": 0,
				"totalItemCount":   0,
				"unreadItemCount":  0,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/outlook/masterCategories":
			http.Error(w, "category service unavailable", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value":            []map[string]any{},
				"@odata.deltaLink": "https://graph.test/delta/inbox",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncOutlookGraphAccount() error = %v, want category catalog failure ignored", err)
	}
}

func TestSyncOutlookGraphAccountReturnsFolderSyncError(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id":               "folder-inbox",
				"displayName":      "Inbox",
				"childFolderCount": 0,
				"totalItemCount":   1,
				"unreadItemCount":  0,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/outlook/masterCategories":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			http.Error(w, "delta failed", http.StatusInternalServerError)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	err := orchestrator.syncOutlookGraphAccount(ctx, "acc", true)
	if err == nil || !strings.Contains(err.Error(), "Outlook Graph folder sync") {
		t.Fatalf("syncOutlookGraphAccount() error = %v, want folder sync failure", err)
	}
	var syncErr string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(sync_error, '') FROM folders WHERE provider_remote_id = 'folder-inbox'`).Scan(&syncErr); err != nil {
		t.Fatalf("query folder sync_error: %v", err)
	}
	if !strings.Contains(syncErr, "provider api returned 500") {
		t.Fatalf("sync_error = %q, want provider 500", syncErr)
	}
}

func TestSyncOutlookGraphAccountImportsFoldersMessagesAndDeltaCursor(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "acc_inbox", AccountID: "acc", RemoteID: "Inbox", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatalf("UpsertFolders(legacy) error = %v", err)
	}
	if err := db.UpsertSyncMessages(ctx, []storage.SyncMessage{{
		AccountID: "acc",
		FolderID:  "acc_inbox",
		RemoteUID: 42,
		MessageID: "<legacy@example.com>",
		Subject:   "Legacy",
		FromEmail: "sender@example.com",
		DateSent:  time.Now(),
		IsRead:    true,
	}}); err != nil {
		t.Fatalf("UpsertSyncMessages(legacy) error = %v", err)
	}

	var sawDeltaPrefer bool
	var sawBackfillLookup bool
	deltaLink := "https://graph.test/delta/messages"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer graph-token" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id":               "folder-inbox",
				"displayName":      "Inbox",
				"childFolderCount": 0,
				"totalItemCount":   1,
				"unreadItemCount":  0,
			}}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/me/mailFolders/") && !strings.HasSuffix(r.URL.Path, "/messages/delta"):
			if r.URL.Path == "/me/mailFolders/inbox" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":               "folder-inbox",
					"displayName":      "Inbox",
					"childFolderCount": 0,
					"totalItemCount":   1,
					"unreadItemCount":  0,
				})
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/me/outlook/masterCategories":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"displayName": "Projects", "color": "preset0"}}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			sawDeltaPrefer = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-message-1",
					"internetMessageId": "<graph@example.com>",
					"conversationId":    "conversation-1",
					"parentFolderId":    "folder-inbox",
					"subject":           "Graph subject",
					"bodyPreview":       "Graph preview",
					"body":              map[string]string{"contentType": "html", "content": `<p>Hello <b>Graph</b><img src="cid:logo@example.com"></p>`},
					"categories":        []string{"Projects"},
					"from":              map[string]any{"emailAddress": map[string]string{"name": "Sender", "address": "sender@example.com"}},
					"toRecipients":      []map[string]any{{"emailAddress": map[string]string{"name": "Recipient", "address": "recipient@example.com"}}},
					"ccRecipients":      []map[string]any{{"emailAddress": map[string]string{"name": "Carbon", "address": "carbon@example.com"}}},
					"bccRecipients":     []map[string]any{{"emailAddress": map[string]string{"name": "Hidden", "address": "hidden@example.com"}}},
					"receivedDateTime":  "2026-06-22T10:00:00Z",
					"sentDateTime":      "2026-06-22T09:59:00Z",
					"isRead":            true,
					"isDraft":           false,
					"hasAttachments":    true,
					"flag":              map[string]string{"flagStatus": "flagged"},
					"internetMessageHeaders": []map[string]string{
						{"name": "References", "value": "<root@example.com>"},
					},
				}},
				"@odata.deltaLink": deltaLink,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1/attachments":
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id":          "graph-attachment-1",
				"name":        "logo.png",
				"contentType": "image/png",
				"size":        1234,
				"isInline":    true,
				"contentId":   "<logo@example.com>",
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages":
			filter := r.URL.Query().Get("$filter")
			if !strings.Contains(filter, "internetMessageId eq '<legacy@example.com>'") {
				t.Fatalf("$filter = %q, want legacy backfill lookup", filter)
			}
			sawBackfillLookup = true
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{{"id": "graph-legacy-1"}}})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	blobs := store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	orchestrator := NewSyncOrchestrator(db, nil, blobs, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncOutlookGraphAccount() error = %v", err)
	}
	if !sawDeltaPrefer {
		t.Fatal("delta request was not observed")
	}
	if !sawBackfillLookup {
		t.Fatal("Graph ID backfill lookup was not observed")
	}

	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	if len(folders) != 1 || folders[0].RemoteID != "Inbox" || folders[0].ProviderRemoteID != "folder-inbox" || folders[0].SyncCursor != deltaLink {
		t.Fatalf("folders = %#v, want Inbox with Graph provider id and delta cursor", folders)
	}

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<graph@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	var providerID string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(remote_message_id, '') FROM messages WHERE id = ?`, msgID).Scan(&providerID); err != nil {
		t.Fatalf("query remote_message_id: %v", err)
	}
	if providerID != "graph-message-1" {
		t.Fatalf("remote_message_id = %q, want graph-message-1", providerID)
	}
	legacyID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<legacy@example.com>")
	if err != nil || legacyID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID(legacy) = %d, %v", legacyID, err)
	}
	var legacyProviderID string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(remote_message_id, '') FROM messages WHERE id = ?`, legacyID).Scan(&legacyProviderID); err != nil {
		t.Fatalf("query legacy remote_message_id: %v", err)
	}
	if legacyProviderID != "graph-legacy-1" {
		t.Fatalf("legacy remote_message_id = %q, want graph-legacy-1", legacyProviderID)
	}
	if !db.IsBodyFetched(ctx, msgID) {
		t.Fatal("Graph body was not persisted")
	}
	body, err := db.GetEmailBody(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailBody() error = %v", err)
	}
	if strings.Contains(string(body), "cid:logo@example.com") || !strings.Contains(string(body), "/api/inline-content/") {
		t.Fatalf("stored body = %q, want cid reference rewritten to inline route", string(body))
	}

	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if email.Subject != "Graph subject" || !email.IsRead || !email.IsStarred || email.From.Email != "sender@example.com" {
		t.Fatalf("email = %#v, want Graph subject/read/starred/from", email)
	}
	if len(email.To) != 1 || email.To[0].Email != "recipient@example.com" || len(email.CC) != 1 || email.CC[0].Email != "carbon@example.com" || len(email.BCC) != 1 || email.BCC[0].Email != "hidden@example.com" {
		t.Fatalf("recipients to=%#v cc=%#v bcc=%#v, want Graph to/cc/bcc", email.To, email.CC, email.BCC)
	}
	if len(email.Labels) != 1 || email.Labels[0].Name != "Projects" || email.Labels[0].ProviderType != storage.LabelProviderOutlook {
		t.Fatalf("labels = %#v, want Projects Outlook category", email.Labels)
	}
	if len(email.Attachments) != 1 || email.Attachments[0].Filename != "logo.png" || !email.Attachments[0].Inline || email.Attachments[0].ContentID != "logo@example.com" {
		t.Fatalf("attachments = %#v, want Graph attachment metadata", email.Attachments)
	}
	var attachmentProviderID string
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COALESCE(provider_remote_id, '') FROM attachments WHERE message_id = ?`, msgID,
	).Scan(&attachmentProviderID); err != nil {
		t.Fatalf("query attachment provider id: %v", err)
	}
	if attachmentProviderID != "graph-attachment-1" {
		t.Fatalf("attachment provider_remote_id = %q, want graph-attachment-1", attachmentProviderID)
	}
}

func TestSyncOutlookGraphAttachmentMetadataClearsStaleRowsWhenGraphHasNoAttachments(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "acc_inbox", AccountID: "acc", RemoteID: "Inbox", ProviderRemoteID: "folder-inbox", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	ids, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<stale-attachment@example.com>",
		Subject:           "Stale attachment",
		FromEmail:         "sender@example.com",
		DateSent:          time.Now(),
		DateReceived:      time.Now(),
		IsRead:            true,
		HasAttachments:    true,
	}})
	if err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
	msgID := ids["graph-message-1"]
	if msgID == 0 {
		t.Fatal("provider message was not inserted")
	}
	if err := db.ReplaceAttachments(ctx, msgID, []storage.AttachmentRow{{
		Filename:         "old.pdf",
		ContentType:      "application/pdf",
		SizeBytes:        128,
		ProviderRemoteID: "old-attachment",
	}}); err != nil {
		t.Fatalf("ReplaceAttachments() error = %v", err)
	}

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	orchestrator.syncOutlookGraphAttachmentMetadata(ctx, "graph-token", map[string]int64{"graph-message-1": msgID}, []outlookGraphMessage{{
		ID:             "graph-message-1",
		HasAttachments: false,
	}})

	var attachmentCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE message_id = ?`, msgID).Scan(&attachmentCount); err != nil {
		t.Fatalf("query attachment count: %v", err)
	}
	if attachmentCount != 0 {
		t.Fatalf("attachment count = %d, want stale rows cleared", attachmentCount)
	}
	var hasAttachments int
	if err := db.Read().QueryRowContext(ctx, `SELECT has_attachments FROM messages WHERE id = ?`, msgID).Scan(&hasAttachments); err != nil {
		t.Fatalf("query has_attachments: %v", err)
	}
	if hasAttachments != 0 {
		t.Fatalf("has_attachments = %d, want 0", hasAttachments)
	}
}
