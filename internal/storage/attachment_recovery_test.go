package storage

import (
	"errors"
	"testing"
)

func seedAttachmentRecovery(t *testing.T) *DB {
	t.Helper()
	db, _ := seedUserLabelTest(t, "gmail")
	if _, err := db.Write().Exec(`INSERT INTO attachments(id,message_id,filename,content_type,size_bytes,inline,storage_path,provider_remote_id) VALUES(1,1,'same.txt','text/plain',4,0,'old-path','part-1'),(2,1,'same.txt','text/plain',4,0,'sibling-path','part-2')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func attachmentSnapshot(t *testing.T, db *DB) *AttachmentRecovery {
	t.Helper()
	s, err := db.GetAttachmentRecoveryForUser(t.Context(), "alice", 2)
	if err != nil || s == nil {
		t.Fatal(s, err)
	}
	return s
}

func TestUserAttachmentSnapshotAndPublicationStayOwned(t *testing.T) {
	db := seedAttachmentRecovery(t)
	s := attachmentSnapshot(t, db)
	if s.PartIndex != 1 || len(s.Parts) != 2 || !s.NetworkAllowed {
		t.Fatal("incomplete attachment layout", s)
	}
	if foreign, err := db.GetAttachmentRecoveryForUser(t.Context(), "bob", 2); err != nil || foreign != nil {
		t.Fatal("foreign attachment snapshot exposed", foreign, err)
	}
	if err := db.PublishRecoveredAttachment(t.Context(), "bob", *s, "m1", "candidate", false); !errors.Is(err, ErrMessageMutationSuperseded) {
		t.Fatal("foreign publication accepted", err)
	}
	if err := db.PublishRecoveredAttachment(t.Context(), "alice", *s, "m1", "candidate", false); err != nil {
		t.Fatal(err)
	}
	var count int
	var path string
	if err := db.Read().QueryRow(`SELECT count(*) FROM attachments WHERE id IN(1,2)`).Scan(&count); err != nil || count != 2 {
		t.Fatal("publication replaced attachment rows", count, err)
	}
	if err := db.Read().QueryRow(`SELECT storage_path FROM attachments WHERE id=1`).Scan(&path); err != nil || path != "old-path" {
		t.Fatal("sibling path changed", path, err)
	}
}

func TestUserAttachmentPublicationRejectsChangedSnapshots(t *testing.T) {
	for _, q := range []string{
		`UPDATE accounts SET provider_account_id='new'`,
		`UPDATE accounts SET auth_method='plain'`,
		`UPDATE accounts SET provider='outlook'`,
		`UPDATE accounts SET is_deleting=1`,
		`UPDATE users SET status='disabled'`,
		`UPDATE messages SET remote_message_id='new'`,
		`UPDATE messages SET internet_message_id='new'`,
		`UPDATE messages SET raw_path='new'`,
		`UPDATE attachments SET storage_path='new' WHERE id=2`,
		`UPDATE attachments SET filename='new' WHERE id=2`,
		`UPDATE attachments SET content_type='image/png' WHERE id=2`,
		`UPDATE attachments SET content_id='new' WHERE id=2`,
		`UPDATE attachments SET size_bytes=5 WHERE id=2`,
		`UPDATE attachments SET inline=1 WHERE id=2`,
		`UPDATE attachments SET provider_remote_id='new' WHERE id=2`,
		`UPDATE attachments SET filename='changed-sibling' WHERE id=1`,
		`DELETE FROM attachments WHERE id=1`,
		`DELETE FROM attachments WHERE id=2`,
		`UPDATE message_folder_state SET is_deleted=1`,
	} {
		t.Run(q, func(t *testing.T) {
			db := seedAttachmentRecovery(t)
			s := attachmentSnapshot(t, db)
			if _, err := db.Write().Exec(q); err != nil {
				t.Fatal(err)
			}
			if err := db.PublishRecoveredAttachment(t.Context(), "alice", *s, "m1", "candidate", false); !errors.Is(err, ErrMessageMutationSuperseded) {
				t.Fatal("changed attachment/mailbox published", err)
			}
		})
	}
}

func TestUserAttachmentLookupAndPathRollbackTogether(t *testing.T) {
	db := seedAttachmentRecovery(t)
	if _, err := db.Write().Exec(`UPDATE messages SET remote_message_id=''; CREATE TRIGGER fail_attachment BEFORE UPDATE OF storage_path ON attachments BEGIN SELECT RAISE(ABORT,'injected path failure'); END`); err != nil {
		t.Fatal(err)
	}
	s := attachmentSnapshot(t, db)
	if err := db.PublishRecoveredAttachment(t.Context(), "alice", *s, "resolved", "candidate", false); err == nil {
		t.Fatal("failed path update reported success")
	}
	var id, path string
	if err := db.Read().QueryRow(`SELECT m.remote_message_id,a.storage_path FROM messages m JOIN attachments a ON a.message_id=m.id WHERE a.id=2`).Scan(&id, &path); err != nil {
		t.Fatal(err)
	}
	if id != "" || path != "sibling-path" {
		t.Fatal("lookup/path commit did not roll back", id, path)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER fail_attachment`); err != nil {
		t.Fatal(err)
	}
	if err := db.PublishRecoveredAttachment(t.Context(), "alice", *s, "resolved", "candidate", false); err != nil {
		t.Fatal(err)
	}
}

func TestUserAttachmentPendingDraftOnlyAllowsItsSavedMIME(t *testing.T) {
	db := seedAttachmentRecovery(t)
	if _, err := db.Write().Exec(`UPDATE messages SET raw_path='current-immutable-mime'; INSERT INTO gofer_provider_draft_states(account_id,draft_key,provider,mailbox_subject,local_message_id,folder_id,local_revision) VALUES('acc','<label@test.invalid>','gmail','subject',1,'inbox','newer'); INSERT INTO gofer_provider_draft_operations(account_id,draft_key,kind,revision_token,mime_data) VALUES('acc','<label@test.invalid>','upsert','newer','newer')`); err != nil {
		t.Fatal(err)
	}
	s := attachmentSnapshot(t, db)
	if s.NetworkAllowed {
		t.Fatal("pending draft allowed old provider content")
	}
	if err := db.PublishRecoveredAttachment(t.Context(), "alice", *s, "m1", "candidate", false); !errors.Is(err, ErrMessageMutationSuperseded) {
		t.Fatal("pending draft accepted remote bytes", err)
	}
	if err := db.PublishRecoveredAttachment(t.Context(), "alice", *s, "m1", "candidate", true); err != nil {
		t.Fatal("current saved MIME was rejected", err)
	}
}
