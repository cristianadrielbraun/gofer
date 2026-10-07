package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedOwnedCalendarDetails(t *testing.T, f *userStorageFixture) {
	t.Helper()
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			attendees, _ := json.Marshal([]any{map[string]any{"email": owner + "-guest@example.com", "responseStatus": "accepted"}})
			_, err := db.Write().Exec(`UPDATE calendar_events SET etag='"v1"',ical_uid='same-uid',description='<p>Full notes</p><script>secret()</script>',organizer_email=?,response_status='organizer',attendees_json=? WHERE id='same-event'; UPDATE calendar_sources SET is_hidden=1 WHERE id='same-source'`, f.accounts[owner].Email, string(attendees))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserCalendarEventHTTPDetailsCachedGrantsAndOwnerIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newOwnedCalendarSyncFixture(t, provider)
			seedOwnedCalendarDetails(t, f)
			for _, owner := range []string{"alice", "bob"} {
				other := map[string]string{"alice": "bob", "bob": "alice"}[owner]
				form := f.request(owner, "GET", "/api/calendar/events/new?date=2026-10-15", "")
				if form.Code != 200 || !strings.Contains(form.Body.String(), owner+" old primary") || strings.Contains(form.Body.String(), other+" old primary") || !strings.Contains(form.Body.String(), `data-calendar-source-authorized="true"`) || !strings.Contains(form.Body.String(), "2026-10-15") || form.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("new event choices/date/grants", owner, form.Code, form.Body.String())
				}
				for _, suffix := range []string{"", "/delivery"} {
					r := f.request(owner, "GET", "/api/calendar/events/same-event"+suffix, "")
					if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(r.Body.String(), owner+"-guest@example.com") || strings.Contains(r.Body.String(), other+"-guest@example.com") || strings.Contains(r.Body.String(), "-calendar-secret") {
						t.Fatal("cached owner detail", owner, suffix, r.Code, r.Body.String())
					}
					if suffix == "" && (!strings.Contains(r.Body.String(), owner+" old cached appointment") || !strings.Contains(r.Body.String(), "Full notes") || strings.Contains(r.Body.String(), "secret()") || !strings.Contains(r.Body.String(), "data-calendar-edit-trigger") || !strings.Contains(r.Body.String(), "data-calendar-delete-trigger")) {
						t.Fatal("detail/access lost", provider, r.Body.String())
					}
				}
			}
			if provider != "caldav" {
				// Write-grant checks use locally known grants even if the token is
				// expired. A detail view must never perform an OAuth refresh.
				oauth, scopes := "google", mailauth.GoogleCalendarEventsScope
				if provider == "outlook" {
					oauth, scopes = "microsoft", "https://graph.microsoft.com/Calendars.ReadWrite"
				}
				expired := time.Now().Add(-time.Hour)
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, oauth, "same-subject", "alice-access", "alice-refresh", "Bearer", &expired, scopes); err != nil {
					t.Fatal(err)
				}
				if r := f.request("alice", "GET", "/api/calendar/events/same-event", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "data-calendar-edit-trigger") {
					t.Fatal("expired cached token lost known grant", r.Code, r.Body.String())
				}
				readScope := mailauth.GoogleCalendarReadOnlyScope
				if provider == "outlook" {
					readScope = "https://graph.microsoft.com/Calendars.Read"
				}
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, oauth, "same-subject", "alice-access", "alice-refresh", "Bearer", &expired, readScope); err != nil {
					t.Fatal(err)
				}
				if r := f.request("alice", "GET", "/api/calendar/events/same-event", ""); r.Code != 200 || strings.Contains(r.Body.String(), "data-calendar-edit-trigger") || strings.Contains(r.Body.String(), "data-calendar-delete-trigger") || !strings.Contains(r.Body.String(), "grant Calendar write access") {
					t.Fatal("read grant enabled writes", r.Code, r.Body.String())
				}
				if r := f.request("alice", "GET", "/api/calendar/events/new", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `data-calendar-source-authorized="false"`) {
					t.Fatal("new form read grant", r.Code, r.Body.String())
				}
			}
			if r := f.request("alice", "GET", "/api/calendar/events/new?date=invalid", ""); r.Code != 400 {
				t.Fatal("invalid form date", r.Code, r.Body.String())
			}
			if r := f.request("alice", "GET", "/api/calendar/events/unknown", ""); r.Code != 404 {
				t.Fatal("missing event", r.Code)
			}
			if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), "alice", f.accounts["alice"].ID, nil); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "/delivery"} {
				if r := f.request("alice", "GET", "/api/calendar/events/same-event"+suffix, ""); r.Code != 404 {
					t.Fatal("deselected detail", r.Code)
				}
			}
			if r := f.request("bob", "GET", "/api/calendar/events/same-event", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "bob old") {
				t.Fatal("selection crossed owners", r.Code, r.Body.String())
			}
			if _, err := f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, f.accounts["bob"].ID); err != nil {
				t.Fatal(err)
			}
			if r := f.request("bob", "GET", "/api/calendar/events/same-event", ""); r.Code != 404 {
				t.Fatal("central deletion bypassed", r.Code, r.Body.String())
			}
			if r := f.request("bob", "GET", "/api/calendar/events/new", ""); r.Code != 200 || strings.Contains(r.Body.String(), "bob old primary") {
				t.Fatal("central deleting account offered in new form", r.Code, r.Body.String())
			}
			api.mu.Lock()
			calls := api.calls["alice"] + api.calls["bob"]
			api.mu.Unlock()
			api.base.mu.Lock()
			tokenCalls := len(api.base.scopes)
			api.base.mu.Unlock()
			if calls != 0 || tokenCalls != 0 {
				t.Fatal("cached view performed provider I/O", calls, tokenCalls)
			}
		})
	}
}

