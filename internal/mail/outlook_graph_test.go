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

func TestShouldUseOutlookGraphMailAlwaysUsesGraphForOutlook(t *testing.T) {
	orchestrator := NewSyncOrchestrator(nil, nil, nil, nil)
	cfg := &models.AccountConfig{Provider: providers.ProviderOutlook}

	if !orchestrator.shouldUseOutlookGraphMail(cfg) {
		t.Fatal("shouldUseOutlookGraphMail() = false for Outlook")
	}

	t.Setenv("GOFER_OUTLOOK_GRAPH_SYNC", "0")
	if !orchestrator.shouldUseOutlookGraphMail(cfg) {
		t.Fatal("shouldUseOutlookGraphMail() = false when legacy opt-out env is set")
	}
	if orchestrator.shouldUseOutlookGraphMail(&models.AccountConfig{Provider: providers.ProviderIMAP}) {
		t.Fatal("shouldUseOutlookGraphMail() = true for IMAP provider")
	}
}

func TestOutlookGraphMessagesDeltaEndpointUsesPreferHeaderPagingOnly(t *testing.T) {
	endpoint := outlookGraphMessagesDeltaEndpoint("folder-inbox", false)
	if strings.Contains(endpoint, "$top=") || strings.Contains(endpoint, "%24top=") {
		t.Fatalf("delta endpoint = %q, must not use $top because it truncates Outlook baseline sync", endpoint)
	}
	if !strings.Contains(endpoint, "$select=") && !strings.Contains(endpoint, "%24select=") {
		t.Fatalf("delta endpoint = %q, want $select query", endpoint)
	}
	headers := outlookGraphHeaders(outlookGraphMessagePageSize)
	if !strings.Contains(headers["Prefer"], "odata.maxpagesize=50") {
		t.Fatalf("Prefer = %q, want odata.maxpagesize=50", headers["Prefer"])
	}
}

