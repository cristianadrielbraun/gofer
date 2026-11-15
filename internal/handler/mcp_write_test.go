package handler

import (
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
	"strconv"
	"strings"
	"testing"
	"time"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func mcpWriteSession(t *testing.T, scopes ...string) (*Handler, *storage.DB, string, func(string, map[string]any) (map[string]any, bool, string)) {
	t.Helper()
	return mcpWriteSessionFor(t, nil, scopes...)
}

// mcpWriteSessionFor is mcpWriteSession with a token that may reach only
// mailboxes (all of them when empty).
func mcpWriteSessionFor(t *testing.T, mailboxes []string, scopes ...string) (*Handler, *storage.DB, string, func(string, map[string]any) (map[string]any, bool, string)) {
	t.Helper()
	h, db, srv := mcpTestServer(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	ctx := t.Context()
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "own-sent", AccountID: "victim-account", RemoteID: "Sent", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "own-drafts", AccountID: "victim-account", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true},
		{ID: "own-trash", AccountID: "victim-account", RemoteID: "Trash", Name: "Trash", Role: "trash", Selectable: true},
		{ID: "support-drafts", AccountID: "victim-account", RemoteID: "Shared Folders/support@example.com/Drafts", Name: "Drafts", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "drafts"},
		{ID: "support-trash", AccountID: "victim-account", RemoteID: "Shared Folders/support@example.com/Trash", Name: "Trash", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "trash"},
	}); err != nil {
		t.Fatal(err)
	}
	// From addresses for shared mailboxes are made only once chosen.
	if err := db.MergeUISettings(ctx, "owner", map[string]string{sharedmailbox.KeyAutoIdentities: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "victim-account", []string{"support@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A message in the shared mailbox, sent to the person only.
	if _, err := db.SaveDraftMessage(ctx, storage.DraftMessageInput{AccountID: "victim-account", FolderID: "support-inbox", InternetMessageID: "<ticket@example.net>", FromName: "Customer", FromEmail: "customer@example.net", Subject: "Ticket", Snippet: "Please help", ToRecipients: []storage.Recipient{{Email: "owner@example.com"}}}); err != nil {
		t.Fatal(err)
	}
	_, secret, err := db.CreateMCPToken(ctx, storage.MCPTokenInput{UserID: "owner", Scopes: scopes, Mailboxes: mailboxes})
	if err != nil {
		t.Fatal(err)
	}
	session := mcpConnect(t, srv, secret)
	call := func(tool string, args map[string]any) (map[string]any, bool, string) {
		t.Helper()
		out, r := mcpCall[map[string]any](t, session, tool, args)
		return out, r.IsError, mcpResultText(r)
	}
	id, _ := db.GetMessageLocalIDByInternetIDInternal(ctx, "victim-account", "<ticket@example.net>")
	return h, db, strconv.FormatInt(id, 10), call
}

func TestMCPOrganizeMail(t *testing.T) {
	_, db, ticket, call := mcpWriteSession(t, MCPScopeMailOrganize)
	ctx := t.Context()

	if _, isErr, text := call("mark_messages", map[string]any{"message_ids": []string{ticket}, "read": false, "starred": true}); isErr {
		t.Fatalf("mark_messages: %s", text)
	}
	var read, starred int
	_ = db.Read().QueryRowContext(ctx, `SELECT is_read, is_starred FROM message_folder_state WHERE message_id = ? AND folder_id = 'support-inbox'`, ticket).Scan(&read, &starred)
	if read != 0 || starred != 1 {
		t.Fatalf("read=%d starred=%d", read, starred)
	}

	if _, isErr, text := call("delete_messages", map[string]any{"message_ids": []string{ticket}, "permanent": true}); !isErr || !strings.Contains(text, "confirm") {
		t.Fatalf("permanent delete without confirm: %v %s", isErr, text)
	}
	// Deleted from a shared mailbox, it goes to that mailbox's Trash.
	if _, isErr, text := call("delete_messages", map[string]any{"message_ids": []string{ticket}}); isErr {
		t.Fatalf("delete_messages: %s", text)
	}
	var folder string
	_ = db.Read().QueryRowContext(ctx, `SELECT folder_id FROM message_folder_state WHERE message_id = ? AND is_deleted = 0`, ticket).Scan(&folder)
	if folder != "support-trash" {
		t.Fatalf("deleted message is in %q, want the shared mailbox's Trash", folder)
	}
	if _, isErr, text := call("move_messages", map[string]any{"message_ids": []string{ticket}, "folder_id": "own-inbox"}); isErr {
		t.Fatalf("move_messages: %s", text)
	}
	if _, isErr, _ := call("send_message", map[string]any{"to": []string{"a@example.net"}, "subject": "x", "body": "y"}); !isErr {
		t.Fatal("sent without the mail.send scope")
	}
}

func TestMCPSendAsASharedMailbox(t *testing.T) {
	_, db, ticket, call := mcpWriteSession(t, MCPScopeMailSend)
	ctx := t.Context()

	out, isErr, text := call("reply_to_message", map[string]any{"message_id": ticket, "body": "We are on it."})
	if isErr {
		t.Fatalf("reply: %s", text)
	}
	if out["from"] != "support@example.com" || out["subject"] != "Re: Ticket" || out["status"] != "sending" || out["send_id"] == "" {
		t.Fatalf("reply = %+v", out)
	}
	send, err := db.GetOutgoingSend(ctx, out["send_id"].(string))
	if err != nil || send.EnvelopeFrom != "support@example.com" || len(send.EnvelopeRecipients) != 1 || send.EnvelopeRecipients[0] != "customer@example.net" {
		t.Fatalf("queued reply = %+v, %v", send, err)
	}
	if mime := string(send.MIMEData); !strings.Contains(mime, "In-Reply-To: <ticket@example.net>") || !strings.Contains(mime, "> Please help") {
		t.Fatalf("reply MIME:\n%s", mime)
	}

	draft, isErr, text := call("save_draft", map[string]any{"from": "support@example.com", "to": []string{"b@example.net"}, "subject": "Later", "body": "draft"})
	if isErr || draft["draft_id"] == "" {
		t.Fatalf("save_draft: %+v %s", draft, text)
	}
	draftID, _ := db.GetMessageLocalIDByInternetIDInternal(ctx, "victim-account", draft["draft_id"].(string))
	var folder string
	_ = db.Read().QueryRowContext(ctx, `SELECT folder_id FROM message_folder_state WHERE message_id = ?`, draftID).Scan(&folder)
	if folder != "support-drafts" {
		t.Fatalf("draft is in %q, want the shared mailbox's Drafts", folder)
	}

	out, isErr, text = call("send_message", map[string]any{
		"to": []string{"c@example.net"}, "subject": "Report", "body": "Attached.",
		"attachments": []map[string]any{{"filename": "report.txt", "content_base64": "aGVsbG8="}},
	})
	if isErr || out["from"] != "owner@example.com" {
		t.Fatalf("send_message: %+v %s", out, text)
	}
	send, _ = db.GetOutgoingSend(ctx, out["send_id"].(string))
	if !strings.Contains(string(send.MIMEData), "report.txt") {
		t.Fatal("attachment missing from the message")
	}
	if _, isErr, _ := call("send_message", map[string]any{"subject": "nobody"}); !isErr {
		t.Fatal("sent a message without recipients")
	}
	status, isErr, text := call("get_send_status", map[string]any{"send_id": out["send_id"]})
	if isErr || status["result"] == nil {
		t.Fatalf("get_send_status: %+v %s", status, text)
	}
}
