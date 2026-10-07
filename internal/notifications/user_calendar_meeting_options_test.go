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

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedMeetingOptionsCalls struct {
	atomic.Int32
	tokens atomic.Int32
}

func newOwnedMeetingOptionsFixture(t *testing.T, provider string, respond func(http.ResponseWriter, *http.Request) bool) (*userStorageFixture, *ownedMeetingOptionsCalls) {
	t.Helper()
	calls := &ownedMeetingOptionsCalls{}
	f, _, _ := newOwnedCalendarFixture(t, provider, func(base *ownedCalendarDiscoveryAPI) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				calls.tokens.Add(1)
				base.ServeHTTP(w, r)
				return
			}
			owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
			path := "/users/me/calendarList/primary"
			if provider == "outlook" {
				path = "/me/calendars/primary"
			}
			if r.Method != "GET" || r.URL.Path != path || (owner != "alice" && owner != "bob") {
				t.Error("foreign capability request", r.Method, r.URL.Path, owner)
				http.Error(w, "bad request", 400)
				return
			}
			calls.Add(1)
			if respond != nil && respond(w, r) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if provider == "gmail" {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "primary", "accessRole": "owner", "conferenceProperties": map[string]any{"allowedConferenceSolutionTypes": []string{"hangoutsMeet"}}})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "primary", "canEdit": true, "allowedOnlineMeetingProviders": []string{"teamsForBusiness"}})
			}
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
			start := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{{ID: "same-event", RemoteID: "single", ETag: `"v1"`, Summary: owner + " private event", AllDay: true, StartDate: "2026-10-03", EndDate: "2026-10-04"}}, start, start.AddDate(0, 0, 1))
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, calls
}

func TestUserCalendarMeetingOptionsHTTPNativeCapabilitiesAndIsolation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook", "caldav"} {
		t.Run(provider, func(t *testing.T) {
			f, calls := newOwnedMeetingOptionsFixture(t, provider, nil)
			for _, owner := range []string{"alice", "bob"} {
				for _, path := range []string{"/api/calendar/meeting-options", "/api/calendar/teams-options"} {
					for _, suffix := range []string{"?source_id=same-source", "?source_id=same-source&event_id=same-event"} {
						r := f.request(owner, "GET", path+suffix, "")
						if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || r.Header().Get("X-Gofer-Calendar-Source") != "same-source" {
							t.Fatal("capability response", r.Code, r.Body.String())
						}
						if provider == "caldav" {
							if r.Body.Len() != 0 {
								t.Fatal("DAV offered online meeting", r.Body.String())
							}
							continue
						}
						title := "Teams meeting"
						if provider == "gmail" {
							title = "Google Meet meeting"
						}
						if !strings.Contains(r.Body.String(), title) || !strings.Contains(r.Body.String(), `data-calendar-teams-available="true"`) {
							t.Fatal("native capability missing", r.Body.String())
						}
					}
				}
			}
			want := int32(8)
			if provider == "caldav" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatal("capability request replay", calls.Load(), want)
			}
			for _, query := range []string{"", "?source_id=", "?source_id=same-source&source_id=same-source", "?source_id=same-source&event_id=x&event_id=y", "?source_id=same-source&account_id=" + f.accounts["bob"].ID} {
				if r := f.request("alice", "GET", "/api/calendar/meeting-options"+query, ""); r.Code != 400 {
					t.Fatal("invalid fields", query, r.Code)
				}
			}
			if r := f.request("alice", "GET", "/api/calendar/meeting-options?source_id=unknown", ""); r.Code != 404 {
				t.Fatal("foreign source", r.Code)
			}
			if provider != "caldav" {
				if r := f.request("alice", "GET", "/api/calendar/meeting-options?source_id=same-source&event_id=unknown", ""); r.Code != 404 {
					t.Fatal("foreign event", r.Code)
				}
			}
			if calls.Load() != want {
				t.Fatal("invalid request reached provider")
			}
			var n int
			if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&n); err != nil || n != 0 {
				t.Fatal("shared fallback", n, err)
			}
		})
	}
}

