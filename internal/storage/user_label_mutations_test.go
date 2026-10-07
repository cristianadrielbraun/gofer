package storage

import (
	"errors"
	"testing"
	"time"
)

func seedUserLabelTest(t *testing.T, provider string) (*DB, ThreadMessageMutationInfo) {
	t.Helper()
	m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{})
	db := acquireUserStore(t, m, "alice").DB()
	auth, subject := "plain", ""
	if provider != "imap" {
		auth, subject = "oauth2", "subject"
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,provider,auth_method,provider_account_id,email_address) VALUES('acc','alice',?,?,?,'alice@test.invalid')`, provider, auth, subject); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,name,role,uid_validity) VALUES('inbox','acc','INBOX','Inbox','inbox',42),('junk','acc','Junk','Junk','junk',42)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,remote_message_id,internet_message_id,subject) VALUES(1,'acc','m1','<label@test.invalid>','Label')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(1,'inbox',2)`); err != nil {
		t.Fatal(err)
	}
	info, err := db.GetMessageMutationInfoForUser(t.Context(), 1, "alice")
	if err != nil || info == nil {
		t.Fatal(err)
	}
	return db, ThreadMessageMutationInfo{MessageID: 1, MessageMutationInfo: *info}
}
func labelEntry(t *testing.T, db *DB, provider string) LabelMutationQueueEntry {
	t.Helper()
	entries, err := db.ListDueLabelMutations(t.Context(), "acc", provider, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("due label entries: %#v %v", entries, err)
	}
	return entries[0]
}
func TestUserLabelsTransactionCoalescesIntentAndRejectsStaleCompletion(t *testing.T) {
	db, info := seedUserLabelTest(t, "gmail")
	if err := db.QueueUserLabels(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "Projects", LabelMutationAdd); err != nil {
		t.Fatal(err)
	}
	old := labelEntry(t, db, LabelProviderGmail)
	if err := db.QueueUserLabels(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "projects", LabelMutationRemove); err != nil {
		t.Fatal(err)
	}
	current := labelEntry(t, db, LabelProviderGmail)
	if current.ID <= old.ID || current.Operation != "remove" {
		t.Fatal("opposite intent did not receive a new ID")
	}
	if err := db.CompleteUserLabelMutation(t.Context(), old, info.MessageMutationInfo, "subject", "m1", 42, LabelInput{Name: "Projects", ProviderType: LabelProviderGmail, ProviderID: "label-id"}); !errors.Is(err, ErrMessageMutationSuperseded) {
		t.Fatalf("stale completion: %v", err)
	}
	if err := db.FailUserLabelMutation(t.Context(), old, errors.New("old failure"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var attempts, count int
	if err := db.Read().QueryRow(`SELECT attempts FROM label_mutation_queue WHERE id=?`, current.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("old failure changed new row: %d %v", attempts, err)
	}
	if err := db.ReplaceMessageLabelsForProvider(t.Context(), 1, "acc", LabelProviderGmail, []LabelInput{{Name: "Projects", ProviderType: LabelProviderGmail, ProviderID: "label-id"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT count(*) FROM message_labels`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("receiving resurrected pending removal: %d %v", count, err)
	}
}
func TestUserLabelsAtomicQueueAndSpamRollback(t *testing.T) {
	for _, spam := range []bool{false, true} {
		t.Run(map[bool]string{false: "label", true: "spam"}[spam], func(t *testing.T) {
			db, info := seedUserLabelTest(t, "imap")
			if _, err := db.Write().Exec(`CREATE TRIGGER fail_label_queue BEFORE INSERT ON label_mutation_queue BEGIN SELECT RAISE(ABORT,'injected queue failure'); END`); err != nil {
				t.Fatal(err)
			}
			var err error
			if spam {
				err = db.QueueUserIMAPSpam(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "junk", true)
			} else {
				err = db.QueueUserLabels(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "Projects", LabelMutationAdd)
			}
			if err == nil {
				t.Fatal("failed queue publication reported success")
			}
			var count int
			if err := db.Read().QueryRow(`SELECT (SELECT count(*) FROM message_labels)+(SELECT count(*) FROM label_mutation_queue)+(SELECT count(*) FROM message_mutations)`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial action: %d %v", count, err)
			}
			if err := db.Read().QueryRow(`SELECT count(*) FROM message_folder_state WHERE folder_id='inbox' AND is_deleted=0`).Scan(&count); err != nil || count != 1 {
				t.Fatal("failed action hid source")
			}
			if _, err := db.Write().Exec(`DROP TRIGGER fail_label_queue`); err != nil {
				t.Fatal(err)
			}
			if err := db.QueueUserLabels(t.Context(), "bob", []ThreadMessageMutationInfo{info}, "Projects", LabelMutationAdd); err == nil {
				t.Fatal("foreign owner was allowed")
			}
			if spam {
				if err := db.QueueUserIMAPSpam(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "junk", true); err != nil {
					t.Fatal(err)
				}
				if err := db.Read().QueryRow(`SELECT count(*) FROM label_mutation_queue`).Scan(&count); err != nil || count != 2 {
					t.Fatal("training flags missing")
				}
				if err := db.Read().QueryRow(`SELECT count(*) FROM message_mutations WHERE kind='move'`).Scan(&count); err != nil || count != 1 {
					t.Fatal("training move missing")
				}
			}
		})
	}
}
func TestUserLabelsRejectChangedIdentityAndUIDEpoch(t *testing.T) {
	for _, mutation := range []string{"message", "subject", "folder", "epoch", "membership", "auth"} {
		t.Run(mutation, func(t *testing.T) {
			db, info := seedUserLabelTest(t, "imap")
			if err := db.QueueUserLabels(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "Projects", LabelMutationAdd); err != nil {
				t.Fatal(err)
			}
			entry := labelEntry(t, db, LabelProviderIMAPKeyword)
			statements := map[string]string{"message": `UPDATE messages SET internet_message_id='<replacement@test.invalid>'`, "subject": `UPDATE accounts SET provider_account_id='replacement'`, "folder": `UPDATE folders SET remote_id='Other' WHERE id='inbox'`, "epoch": `UPDATE folders SET uid_validity=43 WHERE id='inbox'`, "membership": `UPDATE message_folder_state SET is_deleted=1`, "auth": `UPDATE accounts SET auth_method='oauth2'`}
			if _, err := db.Write().Exec(statements[mutation]); err != nil {
				t.Fatal(err)
			}
			if err := db.CompleteUserLabelMutation(t.Context(), entry, info.MessageMutationInfo, "", "m1", 42, LabelInput{Name: "Projects", ProviderID: "Projects", ProviderType: LabelProviderIMAPKeyword}); err == nil {
				t.Fatal("changed identity was published")
			}
			var pending, remote int
			if err := db.Read().QueryRow(`SELECT (SELECT count(*) FROM label_mutation_queue),(SELECT count(*) FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE l.provider_type!='local')`).Scan(&pending, &remote); err != nil || pending != 1 || remote != 0 {
				t.Fatalf("failed guard changed state: %d %d %v", pending, remote, err)
			}
		})
	}
}
func TestUserLabelsMissingProviderIDPublicationRollsBackWithAssociations(t *testing.T) {
	db, info := seedUserLabelTest(t, "outlook")
	if _, err := db.Write().Exec(`UPDATE messages SET remote_message_id=NULL WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	info.RemoteMessageID = ""
	if err := db.QueueUserLabels(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "Projects", LabelMutationAdd); err != nil {
		t.Fatal(err)
	}
	entry := labelEntry(t, db, LabelProviderOutlook)
	if _, err := db.Write().Exec(`CREATE TRIGGER fail_remote_label BEFORE INSERT ON message_labels WHEN (SELECT provider_type FROM labels WHERE id=NEW.label_id)='outlook' BEGIN SELECT RAISE(ABORT,'injected finalization failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserLabelMutation(t.Context(), entry, info.MessageMutationInfo, "subject", "resolved", 42, LabelInput{Name: "Projects", ProviderID: "Projects", ProviderType: LabelProviderOutlook}); err == nil {
		t.Fatal("failure accepted")
	}
	var id string
	if err := db.Read().QueryRow(`SELECT COALESCE(remote_message_id,'') FROM messages WHERE id=1`).Scan(&id); err != nil || id != "" {
		t.Fatalf("identity escaped rollback: %q %v", id, err)
	}
	if _, err := db.Write().Exec(`DROP TRIGGER fail_remote_label`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteUserLabelMutation(t.Context(), entry, info.MessageMutationInfo, "subject", "resolved", 42, LabelInput{Name: "Projects", ProviderID: "Projects", ProviderType: LabelProviderOutlook}); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COALESCE(remote_message_id,'') FROM messages WHERE id=1`).Scan(&id); err != nil || id != "resolved" {
		t.Fatalf("identity not published: %q %v", id, err)
	}
	if entries, err := db.ListDueLabelMutations(t.Context(), "acc", LabelProviderOutlook, 1); err != nil || len(entries) != 0 {
		t.Fatalf("completed queue retained: %#v %v", entries, err)
	}
}

func TestUserLabelsIMAPAliasSurvivesServerKeywordCasing(t *testing.T) {
	db, info := seedUserLabelTest(t, "imap")
	if err := db.QueueUserLabels(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "Important", LabelMutationAdd); err != nil {
		t.Fatal(err)
	}
	entry := labelEntry(t, db, LabelProviderIMAPKeyword)
	if err := db.CompleteUserLabelMutation(t.Context(), entry, info.MessageMutationInfo, "", "", 42, LabelInput{Name: "Important", ProviderID: "$label1", ProviderType: LabelProviderIMAPKeyword}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceMessageLabelsForProvider(t.Context(), 1, "acc", LabelProviderIMAPKeyword, []LabelInput{{Name: "$LABEL1", ProviderID: "$LABEL1", ProviderType: LabelProviderIMAPKeyword}}); err != nil {
		t.Fatal(err)
	}
	labels, err := db.GetProviderMessageLabels(t.Context(), 1, "acc", LabelProviderIMAPKeyword)
	if err != nil || len(labels) != 1 || labels[0].Name != "Important" || labels[0].ProviderID != "$label1" {
		t.Fatalf("server casing replaced alias: %#v %v", labels, err)
	}
}

func TestUserLabelsSpamMoveWaitsForTrainingAttempt(t *testing.T) {
	db, info := seedUserLabelTest(t, "imap")
	if err := db.QueueUserIMAPSpam(t.Context(), "alice", []ThreadMessageMutationInfo{info}, "junk", true); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimDueMessageMutationsForAccount(t.Context(), "acc", time.Now(), 1); err != nil || len(claimed) != 0 {
		t.Fatalf("move overtook initial training: %#v %v", claimed, err)
	}
	// Explicit failed attempts preserve the historical folder-move fallback.
	entries, err := db.ListDueLabelMutations(t.Context(), "acc", LabelProviderIMAPKeyword, 10)
	if err != nil || len(entries) != 2 {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := db.FailUserLabelMutation(t.Context(), entry, errors.New("server rejects training flag"), time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if claimed, err := db.ClaimDueMessageMutationsForAccount(t.Context(), "acc", time.Now(), 1); err != nil || len(claimed) != 1 {
		t.Fatalf("failed training blocked fallback: %#v %v", claimed, err)
	}
}
