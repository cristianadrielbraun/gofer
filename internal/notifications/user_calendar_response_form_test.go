package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newOwnedCalendarResponseFormFixture(t *testing.T, provider string, before func(*http.Request), wrappers ...func(http.Handler) http.Handler) (*userStorageFixture, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	f, _, server := newOwnedCalendarFixture(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler {
		var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				base.ServeHTTP(w, r)
				return
			}
			owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
			if provider == "caldav" {
				username, password, ok := r.BasicAuth()
				if !ok || password != username+"-calendar-secret" {
					t.Error("wrong DAV authentication")
					http.Error(w, "bad auth", 401)
					return
				}
				owner = username
			}
			if owner != "alice" && owner != "bob" {
				t.Error("wrong owner", owner)
				http.Error(w, "bad owner", 401)
				return
			}
			expected := "/calendars/primary/events/invitation"
			if provider == "outlook" {
				expected = "/me" + expected
			}
			if provider == "caldav" {
				expected = "/users/" + owner + "/calendars/primary/invitation.ics"
			}
			if provider == "caldav" && r.Method == "OPTIONS" && r.URL.Path == "/users/"+owner+"/calendars/primary/" {
				calls.Add(1)
				w.WriteHeader(204)
				return
			}
			if r.Method != "GET" || r.URL.Path != expected {
				t.Error("wrong preflight target", r.Method, r.URL.Path)
				http.Error(w, "unexpected", 400)
				return
			}
			calls.Add(1)
			if before != nil {
				before(r)
			}
			if r.Context().Err() != nil {
				return
			}
			address := owner + "@provider.test"
			if provider == "caldav" {
				address = owner + "@example.com"
			}
			if provider == "caldav" {
				w.Header().Set("Content-Type", "text/calendar")
				w.Header().Set("ETag", `"v1"`)
				_, _ = fmt.Fprintf(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Test//EN\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:%s native invitation\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:%s\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", owner, address)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			var event any = map[string]any{"id": "invitation", "iCalUID": "private-uid", "etag": `"v1"`, "status": "confirmed", "summary": owner + " native invitation", "organizer": map[string]any{"email": "organizer@example.com", "self": false}, "attendees": []any{map[string]any{"email": address, "self": true, "responseStatus": "needsAction"}}, "start": map[string]string{"dateTime": "2026-10-03T10:00:00Z"}, "end": map[string]string{"dateTime": "2026-10-03T11:00:00Z"}}
			if provider == "outlook" {
				if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
					t.Error("missing immutable ID")
				}
				event = map[string]any{"id": "invitation", "iCalUId": "private-uid", "changeKey": `"v1"`, "@odata.etag": `W/"v1"`, "type": "singleInstance", "subject": owner + " native invitation", "isOrganizer": false, "organizer": map[string]any{"emailAddress": map[string]string{"address": "organizer@example.com"}}, "responseStatus": map[string]string{"response": "notResponded"}, "attendees": []any{map[string]any{"emailAddress": map[string]string{"address": address}, "status": map[string]string{"response": "notResponded"}}}, "start": map[string]string{"dateTime": "2026-10-03T10:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-03T11:00:00", "timeZone": "UTC"}}
			}
			_ = json.NewEncoder(w).Encode(event)
		})
		for _, wrap := range wrappers {
			handler = wrap(handler)
		}
		return handler
	})
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
		address := owner + "@provider.test"
		if provider == "caldav" {
			address = owner + "@example.com"
		}
		path, err := f.blobs.StoreRaw(t.Context(), f.accounts[owner].ID, 1, []byte(ownedMailCalendarMIME(owner, address)))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(_ *config.AccountStore, db *storage.DB) error {
			if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,name,role,uid_validity) VALUES(?,?,'INBOX','Inbox','inbox',100)`, owner+"-response-mail", f.accounts[owner].ID); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,remote_message_id,internet_message_id,raw_path) VALUES(1,?,'m1',?,?)`, f.accounts[owner].ID, "<"+owner+"-invitation@example.com>", path); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(1,?,2)`, owner+"-response-mail"); err != nil {
				return err
			}
			start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			remote := "invitation"
			if provider == "caldav" {
				remote = server.URL + "/users/" + owner + "/calendars/primary/invitation.ics"
			}
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{{ID: "same-event", ICalUID: "private-uid", RemoteID: remote, ETag: `"v1"`, Summary: owner + " cached invitation", OrganizerEmail: "organizer@example.com", ResponseStatus: "needsAction", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, calls
}

func TestUserCalendarResponseFormHTTPProvidersAndMailOwnership(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, calls := newOwnedCalendarResponseFormFixture(t, provider, nil)
			for _, owner := range []string{"alice", "bob"} {
				for _, suffix := range []string{"?scope=event", "?scope=event&mail_id=1", "?scope=event&mail_id=1&edit=1"} {
					r := f.request(owner, "GET", "/api/calendar/events/same-event/response"+suffix, "")
					if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(r.Body.String(), `name="version" value="&#34;v1&#34;"`) {
						t.Fatal("provider response form", provider, owner, suffix, r.Code, r.Body.String())
					}
					if strings.Contains(suffix, "mail_id") && !strings.Contains(r.Body.String(), `data-calendar-response-ready="true"`) {
						t.Fatal("mail preflight did not enable response", r.Body.String())
					}
					if provider == "caldav" && suffix == "?scope=event" && !strings.Contains(r.Body.String(), "emailed to the organizer using this account") {
						t.Fatal("DAV delivery method missing", r.Body.String())
					}
				}
			}
			want := int32(6)
			if provider == "caldav" {
				want = 12
			}
			if calls.Load() != want {
				t.Fatal("wrong provider read count", calls.Load(), want)
			}
			for _, q := range []string{"", "?scope=series", "?scope=event&scope=event", "?scope=event&mail_id=", "?scope=event&mail_id=1&mail_id=1", "?scope=event&edit=1", "?scope=event&mail_id=1&edit=0", "?scope=event&extra=1"} {
				if r := f.request("alice", "GET", "/api/calendar/events/same-event/response"+q, ""); r.Code != 400 {
					t.Fatal("malformed preflight", q, r.Code, r.Body.String())
				}
			}
			if r := f.request("alice", "GET", "/api/calendar/events/same-event/response?scope=event&mail_id=999", ""); r.Code != 403 {
				t.Fatal("missing mail accepted", r.Code)
			}
			if calls.Load() != want {
				t.Fatal("malformed request reached provider")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				event, err := db.GetCalendarEvent(t.Context(), "alice", "same-event")
				if err == nil && event.Summary != "alice cached invitation" {
					return fmt.Errorf("preflight changed cached event")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&n); err != nil || n != 0 {
				t.Fatal("shared fallback", n, err)
			}
		})
	}
}

func TestUserCalendarResponseFormHTTPRejectsInFlightChanges(t *testing.T) {
	for _, mode := range []string{"event", "selected-source", "grant", "dav-config", "smtp-port", "mail-identity", "root-stop"} {
		t.Run(mode, func(t *testing.T) {
			provider := "outlook"
			if mode == "dav-config" || mode == "smtp-port" {
				provider = "caldav"
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			f, _ := newOwnedCalendarResponseFormFixture(t, provider, func(r *http.Request) {
				owner := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if provider == "caldav" {
					owner, _, _ = r.BasicAuth()
				}
				if strings.HasPrefix(owner, "alice") {
					once.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- f.request("alice", "GET", "/api/calendar/events/same-event/response?scope=event&mail_id=1", "")
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("native preflight did not begin")
			}
			if r := f.request("bob", "GET", "/api/calendar/events/same-event/response?scope=event", ""); r.Code != 200 {
				t.Fatal("preflight pinned sole store", r.Code, r.Body.String())
			}
			switch mode {
			case "event":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE calendar_events SET attendees_json='[{"email":"other@example.com"}]' WHERE id='same-event'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "selected-source":
				if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), "alice", f.accounts["alice"].ID, nil); err != nil {
					t.Fatal(err)
				}
			case "grant":
				expiry := time.Now().Add(time.Hour)
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "microsoft", "same-subject", "new-access", "new-refresh", "Bearer", &expiry, userOutlookHTTPScopes); err != nil {
					t.Fatal(err)
				}
			case "dav-config":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE account_caldav_configs SET username='changed' WHERE account_id=?`, f.accounts["alice"].ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "smtp-port":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET smtp_port=0 WHERE id=?`, f.accounts["alice"].ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "mail-identity":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE messages SET remote_message_id='changed' WHERE id=1`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "root-stop":
				f.stopIMAP()
			}
			unblock()
			select {
			case r := <-done:
				if r.Code == 200 || strings.Contains(r.Body.String(), `data-calendar-response-ready="true"`) {
					t.Fatal("stale RSVP preflight enabled response", mode, r.Code, r.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("preflight did not finish")
			}
		})
	}
}

