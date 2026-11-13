package mail

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const testJMAPRawMIME = "From: Sam <sam@example.test>\r\nTo: robin@example.test\r\nSubject: Project notes\r\n" +
	"Message-ID: <e1@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=parts\r\n\r\n" +
	"--parts\r\nContent-Type: text/plain\r\n\r\nOriginal JMAP body\r\n" +
	"--parts\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=notes.txt\r\n\r\nProject attachment\r\n--parts--\r\n"

func newJMAPRawMessageFixture(t *testing.T) (*userGmailFixture, *fakeJMAPServer, int64) {
	t.Helper()
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	email := testJMAPEmail("e1", "Project notes", 1, "mb-in")
	email.Size = int64(len(testJMAPRawMIME))
	server.emails = []jmap.Email{email}
	server.blobs["blob-e1"] = testJMAPRawMIME
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	return f, server, f.messageID(t, "alice", "e1")
}

func jmapRawDownloads(server *fakeJMAPServer) int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.downloads
}

func TestUserJMAPRawMessageReadsAndExportsUncachedMIME(t *testing.T) {
	f, server, id := newJMAPRawMessageFixture(t)
	if _, err := f.worker.ReadRawMessage(t.Context(), "bob", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign raw read = %v", err)
	}
	if _, err := f.worker.ReadRawMessageForExport(t.Context(), "alice", id, false); !errors.Is(err, ErrRawMessageNotSaved) {
		t.Fatalf("saved-only export = %v", err)
	}
	if jmapRawDownloads(server) != 0 {
		t.Fatal("foreign or saved-only read downloaded a message")
	}
	raw, err := f.worker.ReadRawMessageForExport(t.Context(), "alice", id, true)
	if err != nil || string(raw) != testJMAPRawMIME {
		t.Fatalf("uncached export = %q, %v", raw, err)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var rawPath, bodyPath string
		if err := db.Read().QueryRow(`SELECT COALESCE(raw_path,''),COALESCE(body_text_path,'') FROM messages WHERE id=?`, id).Scan(&rawPath, &bodyPath); err != nil {
			return err
		}
		if rawPath != "" || bodyPath != "" {
			t.Fatal("export changed the message cache", rawPath, bodyPath)
		}
		return nil
	})
	stored, err := f.worker.ReadRawMessage(t.Context(), "alice", id)
	if err != nil || string(stored.Bytes()) != testJMAPRawMIME {
		t.Fatalf("uncached read = %v", err)
	}
	if err := f.worker.ValidateRawMessage(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	if jmapRawDownloads(server) != 2 {
		t.Fatal("normal read reused an export download that should not be cached")
	}
	// Both forms of read reuse a stored original, without another download.
	if _, err := f.worker.ReadRawMessage(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if raw, err := f.worker.ReadRawMessageForExport(t.Context(), "alice", id, false); err != nil || string(raw) != testJMAPRawMIME {
		t.Fatalf("cached export = %q, %v", raw, err)
	}
	if jmapRawDownloads(server) != 2 {
		t.Fatal("cached original was downloaded again")
	}
}

func TestUserJMAPRawMessageRecoveryPreservesParsedContent(t *testing.T) {
	f, server, id := newJMAPRawMessageFixture(t)
	if err := f.worker.EnsureBody(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	type content struct {
		body, attachment string
		attachmentID     int64
	}
	savedContent := func() (string, content) {
		t.Helper()
		var rawPath string
		var c content
		f.local(t, "alice", func(db *storage.DB) error {
			if err := db.Read().QueryRow(`SELECT raw_path,body_text_path FROM messages WHERE id=?`, id).Scan(&rawPath, &c.body); err != nil {
				return err
			}
			return db.Read().QueryRow(`SELECT id,storage_path FROM attachments WHERE message_id=?`, id).Scan(&c.attachmentID, &c.attachment)
		})
		return rawPath, c
	}
	path, before := savedContent()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reads := make([]<-chan error, 4)
	for i := range reads {
		reads[i] = gmailAsync(func() error {
			raw, err := f.worker.ReadRawMessage(t.Context(), "alice", id)
			if err != nil {
				return err
			}
			if string(raw.Bytes()) != testJMAPRawMIME {
				return errors.New("recovered the wrong MIME")
			}
			return f.worker.ValidateRawMessage(t.Context(), raw)
		})
	}
	for _, read := range reads {
		if err := gmailAwait(t, read); err != nil {
			t.Fatal(err)
		}
	}
	path, after := savedContent()
	if before != after || jmapRawDownloads(server) != 2 {
		t.Fatal("raw recovery replaced parsed content or downloaded duplicates", before, after, jmapRawDownloads(server))
	}
	if saved, err := os.ReadFile(path); err != nil || string(saved) != testJMAPRawMIME {
		t.Fatalf("restored original = %q, %v", saved, err)
	}
}

func TestUserJMAPRawMessageRejectsChangesDuringDownload(t *testing.T) {
	for _, mode := range []string{"server", "export-server", "message", "password", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, server, id := newJMAPRawMessageFixture(t)
			started, release := make(chan struct{}, 1), make(chan struct{})
			server.mu.Lock()
			server.downloadStarted, server.downloadRelease = started, release
			server.mu.Unlock()
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := gmailAsync(func() error {
				if mode == "export-server" {
					_, err := f.worker.ReadRawMessageForExport(ctx, "alice", id, true)
					return err
				}
				_, err := f.worker.ReadRawMessage(ctx, "alice", id)
				return err
			})
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("JMAP download did not begin")
			}
			if mode == "cancel" {
				cancel()
			} else {
				change := `UPDATE accounts SET jmap_session_url='https://changed.example.test/.well-known/jmap' WHERE id=?`
				args := []any{f.ids["alice"]}
				switch mode {
				case "message":
					change, args = `UPDATE message_folder_state SET remote_message_id='replacement' WHERE message_id=?; UPDATE message_folder_state SET remote_message_id='replacement' WHERE message_id=?`, []any{id, id}
				case "password":
					change = `UPDATE accounts SET encrypted_password=X'01' WHERE id=?`
				}
				// This write must finish while HTTP is blocked: no user-store
				// lease may be held across the download.
				writeCtx, stop := context.WithTimeout(t.Context(), 2*time.Second)
				defer stop()
				if err := f.routing.WithUser(writeCtx, "alice", func(db *storage.DB) error {
					_, err := db.Write().ExecContext(writeCtx, change, args...)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			want := storage.ErrMessageMutationSuperseded
			if mode == "cancel" {
				want = context.Canceled
			}
			if err := gmailAwait(t, done); !errors.Is(err, want) {
				t.Fatalf("late JMAP download = %v, want %v", err, want)
			}
			f.local(t, "alice", func(db *storage.DB) error {
				var path string
				if err := db.Read().QueryRow(`SELECT COALESCE(raw_path,'') FROM messages WHERE id=?`, id).Scan(&path); err != nil {
					return err
				}
				if path != "" {
					t.Fatal("stale MIME was cached", path)
				}
				return nil
			})
		})
	}
}

func TestUserJMAPRawMessageHonorsLimits(t *testing.T) {
	f, server, id := newJMAPRawMessageFixture(t)
	_, err := f.worker.ReadRawMessage(message.WithDownloadLimit(t.Context(), 128), "alice", id)
	var tooLarge *message.TooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != 128 || jmapRawDownloads(server) != 0 {
		t.Fatalf("known oversized message = %v, downloads %d", err, jmapRawDownloads(server))
	}
	// Check the bounded stream too, when the server underreports the size.
	server.mu.Lock()
	server.emails[0].Size = 1
	server.mu.Unlock()
	_, err = f.worker.ReadRawMessage(message.WithDownloadLimit(t.Context(), 128), "alice", id)
	if !errors.As(err, &tooLarge) || tooLarge.Limit != 128 || jmapRawDownloads(server) != 1 {
		t.Fatalf("oversized stream = %v, downloads %d", err, jmapRawDownloads(server))
	}
}
