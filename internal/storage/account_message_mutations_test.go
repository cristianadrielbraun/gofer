package storage

import (
	"testing"
	"time"
)

func TestAccountMessageMutationClaimsAndRecoveryDoNotTouchOtherAccount(t *testing.T) {
	db, first := seedMessageMutationTest(t, "imap", []UpsertFolderInput{
		{ID: "inbox", AccountID: "acc", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	})
	ctx := t.Context()
	if _, err := db.Write().Exec("INSERT INTO accounts(id,user_id,provider,email_address) VALUES ('second','default','imap','second@example.com')"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{{ID: "second-inbox", AccountID: "second", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSyncMessages(ctx, []SyncMessage{{AccountID: "second", FolderID: "second-inbox", RemoteUID: 1, MessageID: "<second@example.com>", Subject: "Second", DateSent: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	second, err := db.GetMessageLocalIDByInternetIDInternal(ctx, "second", "<second@example.com>")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first, second} {
		if err := db.SetMessageReadAndQueue(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := db.ClaimDueMessageMutationsForAccount(ctx, "second", time.Now().Add(time.Second), 25)
	if err != nil || len(claimed) != 1 || claimed[0].AccountID != "second" {
		t.Fatalf("account claim: %#v %v", claimed, err)
	}
	if count, err := db.RecoverMessageMutationsForAccount(ctx, "acc"); err != nil || count != 0 {
		t.Fatalf("foreign processing row reset: %d %v", count, err)
	}
	other, err := db.GetMessageMutationInternal(ctx, claimed[0].ID)
	if err != nil || other.Status != MessageMutationProcessing {
		t.Fatal("recovery changed the other account's live claim")
	}
	if count, err := db.RecoverMessageMutationsForAccount(ctx, "second"); err != nil || count != 1 {
		t.Fatalf("owned interrupted row not recovered: %d %v", count, err)
	}
	if next, err := db.NextMessageMutationAttempt(ctx, "acc"); err != nil || next.IsZero() {
		t.Fatalf("pending deadline: %v %v", next, err)
	}
	if _, err := db.ClaimDueMessageMutationsForAccount(ctx, "", time.Now(), 1); err == nil {
		t.Fatal("empty account permitted a global claim")
	}
}
