package storage

import (
	"errors"
	"testing"
	"time"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
)

func seedUserProviderDraft(t *testing.T) *DB {
	t.Helper()
	db, _ := seedUserLabelTest(t, "gmail")
	if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,provider_remote_id,name,role) VALUES('drafts','acc','DRAFT','DRAFT','Drafts','drafts')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func publishUserProviderDraft(t *testing.T, db *DB, revision string) (int64, error) {
	t.Helper()
	return db.PublishDraft(t.Context(), DraftMessageInput{AccountID: "acc", FolderID: "drafts", InternetMessageID: "<draft@mail.test>", Subject: revision, Date: time.Now()}, func(id int64) (DraftPublication, error) {
		return DraftPublication{TextPath: "body-" + revision, ProviderSync: &QueueUserProviderDraftInput{State: UserProviderDraftState{AccountID: "acc", DraftKey: "<draft@mail.test>", FolderID: "drafts", Provider: "gmail", MailboxSubject: "subject"}, RevisionToken: revision, MIMEData: []byte(revision), MessageDate: time.Now()}}, nil
	})
}

func claimUserProviderDraft(t *testing.T, db *DB) UserProviderDraftOperation {
	t.Helper()
	op, err := db.ClaimUserProviderDraft(t.Context(), "acc", time.Now().Add(time.Second))
	if err != nil || op == nil {
		t.Fatalf("claim provider draft: %v %v", op, err)
	}
	return *op
}

