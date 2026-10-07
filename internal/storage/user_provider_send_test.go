package storage

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func seedUserProviderSend(t *testing.T) (*DB, OutgoingSend) {
	t.Helper()
	db, _ := seedUserLabelTest(t, "gmail")
	input := outgoingTestInput("acc", 0, time.Now(), false)
	input.Transport = OutgoingTransportGmail
	if _, err := db.QueueOutgoingSend(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), "acc", time.Now().Add(time.Second), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	return db, claimed[0]
}

func TestUserProviderSendAcceptanceAtomicAndRecoveryNeverRequeues(t *testing.T) {
	db, send := seedUserProviderSend(t)
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_send_acceptance BEFORE UPDATE OF status ON outgoing_sends WHEN NEW.status='sent' BEGIN SELECT RAISE(ABORT,'injected acceptance publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderSend(t.Context(), send, "subject", "<sent@test.invalid>", "remote"); err == nil {
		t.Fatal("failed publication accepted")
	}
	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_send_receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial acceptance receipt", count, err)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER reject_send_acceptance`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderSend(t.Context(), send, "subject", "<sent@test.invalid>", "remote"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecoverAccountMailQueue(t.Context(), "acc"); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), "acc", time.Now().Add(time.Hour), 1)
	if err != nil || len(claimed) != 0 {
		t.Fatal("accepted send was requeued", err)
	}
	stored, err := db.GetOutgoingSend(t.Context(), send.ID)
	if err != nil || stored.Status != OutgoingSendSent || stored.SentCopyStatus != SentCopyPending || len(stored.MIMEData) == 0 {
		t.Fatal("accepted snapshot did not retain cache recovery work", err)
	}
}

func TestUserProviderSendRejectsStaleAttemptAndMailboxIdentity(t *testing.T) {
	for _, change := range []string{"attempt", "subject", "provider", "auth", "deleting"} {
		t.Run(change, func(t *testing.T) {
			db, send := seedUserProviderSend(t)
			switch change {
			case "attempt":
				send.AttemptCount++
			case "subject":
				_, _ = db.Write().Exec(`UPDATE accounts SET provider_account_id='changed'`)
			case "provider":
				_, _ = db.Write().Exec(`UPDATE accounts SET provider='outlook'`)
			case "auth":
				_, _ = db.Write().Exec(`UPDATE accounts SET auth_method='plain'`)
			case "deleting":
				_, _ = db.Write().Exec(`UPDATE accounts SET is_deleting=1`)
			}
			if err := db.CompleteUserProviderSend(t.Context(), send, "subject", "<sent@test.invalid>", "remote"); !errors.Is(err, ErrMessageMutationSuperseded) {
				t.Fatal("stale acceptance published", err)
			}
			var count int
			if err := db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_send_receipts`).Scan(&count); err != nil || count != 0 {
				t.Fatal("stale receipt published", count, err)
			}
		})
	}
}

func TestUserProviderSendCacheCompletionRollsBackIdentityAndPayload(t *testing.T) {
	db, send := seedUserProviderSend(t)
	if err := db.CompleteUserProviderSend(t.Context(), send, "subject", "<sent@test.invalid>", "remote"); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimDueSentCopiesForAccount(t.Context(), "acc", time.Now().Add(time.Second), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal("cache claim", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,name,remote_id,role) VALUES('sent','acc','Sent','SENT','sent')`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSyncMessages(t.Context(), []SyncMessage{{AccountID: "acc", FolderID: "sent", MessageID: "<sent@test.invalid>", Subject: "Sent", FromEmail: "alice@test.invalid", DateSent: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`CREATE TRIGGER reject_cache_completion BEFORE DELETE ON gofer_provider_send_receipts BEGIN SELECT RAISE(ABORT,'injected cache completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderSentCache(t.Context(), claimed[0], "subject", "<sent@test.invalid>", "sent", "remote"); err == nil {
		t.Fatal("failed completion reported success")
	}
	stored, err := db.GetOutgoingSend(t.Context(), send.ID)
	if err != nil || stored.SentCopyStatus != SentCopyCopying || len(stored.MIMEData) == 0 {
		t.Fatal("failed cache completion discarded payload", err)
	}
	var remote string
	if err := db.Read().QueryRow(`SELECT COALESCE(remote_message_id,'') FROM messages WHERE internet_message_id='<sent@test.invalid>'`).Scan(&remote); err != nil || remote != "" {
		t.Fatal("failed cache completion leaked remote identity", err)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER reject_cache_completion`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderSentCache(t.Context(), claimed[0], "subject", "<sent@test.invalid>", "sent", "remote"); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserProviderSentCache(t.Context(), claimed[0], "subject", "<sent@test.invalid>", "sent", "remote"); !errors.Is(err, ErrMessageMutationSuperseded) {
		t.Fatal("old cache completion accepted", err)
	}
}

func TestUserProviderSendReceiptRemainsBoundToAcceptedMailbox(t *testing.T) {
	db, send := seedUserProviderSend(t)
	if err := db.CompleteUserProviderSend(t.Context(), send, "subject", "<sent@test.invalid>", "remote"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='replacement-subject'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UserProviderSendReceipt(t.Context(), send.ID, send.AccountID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("accepted mail was attached to a replacement mailbox", err)
	}
	var retained int
	if err := db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_send_receipts`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("mailbox mismatch destroyed acceptance proof", err)
	}
}
