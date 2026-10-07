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
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Exercises registered authenticated routes and the real provider parsers.
// Both owners intentionally use identical local event/source and OAuth IDs.
func newOwnedCalendarFormsFixture(t *testing.T, provider string, beforeReply func(*http.Request)) (*userStorageFixture, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	f, _, server := newOwnedCalendarFixture(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				base.ServeHTTP(w, r)
				return
			}
			owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
			if provider == "caldav" {
				username, password, ok := r.BasicAuth()
				if !ok || password != username+"-calendar-secret" {
					http.Error(w, "wrong DAV credentials", 401)
					return
				}
				owner = username
			}
			expected := "/calendars/primary/events/master"
			if provider == "outlook" {
				expected = "/me" + expected
			}
			if provider == "caldav" {
				expected = "/users/" + owner + "/calendars/primary/master.ics"
			}
			if (owner != "alice" && owner != "bob") || r.Method != "GET" || r.URL.Path != expected {
				t.Error("wrong owner, method or selected master", owner, r.Method, r.URL.Path)
				http.Error(w, "unexpected resource", 400)
				return
			}
			calls.Add(1)
			if beforeReply != nil {
				beforeReply(r)
			}
			if r.Context().Err() != nil {
				return
			}
			if provider == "caldav" {
				w.Header().Set("Content-Type", "text/calendar")
				w.Header().Set("ETag", `"master-v2"`)
				_, _ = fmt.Fprintf(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Test//EN\r\nBEGIN:VEVENT\r\nUID:uid\r\nDTSTART;VALUE=DATE:20241002\r\nDTEND;VALUE=DATE:20241003\r\nRRULE:FREQ=DAILY;COUNT=10\r\nSUMMARY:%s private master\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", owner)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			event := map[string]any{"id": "master", "etag": `"master-v2"`, "status": "confirmed", "summary": owner + " private master", "start": map[string]string{"date": "2024-10-02"}, "end": map[string]string{"date": "2024-10-03"}, "recurrence": []string{"RRULE:FREQ=DAILY;COUNT=10"}}
			if provider == "outlook" {
				event = map[string]any{"id": "master", "changeKey": "master-v2", "@odata.etag": `W/"master-v2"`, "type": "seriesMaster", "subject": owner + " private master", "isAllDay": true, "originalStartTimeZone": "UTC", "start": map[string]string{"dateTime": "2024-10-02T00:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2024-10-03T00:00:00", "timeZone": "UTC"}, "recurrence": map[string]any{"pattern": map[string]any{"type": "daily", "interval": 1}, "range": map[string]any{"type": "numbered", "startDate": "2024-10-02", "recurrenceTimeZone": "UTC", "numberOfOccurrences": 10}}}
			}
			_ = json.NewEncoder(w).Encode(event)
		})
	})
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.SetCalendarServiceEnabled(t.Context(), owner, f.accounts[owner].ID, true); err != nil {
			t.Fatal(err)
		}
		if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), owner, f.accounts[owner].ID, []string{"same-source"}); err != nil {
			t.Fatal(err)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			remote, master := "single", "master"
			if provider == "caldav" {
				remote = server.URL + "/users/" + owner + "/calendars/primary/single.ics"
				master = server.URL + "/users/" + owner + "/calendars/primary/master.ics"
			}
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{
				{ID: "same-event", RemoteID: remote, ETag: `"single-v1"`, Summary: owner + " private singleton", AllDay: true, StartDate: "2026-10-15", EndDate: "2026-10-17"},
				{ID: "same-occurrence", RemoteID: master + "#2026-10-03", SeriesRemoteID: master, ETag: `"occurrence-v1"`, Summary: owner + " private occurrence", AllDay: true, StartDate: "2026-10-03", EndDate: "2026-10-04"},
			}, start, start.AddDate(0, 1, 0))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, calls
}

