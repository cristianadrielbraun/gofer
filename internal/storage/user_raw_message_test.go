package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func rawMessageTestSnapshot(t *testing.T, db *DB) *RawMessageSnapshot {
	t.Helper()
	s, err := db.SnapshotRawMessage(t.Context(), "alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUserRawMessagePublicationRestoresOnlyRawPath(t *testing.T) {
	db := seedAttachmentRecovery(t)
	if _, err := db.Write().Exec(`UPDATE messages SET body_text_path='existing-body',body_html_path='existing-html',subject='existing-subject' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	s := rawMessageTestSnapshot(t, db)
	if _, err := db.SnapshotRawMessage(t.Context(), "bob", 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign lookup", err)
	}
	var checks int
	guard := func() error { checks++; return nil }
	next, err := db.PublishRawMessage(t.Context(), s, "restored-raw", guard)
	if err != nil || checks != 2 {
		t.Fatal("publication", checks, err)
	}
	if err := db.ValidateRawMessage(t.Context(), s, guard); !errors.Is(err, ErrMessageMutationSuperseded) {
		t.Fatal("old path still current", err)
	}
	if !s.SameIdentity(next) || next.Info().RawPath != "restored-raw" {
		t.Fatal("changed identity", next)
	}
	if err := db.ValidateRawMessage(t.Context(), next, guard); err != nil {
		t.Fatal(err)
	}
	var body, html, subject, path string
	var attachments int
	if err := db.Read().QueryRow(`SELECT body_text_path,body_html_path,subject,raw_path FROM messages WHERE id=1`).Scan(&body, &html, &subject, &path); err != nil {
		t.Fatal(err)
	}
	if body != "existing-body" || html != "existing-html" || subject != "existing-subject" || path != "restored-raw" {
		t.Fatal("body metadata replaced", body, html, subject, path)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM attachments WHERE (id=1 AND storage_path='old-path') OR (id=2 AND storage_path='sibling-path')`).Scan(&attachments); err != nil || attachments != 2 {
		t.Fatal("attachment identity changed", attachments, err)
	}
}

func TestUserRawMessagePublicationRejectsWriterWaitChanges(t *testing.T) {
	for _, change := range []string{
		`UPDATE messages SET remote_message_id='changed'`,
		`UPDATE messages SET internet_message_id='changed'`,
		`UPDATE messages SET raw_path='newer-cache'`,
		`UPDATE accounts SET provider_account_id='changed'`,
		`UPDATE accounts SET imap_host='changed'`,
		`UPDATE accounts SET imap_port=995`,
		`UPDATE accounts SET encrypted_password=X'01'`,
		`UPDATE accounts SET is_deleting=1`,
		`UPDATE users SET status='disabled'`,
		`UPDATE message_folder_state SET remote_uid=99`,
		`UPDATE message_folder_state SET is_deleted=1`,
		`UPDATE folders SET uid_validity=999`,
		"central-revocation",
	} {
		t.Run(change, func(t *testing.T) {
			db := seedAttachmentRecovery(t)
			s := rawMessageTestSnapshot(t, db)
			tx, err := db.Write().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var revoked atomic.Bool
			guard := func() error {
				if revoked.Load() {
					return ErrAccountRoute
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			before := db.Write().Stats().WaitCount
			go func() { _, err := db.PublishRawMessage(ctx, s, "candidate", guard); done <- err }()
			for db.Write().Stats().WaitCount == before {
				select {
				case <-ctx.Done():
					t.Fatal("publication did not wait")
				case <-time.After(time.Millisecond):
				}
			}
			if change == "central-revocation" {
				revoked.Store(true)
			} else if _, err := tx.ExecContext(ctx, change); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrMessageMutationSuperseded) && !errors.Is(err, ErrAccountRoute) {
					t.Fatal("stale MIME published", err)
				}
			case <-ctx.Done():
				t.Fatal("publication did not finish")
			}
			var path string
			if err := db.Read().QueryRow(`SELECT COALESCE(raw_path,'') FROM messages WHERE id=1`).Scan(&path); err != nil || path == "candidate" {
				t.Fatal("candidate leaked", path, err)
			}
		})
	}
}

func TestUserRawMessagePublicationTriggerFaultsRollBack(t *testing.T) {
	for _, trigger := range []string{
		`CREATE TRIGGER fault BEFORE UPDATE OF raw_path ON messages BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER fault AFTER UPDATE OF raw_path ON messages BEGIN UPDATE messages SET remote_message_id='wrong' WHERE id=NEW.id; END`,
		`CREATE TRIGGER fault AFTER UPDATE OF raw_path ON messages BEGIN UPDATE messages SET raw_path='wrong' WHERE id=NEW.id; END`,
	} {
		t.Run(trigger, func(t *testing.T) {
			db := seedAttachmentRecovery(t)
			s := rawMessageTestSnapshot(t, db)
			if _, err := db.Write().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := db.PublishRawMessage(t.Context(), s, "candidate", func() error { return nil }); !errors.Is(err, ErrMessageMutationSuperseded) {
				t.Fatal("trigger fault accepted", err)
			}
			if err := db.ValidateRawMessage(t.Context(), s, func() error { return nil }); err != nil {
				t.Fatal("failed update escaped rollback", err)
			}
		})
	}
}
