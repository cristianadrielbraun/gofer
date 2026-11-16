package storage

import (
	"context"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
)

func identityByEmail(t *testing.T, db *DB, email string) (models.Identity, int) {
	t.Helper()
	identities, err := db.ListAccountIdentitiesInternal(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	var found models.Identity
	n := 0
	for _, identity := range identities {
		if identity.Email == email {
			found = identity
			n++
		}
	}
	return found, n
}

func sharedLinkState(t *testing.T, db *DB, email string) string {
	t.Helper()
	links, err := db.ListSharedMailboxIdentities(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.MailboxEmail == email {
			return link.State
		}
	}
	return ""
}

// sharedIdentitiesOn turns on, for the default user, the From addresses and
// sender check that are off until the user chooses them.
func sharedIdentitiesOn(t *testing.T, db *DB) {
	t.Helper()
	if err := db.MergeUISettings(context.Background(), "default", map[string]string{sharedmailbox.KeyAutoIdentities: "true", sharedmailbox.KeyVerifySender: "true"}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedMailboxDiscoveryCreatesOneIdentityEach(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	sharedIdentitiesOn(t, db)
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	seen := []string{"support@example.com", "sales@example.com", "Support@Example.com"}

	for i := 0; i < 100; i++ {
		mailboxes, identities, err := db.ApplySharedMailboxDiscovery(ctx, "work", seen, true, at)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (len(mailboxes.Added) != 2 || len(identities.Created) != 2 || len(identities.Unchecked) != 2) {
			t.Fatalf("first run: %+v %+v", mailboxes, identities)
		}
		if i > 0 && (mailboxes.Changed() || identities.Changed()) {
			t.Fatalf("run %d changed something: %+v %+v", i, mailboxes, identities)
		}
	}
	support, n := identityByEmail(t, db, "support@example.com")
	if n != 1 || support.Name != "Example Support" || support.Source != models.IdentitySourceManual {
		t.Fatalf("support identity = %+v (%d)", support, n)
	}
	// Other accounts get nothing.
	if others, _ := db.ListAccountIdentitiesInternal(ctx, "home"); len(others) != 0 {
		t.Fatalf("other account identities = %+v", others)
	}
}

func TestSharedMailboxDiscoveryKeepsWhatTheUserDid(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	sharedIdentitiesOn(t, db)
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	// An identity the user made for the address beforehand is left alone.
	manual, err := db.SaveAccountIdentityGuarded(ctx, "default", models.Identity{AccountID: "work", Name: "Our Sales", Email: "sales@example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support@example.com", "sales@example.com"}, true, at); err != nil {
		t.Fatal(err)
	}
	if sales, n := identityByEmail(t, db, "sales@example.com"); n != 1 || sales.ID != manual.ID || sales.Name != "Our Sales" {
		t.Fatalf("sales = %+v (%d)", sales, n)
	}

	// A name the user changed is kept, even when the naming rules change.
	support, _ := identityByEmail(t, db, "support@example.com")
	support.Name = "Help Desk"
	if _, err := db.SaveAccountIdentityGuarded(ctx, "default", support, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MergeUISettings(ctx, "default", map[string]string{sharedmailbox.KeyPrefixMode: sharedmailbox.PrefixNone}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support@example.com", "sales@example.com", "team@example.com"}, true, at); err != nil {
		t.Fatal(err)
	}
	if got, _ := identityByEmail(t, db, "support@example.com"); got.Name != "Help Desk" {
		t.Fatalf("user's name replaced: %+v", got)
	}
	if team, _ := identityByEmail(t, db, "team@example.com"); team.Name != "Team" {
		t.Fatalf("new name under changed rules = %+v", team)
	}

	// A deleted identity is not made again.
	team, _ := identityByEmail(t, db, "team@example.com")
	if err := db.DeleteAccountIdentityGuarded(ctx, "default", "work", team.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support@example.com", "sales@example.com", "team@example.com"}, true, at); err != nil {
		t.Fatal(err)
	}
	if _, n := identityByEmail(t, db, "team@example.com"); n != 0 {
		t.Fatal("deleted identity came back")
	}
	if state := sharedLinkState(t, db, "team@example.com"); state != SharedIdentityDeleted {
		t.Fatalf("team state = %q", state)
	}

	// Turned off by the user, it stays off.
	if err := db.SetSharedIdentityActive(ctx, "default", "work", support.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support@example.com", "sales@example.com", "team@example.com"}, true, at); err != nil {
		t.Fatal(err)
	}
	if state := sharedLinkState(t, db, "support@example.com"); state != SharedIdentityDisabled {
		t.Fatalf("support state = %q", state)
	}
}

func TestSharedMailboxRemovalPutsIdentityAside(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	sharedIdentitiesOn(t, db)
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	both := []string{"support@example.com", "sales@example.com"}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", both, true, at); err != nil {
		t.Fatal(err)
	}

	// A listing that can't be trusted to be complete removes nothing.
	mailboxes, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", nil, false, at)
	if err != nil || mailboxes.Changed() {
		t.Fatalf("incomplete listing: %+v, %v", mailboxes, err)
	}

	mailboxes, identities, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support@example.com"}, true, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(mailboxes.Removed) != 1 || mailboxes.Removed[0] != "sales@example.com" || len(identities.Deactivated) != 1 {
		t.Fatalf("removal: %+v %+v", mailboxes, identities)
	}
	if _, n := identityByEmail(t, db, "sales@example.com"); n != 1 {
		t.Fatal("identity of a removed mailbox was deleted")
	}
	if state := sharedLinkState(t, db, "sales@example.com"); state != SharedIdentityInactive {
		t.Fatalf("sales state = %q", state)
	}
	if state := sharedLinkState(t, db, "support@example.com"); state != SharedIdentityActive {
		t.Fatalf("support state = %q", state)
	}

	// Shared again, it comes back with the same identity.
	before, _ := identityByEmail(t, db, "sales@example.com")
	mailboxes, identities, err = db.ApplySharedMailboxDiscovery(ctx, "work", both, true, at.Add(2*time.Hour))
	if err != nil || len(mailboxes.Restored) != 1 || len(identities.Reactivated) != 1 {
		t.Fatalf("restore: %+v %+v %v", mailboxes, identities, err)
	}
	if after, n := identityByEmail(t, db, "sales@example.com"); n != 1 || after.ID != before.ID {
		t.Fatalf("restored identity = %+v (%d)", after, n)
	}
}

// Until the user chooses them, shared mailboxes get no From address.
func TestSharedMailboxDiscoveryMakesNoIdentityByDefault(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	if _, identities, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support@example.com"}, true, time.Now()); err != nil || identities.Changed() {
		t.Fatalf("identities = %+v, %v", identities, err)
	}
	if _, n := identityByEmail(t, db, "support@example.com"); n != 0 {
		t.Fatal("identity made without the user choosing it")
	}
}

func TestSharedMailboxFolderFindsFolderByRole(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "own-sent", AccountID: "work", RemoteID: "Sent Items", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "root", AccountID: "work", RemoteID: "Shared Folders/support@example.com", Name: "support@example.com", Role: "custom", SharedMailbox: "support@example.com", SharedRole: "root"},
		{ID: "support-sent", AccountID: "work", RemoteID: "Shared Folders/support@example.com/Gesendet", Name: "Gesendet", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "sent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id, remote, ok, err := db.SharedMailboxFolder(ctx, "work", "Support@example.com", "sent")
	if err != nil || !ok || id != "support-sent" || remote != "Shared Folders/support@example.com/Gesendet" {
		t.Fatalf("shared sent = %q %q %v %v", id, remote, ok, err)
	}
	if _, _, ok, _ := db.SharedMailboxFolder(ctx, "work", "support@example.com", "drafts"); ok {
		t.Fatal("found a Drafts the mailbox does not have")
	}
	if mailbox, _ := db.FolderSharedMailbox(ctx, "support-sent"); mailbox != "support@example.com" {
		t.Fatalf("folder mailbox = %q", mailbox)
	}
	if mailbox, _ := db.FolderSharedMailbox(ctx, "own-sent"); mailbox != "" {
		t.Fatalf("own folder mailbox = %q", mailbox)
	}
}

func TestSharedMailboxNamedByUserNameGetsNoIdentity(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	sharedIdentitiesOn(t, db)
	mailboxes, identities, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support", "sales@example.com"}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(mailboxes.Added) != 2 || len(identities.Created) != 1 || identities.Created[0] != "sales@example.com" {
		t.Fatalf("mailboxes %+v identities %+v", mailboxes, identities)
	}
}

