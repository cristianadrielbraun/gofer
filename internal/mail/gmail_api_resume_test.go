package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

// gmailResumeTestMailbox answers the Gmail API for an Inbox listed in pages
// and a Sent folder. Messages it is asked for get the labels in labelsOf.
type gmailResumeTestMailbox struct {
	t           *testing.T
	inboxPages  [][]string
	sent        []string
	labelsOf    map[string][]string
	failMessage map[string]int
	failPage    int

	mu   sync.Mutex
	gets map[string]int
}

func (m *gmailResumeTestMailbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	list := func(ids []string, next string) {
		refs := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, map[string]string{"id": id, "threadId": "thread-" + id})
		}
		body := map[string]any{"messages": refs}
		if next != "" {
			body["nextPageToken"] = next
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
		labels := []map[string]any{{"id": "INBOX", "name": "INBOX", "type": "system"}}
		if m.sent != nil {
			labels = append(labels, map[string]any{"id": "SENT", "name": "SENT", "type": "system"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": labels})
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/profile":
		_ = json.NewEncoder(w).Encode(map[string]any{"historyId": "500"})
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "INBOX":
		index := 0
		if token := r.URL.Query().Get("pageToken"); token != "" {
			index = int(token[len(token)-1] - '0')
		}
		if m.failPage > 0 && index == m.failPage {
			http.Error(w, `{"error":{"code":403,"message":"Daily limit reached"}}`, http.StatusForbidden)
			return
		}
		next := ""
		if index+1 < len(m.inboxPages) {
			next = "page-" + string(rune('0'+index+1))
		}
		list(m.inboxPages[index], next)
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && r.URL.Query().Get("labelIds") == "SENT":
		list(m.sent, "")
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages" && strings.Contains(r.URL.Query().Get("q"), "-in:inbox"):
		_ = json.NewEncoder(w).Encode(map[string]any{})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/me/messages/"):
		id := strings.TrimPrefix(r.URL.Path, "/users/me/messages/")
		m.mu.Lock()
		m.gets[id]++
		m.mu.Unlock()
		if status := m.failMessage[id]; status != 0 {
			http.Error(w, `{"error":{"code":400,"message":"Invalid message"}}`, status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           id,
			"threadId":     "thread-" + id,
			"labelIds":     m.labelsOf[id],
			"snippet":      "Note " + id,
			"historyId":    "400",
			"internalDate": "1760000600000",
			"payload": map[string]any{
				"headers": []map[string]string{
					{"name": "Message-ID", "value": "<" + id + "@mail.example>"},
					{"name": "Subject", "value": "Note " + id},
					{"name": "From", "value": "Sender <sender@example.com>"},
					{"name": "Date", "value": "Fri, 26 Jun 2026 12:10:00 +0000"},
				},
			},
		})
	default:
		m.t.Fatalf("unexpected Gmail request %s %s", r.Method, r.URL.String())
	}
}

func (m *gmailResumeTestMailbox) fetched() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.gets))
	for id, n := range m.gets {
		out[id] = n
	}
	return out
}

func startGmailResumeTestMailbox(t *testing.T, mailbox *gmailResumeTestMailbox) {
	t.Helper()
	mailbox.t = t
	mailbox.gets = map[string]int{}
	server := httptest.NewServer(mailbox)
	t.Cleanup(server.Close)
	previous := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previous })
}

// newGmailResumeTestAccount stores a Gmail account whose import has finished,
// with the given messages already in its Inbox.
func newGmailResumeTestAccount(t *testing.T, stored ...string) (*storage.DB, *SyncOrchestrator) {
	t.Helper()
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address, auth_method) VALUES ('acc', 'default', ?, 'user@example.com', 'oauth2')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: gmailAPITestFolderID("INBOX"), AccountID: "acc", RemoteID: "INBOX", ProviderRemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatalf("UpsertFolders() error = %v", err)
	}
	for _, id := range stored {
		if _, err := db.UpsertProviderSyncMessages(ctx, []storage.ProviderSyncMessage{{
			AccountID: "acc", FolderID: gmailAPITestFolderID("INBOX"), ProviderMessageID: id,
			InternetMessageID: "<" + id + "@mail.example>", Subject: "Note " + id, FromEmail: "sender@example.com",
			DateSent: mustParseGmailAPITestTime(t), DateReceived: mustParseGmailAPITestTime(t), IsRead: true,
		}}); err != nil {
			t.Fatalf("UpsertProviderSyncMessages(%s) error = %v", id, err)
		}
	}
	if err := db.MarkLabelSyncRun(ctx, storage.LabelSyncRunStats{
		AccountID: "acc", ProviderType: storage.LabelProviderGmail, Scope: "messages", Cursor: "105", Full: true,
	}, nil); err != nil {
		t.Fatalf("MarkLabelSyncRun() error = %v", err)
	}
	accounts, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	return db, NewSyncOrchestrator(db, accounts, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), labelSyncTestTokens{})
}

func gmailResumeTestProgress(t *testing.T, db *storage.DB) string {
	t.Helper()
	state, err := db.GetLabelSyncState(context.Background(), "acc", storage.LabelProviderGmail, gmailAPIRepairScope)
	if err != nil {
		t.Fatalf("GetLabelSyncState(repair) error = %v", err)
	}
	return state.Cursor
}

