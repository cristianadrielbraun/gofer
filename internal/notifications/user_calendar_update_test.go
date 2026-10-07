package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarWritesHTTPBrowserDetachRootJoinAndOwnerProgress(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		for _, mode := range []string{"browser-detach", "root-stop"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				f, api, _ := newOwnedCalendarCreateFixture(t, "outlook", "")
				path, form := "/api/calendar/events", ownedCalendarCreateForm()
				var eventID string
				if method == http.MethodPatch {
					created := f.request("alice", http.MethodPost, path, form.Encode())
					var result struct {
						EventID string `json:"event_id"`
					}
					if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || created.Code != 201 || result.EventID == "" {
						t.Fatal("initial create", created.Code, created.Body.String(), err)
					}
					eventID = result.EventID
					path += "/" + eventID
					form.Set("version", "created-v1")
					form.Set("summary", "Edited title")
				}
				entered, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				api.mu.Lock()
				api.beforeWrite = func(r *http.Request) {
					if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer alice-") {
						return
					}
					// Finish receiving the request before simulating provider
					// processing. HTTP/1 server cancellation is observable once
					// its request-body reader has completed.
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						close(canceled)
					}
				}
				api.mu.Unlock()
				browser, stopBrowser := context.WithCancel(t.Context())
				defer stopBrowser()
				req := httptest.NewRequest(method, path, strings.NewReader(form.Encode())).WithContext(browser)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { rec := httptest.NewRecorder(); f.http.ServeHTTP(rec, req); done <- rec }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("write did not dispatch")
				}
				// Exercise real local reads for both owners and a separate native
				// write while Alice's request holds her account/source gates.
				progress := make(chan error, 1)
				go func() {
					for _, owner := range []string{"alice", "bob"} {
						if r := f.request(owner, http.MethodGet, "/api/calendar/events/new", ""); r.Code != 200 {
							progress <- fmt.Errorf("cached form for %s: %d %s", owner, r.Code, r.Body.String())
							return
						}
					}
					if r := f.request("bob", http.MethodPost, "/api/calendar/events", ownedCalendarCreateForm().Encode()); r.Code != 201 {
						progress <- fmt.Errorf("Bob native create: %d %s", r.Code, r.Body.String())
						return
					}
					progress <- nil
				}()
				select {
				case err := <-progress:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("provider wait blocked local reads or another owner")
				}
				if mode == "browser-detach" {
					stopBrowser()
					unblock()
				} else {
					f.stopIMAP()
				}
				select {
				case r := <-done:
					want := 201
					if method == http.MethodPatch {
						want = 200
					}
					if mode == "browser-detach" && r.Code != want {
						t.Fatal("detached write did not finish", r.Code, r.Body.String())
					}
					if mode == "root-stop" && (r.Code < 400 || !strings.Contains(r.Body.String(), `"uncertain":true`)) {
						t.Fatal("shutdown did not preserve uncertain write", r.Code, r.Body.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("write did not join shutdown")
				}
				if mode == "root-stop" {
					select {
					case <-canceled:
					case <-time.After(5 * time.Second):
						t.Fatal("root shutdown did not cancel native request")
					}
					f.imap.Wait()
				} else {
					select {
					case <-canceled:
						t.Fatal("browser cancellation reached native request")
					default:
					}
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var count int
					if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count); err != nil {
						return err
					}
					want := 1
					if method == http.MethodPost && mode == "root-stop" {
						want = 0
					}
					if count != want {
						return fmt.Errorf("Alice cache count %d, want %d", count, want)
					}
					if method == http.MethodPatch {
						event, err := db.GetCalendarEvent(t.Context(), "alice", eventID)
						if err != nil {
							return err
						}
						want := "Planning"
						if mode == "browser-detach" {
							want = "Edited title"
						}
						if event.Summary != want {
							return fmt.Errorf("Alice cache title %q, want %q", event.Summary, want)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserCalendarUpdateHTTPNativeConditionalWriteAndOwnerIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, mode := range []string{"confirmed", "lost-ack", "rejected", "changed-after-write", "publication-failure"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, api, _ := newOwnedCalendarCreateFixture(t, provider, "")
				form := ownedCalendarCreateForm()
				var eventID string
				for _, owner := range []string{"alice", "bob"} {
					response := f.request(owner, "POST", "/api/calendar/events", form.Encode())
					var result map[string]any
					if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 201 {
						t.Fatal("initial create", response.Code, response.Body.String(), err)
					}
					if owner == "alice" {
						eventID, _ = result["event_id"].(string)
					}
				}
				events := f.events.Subscribe()
				defer f.events.Unsubscribe(events)
				form.Set("summary", "Edited title")
				var version string
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					event, err := db.GetCalendarEvent(t.Context(), "alice", eventID)
					version = event.ETag
					return err
				}); err != nil {
					t.Fatal(err)
				}
				form.Set("version", version)
				api.mu.Lock()
				api.mode = mode
				api.writes["alice"] = 0
				api.writes["bob"] = 0
				api.mu.Unlock()
				if mode == "changed-after-write" {
					api.mu.Lock()
					api.afterWrite = func(_ *http.Request) {
						if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
							_, err := db.Write().Exec(`UPDATE calendar_sources SET remote_id='replaced' WHERE id='same-source'`)
							return err
						}); err != nil {
							t.Error(err)
						}
					}
					api.mu.Unlock()
				}
				if mode == "publication-failure" {
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						_, err := db.Write().Exec(`CREATE TRIGGER fail_update BEFORE UPDATE ON calendar_events BEGIN SELECT RAISE(ABORT,'test publication failure'); END`)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				response := f.request("alice", "PATCH", "/api/calendar/events/"+eventID, form.Encode())
				expected, uncertain := 200, false
				if mode != "confirmed" {
					expected = 502
					uncertain = mode != "rejected"
				}
				if mode == "publication-failure" {
					expected = 503
				}
				if response.Code != expected || expected != 200 && !strings.Contains(response.Body.String(), fmt.Sprintf(`"uncertain":%t`, uncertain)) {
					t.Fatal("conditional update", response.Code, response.Body.String())
				}
				for _, owner := range []string{"alice", "bob"} {
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						var title string
						if err := db.Read().QueryRow(`SELECT summary FROM calendar_events`).Scan(&title); err != nil {
							return err
						}
						want := "Planning"
						if owner == "alice" && mode == "confirmed" {
							want = "Edited title"
						}
						if title != want {
							t.Fatal("wrong owner/cache publication", owner, title, want)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				api.mu.Lock()
				writes, other := api.writes["alice"], api.writes["bob"]
				api.mu.Unlock()
				if writes != 1 || other != 0 {
					t.Fatal("update replayed or crossed owners", writes, other)
				}
				changes := 0
				for len(events) > 0 {
					event := <-events
					if event.Type == mail.EventCalendarChanged {
						changes++
						if event.UserID != "alice" || event.AccountID != f.accounts["alice"].ID {
							t.Fatal("wrong change scope", event)
						}
					}
				}
				if mode == "confirmed" && changes != 1 || mode != "confirmed" && changes != 0 {
					t.Fatal("unconfirmed update published success", changes)
				}
			})
		}
	}
}