func TestUserCalendarFormsHTTPCachedEditOccurrenceAndIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, calls := newOwnedCalendarFormsFixture(t, provider, nil)
			paths := []string{"/api/calendar/events/same-event/edit", "/api/calendar/events/same-occurrence/edit?scope=occurrence", "/api/calendar/events/same-occurrence/delete-occurrence-confirmation"}
			for _, owner := range []string{"alice", "bob"} {
				other := map[string]string{"alice": "bob", "bob": "alice"}[owner]
				for _, path := range paths {
					r := f.request(owner, "GET", path, "")
					if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(r.Body.String(), owner+" private") || strings.Contains(r.Body.String(), other+" private") || strings.Contains(r.Body.String(), "-calendar-secret") {
						t.Fatal("owned form", provider, owner, path, r.Code, r.Body.String())
					}
					if path == paths[0] && (!strings.Contains(r.Body.String(), `name="end_date" value="2026-10-16"`) || !strings.Contains(r.Body.String(), `single-v1`)) {
						t.Fatal("all day/version prefill", r.Body.String())
					}
					if path == paths[1] && !strings.Contains(r.Body.String(), `name="edit_scope" value="occurrence"`) {
						t.Fatal("occurrence form scope", r.Body.String())
					}
				}
			}
			for _, tc := range []struct {
				path   string
				status int
			}{
				{"/api/calendar/events/same-occurrence/edit", 400},
				{"/api/calendar/events/same-occurrence/edit?scope=series&scope=occurrence", 400},
				{"/api/calendar/events/same-event/edit?scope=occurrence", 400},
				{"/api/calendar/events/same-event/delete-series-confirmation", 409},
				{"/api/calendar/events/same-event/delete-occurrence-confirmation", 403},
				{"/api/calendar/events/unknown/edit", 404},
			} {
				if r := f.request("alice", "GET", tc.path, ""); r.Code != tc.status {
					t.Fatal("scope validation", tc.path, r.Code, r.Body.String())
				}
			}
			if calls.Load() != 0 {
				t.Fatal("cached form performed provider HTTP", calls.Load())
			}
			if provider != "caldav" {
				oauth, scope := "google", mailauth.GoogleCalendarReadOnlyScope
				if provider == "outlook" {
					oauth, scope = "microsoft", "https://graph.microsoft.com/Calendars.Read"
				}
				expired := time.Now().Add(-time.Hour)
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, oauth, "same-subject", "alice-access", "alice-refresh", "Bearer", &expired, scope); err != nil {
					t.Fatal(err)
				}
				if r := f.request("alice", "GET", paths[0], ""); r.Code != 403 {
					t.Fatal("read grant offered editor", r.Code, r.Body.String())
				}
			}
			if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), "alice", f.accounts["alice"].ID, nil); err != nil {
				t.Fatal(err)
			}
			for _, path := range paths {
				if r := f.request("alice", "GET", path, ""); r.Code != 404 {
					t.Fatal("deselected calendar", r.Code)
				}
			}
			if r := f.request("bob", "GET", paths[0], ""); r.Code != 200 {
				t.Fatal("foreign selection changed", r.Code)
			}
			var n int
			if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&n); err != nil || n != 0 {
				t.Fatal("central content fallback", n, err)
			}
		})
	}
}

func TestUserCalendarFormsHTTPSeriesMasterProviderVersionAndNoCacheMutation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, calls := newOwnedCalendarFormsFixture(t, provider, nil)
			for _, owner := range []string{"alice", "bob"} {
				for _, path := range []string{"/api/calendar/events/same-occurrence/edit?scope=series", "/api/calendar/events/same-occurrence/delete-series-confirmation"} {
					r := f.request(owner, "GET", path, "")
					if r.Code != 200 || !strings.Contains(r.Body.String(), "master-v2") || !strings.Contains(r.Body.String(), owner+" private master") || strings.Contains(r.Body.String(), map[string]string{"alice": "bob", "bob": "alice"}[owner]+" private") {
						t.Fatal("master form", provider, path, r.Code, r.Body.String())
					}
					if strings.Contains(path, "/edit") && (!strings.Contains(r.Body.String(), `name="start_date" value="2024-10-02"`) || !strings.Contains(r.Body.String(), `name="repeat_count" value="10"`) || !strings.Contains(r.Body.String(), `name="edit_scope" value="series"`)) {
						t.Fatal("series prefill", r.Body.String())
					}
				}
				if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
					event, err := db.GetCalendarEvent(t.Context(), owner, "same-occurrence")
					if err == nil && (event.ETag != `"occurrence-v1"` || event.Summary != owner+" private occurrence") {
						t.Fatal("master replaced cached occurrence", event)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 4 {
				t.Fatal("master HTTP replay/missing", calls.Load())
			}
		})
	}
}

