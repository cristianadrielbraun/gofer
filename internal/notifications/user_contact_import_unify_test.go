package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactUnifyHTTPAtomicQueueAndOwnerIsolation(t *testing.T) {
	f, fake, _ := newOwnedDAVSetupFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		seedOwnedEditor(t, f, owner)
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.ReplaceSyncedContactFieldsForProfile(t.Context(), owner, "same-contact-id", f.accounts[owner].ID, models.Contact{Name: owner + " imported", Email: owner + "-contact@example.com", Phone: owner + "-provider-phone"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var before string
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if err := db.Read().QueryRow(`SELECT GROUP_CONCAT(id || ':' || source || ':' || value) FROM (SELECT id,source,value FROM contact_fields ORDER BY id)`).Scan(&before); err != nil {
			return err
		}
		_, err := db.Write().Exec(`CREATE TRIGGER reject_unify_activity BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='contact_sync_queued' BEGIN SELECT RAISE(ABORT,'synthetic unify failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/contacts/same-contact-id/unify"
	form := url.Values{"email": {"injected@example.com"}, "save_targets": {"book:bob-owned-book"}}
	if response := requestContactEditor(t.Context(), f, "alice", path, form); response.Code != 503 || strings.Contains(response.Body.String(), `"contact_id"`) {
		t.Fatal("failed unify response", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var after string
		var queued int
		if err := db.Read().QueryRow(`SELECT GROUP_CONCAT(id || ':' || source || ':' || value) FROM (SELECT id,source,value FROM contact_fields ORDER BY id)`).Scan(&after); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued); err != nil {
			return err
		}
		if after != before || queued != 0 {
			t.Fatal("failed unify left partial fields/queue", queued)
		}
		_, err := db.Write().Exec(`DROP TRIGGER reject_unify_activity`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_unify_wake BEFORE INSERT ON gofer_contact_queue_schedule BEGIN SELECT RAISE(ABORT,'synthetic wake failure'); END`); err != nil {
		t.Fatal(err)
	}
	response := requestContactEditor(t.Context(), f, "alice", path, form)
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result["action"] != "unify" || result["contact_sync_queued"] != true || result["location"] != "/contacts?contact=same-contact-id&sync=queued" {
		t.Fatal("committed unify response", response.Code, response.Body.String(), err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			contact, err := db.GetContact(t.Context(), owner, "same-contact-id")
			if err != nil {
				return err
			}
			if contact == nil || contact.Email != owner+"-contact@example.com" || contact.AvatarURL != "https://avatar.test/"+owner || !contact.GoferSyncEnabled || len(contact.SaveTargets) != 2 || contact.SaveTargets[1] != "book:"+owner+"-owned-book" {
				t.Fatal("unify browser injection or owner crossing", owner, contact)
			}
			var queued, providerFields int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE source=? AND kind='phone' AND value=?`, "synced:"+f.accounts[owner].ID, owner+"-provider-phone").Scan(&providerFields); err != nil {
				return err
			}
			want := 0
			if owner == "alice" {
				want = 1
			}
			if queued != want || providerFields != 1 {
				t.Fatal("unify lost source fields or crossed queues", owner, queued, providerFields)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if response := requestContactEditor(t.Context(), f, "alice", "/api/contacts/missing/unify", nil); response.Code != 404 {
		t.Fatal("missing unify", response.Code)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("unify performed provider HTTP")
	}
}

func requestContactImport(ctx context.Context, f *userStorageFixture, owner, targets, wire string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("save_targets", targets)
	file, err := form.CreateFormFile("vcard", "contacts.vcf")
	if err != nil {
		panic(err)
	}
	_, _ = io.WriteString(file, wire)
	_ = form.Close()
	r := httptest.NewRequest("POST", "/api/contacts/import", &body).WithContext(ctx)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
	response := httptest.NewRecorder()
	f.http.ServeHTTP(response, r)
	return response
}

const contactImportOne = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:same-contact-id\r\nFN:Imported one\r\nEMAIL:same-import@example.com\r\nTEL:+12025550101\r\nEND:VCARD\r\n"
const contactImportTwo = "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Imported two\r\nEMAIL:second-import@example.com\r\nEND:VCARD\r\n"

func TestUserContactImportHTTPOwnedDestinationsAndDisabledSync(t *testing.T) {
	f, fake, _ := newOwnedDAVSetupFixture(t)
	if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
		_, err := db.SaveContact(t.Context(), "bob", models.Contact{ID: "same-imported-id", Name: "Bob kept", Email: "same-import@example.com"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response := requestContactImport(t.Context(), f, "alice", "local,book:alice-owned-book,book:bob-owned-book,account:"+f.accounts["bob"].ID, contactImportOne+contactImportTwo)
	if response.Code != 303 || response.Header().Get("Location") != "/contacts?imported=2" {
		t.Fatal("import response", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		for _, email := range []string{"same-import@example.com", "second-import@example.com"} {
			profile, err := db.FindContactProfileByIdentity(t.Context(), "alice", "email", email)
			if err != nil {
				return err
			}
			if profile == nil {
				t.Fatal("missing imported profile", email)
			}
			contact, err := db.GetContact(t.Context(), "alice", profile.ID)
			if err != nil {
				return err
			}
			if contact.ID == "same-contact-id" || contact.ID == "same-imported-id" || contact.GoferSyncEnabled || len(contact.SaveTargets) != 2 || contact.SaveTargets[1] != "book:alice-owned-book" {
				t.Fatal("import trusted UID/foreign target or enabled sync", contact)
			}
		}
		original, err := db.GetContact(t.Context(), "alice", "same-contact-id")
		if err != nil {
			return err
		}
		if original == nil || original.Email != "alice-contact@example.com" {
			t.Fatal("vCard UID overwrote unrelated profile")
		}
		var queued int
		err = db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued)
		if queued != 0 {
			t.Fatal("import queued provider writes")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
		contact, err := db.GetContact(t.Context(), "bob", "same-imported-id")
		if err == nil && (contact == nil || contact.Name != "Bob kept") {
			t.Fatal("foreign import changed profile", contact)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("import performed provider HTTP")
	}
	// Repeated emails in one file retain their original owner/profile identity,
	// just as replaying an upload must update rather than duplicate the contact.
	var firstID string
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		profile, err := db.FindContactProfileByIdentity(t.Context(), "alice", "email", "same-import@example.com")
		if err == nil {
			firstID = profile.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response = requestContactImport(t.Context(), f, "alice", "local,book:alice-owned-book", contactImportOne+strings.ReplaceAll(contactImportOne, "Imported one", "Updated one"))
	if response.Code != 303 || response.Header().Get("Location") != "/contacts?imported=2" {
		t.Fatal("repeated import", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		profile, err := db.FindContactProfileByIdentity(t.Context(), "alice", "email", "same-import@example.com")
		if err != nil {
			return err
		}
		if profile == nil || profile.ID != firstID || profile.DisplayName != "Updated one" {
			t.Fatal("replayed email duplicated or failed to update", profile)
		}
		var count int
		err = db.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles WHERE primary_email='same-import@example.com'`).Scan(&count)
		if count != 1 {
			t.Fatal("duplicate import profiles", count)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles`).Scan(&count); err != nil || count != 0 {
		t.Fatal("import central fallback", count, err)
	}
}

func TestUserContactImportHTTPLaterFailureKeepsOnlyCommittedContacts(t *testing.T) {
	f, _, _ := newOwnedDAVSetupFixture(t)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_second_import BEFORE INSERT ON contact_fields WHEN NEW.kind='email' AND NEW.value='second-import@example.com' BEGIN SELECT RAISE(ABORT,'synthetic second import failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	response := requestContactImport(t.Context(), f, "alice", "local", contactImportOne+contactImportTwo+strings.ReplaceAll(contactImportTwo, "second-import", "third-import"))
	if response.Code != 503 || !strings.Contains(response.Body.String(), "after saving 1 contact.") {
		t.Fatal("partial import response", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var profiles, cards int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles WHERE primary_email IN ('same-import@example.com','second-import@example.com','third-import@example.com')`).Scan(&profiles); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_cards WHERE profile_id NOT IN (SELECT id FROM contact_profiles)`).Scan(&cards); err != nil {
			return err
		}
		if profiles != 1 || cards != 0 {
			t.Fatal("failed import left partial/later contacts", profiles, cards)
		}
		var second int
		err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE value='second-import@example.com'`).Scan(&second)
		if second != 0 {
			t.Fatal("failed contact fields escaped")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