func TestSyncGmailAPIHistoricalImportQueuesFailedMessage(t *testing.T) {
	ctx := context.Background()
	db := newLabelSyncTestDB(t)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', ?, 'user@example.com')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	mailbox := &gmailResumeTestMailbox{
		inboxPages:  [][]string{{"msg-good", "msg-bad"}},
		labelsOf:    map[string][]string{"msg-good": {"INBOX"}, "msg-bad": {"INBOX"}},
		failMessage: map[string]int{"msg-bad": http.StatusBadRequest},
	}
	startGmailResumeTestMailbox(t, mailbox)

	orchestrator := NewSyncOrchestrator(db, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), labelSyncTestTokens{})
	if err := orchestrator.syncGmailAPIAccount(ctx, "acc", true); err != nil {
		t.Fatalf("syncGmailAPIAccount() error = %v, want the import to finish", err)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if !state.LastFullSyncAt.Valid || state.LastFailedMessages != 1 {
		t.Fatalf("sync state = %#v, want a finished import with one failed message", state)
	}
	var queued int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM gmail_message_fetch_queue WHERE account_id = 'acc' AND provider_message_id = 'msg-bad'`).Scan(&queued); err != nil {
		t.Fatalf("query Gmail fetch queue: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued msg-bad = %d, want it waiting for a retry", queued)
	}
}

func TestRepairGmailAPIAccountSavesProgressWhenStopped(t *testing.T) {
	db, orchestrator := newGmailResumeTestAccount(t, "msg-a", "msg-b")
	mailbox := &gmailResumeTestMailbox{
		inboxPages: [][]string{{"msg-a"}, {"msg-b"}},
		labelsOf:   map[string][]string{"msg-a": {"INBOX"}, "msg-b": {"INBOX"}},
		failPage:   1,
	}
	startGmailResumeTestMailbox(t, mailbox)

	if err := orchestrator.repairGmailAPIAccount(context.Background(), "acc"); err == nil {
		t.Fatal("repairGmailAPIAccount() error = nil, want the stopped listing")
	}
	if got := gmailResumeTestProgress(t, db); got != `{"label":"INBOX","pages":1}` {
		t.Fatalf("saved repair progress = %q, want the first Inbox page done", got)
	}
}

func TestRepairGmailAPIAccountContinuesFromSavedProgress(t *testing.T) {
	ctx := context.Background()
	db, orchestrator := newGmailResumeTestAccount(t, "msg-a", "msg-b", "msg-c")
	if err := db.SaveLabelSyncCursor(ctx, "acc", storage.LabelProviderGmail, gmailAPIRepairScope, `{"label":"INBOX","pages":1}`); err != nil {
		t.Fatalf("SaveLabelSyncCursor() error = %v", err)
	}
	mailbox := &gmailResumeTestMailbox{
		inboxPages: [][]string{{"msg-a"}, {"msg-b"}},
		sent:       []string{"msg-c"},
		labelsOf:   map[string][]string{"msg-a": {"INBOX"}, "msg-b": {"INBOX"}, "msg-c": {"SENT"}},
	}
	startGmailResumeTestMailbox(t, mailbox)

	if err := orchestrator.repairGmailAPIAccount(ctx, "acc"); err != nil {
		t.Fatalf("repairGmailAPIAccount() error = %v", err)
	}
	got := mailbox.fetched()
	if got["msg-a"] != 0 || got["msg-b"] != 1 || got["msg-c"] != 1 {
		t.Fatalf("fetched = %#v, want only the messages after the saved page", got)
	}
	if progress := gmailResumeTestProgress(t, db); progress != "" {
		t.Fatalf("saved repair progress = %q, want it cleared after the repair", progress)
	}
	state, err := db.GetLabelSyncState(ctx, "acc", storage.LabelProviderGmail, "messages")
	if err != nil {
		t.Fatalf("GetLabelSyncState() error = %v", err)
	}
	if state.Cursor != "500" || !state.LastFullSyncAt.Valid {
		t.Fatalf("sync state = %#v, want the cursor read before the repair", state)
	}
	// msg-a was only listed, so it stays in the Inbox.
	var inInbox int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM message_folder_state WHERE folder_id = ? AND is_deleted = 0`, gmailAPITestFolderID("INBOX")).Scan(&inInbox); err != nil {
		t.Fatalf("count Inbox: %v", err)
	}
	if inInbox != 2 {
		t.Fatalf("Inbox messages = %d, want msg-a and msg-b", inInbox)
	}
}

func TestRepairGmailAPIAccountStartsOverWithoutProgress(t *testing.T) {
	db, orchestrator := newGmailResumeTestAccount(t, "msg-a", "msg-b")
	mailbox := &gmailResumeTestMailbox{
		inboxPages: [][]string{{"msg-a"}, {"msg-b"}},
		labelsOf:   map[string][]string{"msg-a": {"INBOX"}, "msg-b": {"INBOX"}},
	}
	startGmailResumeTestMailbox(t, mailbox)

	if err := orchestrator.repairGmailAPIAccount(context.Background(), "acc"); err != nil {
		t.Fatalf("repairGmailAPIAccount() error = %v", err)
	}
	got := mailbox.fetched()
	if got["msg-a"] != 1 || got["msg-b"] != 1 {
		t.Fatalf("fetched = %#v, want every message fetched again", got)
	}
	if progress := gmailResumeTestProgress(t, db); progress != "" {
		t.Fatalf("saved repair progress = %q, want none after the repair", progress)
	}
}
