package notifications

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedHTTPContactPreview(t *testing.T, f *userStorageFixture, owner string) {
	t.Helper()
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		_, err := db.SaveContactProfile(t.Context(), owner, models.ContactProfile{ID: "same-preview-donor", DisplayName: owner + " edited", PrimaryEmail: owner + "-imported@example.com",
			Cards:  []models.ContactCard{{ID: "same-preview-card", Kind: "provider", Provider: "carddav", AccountID: f.accounts[owner].ID, AddressBookID: owner + "-owned-book", RemoteID: owner + ".vcf", Etag: owner + "-etag", RawPayload: owner + "-raw", RawPayloadType: "text/vcard", SyncStatus: "pending", LastError: "retained"}},
			Fields: []models.ContactField{{ID: "same-preview-email", CardID: "same-preview-card", Kind: "email", Value: owner + "-imported@example.com", Source: "synced:" + f.accounts[owner].ID}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactPreviewHTTPAtomicStoredValuesAndOwnerIsolation(t *testing.T) {
	f, fake, _ := newOwnedDAVSetupFixture(t)
	alice, bob := stagedContactSetup(t, f, "alice"), stagedContactSetup(t, f, "bob")
	seedHTTPContactPreview(t, f, "alice")
	seedHTTPContactPreview(t, f, "bob")
	path := "/api/contacts/" + alice + "/sync-setup"
	if response := f.request("alice", "GET", path, ""); response.Code != 200 || !strings.Contains(response.Body.String(), path+"/findings") {
		t.Fatal("setup dialog", response.Code, response.Body.String())
	}
	if response := f.request("bob", "GET", path, ""); response.Code != 404 {
		t.Fatal("foreign setup dialog", response.Code)
	}
	path += "/preview"
	key := "candidate_" + f.accounts["alice"].ID
	for _, invalid := range []string{"stored:missing", "gmail:people/other"} {
		if response := requestContactEditor(t.Context(), f, "alice", path, url.Values{key: {invalid}}); response.Code != 400 {
			t.Fatal("invalid candidate accepted", invalid, response.Code, response.Body.String())
		}
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_preview_http BEFORE UPDATE ON contact_cards WHEN NEW.id='same-preview-card' BEGIN SELECT RAISE(ABORT,'synthetic preview failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{key: {"stored:same-preview-donor"}, "email": {"injected@example.com"}, "save_targets": {"book:bob-owned-book"}}
	response := requestContactEditor(t.Context(), f, "alice", path, form)
	if response.Code != 503 || strings.Contains(response.Body.String(), "preferred_email") {
		t.Fatal("failed preview returned choices", response.Code, response.Body.String())
	}
	check := func(owner, recipient string, attached bool) {
		t.Helper()
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var profile, raw, tag, book string
			if err := db.Read().QueryRow(`SELECT profile_id,raw_payload,etag,address_book_id FROM contact_cards WHERE id='same-preview-card'`).Scan(&profile, &raw, &tag, &book); err != nil {
				return err
			}
			want := "same-preview-donor"
			if attached {
				want = recipient
			}
			if profile != want || raw != owner+"-raw" || tag != owner+"-etag" || book != owner+"-owned-book" {
				t.Fatal("card metadata or owner changed", owner, profile, raw, tag, book)
			}
			var fields, queued int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE profile_id=? AND source=? AND value=?`, recipient, "synced:"+f.accounts[owner].ID, owner+"-imported@example.com").Scan(&fields); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued); err != nil {
				return err
			}
			if (fields == 1) != attached || fields > 1 || queued != 0 {
				t.Fatal("partial or premature publication", owner, fields, queued)
			}
			contact, err := db.GetContact(t.Context(), owner, recipient)
			if err != nil {
				return err
			}
			if contact == nil || contact.GoferSyncEnabled || contact.Email != owner+"-setup@example.com" {
				t.Fatal("preview changed canonical values", contact)
			}
			var donorCard sql.NullString
			if err := db.Read().QueryRow(`SELECT card_id FROM contact_fields WHERE id='same-preview-email'`).Scan(&donorCard); err != nil {
				return err
			}
			if donorCard.Valid == attached {
				t.Fatal("donor field binding", donorCard, attached)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check("alice", alice, false)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER reject_preview_http`); return err }); err != nil {
		t.Fatal(err)
	}
	response = requestContactEditor(t.Context(), f, "alice", path, form)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "preferred_email") || !strings.Contains(response.Body.String(), "alice-imported@example.com") || !strings.Contains(response.Body.String(), "alice-setup@example.com") || strings.Contains(response.Body.String(), "bob-imported@example.com") {
		t.Fatal("preview choices", response.Code, response.Body.String())
	}
	check("alice", alice, true)
	check("bob", bob, false)
	if fake.calls.Load() != 0 {
		t.Fatal("stored preview performed provider HTTP")
	}
	for _, table := range []string{"contact_profiles", "contact_cards", "contact_sync_operations"} {
		var count int
		if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("central fallback", table, count, err)
		}
	}
}

func TestUserContactPreviewHTTPRootShutdownJoinsBlockedWriter(t *testing.T) {
	f, _, _ := newOwnedDAVSetupFixture(t)
	id := stagedContactSetup(t, f, "alice")
	seedHTTPContactPreview(t, f, "alice")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		before := db.Write().Stats().WaitCount
		response := make(chan int, 1)
		go func() {
			response <- requestContactEditor(ctx, f, "alice", "/api/contacts/"+id+"/sync-setup/preview", url.Values{"candidate_" + f.accounts["alice"].ID: {"stored:same-preview-donor"}}).Code
		}()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for db.Write().Stats().WaitCount == before {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
		f.stopIMAP()
		drained := make(chan struct{})
		go func() { f.imap.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case code := <-response:
			if code == 200 {
				t.Fatal("stopped root published preview")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		return tx.Rollback()
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
		var profile string
		if err := db.Read().QueryRow(`SELECT profile_id FROM contact_cards WHERE id='same-preview-card'`).Scan(&profile); err != nil {
			return err
		}
		if profile != "same-preview-donor" {
			t.Fatal("late preview resumed", profile)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
