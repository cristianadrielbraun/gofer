package mail

import (
	"context"
	"os"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Work on a JMAP message's cached content, as translating it or loading its
// remote images do, runs like it does for other accounts.
func TestRunMessageWorkAcceptsJMAPMessages(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	server.emails = []jmap.Email{testJMAPEmail("e1", "Lunch on Friday?", 1, "mb-in")}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	var id int64
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT DISTINCT s.message_id FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE f.account_id=? AND s.remote_message_id='e1'`, f.ids["alice"]).Scan(&id)
	})
	ran := ""
	if err := f.worker.RunMessageWork(t.Context(), "alice", id, func(_ context.Context, account string) error {
		ran = account
		return nil
	}); err != nil || ran != f.ids["alice"] {
		t.Fatalf("RunMessageWork() = %v, ran for %q", err, ran)
	}
}

// A JMAP attachment whose file is gone is rebuilt from the saved original,
// or, without one, from the original downloaded again; with JMAP turned off
// it isn't downloaded.
func TestEnsureAttachmentRecoversJMAPAttachments(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	email := testJMAPEmail("e1", "Receipt", 1, "mb-in")
	server.blobs["blob-e1"] = "From: Sam Ortiz <sam@example.test>\r\nTo: robin@example.test\r\nSubject: Receipt\r\n" +
		"Message-ID: <e1@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nYour receipt is attached.\r\n" +
		"--b1\r\nContent-Type: text/plain; name=receipt.txt\r\nContent-Disposition: attachment; filename=receipt.txt\r\n\r\nTotal: 12.50\r\n--b1--\r\n"
	email.Size = int64(len(server.blobs["blob-e1"]))
	server.emails = []jmap.Email{email}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	var messageID, attachmentID int64
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT DISTINCT s.message_id FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE f.account_id=? AND s.remote_message_id='e1'`, f.ids["alice"]).Scan(&messageID)
	})
	if err := f.worker.EnsureBody(t.Context(), "alice", messageID); err != nil {
		t.Fatal(err)
	}
	var stored, rawPath string
	f.local(t, "alice", func(db *storage.DB) error {
		if err := db.Read().QueryRow(`SELECT id, storage_path FROM attachments WHERE message_id=?`, messageID).Scan(&attachmentID, &stored); err != nil {
			return err
		}
		return db.Read().QueryRow(`SELECT COALESCE(raw_path,'') FROM messages WHERE id=?`, messageID).Scan(&rawPath)
	})
	recover := func() (*storage.AttachmentFetchInfo, error) {
		t.Helper()
		if err := os.Remove(stored); err != nil {
			t.Fatal(err)
		}
		info, err := f.worker.EnsureAttachment(t.Context(), "alice", attachmentID)
		if err == nil {
			content, readErr := os.ReadFile(info.StoragePath)
			if readErr != nil || string(content) != "Total: 12.50" {
				t.Fatalf("recovered attachment = %q, %v", content, readErr)
			}
			stored = info.StoragePath
		}
		return info, err
	}

	if rawPath == "" {
		t.Fatal("opening the message saved no original")
	}
	downloads := server.downloads
	if _, err := recover(); err != nil {
		t.Fatalf("recover from the saved original: %v", err)
	}
	if server.downloads != downloads {
		t.Fatal("downloaded the message although its original was saved")
	}
	if err := os.Remove(rawPath); err != nil {
		t.Fatal(err)
	}

	downloads = server.downloads
	if _, err := recover(); err != nil {
		t.Fatalf("recover from the server: %v", err)
	}
	if server.downloads != downloads+1 {
		t.Fatalf("downloads = %d, want one more than %d", server.downloads, downloads)
	}
}