func TestUserCalendarUpdateHTTPRejectsFormsStaleVersionsAndRetargeting(t *testing.T) {
	f, api, _ := newOwnedCalendarCreateFixture(t, "gmail", "")
	created := f.request("alice", "POST", "/api/calendar/events", ownedCalendarCreateForm().Encode())
	var result map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || created.Code != 201 {
		t.Fatal("create", created.Code, created.Body.String(), err)
	}
	eventID, _ := result["event_id"].(string)
	form := ownedCalendarCreateForm()
	form.Set("version", `"created-v1"`)
	path := "/api/calendar/events/" + eventID
	for _, bad := range []string{form.Encode() + "&owner=bob", form.Encode() + "&version=other", form.Encode() + "&edit_scope=other"} {
		if r := f.request("alice", "PATCH", path, bad); r.Code != 400 {
			t.Fatal("invalid update form", r.Code, r.Body.String())
		}
	}
	if r := f.request("alice", "PATCH", path+"?source_id=other", form.Encode()); r.Code != 400 {
		t.Fatal("query retarget", r.Code, r.Body.String())
	}
	form.Set("version", `"stale"`)
	if r := f.request("alice", "PATCH", path, form.Encode()); r.Code != 409 {
		t.Fatal("stale version", r.Code, r.Body.String())
	}
	form.Set("version", `"created-v1"`)
	form.Set("source_id", "other")
	if r := f.request("alice", "PATCH", path, form.Encode()); r.Code != 400 {
		t.Fatal("source retarget", r.Code, r.Body.String())
	}
	form.Set("source_id", "same-source")
	if r := f.request("bob", "PATCH", path, form.Encode()); r.Code != 404 {
		t.Fatal("foreign event accepted", r.Code, r.Body.String())
	}
	api.mu.Lock()
	writes := api.writes["alice"]
	api.mu.Unlock()
	if writes != 1 {
		t.Fatal("invalid update reached provider", writes)
	}
}

