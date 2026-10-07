package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactSuppressionHTTPBulkRollbackThenOwnedClearAndProviderIsolation(t *testing.T) {
	f, api, _ := newOwnedContactDeleteFixture(t, "gmail")
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_http_observed_delete BEFORE INSERT ON contact_activity_events WHEN NEW.event_type='observed_contacts_deleted' BEGIN SELECT RAISE(ABORT,'synthetic bulk delete failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/settings/contacts/delete-observed"
	if response := f.request("alice", "POST", path, ""); response.Code != 503 {
		t.Fatal("failed bulk response", response.Code, response.Body.String())
	}
	assertContactDeleteHTTPState(t, f, "alice", 2, false)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`DROP TRIGGER reject_http_observed_delete`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if response := f.request("alice", "POST", path, ""); response.Code != 303 || response.Header().Get("Location") != "/settings/contacts" {
		t.Fatal("bulk response", response.Code, response.Body.String())
	}
	for _, owner := range []string{"alice", "bob"} {
		for _, partial := range []bool{false, true} {
			request := httptest.NewRequest("GET", "/settings/contacts", nil).WithContext(t.Context())
			request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
			if partial {
				request.Header.Set("HX-Request", "true")
			}
			response := httptest.NewRecorder()
			f.http.ServeHTTP(response, request)
			other := "bob"
			if owner == "bob" {
				other = "alice"
			}
			if response.Code != 200 || !strings.Contains(response.Body.String(), owner+"@provider.test") || strings.Contains(response.Body.String(), other+"@provider.test") {
				t.Fatal("contacts settings redirect destination", owner, partial, response.Code, response.Body.String())
			}
		}
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var deleted, cards, queue, events int
		for query, dest := range map[string]*int{
			`SELECT is_deleted FROM contact_profiles WHERE id='same-delete-profile'`:                                      &deleted,
			`SELECT COUNT(*) FROM contact_cards WHERE profile_id='same-delete-profile' AND kind='provider'`:               &cards,
			`SELECT COUNT(*) FROM contact_sync_operations WHERE contact_id='same-delete-profile' AND status='done'`:       &queue,
			`SELECT COUNT(*) FROM contact_activity_events WHERE event_type='observed_contacts_deleted' AND event_count=1`: &events,
		} {
			if err := db.Read().QueryRow(query).Scan(dest); err != nil {
				return err
			}
		}
		if deleted != 1 || cards != 2 || queue != 1 || events != 1 {
			t.Fatal("bulk changed remote copies or missed atomic state", deleted, cards, queue, events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertContactDeleteHTTPState(t, f, "bob", 2, false)
	if api.count("alice", "one") != 0 || api.count("alice", "two") != 0 {
		t.Fatal("local bulk performed remote deletes")
	}
	path = "/api/settings/contacts/suppressed"
	response := f.request("alice", "GET", path, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "alice-delete@example.com") || strings.Contains(response.Body.String(), "bob-delete@example.com") {
		t.Fatal("suppression view", response.Code, response.Body.String())
	}
	if response := f.request("bob", "POST", path+"/same-delete-profile/clear", ""); response.Code != 404 {
		t.Fatal("foreign suppression cleared", response.Code)
	}
	if response := f.request("alice", "POST", path+"/same-delete-profile/clear", ""); response.Code != 200 || !strings.Contains(response.Body.String(), "No suppressed contacts.") {
		t.Fatal("clear one", response.Code, response.Body.String())
	}
	if response := f.request("alice", "POST", path+"/clear", ""); response.Code != 200 || !strings.Contains(response.Body.String(), "No suppressed contacts.") {
		t.Fatal("empty clear all", response.Code, response.Body.String())
	}
	for _, table := range []string{"contact_profiles", "contact_observations", "contact_activity_events"} {
		var count int
		if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("suppression central fallback", table, count, err)
		}
	}
}

func TestUserContactSuppressionHTTPRenderReleasesSoleCacheSlot(t *testing.T) {
	f, _, _ := newOwnedContactDeleteFixture(t, "gmail")
	if response := f.request("alice", "POST", "/api/settings/contacts/delete-observed", ""); response.Code != 303 {
		t.Fatal(response.Code, response.Body.String())
	}
	writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(writer.release) }) }
	defer release()
	request := httptest.NewRequest("GET", "/api/settings/contacts/suppressed", nil).WithContext(t.Context())
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { f.http.ServeHTTP(writer, request); close(done) }()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("suppression render not reached")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetContact(ctx, "bob", "same-delete-profile"); return err }); err != nil {
		t.Fatal("suppression render pinned store", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not finish")
	}
	if writer.Code != 200 || !strings.Contains(writer.Body.String(), "alice-delete@example.com") || strings.Contains(writer.Body.String(), "bob-delete@example.com") {
		t.Fatal("copied suppression changed owner", writer.Code, writer.Body.String())
	}
}

func TestUserContactSuppressionHTTPRootShutdownJoinsBlockedWriter(t *testing.T) {
	f, _, _ := newOwnedContactDeleteFixture(t, "gmail")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
		tx, err := db.Write().BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		before := db.Write().Stats().WaitCount
		done := make(chan int, 1)
		go func() { done <- f.request("alice", "POST", "/api/settings/contacts/delete-observed", "").Code }()
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
		case code := <-done:
			if code == 303 {
				t.Fatal("shutdown bulk claimed success")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		var deleted int
		if err := db.Read().QueryRow(`SELECT is_deleted FROM contact_profiles WHERE id='same-delete-profile'`).Scan(&deleted); err != nil {
			return err
		}
		if deleted != 0 {
			t.Fatal("shutdown left tombstone")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
