package mail

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserRawMessageRecoveryPreservesBodyAndCoalescesOwners(t *testing.T) {
	f := newUserGmailFixture(t)
	ids := make(map[string]int64)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.worker.Sync(t.Context(), owner, f.ids[owner]); err != nil {
			t.Fatal(err)
		}
		ids[owner] = f.messageID(t, owner, "m1")
		if err := f.worker.EnsureBody(t.Context(), owner, ids[owner]); err != nil {
			t.Fatal(err)
		}
	}
	if ids["alice"] != ids["bob"] {
		t.Fatal("fixture lacks overlapping IDs")
	}
	var bodyPath, attachmentPath string
	var attachmentID int64
	f.local(t, "alice", func(db *storage.DB) error {
		var rawPath string
		if err := db.Read().QueryRow(`SELECT raw_path,body_text_path FROM messages WHERE id=?`, ids["alice"]).Scan(&rawPath, &bodyPath); err != nil {
			return err
		}
		if err := os.Remove(rawPath); err != nil {
			return err
		}
		return db.Read().QueryRow(`SELECT id,storage_path FROM attachments WHERE message_id=?`, ids["alice"]).Scan(&attachmentID, &attachmentPath)
	})
	block := f.api.stopAt("alice", "/users/me/messages/m1", "raw")
	defer block.unblock()
	reads := make([]<-chan error, 8)
	for i := range reads {
		reads[i] = gmailAsync(func() error {
			raw, err := f.worker.ReadRawMessage(t.Context(), "alice", ids["alice"])
			if err != nil {
				return err
			}
			if !strings.Contains(string(raw.Bytes()), "alice private body") {
				return errors.New("wrong owner MIME")
			}
			return f.worker.ValidateRawMessage(t.Context(), raw)
		})
	}
	select {
	case <-block.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("raw recovery did not run for cached body")
	}
	raw, err := f.worker.ReadRawMessage(t.Context(), "bob", ids["bob"])
	if err != nil || !strings.Contains(string(raw.Bytes()), "bob private body") {
		t.Fatal("sole store retained over network", err)
	}
	block.unblock()
	for _, read := range reads {
		if err := gmailAwait(t, read); err != nil {
			t.Fatal(err)
		}
	}
	if f.api.count("alice", "/users/me/messages/m1", "raw") != 2 || f.api.count("bob", "/users/me/messages/m1", "raw") != 1 {
		t.Fatal("duplicate raw fetch")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var body, path string
		var id int64
		if err := db.Read().QueryRow(`SELECT body_text_path FROM messages WHERE id=?`, ids["alice"]).Scan(&body); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT id,storage_path FROM attachments WHERE message_id=?`, ids["alice"]).Scan(&id, &path); err != nil {
			return err
		}
		if body != bodyPath || id != attachmentID || path != attachmentPath {
			return errors.New("raw restoration changed parsed body or attachment identity")
		}
		return nil
	})
}

func TestUserRawMessageRejectsChangedProviderIdentityAndGrant(t *testing.T) {
	for _, mode := range []string{"message", "reconnect", "root-stop"} {
		t.Run(mode, func(t *testing.T) {
			f := newUserGmailFixture(t)
			if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
				t.Fatal(err)
			}
			id := f.messageID(t, "alice", "m1")
			block := f.api.stopAt("alice", "/users/me/messages/m1", "raw")
			defer block.unblock()
			done := gmailAsync(func() error { _, err := f.worker.ReadRawMessage(t.Context(), "alice", id); return err })
			select {
			case <-block.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("raw GET did not begin")
			}
			switch mode {
			case "message":
				f.local(t, "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE messages SET remote_message_id='new' WHERE id=?`, id)
					return err
				})
			case "reconnect":
				expiry := time.Now().Add(time.Hour)
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.ids["alice"], "google", "same-subject", "reconnected", "new-refresh", "Bearer", &expiry, "https://mail.google.com/"); err != nil {
					t.Fatal(err)
				}
			case "root-stop":
				f.cancel()
			}
			block.unblock()
			err := gmailAwait(t, done)
			if mode == "message" && !errors.Is(err, storage.ErrMessageMutationSuperseded) || mode == "reconnect" && !errors.Is(err, mailauth.ErrMailboxAuthorizationChanged) || mode == "root-stop" && !errors.Is(err, context.Canceled) {
				t.Fatal("late MIME accepted", mode, err)
			}
			f.local(t, "alice", func(db *storage.DB) error {
				var path string
				if err := db.Read().QueryRow(`SELECT COALESCE(raw_path,'') FROM messages WHERE id=?`, id).Scan(&path); err != nil {
					return err
				}
				if path != "" {
					return errors.New("stale MIME path persisted")
				}
				return nil
			})
		})
	}
}

func TestUserRawMessageOwn401RefreshIsBounded(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	id := f.messageID(t, "alice", "m1")
	f.api.change("alice", func(s *routedGmailState) { s.rejectOld = true })
	raw, err := f.worker.ReadRawMessage(t.Context(), "alice", id)
	if err != nil || !strings.Contains(string(raw.Bytes()), "alice private body") {
		t.Fatal("own bounded refresh", err)
	}
	if f.api.count("alice", "/users/me/messages/m1", "raw") != 2 || f.api.count("alice", "/token", "") != 1 {
		t.Fatal("wrong retry count")
	}
	if err := f.worker.ValidateRawMessage(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.ReadRawMessage(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/users/me/messages/m1", "raw") != 2 {
		t.Fatal("accepted MIME fetched again")
	}
}