func TestUserCalendarUpdateHTTPPreservesHiddenSecondsUnlessMinuteChanges(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		for _, changeMinute := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/change-minute=%t", provider, changeMinute), func(t *testing.T) {
				f, api, _ := newOwnedCalendarCreateFixture(t, provider, "")
				form := ownedCalendarCreateForm()
				created := f.request("alice", http.MethodPost, "/api/calendar/events", form.Encode())
				var result struct {
					EventID string `json:"event_id"`
				}
				if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || created.Code != 201 || result.EventID == "" {
					t.Fatal("initial create", created.Code, created.Body.String(), err)
				}
				start := time.Date(2026, 10, 7, 10, 0, 37, 0, time.UTC)
				end := time.Date(2026, 10, 7, 11, 0, 49, 0, time.UTC)
				// Model an event imported with seconds that the minute-resolution
				// editor cannot display. Native data and the owned cache agree.
				api.mu.Lock()
				if provider == "caldav" {
					api.dav["alice"] = strings.ReplaceAll(strings.ReplaceAll(api.dav["alice"], "20261007T100000", "20261007T100037"), "20261007T110000", "20261007T110049")
				} else {
					layout := time.RFC3339
					if provider == "outlook" {
						layout = "2006-01-02T15:04:05"
					}
					api.remote["alice"]["start"].(map[string]any)["dateTime"] = start.Format(layout)
					api.remote["alice"]["end"].(map[string]any)["dateTime"] = end.Format(layout)
				}
				api.mu.Unlock()
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE calendar_events SET start_at=?,end_at=? WHERE id=?`, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), result.EventID)
					if err != nil {
						return err
					}
					event, err := db.GetCalendarEvent(t.Context(), "alice", result.EventID)
					form.Set("version", event.ETag)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				form.Set("summary", "Title edited without losing precision")
				if changeMinute {
					form.Set("start_time", "10:01")
					form.Set("end_time", "11:02")
					start = time.Date(2026, 10, 7, 10, 1, 0, 0, time.UTC)
					end = time.Date(2026, 10, 7, 11, 2, 0, 0, time.UTC)
				}
				saved := f.request("alice", http.MethodPatch, "/api/calendar/events/"+result.EventID, form.Encode())
				if saved.Code != 200 {
					t.Fatal("edit", saved.Code, saved.Body.String())
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					event, err := db.GetCalendarEvent(t.Context(), "alice", result.EventID)
					if err != nil {
						return err
					}
					if event.StartAt == nil || event.EndAt == nil || !event.StartAt.Equal(start) || !event.EndAt.Equal(end) {
						return fmt.Errorf("saved event times %v/%v, want %v/%v", event.StartAt, event.EndAt, start, end)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				if provider == "caldav" {
					if !strings.Contains(api.dav["alice"], start.Format("20060102T150405")) || !strings.Contains(api.dav["alice"], end.Format("20060102T150405")) {
						t.Error("native ICS lost exact submitted times", api.dav["alice"])
					}
				} else {
					layout := time.RFC3339
					if provider == "outlook" {
						layout = "2006-01-02T15:04:05"
					}
					for key, want := range map[string]time.Time{"start": start, "end": end} {
						if got := api.remote["alice"][key].(map[string]any)["dateTime"]; got != want.Format(layout) {
							t.Errorf("native %s %v, want %s", key, got, want.Format(layout))
						}
					}
				}
				if api.writes["alice"] != 2 || api.writes["bob"] != 0 {
					t.Error("duplicate or cross-owner write", api.writes)
				}
				api.mu.Unlock()
			})
		}
	}
}

func TestUserCalendarWritesHTTPNativeGuestsCreateEditAndRemoval(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api, _ := newOwnedCalendarCreateFixture(t, provider, "")
			api.mu.Lock()
			api.beforeWrite = func(r *http.Request) {
				if provider == "gmail" && r.URL.Query().Get("sendUpdates") != "all" {
					t.Error("guest write did not request native guest notification", r.URL.RawQuery)
				}
			}
			api.mu.Unlock()
			ids := map[string]string{}
			for _, owner := range []string{"alice", "bob"} {
				form := ownedCalendarCreateForm()
				form.Set("guests", owner+"@provider.test")
				if response := f.request(owner, http.MethodPost, "/api/calendar/events", form.Encode()); response.Code != 400 {
					t.Fatal("organizer invited as guest", response.Code, response.Body.String())
				}
				form.Set("guests", "Guest <"+owner+"-guest@example.com>")
				created := f.request(owner, http.MethodPost, "/api/calendar/events", form.Encode())
				var result struct {
					EventID string `json:"event_id"`
					Notify  bool   `json:"notify_guests"`
				}
				if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || created.Code != 201 || result.EventID == "" || !result.Notify {
					t.Fatal("guest create", created.Code, created.Body.String(), err)
				}
				ids[owner] = result.EventID
			}
			for _, guests := range []string{"New guest <new-guest@example.com>, Other <other-guest@example.com>", ""} {
				form := ownedCalendarCreateForm()
				form.Set("summary", "Guest list edited")
				form.Set("guests", guests)
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					event, err := db.GetCalendarEvent(t.Context(), "alice", ids["alice"])
					form.Set("version", event.ETag)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				saved := f.request("alice", http.MethodPatch, "/api/calendar/events/"+ids["alice"], form.Encode())
				if saved.Code != 200 || !strings.Contains(saved.Body.String(), `"notify_guests":true`) {
					t.Fatal("guest edit", saved.Code, saved.Body.String())
				}
				for _, owner := range []string{"alice", "bob"} {
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						event, err := db.GetCalendarEvent(t.Context(), owner, ids[owner])
						if err != nil {
							return err
						}
						if event.OrganizerEmail != owner+"@provider.test" || event.ResponseStatus != "organizer" {
							return fmt.Errorf("wrong organizer: %+v", event)
						}
						var people []map[string]any
						if err := json.Unmarshal([]byte(event.AttendeesJSON), &people); err != nil {
							return err
						}
						want := 0
						if guests != "" {
							want = 2
						}
						if owner == "bob" {
							want = 1
						}
						if len(people) != want {
							return fmt.Errorf("%s guests %s, want %d", owner, event.AttendeesJSON, want)
						}
						if owner == "bob" && (!strings.Contains(event.AttendeesJSON, "bob-guest@example.com") || event.Summary != "Planning") {
							return fmt.Errorf("Alice edit crossed owners: %+v", event)
						}
						if owner == "alice" && guests != "" && (!strings.Contains(event.AttendeesJSON, "new-guest@example.com") || !strings.Contains(event.AttendeesJSON, "other-guest@example.com") || strings.Contains(event.AttendeesJSON, "alice-guest@example.com")) {
							return fmt.Errorf("old guest metadata on new event version: %s", event.AttendeesJSON)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Failure in the metadata half must roll back the earlier field
			// update too, while reporting that the native save did happen.
			form := ownedCalendarCreateForm()
			form.Set("summary", "Must remain native only")
			form.Set("guests", "failed-publication@example.com")
			var previousVersion string
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				event, err := db.GetCalendarEvent(t.Context(), "alice", ids["alice"])
				if err != nil {
					return err
				}
				previousVersion = event.ETag
				form.Set("version", previousVersion)
				_, err = db.Write().Exec(`CREATE TRIGGER fail_guest_metadata BEFORE UPDATE OF attendees_json ON calendar_events BEGIN SELECT RAISE(ABORT,'guest metadata publication failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			events := f.events.Subscribe()
			defer f.events.Unsubscribe(events)
			failed := f.request("alice", http.MethodPatch, "/api/calendar/events/"+ids["alice"], form.Encode())
			if failed.Code != 503 || !strings.Contains(failed.Body.String(), `"uncertain":true`) {
				t.Fatal("guest publication failure", failed.Code, failed.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				event, err := db.GetCalendarEvent(t.Context(), "alice", ids["alice"])
				if err != nil {
					return err
				}
				if event.ETag != previousVersion || event.Summary != "Guest list edited" || strings.Contains(event.AttendeesJSON, "failed-publication") {
					return fmt.Errorf("partial metadata publication escaped rollback: %+v", event)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for len(events) > 0 {
				if event := <-events; event.Type == mail.EventCalendarChanged {
					t.Error("failed guest publication announced success", event)
				}
			}
			api.mu.Lock()
			if api.writes["alice"] != 4 || api.writes["bob"] != 1 {
				t.Error("duplicate, missing or cross-owner guest write", api.writes)
			}
			api.mu.Unlock()
		})
	}
}