func TestOutlookGraphMessageToProviderSyncFallsBackToInternetHeaders(t *testing.T) {
	msg := outlookGraphMessage{
		ID:                "graph-message-1",
		InternetMessageID: "<message@example.com>",
		ConversationID:    "conversation-1",
		ReceivedDateTime:  time.Now(),
		SentDateTime:      time.Now(),
		InternetMessageHeaders: []outlookGraphHeader{
			{Name: "Subject", Value: "=?UTF-8?Q?Loaded_subject?="},
			{Name: "From", Value: "Loaded Sender <sender@example.com>"},
			{Name: "To", Value: "Loaded Recipient <recipient@example.com>"},
			{Name: "Cc", Value: "Loaded Carbon <carbon@example.com>"},
		},
	}

	syncMessage := outlookGraphMessageToProviderSync("acc", "acc_inbox", msg, nil)
	if syncMessage.Subject != "Loaded subject" || syncMessage.FromName != "Loaded Sender" || syncMessage.FromEmail != "sender@example.com" {
		t.Fatalf("sync message = %#v, want subject/from from internet headers", syncMessage)
	}
	if len(syncMessage.ToRecipients) != 1 || syncMessage.ToRecipients[0].Email != "recipient@example.com" || len(syncMessage.CCRecipients) != 1 || syncMessage.CCRecipients[0].Email != "carbon@example.com" {
		t.Fatalf("sync recipients to=%#v cc=%#v, want recipients from internet headers", syncMessage.ToRecipients, syncMessage.CCRecipients)
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
	var parentTotal, parentUnread int
	if err := db.Read().QueryRowContext(ctx, `SELECT total_count, unread_count FROM folders WHERE id = ?`, parent.ID).Scan(&parentTotal, &parentUnread); err != nil {
		t.Fatalf("query parent counts: %v", err)
	}
	if parentTotal != 2 || parentUnread != 1 {
		t.Fatalf("parent counts total=%d unread=%d, want Graph counts 2/1", parentTotal, parentUnread)
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
			if r.URL.Query().Get("$top") != "" {
				t.Fatalf("full baseline URL = %q, must not use $top", r.URL.String())
			}
			if strings.Contains(","+r.URL.Query().Get("$select")+",", ",body,") {
				t.Fatalf("full baseline $select = %q, must not prefetch message bodies", r.URL.Query().Get("$select"))
			}
			sawDeltaPrefer = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-message-1",
					"internetMessageId": "<graph-message-1@example.com>",
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
			t.Fatalf("full baseline requested attachment metadata")
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			if strings.Contains(r.URL.Query().Get("$select"), "contentId") {
				t.Fatalf("attachment base $select = %q, must not include fileAttachment-only contentId", r.URL.Query().Get("$select"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"@odata.type": "#microsoft.graph.fileAttachment",
				"id":          "graph-attachment-1",
				"name":        "logo.png",
				"contentType": "image/png",
				"size":        1234,
				"isInline":    true,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1/attachments/graph-attachment-1":
			t.Fatalf("full baseline requested attachment detail")
			if got := r.URL.Query().Get("$select"); got != "id,contentId" {
				t.Fatalf("attachment detail $select = %q, want id,contentId", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "graph-attachment-1",
				"contentId": "<logo@example.com>",
			})
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

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<graph-message-1@example.com>")
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
	if db.IsBodyFetched(ctx, msgID) {
		t.Fatal("full baseline prefetch persisted Graph body")
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
	if !email.HasAttachment || len(email.Attachments) != 0 {
		t.Fatalf("attachments has=%v rows=%#v, want attachment flag without full-baseline metadata prefetch", email.HasAttachment, email.Attachments)
	}
	var attachmentCount int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachments WHERE message_id = ?`, msgID).Scan(&attachmentCount); err != nil {
		t.Fatalf("query attachment count: %v", err)
	}
	if attachmentCount != 0 {
		t.Fatalf("attachment rows = %d, want no full-baseline metadata prefetch", attachmentCount)
	}
}

func TestSyncOutlookGraphFolderFullBaselinePrunesMissingProviderMessages(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	seedOutlookGraphProviderMessages(t, ctx, db, "graph-current", "graph-stale")

	deltaLink := "https://graph.test/delta/inbox"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-current",
					"internetMessageId": "<graph-current@example.com>",
					"subject":           "Current",
					"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
				}},
				"@odata.deltaLink": deltaLink,
			})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTarget(""), nil, 1, 1); err != nil {
		t.Fatalf("syncOutlookGraphFolder() error = %v", err)
	}

	if got := queryOutlookGraphDeletedByRemoteID(t, ctx, db, "graph-current"); got != 0 {
		t.Fatalf("graph-current is_deleted = %d, want 0", got)
	}
	if got := queryOutlookGraphDeletedByRemoteID(t, ctx, db, "graph-stale"); got != 1 {
		t.Fatalf("graph-stale is_deleted = %d, want 1 after full baseline reconciliation", got)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != deltaLink {
		t.Fatalf("sync_cursor = %q, want %q", got, deltaLink)
	}
}

func TestSyncOutlookGraphFolderDoesNotPruneOrCheckpointBeforeDeltaLink(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	seedOutlookGraphProviderMessages(t, ctx, db, "graph-current", "graph-stale")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-current",
					"internetMessageId": "<graph-current@example.com>",
					"subject":           "Current",
					"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
				}},
				"@odata.nextLink": server.URL + "/delta/page-2",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/delta/page-2":
			http.Error(w, "delta page failed", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTarget(""), nil, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "provider api returned 500") {
		t.Fatalf("syncOutlookGraphFolder() error = %v, want failed second page", err)
	}
	if got := queryOutlookGraphDeletedByRemoteID(t, ctx, db, "graph-stale"); got != 0 {
		t.Fatalf("graph-stale is_deleted = %d, want no prune before terminal deltaLink", got)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != server.URL+"/delta/page-2" {
		t.Fatalf("sync_cursor = %q, want saved nextLink before terminal deltaLink", got)
	}
}

func TestSyncOutlookGraphFolderFullBaselineSavesNextLinkAndResumes(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)

	failSecondPage := true
	var baselineRequests int
	deltaLink := "https://graph.test/delta/resumed"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta" && r.URL.Query().Get("$skiptoken") == "":
			baselineRequests++
			if baselineRequests > 1 {
				t.Fatalf("full baseline restarted from first page instead of resuming saved nextLink")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-page-1",
					"internetMessageId": "<graph-page-1@example.com>",
					"subject":           "Page 1",
					"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
				}},
				"@odata.nextLink": server.URL + "/me/mailFolders/folder-inbox/messages/delta?$skiptoken=page-2",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta" && r.URL.Query().Get("$skiptoken") == "page-2":
			if failSecondPage {
				http.Error(w, "second page failed", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{
					{
						"id":                "graph-page-2",
						"internetMessageId": "<graph-page-2@example.com>",
						"subject":           "Page 2",
						"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
					},
					{
						"id":                "graph-page-3",
						"internetMessageId": "<graph-page-3@example.com>",
						"subject":           "Page 3",
						"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
					},
				},
				"@odata.deltaLink": deltaLink,
			})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 3 WHERE id = 'acc_inbox'`); err != nil {
		t.Fatalf("seed provider total: %v", err)
	}

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "provider api returned 500") {
		t.Fatalf("first sync error = %v, want second page failure", err)
	}
	nextLink := server.URL + "/me/mailFolders/folder-inbox/messages/delta?$skiptoken=page-2"
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != nextLink {
		t.Fatalf("sync_cursor after interrupted full baseline = %q, want nextLink %q", got, nextLink)
	}
	if got := queryOutlookGraphProviderMessageCount(t, ctx, db); got != 1 {
		t.Fatalf("provider-backed message count after first page = %d, want 1", got)
	}
	if got := queryOutlookGraphVisibleThreadCount(t, ctx, db); got != 1 {
		t.Fatalf("visible thread count after first interrupted page = %d, want 1", got)
	}

	failSecondPage = false
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("resumed sync error = %v", err)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != deltaLink {
		t.Fatalf("sync_cursor after resumed full baseline = %q, want delta link %q", got, deltaLink)
	}
	if got := queryOutlookGraphProviderMessageCount(t, ctx, db); got != 3 {
		t.Fatalf("provider-backed message count after resume = %d, want 3", got)
	}
	if got := queryOutlookGraphVisibleThreadCount(t, ctx, db); got != 3 {
		t.Fatalf("visible thread count after resumed full baseline = %d, want 3", got)
	}
}

func TestSyncOutlookGraphFolderOldFullSyncStillUsesDelta(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	seedOutlookGraphProviderMessages(t, ctx, db, "graph-current", "graph-stale")

	deltaLink := "https://graph.test/delta/current-complete"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/delta/current":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value":            []map[string]any{},
				"@odata.deltaLink": deltaLink,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			t.Fatalf("syncOutlookGraphFolder() used full baseline just because last_full_sync_at was old")
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	lastFull := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 2, sync_cursor = ?, last_full_sync_at = ? WHERE id = 'acc_inbox'`, server.URL+"/delta/current", lastFull); err != nil {
		t.Fatalf("set stale folder cursor: %v", err)
	}

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("syncOutlookGraphFolder() error = %v", err)
	}
	if got := queryOutlookGraphDeletedByRemoteID(t, ctx, db, "graph-stale"); got != 0 {
		t.Fatalf("graph-stale is_deleted = %d, want no prune without a full baseline", got)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != deltaLink {
		t.Fatalf("sync_cursor = %q, want delta link %q", got, deltaLink)
	}
}

func TestSyncOutlookGraphFolderCountDriftRequiresSecondCompletedDelta(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	seedOutlookGraphProviderMessages(t, ctx, db, "graph-existing")

	var firstDelta, secondDelta, fullDelta string
	var baselineRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/delta/current":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value":            []map[string]any{},
				"@odata.deltaLink": firstDelta,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/delta/after-first":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value":            []map[string]any{},
				"@odata.deltaLink": secondDelta,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":              "folder-inbox",
				"displayName":     "Inbox",
				"totalItemCount":  2,
				"unreadItemCount": 0,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			baselineRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{
					{
						"id":                "graph-existing",
						"internetMessageId": "<graph-existing@example.com>",
						"subject":           "Existing",
						"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
					},
					{
						"id":                "graph-missing",
						"internetMessageId": "<graph-missing@example.com>",
						"subject":           "Missing",
						"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
					},
				},
				"@odata.deltaLink": fullDelta,
			})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	firstDelta = server.URL + "/delta/after-first"
	secondDelta = server.URL + "/delta/after-second"
	fullDelta = server.URL + "/delta/full-recovered"

	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 2, sync_cursor = ?, last_full_sync_at = CURRENT_TIMESTAMP WHERE id = 'acc_inbox'`, server.URL+"/delta/current"); err != nil {
		t.Fatalf("set drift candidate folder state: %v", err)
	}

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("first sync error = %v", err)
	}
	if baselineRequests != 0 {
		t.Fatalf("baseline requests after first drift observation = %d, want 0", baselineRequests)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != firstDelta {
		t.Fatalf("sync_cursor after first drift observation = %q, want %q", got, firstDelta)
	}
	if got := queryOutlookGraphCountDriftConfirmations(t, ctx, db); got != 1 {
		t.Fatalf("drift confirmations after first sync = %d, want 1", got)
	}
	if got := queryOutlookGraphProviderMessageCount(t, ctx, db); got != 1 {
		t.Fatalf("provider-backed message count after first sync = %d, want still 1", got)
	}

	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("second sync error = %v", err)
	}
	if baselineRequests != 1 {
		t.Fatalf("baseline requests after confirmed drift = %d, want 1", baselineRequests)
	}
	if got := queryOutlookGraphProviderMessageCount(t, ctx, db); got != 2 {
		t.Fatalf("provider-backed message count after repair = %d, want 2", got)
	}
	if got := queryOutlookGraphCountDriftConfirmations(t, ctx, db); got != 0 {
		t.Fatalf("drift confirmations after repair = %d, want cleared", got)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != fullDelta {
		t.Fatalf("sync_cursor after repair = %q, want recovered delta link %q", got, fullDelta)
	}
}

func TestSyncOutlookGraphFolderFullReconcilesMissingSenderMetadata(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	now := time.Now()
	if _, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
		AccountID:         "acc",
		FolderID:          "acc_inbox",
		ProviderMessageID: "graph-message-1",
		InternetMessageID: "<graph-message-1@example.com>",
		DateSent:          now,
		DateReceived:      now,
		IsRead:            true,
	}}); err != nil {
		t.Fatalf("seed incomplete provider message: %v", err)
	}

	deltaLink := "https://graph.test/delta/repaired"
	var baselineRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/delta/current":
			t.Fatalf("incremental cursor should not be used while sender metadata is missing")
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			baselineRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-message-1",
					"internetMessageId": "<graph-message-1@example.com>",
					"internetMessageHeaders": []map[string]string{
						{"name": "Subject", "value": "Recovered subject"},
						{"name": "From", "value": "Recovered Sender <sender@example.com>"},
					},
				}},
				"@odata.deltaLink": deltaLink,
			})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 1, sync_cursor = ?, last_full_sync_at = CURRENT_TIMESTAMP WHERE id = 'acc_inbox'`, server.URL+"/delta/current"); err != nil {
		t.Fatalf("set folder state: %v", err)
	}

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("syncOutlookGraphFolder() error = %v", err)
	}
	if baselineRequests != 1 {
		t.Fatalf("baseline requests = %d, want 1", baselineRequests)
	}

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<graph-message-1@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if email.Subject != "Recovered subject" || email.From.Name != "Recovered Sender" || email.From.Email != "sender@example.com" {
		t.Fatalf("email = %#v, want recovered subject/from after metadata reconcile", email)
	}
}

func TestSyncOutlookGraphFolderHydratesIncompleteDeltaMessage(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)

	deltaLink := "https://graph.test/delta/hydrated"
	var detailRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/delta/current":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":     "graph-message-1",
					"isRead": true,
				}},
				"@odata.deltaLink": deltaLink,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1":
			detailRequests++
			if !strings.Contains(r.URL.Query().Get("$select"), "body") {
				t.Fatalf("detail $select = %q, want body for incremental hydration", r.URL.Query().Get("$select"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                "graph-message-1",
				"internetMessageId": "<graph-message-1@example.com>",
				"conversationId":    "conversation-1",
				"subject":           "Hydrated subject",
				"bodyPreview":       "Hydrated preview",
				"body":              map[string]string{"contentType": "text", "content": "Hydrated body"},
				"from":              map[string]any{"emailAddress": map[string]string{"name": "Hydrated Sender", "address": "sender@example.com"}},
				"toRecipients":      []map[string]any{{"emailAddress": map[string]string{"name": "Hydrated Recipient", "address": "recipient@example.com"}}},
				"receivedDateTime":  "2026-06-22T10:00:00Z",
				"sentDateTime":      "2026-06-22T09:59:00Z",
				"isRead":            true,
			})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 1, sync_cursor = ?, last_full_sync_at = CURRENT_TIMESTAMP WHERE id = 'acc_inbox'`, server.URL+"/delta/current"); err != nil {
		t.Fatalf("set incremental folder state: %v", err)
	}

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("syncOutlookGraphFolder() error = %v", err)
	}
	if detailRequests != 1 {
		t.Fatalf("detail requests = %d, want 1", detailRequests)
	}

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<graph-message-1@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	email, err := db.GetEmailByID(ctx, strconv.FormatInt(msgID, 10))
	if err != nil {
		t.Fatalf("GetEmailByID() error = %v", err)
	}
	if email.Subject != "Hydrated subject" || email.From.Email != "sender@example.com" || len(email.To) != 1 || email.To[0].Email != "recipient@example.com" {
		t.Fatalf("email = %#v, want hydrated metadata from detail fetch", email)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != deltaLink {
		t.Fatalf("sync_cursor = %q, want %q", got, deltaLink)
	}
}

