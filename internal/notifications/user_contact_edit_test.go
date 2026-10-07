package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func requestContactEditor(ctx context.Context, f *userStorageFixture, owner, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
	response := httptest.NewRecorder()
	f.http.ServeHTTP(response, req)
	return response
}

func seedOwnedEditor(t *testing.T, f *userStorageFixture, owner string) {
	t.Helper()
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		_, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-contact-id", Email: owner + "-contact@example.com", Name: owner, Phone: owner + "-before", AvatarURL: "https://avatar.test/" + owner, GoferSyncEnabled: true, SaveTargets: []string{"local", "book:" + owner + "-owned-book"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func editorForm(owner string) url.Values {
	return url.Values{"name": {owner + " edited"}, "email": {owner + "-contact@example.com"}, "phone": {owner + "-edited"}, "phone_label": {"home"},
		"additional_emails": {owner + "-alternate@example.com"}, "additional_email_labels": {"work"}, "sync_enabled": {"on"}, "avatar_action": {"preserve"}, "save_targets": {"local,book:" + owner + "-owned-book"}}
}

func TestUserContactEditHTTPAtomicOwnedQueueAndWakeRecovery(t *testing.T) {
	f, fake, _ := newOwnedDAVSetupFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		seedOwnedEditor(t, f, owner)
	}
	form := editorForm("alice")
	response := requestContactEditor(t.Context(), f, "alice", "/api/contacts?id=same-contact-id", form)
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result["contact_sync_queued"] != true || result["refresh_detail"] != true || result["avatar_url"] != "https://avatar.test/alice" {
		t.Fatal("editor response", response.Code, response.Body.String(), err)
	}
	if _, found := result["contact_sync_setup_url"]; found {
		t.Fatal("unchanged destination requested new setup")
	}
	var wake int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_contact_queue_schedule WHERE user_id='alice' AND next_due_ms=0`).Scan(&wake); err != nil || wake != 1 {
		t.Fatal("committed editor queue was not woken", wake, err)
	}
	if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_editor_wake BEFORE INSERT ON gofer_contact_queue_schedule BEGIN SELECT RAISE(ABORT,'synthetic wake failure'); END`); err != nil {
		t.Fatal(err)
	}
	form.Set("phone", "second-edit")
	response = requestContactEditor(t.Context(), f, "alice", "/api/contacts?id=same-contact-id", form)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"contact_sync_queued":true`) {
		t.Fatal("lost durable queue after wake failure", response.Code, response.Body.String())
	}
	if _, err := f.system.Write().Exec(`DROP TRIGGER reject_editor_wake`); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_editor_queue BEFORE INSERT ON contact_sync_operations BEGIN SELECT RAISE(ABORT,'synthetic queue failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	form.Set("phone", "rolled-back")
	response = requestContactEditor(t.Context(), f, "alice", "/api/contacts?id=same-contact-id", form)
	if response.Code != 503 || strings.Contains(response.Body.String(), `"contact_id"`) {
		t.Fatal("failed transaction returned saved identity", response.Code, response.Body.String())
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			contact, err := db.GetContact(t.Context(), owner, "same-contact-id")
			if err != nil {
				return err
			}
			phone, queued := owner+"-before", 0
			if owner == "alice" {
				phone, queued = "second-edit", 2
			}
			if contact == nil || contact.Phone != phone {
				t.Fatal("editor crossed owners or escaped rollback", owner, contact)
			}
			var count int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&count); err != nil {
				return err
			}
			if count != queued {
				t.Fatal("queue count", owner, count)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"contact_profiles", "contact_sync_operations", "contact_sync_memberships"} {
		var central int
		if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&central); err != nil || central != 0 {
			t.Fatal("shared fallback", table, central, err)
		}
	}
	if fake.calls.Load() != 0 {
		t.Fatal("ordinary editor held request for provider work")
	}
}

func TestUserContactEditHTTPSetupNewIdentityAndForeignLocations(t *testing.T) {
	f, fake, _ := newOwnedDAVSetupFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		seedOwnedEditor(t, f, owner)
	}
	form := editorForm("alice")
	form.Set("email", "new-editor@example.com")
	response := requestContactEditor(t.Context(), f, "alice", "/api/contacts", form)
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || result["contact_sync_queued"] != false || result["refresh_detail"] != false {
		t.Fatal("new editor", response.Code, response.Body.String(), err)
	}
	id, ok := result["contact_id"].(string)
	if !ok || id == "" || result["contact_sync_setup_url"] != "/api/contacts/"+id+"/sync-setup" {
		t.Fatal("missing setup route", result)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		contact, err := db.GetContact(t.Context(), "alice", id)
		if err != nil {
			return err
		}
		if contact == nil || contact.GoferSyncEnabled || len(contact.SaveTargets) != 2 {
			t.Fatal("setup state", contact)
		}
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_activity_events WHERE event_type='manual_contact_added' AND email='new-editor@example.com'`).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatal("creation activity", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A no-ID save of an existing email preserves the legacy setup flow.
	form.Set("email", "alice-contact@example.com")
	response = requestContactEditor(t.Context(), f, "alice", "/api/contacts", form)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"contact_id":"same-contact-id"`) || !strings.Contains(response.Body.String(), "contact_sync_setup_url") {
		t.Fatal("identity lookup changed setup behavior", response.Code, response.Body.String())
	}
	for _, missing := range []string{"does-not-exist", id} {
		response = requestContactEditor(t.Context(), f, "bob", "/api/contacts?id="+missing, editorForm("bob"))
		if response.Code != 404 {
			t.Fatal("foreign/missing edit became creation", missing, response.Code)
		}
	}
	form = editorForm("bob")
	form.Set("save_targets", "book:alice-owned-book,account:"+f.accounts["alice"].ID)
	response = requestContactEditor(t.Context(), f, "bob", "/api/contacts?id=same-contact-id", form)
	if response.Code != 200 || strings.Contains(response.Body.String(), `"contact_sync_queued":true`) {
		t.Fatal("foreign destinations accepted", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
		targets, err := db.GetContactSaveTargets(t.Context(), "bob", "same-contact-id")
		if err != nil {
			return err
		}
		if len(targets) != 1 || targets[0] != "local" {
			t.Fatal("foreign destination persisted", targets)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response = f.request("alice", "POST", "/api/contacts", "email=redirect%40example.com&name=Redirect")
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/contacts?contact=") {
		t.Fatal("HTML redirect", response.Code, response.Header())
	}
	form = editorForm("alice")
	form.Set("avatar_action", "invalid")
	response = requestContactEditor(t.Context(), f, "alice", "/api/contacts?id=same-contact-id", form)
	if response.Code != 400 {
		t.Fatal("invalid avatar accepted", response.Code)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("setup save contacted provider before confirmation")
	}
}

func TestUserContactEditHTTPRootShutdownJoinsBlockedWriter(t *testing.T) {
	f, _, _ := newOwnedDAVSetupFixture(t)
	seedOwnedEditor(t, f, "alice")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		before := db.Write().Stats().WaitCount
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			done <- requestContactEditor(ctx, f, "alice", "/api/contacts?id=same-contact-id", editorForm("alice"))
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
		joined := make(chan struct{})
		go func() { f.imap.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case response := <-done:
			if response.Code == 200 {
				t.Fatal("root-cancelled writer committed")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		// Wait returned while the original writer is still held: no detached save
		// can resume later after the transaction below releases the connection.
		return tx.Commit()
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		contact, err := db.GetContact(t.Context(), "alice", "same-contact-id")
		if err != nil {
			return err
		}
		if contact == nil || contact.Phone != "alice-before" {
			t.Fatal("late save after root drain", contact)
		}
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_sync_operations`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("late queue after root drain")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactEditHTTPFailedFirstWakeSurvivesLastAccountDeletion(t *testing.T) {
	f, _, _ := newOwnedDAVSetupFixture(t)
	seedOwnedEditor(t, f, "alice")
	if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_first_editor_wake BEFORE INSERT ON gofer_contact_queue_schedule BEGIN SELECT RAISE(ABORT,'synthetic first wake failure'); END`); err != nil {
		t.Fatal(err)
	}
	response := requestContactEditor(t.Context(), f, "alice", "/api/contacts?id=same-contact-id", editorForm("alice"))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"contact_sync_queued":true`) {
		t.Fatal("durable editor work was rejected", response.Code, response.Body.String())
	}
	if err := f.routing.RequestAccountDeletion(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	owners, err := f.routing.ListDueContactQueueOwners(t.Context(), "", time.Now(), 64, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		if owner == "alice" {
			return
		}
	}
	t.Fatal("committed work became undiscoverable after failed first wake and last account deletion", owners)
}