func TestUserCalendarEventHTTPInvitedJoinAndDAVResponseRemainReadOnly(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newOwnedCalendarSyncFixture(t, provider)
			seedOwnedCalendarDetails(t, f)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE calendar_events SET description='Join <https://teams.live.com/meet/123?p=token&lang=es>',organizer_email='organizer@example.com',response_status='accepted' WHERE id='same-event'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			r := f.request("alice", "GET", "/api/calendar/events/same-event", "")
			if r.Code != 200 || !strings.Contains(r.Body.String(), `href="https://teams.live.com/meet/123?p=token&amp;lang=es"`) || strings.Contains(r.Body.String(), "data-calendar-edit-trigger") || strings.Contains(r.Body.String(), "data-calendar-delete-trigger") || !strings.Contains(r.Body.String(), "data-calendar-response-form") {
				t.Fatal("invitation access/join", provider, r.Code, r.Body.String())
			}
			if provider == "caldav" && !strings.Contains(r.Body.String(), `data-calendar-response-ready="false"`) {
				t.Fatal("DAV preflight bypassed")
			}
			if r := f.request("alice", "GET", "/api/calendar/events/same-event/delivery", ""); r.Code != 404 {
				t.Fatal("attendee exposed organizer delivery", r.Code)
			}
			if api.count("alice") != 0 {
				t.Fatal("cached invitation fetched provider")
			}
		})
	}
}

func TestUserCalendarEventHTTPDeliveryRowsAndRenderRelease(t *testing.T) {
	f, _ := newOwnedCalendarSyncFixture(t, "caldav")
	seedOwnedCalendarDetails(t, f)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			event, err := db.GetCalendarEvent(t.Context(), owner, "same-event")
			if err != nil {
				return err
			}
			ics := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:same-uid\r\nORGANIZER:mailto:%s\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", event.AccountEmail)
			message, _ := json.Marshal(map[string]any{"calendar_notification": map[string]any{"UserID": owner, "SourceID": event.SourceID, "ResourceID": event.RemoteID, "Method": "REQUEST", "Calendar": ics}})
			_, err = db.QueueOutgoingSend(t.Context(), storage.QueueOutgoingSendInput{ID: "same-delivery", AccountID: f.accounts[owner].ID, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: event.AccountEmail, EnvelopeRecipients: []string{owner + "-delivery@example.com"}, MIMEData: []byte("immutable MIME secret"), MessageJSON: message, SendAfter: time.Now().Add(24 * time.Hour)})
			if err != nil {
				return err
			}
			_, err = db.Write().Exec(`UPDATE outgoing_sends SET status='ambiguous' WHERE id='same-delivery'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/calendar/events/same-event", "/api/calendar/events/same-event/delivery", "/api/calendar/events/new?date=2026-10-15"} {
		writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
		var once sync.Once
		release := func() { once.Do(func() { close(writer.release) }) }
		t.Cleanup(release)
		r := httptest.NewRequest("GET", path, nil).WithContext(t.Context())
		r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
		done := make(chan struct{})
		go func() { f.http.ServeHTTP(writer, r); close(done) }()
		select {
		case <-writer.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("event render did not start")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetCalendarEvent(ctx, "bob", "same-event"); return err })
		cancel()
		if err != nil {
			t.Fatal("event render retained sole cache slot", err)
		}
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("event render did not finish")
		}
		body := writer.Body.String()
		if strings.Contains(path, "/new") {
			if writer.Code != 200 || !strings.Contains(body, "alice old primary") || strings.Contains(body, "bob old primary") {
				t.Fatal("new form owner/render", writer.Code, body)
			}
			continue
		}
		if writer.Code != 200 || !strings.Contains(body, "alice-delivery@example.com") || !strings.Contains(body, "delivery uncertain") || strings.Contains(body, "bob-delivery@example.com") || strings.Contains(body, "immutable MIME secret") || strings.Contains(body, "/same-delivery/retry") {
			t.Fatal("delivery ownership/state", writer.Code, body)
		}
	}
}
