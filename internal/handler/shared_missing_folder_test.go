package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Mail in a shared mailbox without Trash, Archive or Spam stays there: the
// account's own folders are never used instead, and the page is told the
// shared mailbox lacks the folder.
func TestSharedMailboxWithoutFolderRefusesTheAction(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, nil, nil, nil)
	ctx := t.Context()
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "own-inbox", AccountID: "victim-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "own-trash", AccountID: "victim-account", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
		{ID: "own-archive", AccountID: "victim-account", RemoteID: "Archive", Name: "Archive", Role: "archive", Selectable: true},
		{ID: "own-junk", AccountID: "victim-account", RemoteID: "Junk", Name: "Junk", Role: "junk", Selectable: true},
		{ID: "support-inbox", AccountID: "victim-account", RemoteID: "Shared Folders/support@example.com", Name: "support@example.com", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "inbox"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "victim-account", []string{"support@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSyncMessages(ctx, []storage.SyncMessage{
		{AccountID: "victim-account", FolderID: "support-inbox", RemoteUID: 3, MessageID: "<ticket@example.com>", Subject: "Ticket", FromEmail: "customer@example.net", DateSent: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	messageID, _ := db.GetMessageLocalIDByInternetIDInternal(ctx, "victim-account", "<ticket@example.com>")
	id := strconv.FormatInt(messageID, 10)
	asOwner := func(req *http.Request) *http.Request {
		return req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "owner", Username: "owner"}))
	}
	refused := func(name string, recorder *httptest.ResponseRecorder, role string) {
		t.Helper()
		if recorder.Code != http.StatusBadRequest || strings.TrimSpace(recorder.Body.String()) != "This shared mailbox has no folder for that, so its mail was left where it is." || recorder.Header().Get(missingFolderHeader) != "victim-account "+role+" shared" {
			t.Fatalf("%s status=%d body=%q header=%q", name, recorder.Code, recorder.Body.String(), recorder.Header().Get(missingFolderHeader))
		}
	}

	req := asOwner(httptest.NewRequest(http.MethodDelete, "/api/messages/"+id+"?folder_id=support-inbox", nil))
	req.SetPathValue("id", id)
	recorder := httptest.NewRecorder()
	h.handleDeleteMessage(recorder, req)
	refused("single delete", recorder, "trash")

	req = asOwner(httptest.NewRequest(http.MethodPost, "/api/messages/"+id+"/thread/archive", nil))
	req.SetPathValue("id", id)
	recorder = httptest.NewRecorder()
	h.handleArchiveThread(recorder, req)
	refused("archive", recorder, "archive")

	body := `{"targets":[{"id":"` + id + `"}],"folder_id":"support-inbox"}`
	bulk := func(handle func(http.ResponseWriter, *http.Request), path string) *httptest.ResponseRecorder {
		req := asOwner(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handle(recorder, req)
		return recorder
	}
	recorder = bulk(h.handleDeleteMessages, "/api/messages/delete")
	var got map[string]int
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || got["updated"] != 0 || got["no_trash"] != 1 || recorder.Header().Get(missingFolderHeader) != "victim-account trash shared" {
		t.Fatalf("bulk delete status=%d body=%s header=%q", recorder.Code, recorder.Body.String(), recorder.Header().Get(missingFolderHeader))
	}
	recorder = bulk(h.handleArchiveMessages, "/api/messages/archive")
	got = nil
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil || got["updated"] != 0 || got["no_archive"] != 1 || recorder.Header().Get(missingFolderHeader) != "victim-account archive shared" {
		t.Fatalf("bulk archive status=%d body=%s header=%q", recorder.Code, recorder.Body.String(), recorder.Header().Get(missingFolderHeader))
	}
	refused("spam", bulk(h.handleMarkMessagesSpam, "/api/messages/spam"), "junk")

	var moves int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE message_id = ?`, messageID).Scan(&moves); err != nil || moves != 0 {
		t.Fatalf("queued %d changes for mail that should have stayed, %v", moves, err)
	}
	var inShared int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_folder_state WHERE message_id = ? AND folder_id = 'support-inbox' AND is_deleted = 0`, messageID).Scan(&inShared); err != nil || inShared != 1 {
		t.Fatalf("message left the shared mailbox: %d, %v", inShared, err)
	}
}
