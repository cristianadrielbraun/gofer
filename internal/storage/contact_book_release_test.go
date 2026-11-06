package storage

import (
	"context"
	"slices"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Stopping an address book drops its copies from Gofer and moves or removes
// the contacts living there, as asked.
func TestReleaseContactAddressBook(t *testing.T) {
	for _, release := range []ContactBookRelease{ContactBookKeepInLocal, ContactBookRemove} {
		t.Run(string(release), func(t *testing.T) {
			ctx := context.Background()
			db := newContactsTestDB(t)
			if _, err := db.Write().Exec(`
				INSERT INTO accounts(id,user_id,provider,email_address) VALUES('acc','default','imap','acc@example.com');
				INSERT INTO account_contact_address_books(account_id,user_id,id,url) VALUES('acc','default','book-1','https://dav.test/book-1/');`); err != nil {
				t.Fatal(err)
			}
			only, err := db.SaveContact(ctx, "default", models.Contact{Name: "Ola Only", Email: "ola@example.com", StoredIn: "book:book-1"})
			if err != nil {
				t.Fatal(err)
			}
			both, err := db.SaveContact(ctx, "default", models.Contact{Name: "Bo Both", Email: "bo@example.com", StoredIn: "book:book-1"})
			if err != nil {
				t.Fatal(err)
			}
			copied, err := db.SaveContact(ctx, "default", models.Contact{Name: "Cy Copy", Email: "cy@example.com", StoredIn: "local", GoferSyncEnabled: true, SaveTargets: []string{"local", "book:book-1"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec(`
				INSERT INTO contact_cards(id,user_id,profile_id,kind,provider,account_id,address_book_id,remote_id) VALUES
					('c1','default',?,'provider','carddav','acc','book-1','r1'),
					('c2','default',?,'provider','carddav','acc','book-1','r2'),
					('c3','default',?,'provider','carddav','acc','book-1','r3')`, only.ID, both.ID, copied.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().Exec(`INSERT INTO contact_cards(id,user_id,profile_id,kind) VALUES('l2','default',?,'local')`, both.ID); err != nil {
				t.Fatal(err)
			}
			usage, err := db.ContactAddressBookUsage(ctx, "default", "book-1")
			if err != nil {
				t.Fatal(err)
			}
			if usage != (ContactBookUsage{Living: 2, OnlyThere: 1, CopiesOnly: 1}) {
				t.Fatalf("usage = %+v", usage)
			}
			moved, removed, err := db.ReleaseContactAddressBook(ctx, "default", "book-1", release, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantMoved, wantRemoved := 2, 0
			if release == ContactBookRemove {
				wantMoved, wantRemoved = 1, 1
			}
			if moved != wantMoved || removed != wantRemoved {
				t.Fatalf("moved, removed = %d, %d, want %d, %d", moved, removed, wantMoved, wantRemoved)
			}
			state := func(id string) (home string, deleted bool) {
				if err := db.Read().QueryRow(`SELECT home, is_deleted FROM contact_profiles WHERE id = ?`, id).Scan(&home, &deleted); err != nil {
					t.Fatal(err)
				}
				return
			}
			if home, deleted := state(only.ID); release == ContactBookRemove && !deleted || release == ContactBookKeepInLocal && (deleted || home != "local") {
				t.Errorf("contact only in the book: home %q, deleted %v", home, deleted)
			}
			if home, deleted := state(both.ID); deleted || home != "local" {
				t.Errorf("contact with a Local copy: home %q, deleted %v", home, deleted)
			}
			if home, deleted := state(copied.ID); deleted || home != "local" {
				t.Errorf("contact copied to the book: home %q, deleted %v", home, deleted)
			}
			var live, members int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE address_book_id = 'book-1' AND is_deleted = 0`).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_memberships WHERE address_book_id = 'book-1'`).Scan(&members); err != nil {
				t.Fatal(err)
			}
			if live != 0 || members != 0 {
				t.Errorf("book copies left: %d cards, %d memberships", live, members)
			}
		})
	}
}

// A contact living in the book and synced elsewhere is listed with its other
// places, and removing the book moves it to the one chosen for it.
func TestReleaseContactAddressBookMovesToTheChosenPlace(t *testing.T) {
	ctx := context.Background()
	db := newContactsTestDB(t)
	if _, err := db.Write().Exec(`
		INSERT INTO accounts(id,user_id,provider,email_address) VALUES('acc','default','imap','acc@example.com');
		INSERT INTO account_contact_address_books(account_id,user_id,id,url) VALUES('acc','default','book-1','https://dav.test/book-1/'),('acc','default','book-2','https://dav.test/book-2/');`); err != nil {
		t.Fatal(err)
	}
	mover, err := db.SaveContact(ctx, "default", models.Contact{Name: "Mo Mover", Email: "mo@example.com", StoredIn: "book:book-1", GoferSyncEnabled: true, SaveTargets: []string{"book:book-1", "book:book-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO contact_cards(id,user_id,profile_id,kind,provider,account_id,address_book_id,remote_id) VALUES('m1','default',?,'provider','carddav','acc','book-1','r1'),('m2','default',?,'provider','carddav','acc','book-2','r2')`, mover.ID, mover.ID); err != nil {
		t.Fatal(err)
	}
	movers, err := db.ContactAddressBookMovers(ctx, "default", "book-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(movers) != 1 || movers[0].ID != mover.ID || !slices.Equal(movers[0].Places, []string{"local", "book:book-2"}) || !slices.Equal(movers[0].Copies, []string{"book:book-2"}) {
		t.Fatalf("movers = %+v", movers)
	}
	if _, _, err := db.ReleaseContactAddressBook(ctx, "default", "book-1", ContactBookRemove, map[string]string{mover.ID: "book:book-2"}); err != nil {
		t.Fatal(err)
	}
	contact, err := db.GetContact(ctx, "default", mover.ID)
	if err != nil || contact == nil || contact.StoredIn != "book:book-2" {
		t.Fatalf("moved contact = %+v, %v", contact, err)
	}
	var syncing int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_memberships WHERE profile_id = ? AND address_book_id = 'book-2' AND enabled = 1`, mover.ID).Scan(&syncing); err != nil || syncing != 1 {
		t.Fatalf("book-2 sync = %d, %v", syncing, err)
	}
}