func TestUserProviderDraftPublicationAndQueueRollbackTogether(t *testing.T) {
	db := seedUserProviderDraft(t)
	if _, err := publishUserProviderDraft(t, db, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_provider_draft BEFORE UPDATE ON gofer_provider_draft_operations BEGIN SELECT RAISE(ABORT,'injected draft queue failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := publishUserProviderDraft(t, db, "second"); err == nil {
		t.Fatal("failed queue committed draft content")
	}
	var subject, path, revision string
	if err := db.Read().QueryRow(`SELECT m.subject,m.body_text_path,s.local_revision FROM messages m JOIN gofer_provider_draft_states s ON s.local_message_id=m.id`).Scan(&subject, &path, &revision); err != nil {
		t.Fatal(err)
	}
	if subject != "first" || path != "body-first" || revision != "first" {
		t.Fatal("draft content/state did not roll back", subject, path, revision)
	}
}

func TestUserProviderDraftOlderCompletionPreservesNewLocalRevision(t *testing.T) {
	db := seedUserProviderDraft(t)
	id, err := publishUserProviderDraft(t, db, "first")
	if err != nil {
		t.Fatal(err)
	}
	first := claimUserProviderDraft(t, db)
	if _, err := publishUserProviderDraft(t, db, "second"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUserProviderDraftCandidate(t.Context(), first, "container", "message-first"); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderDraftUpsert(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	var subject, remote string
	if err := db.Read().QueryRow(`SELECT subject,COALESCE(remote_message_id,'') FROM messages WHERE id=?`, id).Scan(&subject, &remote); err != nil {
		t.Fatal(err)
	}
	if subject != "second" || remote != "" {
		t.Fatal("older completion bound its ID to newer content", subject, remote)
	}
	second := claimUserProviderDraft(t, db)
	if second.State.RemoteID != "container" || second.State.RemoteMessageID != "message-first" || second.RevisionToken != "second" {
		t.Fatal("new revision lost actual remote state", second)
	}
	if err := db.RecordUserProviderDraftCandidate(t.Context(), second, "container", "message-second"); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderDraftUpsert(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COALESCE(remote_message_id,'') FROM messages WHERE id=?`, id).Scan(&remote); err != nil || remote != "message-second" {
		t.Fatal("latest identity not published", remote, err)
	}
}

func TestUserProviderDraftRecoveryRetainsCreateFenceAndCandidate(t *testing.T) {
	db := seedUserProviderDraft(t)
	if _, err := publishUserProviderDraft(t, db, "first"); err != nil {
		t.Fatal(err)
	}
	first := claimUserProviderDraft(t, db)
	if err := db.BeginUserProviderDraftCreate(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverAccountMailQueue(t.Context(), "acc"); err != nil {
		t.Fatal(err)
	}
	second := claimUserProviderDraft(t, db)
	if !second.CreateAttempted || second.AttemptCount != 2 {
		t.Fatal("recovery lost creation uncertainty", second)
	}
	if err := db.RecordUserProviderDraftCandidate(t.Context(), first, "container", "remote"); !errors.Is(err, ErrMessageMutationSuperseded) {
		t.Fatal("stale attempt published", err)
	}
	if err := db.RecordUserProviderDraftCandidate(t.Context(), second, "container", "remote"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_draft_completion BEFORE DELETE ON gofer_provider_draft_operations BEGIN SELECT RAISE(ABORT,'injected draft completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderDraftUpsert(t.Context(), second); err == nil {
		t.Fatal("failed completion accepted")
	}
	var candidate, remote string
	var size int
	if err := db.Read().QueryRow(`SELECT o.candidate_id,length(o.mime_data),s.remote_id FROM gofer_provider_draft_operations o JOIN gofer_provider_draft_states s ON s.account_id=o.account_id AND s.draft_key=o.draft_key`).Scan(&candidate, &size, &remote); err != nil {
		t.Fatal(err)
	}
	if candidate != "container" || size == 0 || remote != "" {
		t.Fatal("completion rollback discarded candidate/MIME or leaked identity", candidate, size, remote)
	}
}

func TestUserProviderDraftReceivingPreservesPendingEditAndDiscard(t *testing.T) {
	db := seedUserProviderDraft(t)
	id, err := publishUserProviderDraft(t, db, "local-edit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE messages SET remote_message_id='old-remote' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	remote := ProviderSyncMessage{AccountID: "acc", FolderID: "drafts", ProviderMessageID: "old-remote", InternetMessageID: "<draft@mail.test>", Subject: "stale remote", IsDraft: true}
	if _, err := db.UpsertProviderSyncMessages(t.Context(), []ProviderSyncMessage{remote}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReconcileProviderFolderSeen(t.Context(), "acc", "drafts", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkProviderMessagesMissingFromFolder(t.Context(), "acc", "drafts", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProviderMessageRemovedFromFolder(t.Context(), "acc", "drafts", "old-remote"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkProviderMessageDeleted(t.Context(), "acc", "old-remote"); err != nil {
		t.Fatal(err)
	}
	var subject string
	var deleted bool
	if err := db.Read().QueryRow(`SELECT m.subject,s.is_deleted FROM messages m JOIN message_folder_state s ON s.message_id=m.id WHERE m.id=?`, id).Scan(&subject, &deleted); err != nil || subject != "local-edit" || deleted {
		t.Fatal("receiving replaced/deleted the local edit", subject, deleted, err)
	}
	if _, err := db.DiscardIMAPDraft(t.Context(), "acc", "<draft@mail.test>"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertProviderSyncMessages(t.Context(), []ProviderSyncMessage{remote}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM messages WHERE internet_message_id='<draft@mail.test>'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("receiving restored discarded draft", count, err)
	}
	op := claimUserProviderDraft(t, db)
	if op.Kind != "delete" || op.State.LocalMessageID != 0 || op.State.RemoteMessageID != "old-remote" {
		t.Fatal("discard lost remote cleanup identity", op)
	}
	if err := db.CompleteUserProviderDraftDelete(t.Context(), op); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_draft_states`).Scan(&count); err != nil || count != 0 {
		t.Fatal("completed deletion retained state", count, err)
	}
}

func TestUserProviderDraftAttemptCannotCrossMailboxOrCoalescedRevision(t *testing.T) {
	for _, change := range []string{"mailbox", "revision"} {
		t.Run(change, func(t *testing.T) {
			db := seedUserProviderDraft(t)
			if _, err := publishUserProviderDraft(t, db, "first"); err != nil {
				t.Fatal(err)
			}
			old := claimUserProviderDraft(t, db)
			if change == "mailbox" {
				if _, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='replacement'`); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.FailUserProviderDraft(t.Context(), old, "failed", "temporary", time.Now(), true); err != nil {
					t.Fatal(err)
				}
				if _, err := publishUserProviderDraft(t, db, "newer"); err != nil {
					t.Fatal(err)
				}
				_ = claimUserProviderDraft(t, db)
			}
			if err := db.RecordUserProviderDraftCandidate(t.Context(), old, "container", "remote"); !errors.Is(err, ErrMessageMutationSuperseded) {
				t.Fatal("stale mailbox/revision published", err)
			}
		})
	}
}

func TestUserProviderDraftPendingEditRejectsOlderBodyPublication(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			db := seedUserProviderDraft(t)
			if _, err := db.Write().Exec(`UPDATE accounts SET provider=? WHERE id='acc'`, provider); err != nil {
				t.Fatal(err)
			}
			id, err := db.PublishDraft(t.Context(), DraftMessageInput{AccountID: "acc", FolderID: "drafts", InternetMessageID: "<draft@mail.test>", Subject: "newer local edit", Date: time.Now()}, func(int64) (DraftPublication, error) {
				return DraftPublication{TextPath: "newer-local-body", ProviderSync: &QueueUserProviderDraftInput{State: UserProviderDraftState{AccountID: "acc", DraftKey: "<draft@mail.test>", FolderID: "drafts", Provider: provider, MailboxSubject: "subject"}, RevisionToken: "newer", MIMEData: []byte("newer")}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec(`UPDATE messages SET remote_message_id='old-remote' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			// This identity is still valid until draft replacement completes:
			// an older cold fetch can therefore return during the queued edit.
			err = db.SaveMessageBodyCache(t.Context(), id, "acc", MessageBodyCache{ProviderType: provider, ProviderAccountID: "subject", ProviderMessageID: "old-remote", TextPath: "older-remote-body", Parsed: &mailmessage.ParsedMessage{MessageID: "<draft@mail.test>", Subject: "older remote subject", TextBody: "older remote body"}})
			if err == nil {
				t.Fatal("older provider body overwrote a queued local draft edit")
			}
			var subject, path string
			if err := db.Read().QueryRow(`SELECT subject,body_text_path FROM messages WHERE id=?`, id).Scan(&subject, &path); err != nil {
				t.Fatal(err)
			}
			if subject != "newer local edit" || path != "newer-local-body" {
				t.Fatal("rejected cold fetch changed local draft content", subject, path)
			}
		})
	}
}