func TestUserCalendarResponseFormHTTPMetadataDoesNotWaitForFileCleanup(t *testing.T) {
	f, _ := newOwnedCalendarResponseFormFixture(t, "outlook", nil)
	release, ok := f.blobs.TryUserFileCleanup("alice")
	if !ok {
		t.Fatal("file cleanup pin unavailable")
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/api/calendar/events/same-event/response?scope=event", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	rec := httptest.NewRecorder()
	f.http.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("metadata preflight blocked by file cleanup", rec.Code, rec.Body.String())
	}
}

func TestUserCalendarResponseFormHTTPDAVRequiresConfiguredSMTP(t *testing.T) {
	f, calls := newOwnedCalendarResponseFormFixture(t, "caldav", nil)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET smtp_port=0 WHERE id=?`, f.accounts["alice"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := f.request("alice", "GET", "/api/calendar/events/same-event/response?scope=event", "")
	if r.Code != 409 || !strings.Contains(r.Body.String(), "Configure SMTP") {
		t.Fatal("unconfigured email reply enabled", r.Code, r.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatal("DAV preflight was skipped", calls.Load())
	}
}

func TestUserCalendarResponseFormHTTPReadGrantAndForeignMailRejected(t *testing.T) {
	f, calls := newOwnedCalendarResponseFormFixture(t, "outlook", nil)
	expiry := time.Now().Add(time.Hour)
	if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "microsoft", "same-subject", "cached", "refresh", "Bearer", &expiry, "https://graph.microsoft.com/Calendars.Read"); err != nil {
		t.Fatal(err)
	}
	if r := f.request("alice", "GET", "/api/calendar/events/same-event/response?scope=event", ""); r.Code != 403 {
		t.Fatal("read grant enabled RSVP", r.Code, r.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("known grant failure reached provider")
	}
	if r := f.request("bob", "GET", "/api/calendar/events/same-event/response?scope=event", ""); r.Code != 200 {
		t.Fatal("foreign grant changed", r.Code)
	}
	// A browser cannot select another receiving account by attaching an ID.
	if r := f.request("bob", "GET", "/api/calendar/events/same-event/response?scope=event&mail_id=1&account_id="+f.accounts["alice"].ID, ""); r.Code != 400 {
		t.Fatal("foreign account query accepted", r.Code)
	}
}

func TestUserCalendarResponseFormHTTPRenderReleasesSoleStore(t *testing.T) {
	f, _ := newOwnedCalendarResponseFormFixture(t, "outlook", nil)
	writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(writer.release) }) }
	defer unblock()
	request := httptest.NewRequest("GET", "/api/calendar/events/same-event/response?scope=event", nil).WithContext(t.Context())
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { f.http.ServeHTTP(writer, request); close(done) }()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetCalendarEvent(ctx, "bob", "same-event"); return err }); err != nil {
		t.Fatal("render pinned sole store", err)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not finish")
	}
	if writer.Code != 200 {
		t.Fatal("render failed", writer.Code, writer.Body.String())
	}
}

func TestUserCalendarResponseFormHTTPDAVPendingReplyUsesOwnedLocalStatus(t *testing.T) {
	f, calls := newOwnedCalendarResponseFormFixture(t, "caldav", nil)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			event, err := db.GetCalendarEvent(t.Context(), owner, "same-event")
			if err != nil {
				return err
			}
			payload, _ := json.Marshal(map[string]any{"Event": event, "Scope": "event"})
			_, err = db.QueueCalendarReply(t.Context(), storage.CalendarReplyJob{UserID: owner, SourceID: event.SourceID, ResourceID: event.RemoteID, RemoteID: event.RemoteID, Version: event.ETag, Response: "accepted", Payload: string(payload)}, storage.QueueOutgoingSendInput{AccountID: f.accounts[owner].ID, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: f.accounts[owner].Email, EnvelopeRecipients: []string{"organizer@example.com"}, MIMEData: []byte("synthetic MIME"), MessageJSON: []byte(`{}`)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r := f.request(owner, "GET", "/api/calendar/events/same-event/response?scope=event&mail_id=1", "")
		if r.Code != 200 || !strings.Contains(r.Body.String(), "data-calendar-reply-status") || strings.Contains(r.Body.String(), `data-calendar-response-ready="true"`) {
			t.Fatal("pending reply status not routed", owner, r.Code, r.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("pending email reply triggered provider preflight")
	}
}