func TestUserCalendarMeetingOptionsHTTPLocalRestrictionsAndExistingLink(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, calls := newOwnedMeetingOptionsFixture(t, provider, nil)
			for _, mode := range []string{"invitation", "existing", "recurring", "read-grant"} {
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE calendar_events SET response_status='',organizer_email='',attendees_json='[]',online_meeting_json='{}',recurrence_json='[]',series_remote_id='' WHERE id='same-event'`)
					if err != nil {
						return err
					}
					switch mode {
					case "invitation":
						_, err = db.Write().Exec(`UPDATE calendar_events SET response_status='needsAction',organizer_email='host@example.com',attendees_json='[{"email":"alice@provider.test"}]' WHERE id='same-event'`)
					case "existing":
						online := `{"conferenceSolution":{"key":{"type":"hangoutsMeet"}},"entryPoints":[{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij"}]}`
						if provider == "outlook" {
							online = `{"provider":"teamsForBusiness","isOnlineMeeting":true,"joinUrl":"https://teams.microsoft.com/l/meetup-join/meeting"}`
						}
						_, err = db.Write().Exec(`UPDATE calendar_events SET online_meeting_json=?,response_status='organizer' WHERE id='same-event'`, online)
					case "recurring":
						_, err = db.Write().Exec(`UPDATE calendar_events SET recurrence_json='["RRULE:FREQ=DAILY;COUNT=3"]' WHERE id='same-event'`)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if mode == "read-grant" {
					oauth, scope := "google", mailauth.GoogleCalendarReadOnlyScope
					if provider == "outlook" {
						oauth, scope = "microsoft", "https://graph.microsoft.com/Calendars.Read"
					}
					expired := time.Now().Add(-time.Hour)
					if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, oauth, "same-subject", "alice-access", "alice-refresh", "Bearer", &expired, scope); err != nil {
						t.Fatal(err)
					}
				}
				r := f.request("alice", "GET", "/api/calendar/meeting-options?source_id=same-source&event_id=same-event", "")
				if r.Code != 200 || !strings.Contains(r.Body.String(), `data-calendar-teams-available="false"`) {
					t.Fatal("local restriction ignored", mode, r.Code, r.Body.String())
				}
				if mode == "existing" && (!strings.Contains(r.Body.String(), " checked") || (provider == "gmail" && !strings.Contains(r.Body.String(), "https://meet.google.com/abc-defg-hij"))) {
					t.Fatal("saved meeting not retained", r.Body.String())
				}
			}
			if calls.Load() != 0 || calls.tokens.Load() != 0 {
				t.Fatal("local option did provider HTTP", calls.Load(), calls.tokens.Load())
			}
			if r := f.request("bob", "GET", "/api/calendar/meeting-options?source_id=same-source", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `data-calendar-teams-available="true"`) {
				t.Fatal("foreign grant changed", r.Code, r.Body.String())
			}
		})
	}
}

