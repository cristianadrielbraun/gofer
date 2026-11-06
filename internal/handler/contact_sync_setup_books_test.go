package handler

import (
	"context"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Setting up Gofer Sync to another address book of the same account looks
// for copies in that book only, and never offers the contact itself.
func TestContactSyncSetupLooksInTheTargetBookOnly(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	if _, err := db.Write().Exec(`INSERT OR REPLACE INTO account_contact_sync_configs(account_id,user_id,provider,enabled) VALUES('acc','default','carddav',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO account_contact_address_books(account_id,user_id,id,url,name) VALUES('acc','default','book-1','https://dav.test/1/','One'),('acc','default','book-2','https://dav.test/2/','Two')`); err != nil {
		t.Fatal(err)
	}
	save := func(name string) models.Contact {
		contact, err := db.SaveContact(ctx, "default", models.Contact{Name: name, Email: "rhea@example.com", StoredIn: "book:book-1"})
		if err != nil {
			t.Fatal(err)
		}
		return contact
	}
	self, sameBook, otherBook := save("Rhea Vance"), save("Rhea Vance"), save("Rhea Vance")
	for _, card := range []struct{ contact, book, remote string }{{self.ID, "book-1", "r-self"}, {sameBook.ID, "book-1", "r-same"}, {otherBook.ID, "book-2", "r-other"}} {
		if err := db.UpsertContactSource(ctx, storage.ContactSource{UserID: "default", ContactID: card.contact, Provider: "carddav", AccountID: "acc", AddressBookID: card.book, RemoteID: card.remote}); err != nil {
			t.Fatal(err)
		}
	}
	self.SaveTargets = []string{"book:book-1", "book:book-2"}
	candidates, problem := h.searchContactSyncSetupCandidates(ctx, "default", contactSyncAccount{ID: "acc", Provider: "carddav"}, self, []string{"rhea@example.com"})
	if problem != "" || len(candidates) != 1 || candidates[0].ContactID != otherBook.ID {
		t.Fatalf("candidates = %+v, %q", candidates, problem)
	}
	if label := h.contactSetupLocationLabel(ctx, "default", self, "acc", "acc@example.com"); label != "acc@example.com / Two" {
		t.Fatalf("label = %q", label)
	}
}