func TestSyncOutlookGraphFolderExpiredCursorRestartsFullAndPrunes(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	seedOutlookGraphProviderMessages(t, ctx, db, "graph-current", "graph-stale")

	deltaLink := "https://graph.test/delta/recovered"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/expired":
			http.Error(w, "cursor expired", http.StatusGone)
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders/folder-inbox/messages/delta":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-current",
					"internetMessageId": "<graph-current@example.com>",
					"subject":           "Current",
					"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
				}},
				"@odata.deltaLink": deltaLink,
			})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET sync_cursor = ?, last_full_sync_at = CURRENT_TIMESTAMP WHERE id = 'acc_inbox'`, server.URL+"/expired"); err != nil {
		t.Fatalf("set expired folder cursor: %v", err)
	}

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("syncOutlookGraphFolder() error = %v", err)
	}
	if got := queryOutlookGraphDeletedByRemoteID(t, ctx, db, "graph-stale"); got != 1 {
		t.Fatalf("graph-stale is_deleted = %d, want prune after expired cursor recovery", got)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != deltaLink {
		t.Fatalf("sync_cursor = %q, want recovered delta link %q", got, deltaLink)
	}
}

func TestSyncOutlookGraphFolderIncrementalHydratesAttachmentMetadataAndStoresBodyWhenPresent(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	seedOutlookGraphProviderMessages(t, ctx, db, "graph-message-1")

	deltaLink := "https://graph.test/delta/incremental"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/delta/current":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-message-1",
					"internetMessageId": "<graph-message-1@example.com>",
					"subject":           "Graph subject",
					"bodyPreview":       "Graph preview",
					"body":              map[string]string{"contentType": "html", "content": `<p>Hello <img src="cid:logo@example.com"></p>`},
					"hasAttachments":    true,
					"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
					"toRecipients":      []map[string]any{{"emailAddress": map[string]string{"address": "recipient@example.com"}}},
					"receivedDateTime":  "2026-06-22T10:00:00Z",
					"sentDateTime":      "2026-06-22T09:59:00Z",
				}},
				"@odata.deltaLink": deltaLink,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1/attachments":
			if strings.Contains(r.URL.Query().Get("$select"), "contentId") {
				t.Fatalf("attachment base $select = %q, must not include fileAttachment-only contentId", r.URL.Query().Get("$select"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"@odata.type": "#microsoft.graph.fileAttachment",
				"id":          "graph-attachment-1",
				"name":        "logo.png",
				"contentType": "image/png",
				"size":        1234,
				"isInline":    true,
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-message-1/attachments/graph-attachment-1":
			if got := r.URL.Query().Get("$select"); got != "id,contentId" {
				t.Fatalf("attachment detail $select = %q, want id,contentId", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        "graph-attachment-1",
				"contentId": "<logo@example.com>",
			})
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	initialCursor := server.URL + "/delta/current"
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 1, sync_cursor = ?, last_full_sync_at = CURRENT_TIMESTAMP WHERE id = 'acc_inbox'`, initialCursor); err != nil {
		t.Fatalf("set incremental folder state: %v", err)
	}

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	blobs := store.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	orchestrator := NewSyncOrchestrator(db, nil, blobs, labelSyncTestTokens{})
	if err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1); err != nil {
		t.Fatalf("syncOutlookGraphFolder() error = %v", err)
	}

	msgID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<graph-message-1@example.com>")
	if err != nil || msgID == 0 {
		t.Fatalf("GetMessageLocalIDByInternetID() = %d, %v", msgID, err)
	}
	if !db.IsBodyFetched(ctx, msgID) {
		t.Fatal("incremental Graph body was not persisted")
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
	if len(email.Attachments) != 1 || email.Attachments[0].Filename != "logo.png" || !email.Attachments[0].Inline || email.Attachments[0].ContentID != "logo@example.com" {
		t.Fatalf("attachments = %#v, want Graph attachment metadata", email.Attachments)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != deltaLink {
		t.Fatalf("sync_cursor = %q, want %q", got, deltaLink)
	}
}

func TestSyncOutlookGraphFolderAttachmentFailureDoesNotCheckpoint(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	seedOutlookGraphFolder(t, ctx, db)
	seedOutlookGraphProviderMessages(t, ctx, db, "graph-with-attachment")

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/delta/current":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-with-attachment",
					"internetMessageId": "<graph-with-attachment@example.com>",
					"subject":           "Attachment",
					"hasAttachments":    true,
					"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
					"toRecipients":      []map[string]any{{"emailAddress": map[string]string{"address": "recipient@example.com"}}},
					"receivedDateTime":  "2026-06-22T10:00:00Z",
					"sentDateTime":      "2026-06-22T09:59:00Z",
				}},
				"@odata.nextLink": server.URL + "/delta/page-2",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/delta/page-2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]any{{
					"id":                "graph-page-2",
					"internetMessageId": "<graph-page-2@example.com>",
					"subject":           "Second page",
					"from":              map[string]any{"emailAddress": map[string]string{"address": "sender@example.com"}},
					"toRecipients":      []map[string]any{{"emailAddress": map[string]string{"address": "recipient@example.com"}}},
					"receivedDateTime":  "2026-06-22T10:01:00Z",
					"sentDateTime":      "2026-06-22T10:00:00Z",
				}},
				"@odata.deltaLink": "https://graph.test/delta/inbox",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/messages/graph-with-attachment/attachments":
			http.Error(w, "attachments failed", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected Graph request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	previousBaseURL := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousBaseURL })

	initialCursor := server.URL + "/delta/current"
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET total_count = 1, sync_cursor = ?, last_full_sync_at = CURRENT_TIMESTAMP WHERE id = 'acc_inbox'`, initialCursor); err != nil {
		t.Fatalf("set incremental folder state: %v", err)
	}

	orchestrator := NewSyncOrchestrator(db, nil, nil, labelSyncTestTokens{})
	err := orchestrator.syncOutlookGraphFolder(ctx, "acc", "graph-token", outlookGraphInboxTargetFromDB(t, ctx, db), nil, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "attachment metadata") {
		t.Fatalf("syncOutlookGraphFolder() error = %v, want attachment metadata failure", err)
	}
	if got := queryOutlookGraphFolderCursor(t, ctx, db); got != initialCursor {
		t.Fatalf("sync_cursor = %q, want old cursor %q after attachment failure", got, initialCursor)
	}
	page2ID, err := db.GetMessageLocalIDByInternetID(ctx, "acc", "<graph-page-2@example.com>")
	if err != nil || page2ID == 0 {
		t.Fatalf("second page message local ID = %d, %v; want imported despite attachment side-effect failure", page2ID, err)
	}
	var visibleThreads int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folder_thread_state WHERE folder_id = 'acc_inbox'`).Scan(&visibleThreads); err != nil {
		t.Fatalf("query folder thread state: %v", err)
	}
	if visibleThreads != 2 {
		t.Fatalf("visible folder threads = %d, want imported pages refreshed despite blocked cursor", visibleThreads)
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
	if err := orchestrator.syncOutlookGraphAttachmentMetadata(ctx, "graph-token", map[string]int64{"graph-message-1": msgID}, []outlookGraphMessage{{
		ID:             "graph-message-1",
		HasAttachments: false,
	}}); err != nil {
		t.Fatalf("syncOutlookGraphAttachmentMetadata() error = %v", err)
	}

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

func seedOutlookGraphFolder(t *testing.T, ctx context.Context, db *storage.DB) {
	t.Helper()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{
		ID:               "acc_inbox",
		AccountID:        "acc",
		RemoteID:         "Inbox",
		ProviderRemoteID: "folder-inbox",
		Name:             "Inbox",
		Role:             "inbox",
		Selectable:       true,
	}}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
}