func TestSentAndDraftsFolderForASharedSender(t *testing.T) {
	ctx := context.Background()
	db := newIdentitiesTestDB(t)
	sharedIdentitiesOn(t, db)
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "own-sent", AccountID: "work", RemoteID: "Sent Items", Name: "Sent", Role: "sent", Selectable: true},
		{ID: "own-drafts", AccountID: "work", RemoteID: "Drafts", Name: "Drafts", Role: "drafts", Selectable: true},
		{ID: "support-sent", AccountID: "work", RemoteID: "Shared/support@example.com/Sent Items", Name: "Sent Items", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "sent"},
		{ID: "support-drafts", AccountID: "work", RemoteID: "Shared/support@example.com/Drafts", Name: "Drafts", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "drafts"},
		// A shared mailbox without a Drafts folder.
		{ID: "sales-sent", AccountID: "work", RemoteID: "Shared/sales@example.com/Sent", Name: "Sent", Role: "custom", Selectable: true, SharedMailbox: "sales@example.com", SharedRole: "sent"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"support@example.com", "sales@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	check := func(name string, got func() (string, string, error), wantID string) {
		t.Helper()
		id, _, err := got()
		if err != nil || id != wantID {
			t.Errorf("%s = %q, %v; want %q", name, id, err, wantID)
		}
	}
	check("own Sent", func() (string, string, error) { return db.SentFolderFor(ctx, "work", "ana@example.com") }, "own-sent")
	check("support Sent", func() (string, string, error) { return db.SentFolderFor(ctx, "work", "Support@Example.com") }, "support-sent")
	check("support Drafts", func() (string, string, error) { return db.DraftsFolderFor(ctx, "work", "support@example.com") }, "support-drafts")
	check("sales Drafts falls back", func() (string, string, error) { return db.DraftsFolderFor(ctx, "work", "sales@example.com") }, "own-drafts")

	// Once the mailbox is no longer shared, its Sent is not written to.
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "work", []string{"sales@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	check("removed support Sent", func() (string, string, error) { return db.SentFolderFor(ctx, "work", "support@example.com") }, "own-sent")

	// What the server said is kept on the identity.
	if err := db.RecordSharedSenderResult(ctx, "work", "sales@example.com", false, "550 5.7.1 not allowed"); err != nil {
		t.Fatal(err)
	}
	sales, _ := identityByEmail(t, db, "sales@example.com")
	if sales.SharedMailbox != "sales@example.com" || sales.SenderCheck != SenderCheckRejected || sales.SenderCheckMessage != "550 5.7.1 not allowed" {
		t.Fatalf("sales identity = %+v", sales)
	}
	if support, _ := identityByEmail(t, db, "support@example.com"); support.SharedState != SharedIdentityInactive || support.Usable() {
		t.Fatalf("support identity = %+v", support)
	}
}
