package storage

import (
	"context"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func contactHomeOf(t *testing.T, db *DB, id string) (string, int) {
	t.Helper()
	var home string
	var local int
	if err := db.Read().QueryRow(`SELECT home, (SELECT COUNT(*) FROM contact_cards c WHERE c.user_id = p.user_id AND c.profile_id = p.id AND c.kind = 'local' AND c.is_deleted = 0)
		FROM contact_profiles p WHERE p.user_id = 'default' AND p.id = ?`, id).Scan(&home, &local); err != nil {
		t.Fatalf("home of %s: %v", id, err)
	}
	return home, local
}

// Schema 137 gives every contact a home: Gofer Local for observed ones and
// ones with a Local card, else the account copy they came from, and Gofer
// Local, with its card back, for ones stored nowhere.
func TestMigrateV137GivesEveryContactAHome(t *testing.T) {
	db := newContactsTestDB(t)
	if _, err := db.Write().Exec(`
		INSERT INTO contact_profiles (id, user_id, display_name, origin) VALUES
			('observed-nowhere', 'default', 'Observed nowhere', 'observed'),
			('manual-nowhere', 'default', 'Manual nowhere', 'manual'),
			('manual-local', 'default', 'Manual local', 'manual'),
			('synced-book', 'default', 'Synced book', 'synced:acc'),
			('synced-two', 'default', 'Synced two', 'synced:acc'),
			('observed-on-account', 'default', 'Observed on account', 'observed');
		INSERT INTO contact_cards (id, user_id, profile_id, kind, is_deleted) VALUES
			('card-observed-gone', 'default', 'observed-nowhere', 'local', 1),
			('card-manual-local', 'default', 'manual-local', 'local', 0);
		INSERT INTO contact_cards (id, user_id, profile_id, kind, provider, account_id, address_book_id, remote_id) VALUES
			('card-book', 'default', 'synced-book', 'provider', 'carddav', 'acc', 'book-1', 'r1'),
			('card-a-other', 'default', 'synced-two', 'provider', 'gmail', 'other', '', 'r2'),
			('card-z-origin', 'default', 'synced-two', 'provider', 'gmail', 'acc', '', 'r3'),
			('card-observed-account', 'default', 'observed-on-account', 'provider', 'gmail', 'acc', '', 'r4');
		ALTER TABLE contact_profiles DROP COLUMN home;
		DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(136);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV136ToV137(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{
		"observed-nowhere":    "local",
		"manual-nowhere":      "local",
		"manual-local":        "local",
		"synced-book":         "book:book-1",
		"synced-two":          "account:acc",
		"observed-on-account": "local",
	} {
		home, local := contactHomeOf(t, db, id)
		if home != want {
			t.Errorf("%s home = %q, want %q", id, home, want)
		}
		if want == "local" && local != 1 {
			t.Errorf("%s has %d live Local cards, want 1", id, local)
		}
	}
	var restored int
	if err := db.Read().QueryRow(`SELECT is_deleted FROM contact_cards WHERE id = 'card-observed-gone'`).Scan(&restored); err != nil || restored != 0 {
		t.Fatalf("the removed Local card came back = %d, %v", restored, err)
	}
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 137 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

// Saving never takes a contact out of its home, and new contacts get one.
func TestContactHomeKeptOnSave(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	contact, err := db.SaveContact(ctx, "default", models.Contact{Name: "Kept", Email: "kept@example.com", SaveTargets: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	if home, local := contactHomeOf(t, db, contact.ID); home != "local" || local != 1 {
		t.Fatalf("new contact home = %q with %d Local cards", home, local)
	}
	// Leaving Local out of the places keeps the contact in Gofer Local.
	contact.SaveTargets = []string{"account:acc"}
	if _, err := db.SaveContact(ctx, "default", contact); err != nil {
		t.Fatal(err)
	}
	if home, local := contactHomeOf(t, db, contact.ID); home != "local" || local != 1 {
		t.Fatalf("after leaving Local out: home = %q with %d Local cards", home, local)
	}
	// So does replacing the memberships directly.
	if err := db.ReplaceContactSyncMemberships(ctx, "default", contact.ID, []string{"account:acc"}); err != nil {
		t.Fatal(err)
	}
	if _, local := contactHomeOf(t, db, contact.ID); local != 1 {
		t.Fatalf("memberships removed the Local card: %d", local)
	}

	// A contact first found on an account lives in the address book its
	// copy arrives in.
	id, _, err := db.UpsertSyncedContact(ctx, "default", "acc", "From book", "from-book@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if home, _ := contactHomeOf(t, db, id); home != "account:acc" {
		t.Fatalf("pulled contact home = %q", home)
	}
	if err := db.UpsertContactSource(ctx, ContactSource{UserID: "default", ContactID: id, Provider: "carddav", AccountID: "acc", AddressBookID: "book-1", RemoteID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if home, local := contactHomeOf(t, db, id); home != "book:book-1" || local != 0 {
		t.Fatalf("pulled contact home = %q with %d Local cards", home, local)
	}
}

// Schema 138 lets contacts share an address and splits a contact held in
// several places without Gofer Sync into one contact per place; a synced
// one stays one contact.
func TestMigrateV138SplitsContactsByPlace(t *testing.T) {
	db := newContactsTestDB(t)
	if _, err := db.Write().Exec(`
		INSERT INTO contact_profiles (id, user_id, display_name, primary_email, origin, home, sync_enabled) VALUES
			('two-places', 'default', 'Two Places', 'two@example.com', 'synced:gmail', 'account:gmail', 0),
			('linked', 'default', 'Linked', 'linked@example.com', 'manual', 'local', 1);
		INSERT INTO contact_cards (id, user_id, profile_id, kind, provider, account_id, address_book_id, remote_id) VALUES
			('two-gmail', 'default', 'two-places', 'provider', 'gmail', 'gmail', '', 'people/1'),
			('two-dav', 'default', 'two-places', 'provider', 'carddav', 'fastmail', 'book-1', 'r1'),
			('linked-dav', 'default', 'linked', 'provider', 'carddav', 'fastmail', 'book-1', 'r2');
		INSERT INTO contact_cards (id, user_id, profile_id, kind) VALUES ('linked-local', 'default', 'linked', 'local');
		INSERT INTO contact_fields (id, user_id, profile_id, kind, value, normalized_value, is_primary, source) VALUES
			('f1', 'default', 'two-places', 'email', 'two@example.com', 'two@example.com', 1, 'synced:gmail'),
			('f2', 'default', 'two-places', 'phone', '111', '111', 1, 'synced:gmail'),
			('f3', 'default', 'two-places', 'email', 'two@example.com', 'two@example.com', 1, 'synced:fastmail'),
			('f4', 'default', 'two-places', 'phone', '222', '222', 1, 'synced:fastmail');
		DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(137);`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV137ToV138(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	var split, splitHome, splitOrigin string
	if err := db.Read().QueryRow(`SELECT profile_id FROM contact_cards WHERE id = 'two-dav'`).Scan(&split); err != nil || split == "two-places" {
		t.Fatal("the Fastmail copy stayed on the contact", split, err)
	}
	if err := db.Read().QueryRow(`SELECT home, origin FROM contact_profiles WHERE id = ?`, split).Scan(&splitHome, &splitOrigin); err != nil || splitHome != "book:book-1" || splitOrigin != "synced:fastmail" {
		t.Fatal("split contact", splitHome, splitOrigin, err)
	}
	phones := func(id string) string {
		var phones string
		if err := db.Read().QueryRow(`SELECT COALESCE(group_concat(value), '') FROM contact_fields WHERE profile_id = ? AND kind = 'phone'`, id).Scan(&phones); err != nil {
			t.Fatal(err)
		}
		return phones
	}
	if got := phones("two-places"); got != "111" {
		t.Fatal("the Gmail contact kept the Fastmail phone", got)
	}
	if got := phones(split); got != "222" {
		t.Fatal("the Fastmail contact lost its phone", got)
	}
	var sharing int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_identities WHERE kind = 'email' AND normalized_value = 'two@example.com'`).Scan(&sharing); err != nil || sharing != 2 {
		t.Fatal("both contacts have the address", sharing, err)
	}
	var linked int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE profile_id = 'linked' AND is_deleted = 0`).Scan(&linked); err != nil || linked != 2 {
		t.Fatal("the synced contact was split", linked, err)
	}

	// A contact found on another account is its own contact now, though it
	// has the same address.
	ctx := context.Background()
	other, _, err := db.UpsertSyncedContact(ctx, "default", "work", "Two Again", "two@example.com")
	if err != nil || other == "two-places" || other == split {
		t.Fatal("pull merged by address", other, err)
	}
}