func seedOutlookGraphProviderMessages(t *testing.T, ctx context.Context, db *storage.DB, providerMessageIDs ...string) {
	t.Helper()
	now := time.Now()
	msgs := make([]storage.ProviderSyncMessage, 0, len(providerMessageIDs))
	for _, providerMessageID := range providerMessageIDs {
		msgs = append(msgs, storage.ProviderSyncMessage{
			AccountID:         "acc",
			FolderID:          "acc_inbox",
			ProviderMessageID: providerMessageID,
			InternetMessageID: "<" + providerMessageID + "@example.com>",
			Subject:           providerMessageID,
			FromEmail:         "sender@example.com",
			DateSent:          now,
			DateReceived:      now,
			IsRead:            true,
		})
	}
	if _, err := db.UpsertProviderSyncMessages(ctx, msgs); err != nil {
		t.Fatalf("UpsertProviderSyncMessages() error = %v", err)
	}
}

func outlookGraphInboxTarget(syncCursor string) outlookGraphFolderSyncTarget {
	return outlookGraphFolderSyncTarget{
		Folder: storage.FolderSyncInfo{
			ID:               "acc_inbox",
			AccountID:        "acc",
			RemoteID:         "Inbox",
			ProviderRemoteID: "folder-inbox",
			Role:             "inbox",
			SyncCursor:       syncCursor,
		},
		Graph: outlookGraphFolder{
			ID:              "folder-inbox",
			DisplayName:     "Inbox",
			TotalItemCount:  1,
			UnreadItemCount: 0,
		},
	}
}