func TestUserCalendarMeetingOptionsHTTPInFlightAuthorityAndCacheRelease(t *testing.T) {
	for _, mode := range []string{"event", "source", "grant", "root"} {
		t.Run(mode, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once, unblockOnce sync.Once
			unblock := func() { unblockOnce.Do(func() { close(release) }) }
			defer unblock()
			f, _ := newOwnedMeetingOptionsFixture(t, "outlook", func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer alice") {
					once.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				return false
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- f.request("alice", "GET", "/api/calendar/meeting-options?source_id=same-source&event_id=same-event", "")
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("capability did not begin")
			}
			if r := f.request("bob", "GET", "/api/calendar/meeting-options?source_id=same-source", ""); r.Code != 200 {
				t.Fatal("provider pinned sole store", r.Code)
			}
			switch mode {
			case "event":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE calendar_events SET summary='changed' WHERE id='same-event'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "source":
				if err := f.accountStore.SetCalendarSourcesSelected(t.Context(), "alice", f.accounts["alice"].ID, nil); err != nil {
					t.Fatal(err)
				}
			case "grant":
				expires := time.Now().Add(time.Hour)
				if err := f.credentials.UpsertForUser(t.Context(), "alice", f.accounts["alice"].ID, "microsoft", "same-subject", "new-access", "new-refresh", "Bearer", &expires, userOutlookHTTPScopes); err != nil {
					t.Fatal(err)
				}
			case "root":
				f.stopIMAP()
			}
			unblock()
			select {
			case r := <-done:
				if r.Code == 200 || strings.Contains(r.Body.String(), `data-calendar-teams-available="true"`) {
					t.Fatal("stale capability enabled", mode, r.Code, r.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("capability did not finish")
			}
		})
	}
}

func TestUserCalendarMeetingOptionsHTTPMetadataIgnoresFileCleanup(t *testing.T) {
	f, _ := newOwnedMeetingOptionsFixture(t, "gmail", nil)
	release, ok := f.blobs.TryUserFileCleanup("alice")
	if !ok {
		t.Fatal("cleanup unavailable")
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	request := httptest.NewRequest("GET", "/api/calendar/meeting-options?source_id=same-source", nil).WithContext(ctx)
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	rec := httptest.NewRecorder()
	f.http.ServeHTTP(rec, request)
	if rec.Code != 200 {
		t.Fatal(fmt.Sprintf("metadata blocked by file cleanup: %d %s", rec.Code, rec.Body.String()))
	}
}

func TestUserCalendarMeetingOptionsHTTPNativeUnsupportedAndErrors(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, mode := range []string{"unsupported", "foreign-id", "provider-error", "consumer-default"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				f, calls := newOwnedMeetingOptionsFixture(t, provider, func(w http.ResponseWriter, r *http.Request) bool {
					if mode == "provider-error" {
						http.Error(w, "unavailable", 503)
						return true
					}
					id := "primary"
					if mode == "foreign-id" {
						id = "foreign"
					}
					if provider == "gmail" {
						_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "accessRole": "owner", "conferenceProperties": map[string]any{"allowedConferenceSolutionTypes": []string{}}})
					} else {
						selected := "skypeForBusiness"
						if mode == "consumer-default" {
							selected = "unknown"
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "canEdit": true, "defaultOnlineMeetingProvider": selected})
					}
					return true
				})
				r := f.request("alice", "GET", "/api/calendar/meeting-options?source_id=same-source", "")
				available := provider == "outlook" && mode == "consumer-default"
				if r.Code != 200 || !strings.Contains(r.Body.String(), fmt.Sprintf(`data-calendar-teams-available="%t"`, available)) {
					t.Fatal("native capability not preserved", mode, r.Code, r.Body.String())
				}
				if (mode == "foreign-id" || mode == "provider-error") && !strings.Contains(r.Body.String(), `data-calendar-teams-retry`) {
					t.Fatal("failure lost retry UI", r.Body.String())
				}
				if calls.Load() != 1 {
					t.Fatal("uncertain capability replayed", calls.Load())
				}
			})
		}
	}
}

func TestUserCalendarMeetingOptionsHTTPGoogleExplicit401Refresh(t *testing.T) {
	f, calls := newOwnedMeetingOptionsFixture(t, "gmail", func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") == "Bearer alice-access" {
			http.Error(w, "expired", 401)
			return true
		}
		return false
	})
	r := f.request("alice", "GET", "/api/calendar/meeting-options?source_id=same-source", "")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `data-calendar-teams-available="true"`) || calls.Load() != 2 || calls.tokens.Load() != 1 {
		t.Fatal("own 401 did not refresh once", r.Code, r.Body.String(), calls.Load(), calls.tokens.Load())
	}
}

func TestUserCalendarMeetingOptionsHTTPRenderingReleasesStore(t *testing.T) {
	f, _ := newOwnedMeetingOptionsFixture(t, "gmail", nil)
	writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(writer.release) }) }
	defer unblock()
	request := httptest.NewRequest("GET", "/api/calendar/meeting-options?source_id=same-source", nil).WithContext(t.Context())
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
		t.Fatal("render retained sole store", err)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not finish")
	}
	if writer.Code != 200 {
		t.Fatal("render failed", writer.Code)
	}
}

func TestUserCalendarMeetingOptionsHTTPMismatchedSelectedEventSource(t *testing.T) {
	f, calls := newOwnedMeetingOptionsFixture(t, "gmail", nil)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		sources, err := db.ListCalendarSourcesForAccount(t.Context(), "alice", f.accounts["alice"].ID)
		if err != nil {
			return err
		}
		sources = append(sources, storage.CalendarSource{ID: "another-source", RemoteID: "secondary", Name: "second calendar", IsSelected: true, AccessRole: "owner"})
		return db.ReplaceCalendarSources(t.Context(), "alice", f.accounts["alice"].ID, "gmail", sources)
	}); err != nil {
		t.Fatal(err)
	}
	r := f.request("alice", "GET", "/api/calendar/meeting-options?source_id=another-source&event_id=same-event", "")
	if r.Code != 404 || calls.Load() != 0 {
		t.Fatal("event retargeted to another selected calendar", r.Code, r.Body.String(), calls.Load())
	}
}
