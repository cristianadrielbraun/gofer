package mail

import (
	"encoding/json"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestJMAPSharedRefs(t *testing.T) {
	own, shared := jmapTarget{account: "a1"}, jmapTarget{account: "a2", mailbox: "support@example.test"}
	if own.ref("e1") != "e1" || shared.ref("e1") != "a2:e1" || shared.ref("") != "" {
		t.Fatalf("refs = %q %q", own.ref("e1"), shared.ref("e1"))
	}
	if id, ok := shared.owns("a2:e1"); !ok || id != "e1" {
		t.Fatalf("shared owns = %q %v", id, ok)
	}
	if _, ok := shared.owns("e1"); ok {
		t.Fatal("a shared target owns an id of the login's own account")
	}
	if _, ok := own.owns("a2:e1"); ok {
		t.Fatal("the login's own target owns a shared id")
	}
	if own.scope("email") != "email" || shared.scope("email") != "email@a2" {
		t.Fatalf("scopes = %q %q", own.scope("email"), shared.scope("email"))
	}
	session := &jmap.Session{
		PrimaryAccounts: map[string]string{jmap.CapabilityMail: "a1"},
		Accounts: map[string]jmap.Account{
			"a1": {Name: "robin@example.test", IsPersonal: true},
			"a2": {Name: "Support@Example.test", AccountCapabilities: map[string]json.RawMessage{jmap.CapabilityMail: nil, jmap.CapabilitySubmission: nil}},
			"a3": {Name: "Team calendar", AccountCapabilities: map[string]json.RawMessage{jmap.CapabilityMail: nil}},
			"a4": {Name: "files@example.test", AccountCapabilities: map[string]json.RawMessage{"urn:ietf:params:jmap:filenode": nil}},
		},
	}
	targets := jmapSharedAccounts(session)
	if len(targets) != 1 || targets[0].account != "a2" || targets[0].mailbox != "support@example.test" {
		t.Fatalf("shared accounts = %+v", targets)
	}
	if got := jmapSenderAccount(session, "SUPPORT@example.test"); got.account != "a2" {
		t.Fatalf("sender account = %+v", got)
	}
	if got := jmapSenderAccount(session, "Gofer Support <Support@Example.test>"); got.account != "a2" {
		t.Fatalf("sender account for a From header = %+v", got)
	}
	if got := jmapSenderAccount(session, "robin@example.test"); got.account != "a1" {
		t.Fatalf("own sender account = %+v", got)
	}
	if account, id, err := jmapAccountOf(session, "a2:e9"); err != nil || account != "a2" || id != "e9" {
		t.Fatalf("account of = %q %q %v", account, id, err)
	}
	if _, _, err := jmapAccountOf(session, "a9:e9"); err == nil {
		t.Fatal("an account the session no longer lists was accepted")
	}
	if jmapRefIn(session, "a1", "e1") != "e1" || jmapRefIn(session, "a2", "e1") != "a2:e1" {
		t.Fatal("jmapRefIn")
	}
}

func TestSyncJMAPSharedMailbox(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.sharedAccount = "support@example.test"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", TotalEmails: 1, UnreadEmails: 1, MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent Items", Role: "sent", MyRights: readable()},
	)
	server.addEmail(testJMAPEmail("e1", "Hello", 5, "mb-in"))
	useFakeJMAPAccount(t, f, server)

	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	folders := jmapFolders(t, f)
	own, shared := folders["mb-in"], folders["a2:mb-in"]
	if own.Role != "inbox" || own.ID == "" {
		t.Fatalf("own inbox = %+v", own)
	}
	if shared.ID == "" || shared.ID == own.ID || shared.Role != "custom" || shared.RemoteID != "support@example.test/Inbox" {
		t.Fatalf("shared inbox = %+v", shared)
	}
	var mailbox, role string
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT shared_mailbox, shared_role FROM folders WHERE id = ?`, shared.ID).Scan(&mailbox, &role)
	})
	if mailbox != "support@example.test" || role != "inbox" {
		t.Fatalf("shared folder mailbox=%q role=%q", mailbox, role)
	}
	messages := jmapMessages(t, f)
	if messages["e1"] == nil || messages["a2:e1"] == nil || !messages["a2:e1"].Folders["a2:mb-in"] {
		t.Fatalf("messages = %+v", messages)
	}
	var registered []storage.SharedMailbox
	f.local(t, "alice", func(db *storage.DB) error {
		var err error
		registered, err = db.ListSharedMailboxes(t.Context(), f.ids["alice"])
		return err
	})
	if len(registered) != 1 || registered[0].Email != "support@example.test" || !registered[0].Enabled {
		t.Fatalf("shared mailboxes = %+v", registered)
	}

	// Turned off, the shared mailbox keeps its folders and gets no new mail.
	f.local(t, "alice", func(db *storage.DB) error {
		return db.SetSharedMailboxEnabled(t.Context(), f.ids["alice"], "support@example.test", false)
	})
	server.addEmail(testJMAPEmail("e2", "Later", 1, "mb-in"))
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}
	messages = jmapMessages(t, f)
	if messages["e2"] == nil || messages["a2:e2"] != nil {
		t.Fatalf("after turning the shared mailbox off: %+v", messages)
	}
	if folders := jmapFolders(t, f); folders["a2:mb-in"].ID != shared.ID || !folders["a2:mb-in"].Selectable {
		t.Fatalf("shared folder after turning off = %+v", folders["a2:mb-in"])
	}

	// No longer shared, its folders are put aside.
	server.sharedAccount = ""
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("third Sync() error = %v", err)
	}
	if folders := jmapFolders(t, f); folders["a2:mb-in"].Selectable {
		t.Fatalf("shared folder no longer shared = %+v", folders["a2:mb-in"])
	}
	if folders := jmapFolders(t, f); !folders["mb-in"].Selectable {
		t.Fatal("the login's own inbox was put aside with the shared mailbox")
	}
}