func outlookGraphInboxTargetFromDB(t *testing.T, ctx context.Context, db *storage.DB) outlookGraphFolderSyncTarget {
	t.Helper()
	folders, err := db.GetFoldersForAccount(ctx, "acc")
	if err != nil {
		t.Fatalf("GetFoldersForAccount() error = %v", err)
	}
	for _, folder := range folders {
		if folder.ID == "acc_inbox" {
			total := folder.TotalCount
			if total <= 0 {
				total = 1
			}
			return outlookGraphFolderSyncTarget{
				Folder: folder,
				Graph: outlookGraphFolder{
					ID:              "folder-inbox",
					DisplayName:     "Inbox",
					TotalItemCount:  total,
					UnreadItemCount: 0,
				},
			}
		}
	}
	t.Fatalf("acc_inbox not found in folders: %#v", folders)
	return outlookGraphFolderSyncTarget{}
}

func queryOutlookGraphDeletedByRemoteID(t *testing.T, ctx context.Context, db *storage.DB, providerMessageID string) int {
	t.Helper()
	var deleted int
	if err := db.Read().QueryRowContext(ctx, `
		SELECT mfs.is_deleted
		FROM messages m
		JOIN message_folder_state mfs ON mfs.message_id = m.id
		WHERE m.account_id = 'acc' AND m.remote_message_id = ? AND mfs.folder_id = 'acc_inbox'`, providerMessageID).Scan(&deleted); err != nil {
		t.Fatalf("query deleted state for %s: %v", providerMessageID, err)
	}
	return deleted
}

