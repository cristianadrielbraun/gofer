package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func stagedContactSetup(t *testing.T, f *userStorageFixture, owner string) string {
	t.Helper()
	form := editorForm(owner)
	form.Set("email", owner+"-setup@example.com")
	response := requestContactEditor(t.Context(), f, owner, "/api/contacts", form)
	var body struct {
		ID    string `json:"contact_id"`
		Setup string `json:"contact_sync_setup_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 200 || body.ID == "" || body.Setup == "" {
		t.Fatal("stage setup", response.Code, response.Body.String(), err)
	}
	return body.ID
}

func TestUserContactSetupHTTPConfirmationCommittedQueueAndIsolation(t *testing.T) {
	f, fake, _ := newOwnedDAVSetupFixture(t)
	alice, bob := stagedContactSetup(t, f, "alice"), stagedContactSetup(t, f, "bob")
	var aliceField, bobField string
	for owner, id := range map[string]string{"alice": alice, "bob": bob} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var field string
			if err := db.Read().QueryRow(`SELECT id FROM contact_fields WHERE user_id=? AND profile_id=? AND kind='phone' AND source='manual'`, owner, id).Scan(&field); err != nil {
				return err
			}
			if owner == "alice" {
				aliceField = field
			} else {
				bobField = field
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	path := "/api/contacts/" + alice + "/sync-setup/confirm"
	response := requestContactEditor(t.Context(), f, "bob", path, nil)
	if response.Code != 404 {
		t.Fatal("foreign profile confirmed", response.Code, response.Body.String())
	}
	response = requestContactEditor(t.Context(), f, "alice", path, url.Values{"preferred_phone": {bobField}})
	if response.Code != 400 {
		t.Fatal("foreign selected field accepted", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_setup_http_activity BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='contact_sync_queued' BEGIN SELECT RAISE(ABORT,'synthetic setup failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"preferred_phone": {aliceField}, "save_targets": {"book:bob-owned-book"}, "email": {"injected@example.com"}}
	response = requestContactEditor(t.Context(), f, "alice", path, form)
	if response.Code != 503 || strings.Contains(response.Body.String(), `"contact_id"`) {
		t.Fatal("failed confirmation returned success", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		contact, err := db.GetContact(t.Context(), "alice", alice)
		if err != nil {
			return err
		}
		if contact == nil || contact.GoferSyncEnabled {
			t.Fatal("failed confirmation enabled contact", contact)
		}
		var queued, canonical int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_fields WHERE source='canonical'`).Scan(&canonical); err != nil {
			return err
		}
		if queued != 0 || canonical != 0 {
			t.Fatal("failed confirmation published partial state", queued, canonical)
		}
		_, err = db.Write().Exec(`DROP TRIGGER reject_setup_http_activity`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_setup_http_wake BEFORE INSERT ON gofer_contact_queue_schedule BEGIN SELECT RAISE(ABORT,'synthetic wake failure'); END`); err != nil {
		t.Fatal(err)
	}
	response = requestContactEditor(t.Context(), f, "alice", path, form)
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result["ok"] != true || result["contact_id"] != alice || result["contact_sync_queued"] != true || result["location"] != "/contacts?contact="+alice {
		t.Fatal("committed confirmation response", response.Code, response.Body.String(), err)
	}
	for owner, id := range map[string]string{"alice": alice, "bob": bob} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			contact, err := db.GetContact(t.Context(), owner, id)
			if err != nil {
				return err
			}
			want := owner == "alice"
			if contact == nil || contact.GoferSyncEnabled != want || contact.Email != owner+"-setup@example.com" || len(contact.SaveTargets) != 2 || contact.SaveTargets[1] != "book:"+owner+"-owned-book" {
				t.Fatal("confirmation used browser values or crossed owners", owner, contact)
			}
			var queued int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&queued); err != nil {
				return err
			}
			if (queued == 1) != want || queued > 1 {
				t.Fatal("private queue state", owner, queued)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if fake.calls.Load() != 0 {
		t.Fatal("confirmation performed provider HTTP")
	}
	for _, table := range []string{"contact_profiles", "contact_sync_operations", "contact_activity_events"} {
		var count int
		if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("central fallback", table, count, err)
		}
	}
}

func TestUserContactSetupHTTPRootShutdownJoinsBlockedWriter(t *testing.T) {
	f, _, _ := newOwnedDAVSetupFixture(t)
	id := stagedContactSetup(t, f, "alice")
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
			response <- requestContactEditor(ctx, f, "alice", "/api/contacts/"+id+"/sync-setup/confirm", nil).Code
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
		waited := make(chan struct{})
		go func() { f.imap.Wait(); close(waited) }()
		select {
		case <-waited:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case code := <-response:
			if code == http.StatusOK {
				t.Fatal("stopped root accepted confirmation")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		// The request and runtime are drained before this writer is released.
		return tx.Rollback()
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
		contact, err := db.GetContact(ctx, "alice", id)
		if err != nil {
			return err
		}
		if contact == nil || contact.GoferSyncEnabled {
			t.Fatal("late confirmation resumed after root drain", contact)
		}
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("late confirmation queued work")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
