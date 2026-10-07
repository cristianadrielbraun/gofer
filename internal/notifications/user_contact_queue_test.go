package notifications

import (
	"net/http"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactQueueHTTPPrototypeStartsOwnedScheduling(t *testing.T) {
	f := newUserStorageFixtureContacts(t, true, nil, &handler.UserContactSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour})
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var accounts, owners int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_account_service_schedule WHERE service='contacts' AND next_due_ms>?`, time.Now().Add(4*time.Minute).UnixMilli()).Scan(&accounts); err != nil {
			t.Fatal(err)
		}
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE next_due_ms>?`, time.Now().Add(30*time.Minute).UnixMilli()).Scan(&owners); err != nil {
			t.Fatal(err)
		}
		if accounts == 2 && owners == 2 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("prototype options did not start owned scheduling", accounts, owners)
		case <-tick.C:
		}
	}
	if response := f.request("alice", "GET", "/api/contacts/search?q=private", ""); response.Code != http.StatusOK {
		t.Fatal("scheduling broke authenticated local reads", response.Code, response.Body.String())
	}
	f.stopIMAP()
	f.imap.Wait()
}

func TestUserContactQueueHTTPManualOwnerScopeAndDisabledProfile(t *testing.T) {
	f, _, _ := newOwnedContactPullFixture(t, "outlook")
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-contact-id", Email: owner + "@contacts.test", Name: owner, GoferSyncEnabled: true, SaveTargets: []string{"account:" + f.accounts[owner].ID}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if response := f.request("alice", "POST", "/api/contacts/same-contact-id/sync-now", ""); response.Code != http.StatusAccepted {
		t.Fatal("manual queue", response.Code, response.Body.String())
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var count int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&count); err != nil {
				return err
			}
			want := 0
			if owner == "alice" {
				want = 1
			}
			if count != want {
				t.Fatal("manual queue crossed owners", owner, count)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if response := f.request("alice", "POST", "/api/contacts/missing/sync-now", ""); response.Code != http.StatusNotFound {
		t.Fatal("missing contact", response.Code)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		return db.SetContactProfileSyncEnabled(t.Context(), "alice", "same-contact-id", false)
	}); err != nil {
		t.Fatal(err)
	}
	if response := f.request("alice", "POST", "/api/contacts/same-contact-id/sync-now", ""); response.Code != http.StatusConflict {
		t.Fatal("disabled profile", response.Code)
	}
	var central int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&central); err != nil || central != 0 {
		t.Fatal("manual queue used shared storage", central, err)
	}
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE user_id='alice' AND next_due_ms=0`).Scan(&central); err != nil || central != 1 {
		t.Fatal("manual wake not durable", central, err)
	}
}