func queryOutlookGraphFolderCursor(t *testing.T, ctx context.Context, db *storage.DB) string {
	t.Helper()
	var cursor string
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(sync_cursor, '') FROM folders WHERE id = 'acc_inbox'`).Scan(&cursor); err != nil {
		t.Fatalf("query sync cursor: %v", err)
	}
	return cursor
}

func queryOutlookGraphProviderMessageCount(t *testing.T, ctx context.Context, db *storage.DB) int {
	t.Helper()
	var count int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE account_id = 'acc' AND COALESCE(remote_message_id, '') != ''`).Scan(&count); err != nil {
		t.Fatalf("query provider message count: %v", err)
	}
	return count
}

func queryOutlookGraphCountDriftConfirmations(t *testing.T, ctx context.Context, db *storage.DB) int {
	t.Helper()
	var confirmations int
	if err := db.Read().QueryRowContext(ctx, `SELECT COALESCE(provider_count_drift_confirmations, 0) FROM folders WHERE id = 'acc_inbox'`).Scan(&confirmations); err != nil {
		t.Fatalf("query count drift confirmations: %v", err)
	}
	return confirmations
}

func queryOutlookGraphVisibleThreadCount(t *testing.T, ctx context.Context, db *storage.DB) int {
	t.Helper()
	var count int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM folder_thread_state WHERE folder_id = 'acc_inbox'`).Scan(&count); err != nil {
		t.Fatalf("query visible thread count: %v", err)
	}
	return count
}