func TestUserCalendarFormsHTTPRenderDoesNotRetainSoleStore(t *testing.T) {
	f, calls := newOwnedCalendarFormsFixture(t, "caldav", nil)
	for _, path := range []string{"/api/calendar/events/same-event/edit", "/api/calendar/events/same-occurrence/delete-occurrence-confirmation"} {
		writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
		var once sync.Once
		release := func() { once.Do(func() { close(writer.release) }) }
		t.Cleanup(release)
		request := httptest.NewRequest("GET", path, nil).WithContext(t.Context())
		request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
		done := make(chan struct{})
		go func() { f.http.ServeHTTP(writer, request); close(done) }()
		select {
		case <-writer.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("render did not begin")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetCalendarEvent(ctx, "bob", "same-event"); return err })
		cancel()
		if err != nil {
			t.Fatal("form retained sole cache slot", err)
		}
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("render did not finish")
		}
		if writer.Code != 200 || !strings.Contains(writer.Body.String(), "alice private") || strings.Contains(writer.Body.String(), "bob private") {
			t.Fatal("copied form changed", writer.Code, writer.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("cached forms performed HTTP")
	}
}

func TestUserCalendarFormsHTTPRejectsInFlightChangesAndJoinsRoot(t *testing.T) {
	for _, mode := range []string{"same-version-event", "source-selection", "write-grant", "dav-config", "root-stop"} {
		t.Run(mode, func(t *testing.T) {
			provider := "outlook"
			if mode == "dav-config" {
				provider = "caldav"
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f, calls := newOwnedCalendarFormsFixture(t, provider, func(r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- f.request("alice", "GET", "/api/calendar/events/same-occurrence/edit?scope=series", "")
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("master provider not entered")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetCalendarEvent(ctx, "bob", "same-event"); return err }); err != nil {
				t.Fatal("provider read retained sole store", err)
			}
			var err error
			switch mode {
			case "same-version-event":
				err = f.routing.WithUser(ctx, "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE calendar_events SET description='changed while reading' WHERE id='same-occurrence'`)
					return err
				})
			case "source-selection":
				err = f.accountStore.SetCalendarSourcesSelected(ctx, "alice", f.accounts["alice"].ID, nil)
			case "write-grant":
				_, err = f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET revision=revision+1 WHERE account_id=?`, f.accounts["alice"].ID)
			case "dav-config":
				err = f.accountStore.WithAccountForUser(ctx, "alice", f.accounts["alice"].ID, func(local *config.AccountStore, _ *storage.DB) error {
					return local.SaveCalDAVConfig(ctx, "alice", f.accounts["alice"].ID, "https://replaced.test/dav", "other", "other-secret", false)
				})
			case "root-stop":
				f.stopIMAP()
			}
			if err != nil {
				t.Fatal(err)
			}
			unblock()
			select {
			case response := <-done:
				if response.Code == 200 || strings.Contains(response.Body.String(), "private master") || strings.Contains(response.Body.String(), "-secret") {
					t.Fatal("superseded master rendered", mode, response.Code, response.Body.String())
				}
			case <-ctx.Done():
				t.Fatal("root/provider work did not finish", ctx.Err())
			}
			if calls.Load() != 1 {
				t.Fatal("master sequence replayed", calls.Load())
			}
		})
	}
}

func TestUserCalendarFormsHTTPMetadataDoesNotWaitForFileCleanup(t *testing.T) {
	f, _ := newOwnedCalendarFormsFixture(t, "caldav", nil)
	release, ok := f.blobs.TryUserFileCleanup("alice")
	if !ok {
		t.Fatal("could not reserve owner file cleanup")
	}
	defer release()
	for _, path := range []string{"/api/calendar/events/same-event/edit", "/api/calendar/events/same-occurrence/delete-occurrence-confirmation", "/api/calendar/events/same-occurrence/delete-series-confirmation"} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		request := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
		response := httptest.NewRecorder()
		f.http.ServeHTTP(response, request)
		cancel()
		if response.Code != 200 {
			t.Fatal("metadata-only form waited for unrelated blob cleanup", path, response.Code, response.Body.String())
		}
	}
}
