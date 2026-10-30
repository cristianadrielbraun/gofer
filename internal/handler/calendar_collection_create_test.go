package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// fakeCalendarAPI is a Google Calendar and Microsoft Graph calendar list
// with one calendar, that makes the calendars posted to it.
type fakeCalendarAPI struct {
	mu      sync.Mutex
	created []string
	tokens  []string
}

func (f *fakeCalendarAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/calendars" || r.URL.Path == "/me/calendars"):
		var body struct{ Summary, Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body.Summary+body.Name)
		f.tokens = append(f.tokens, r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"id":"made-by-gofer"}`)
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/calendarList":
		items := `{"id":"primary","summary":"Primary","accessRole":"owner","primary":true}`
		for _, name := range f.created {
			items += `,{"id":"made-by-gofer","summary":"` + name + `","accessRole":"owner"}`
		}
		fmt.Fprint(w, `{"items":[`+items+`]}`)
	case r.Method == http.MethodGet && r.URL.Path == "/me/calendars":
		items := `{"id":"main","name":"Calendar","isDefaultCalendar":true,"canEdit":true}`
		for _, name := range f.created {
			items += `,{"id":"made-by-gofer","name":"` + name + `","canEdit":true}`
		}
		fmt.Fprint(w, `{"value":[`+items+`]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// A per-user Google or Microsoft account makes a calendar with its grant for
// it and lists it selected; a Google grant from before Gofer asked for it
// is told to reconnect, and nothing is made.
func TestUserCalendarDiscoveryCreatesOAuthCalendar(t *testing.T) {
	for _, test := range []struct {
		provider, oauthProvider, scopes string
		allowed                         bool
	}{
		{"gmail", "google", mailauth.GoogleCalendarReadOnlyScope + " " + mailauth.GoogleCalendarEventsScope + " " + mailauth.GoogleCalendarAppCreatedScope, true},
		{"gmail", "google", mailauth.GoogleCalendarReadOnlyScope + " " + mailauth.GoogleCalendarEventsScope, false},
		{"outlook", "microsoft", "https://graph.microsoft.com/Calendars.ReadWrite", true},
	} {
		t.Run(fmt.Sprintf("%s allowed=%v", test.provider, test.allowed), func(t *testing.T) {
			api := &fakeCalendarAPI{}
			f := newUserContactPushFixture(t, test.provider, api)
			previousGoogle, previousGraph := googleCalendarAPIBaseURL, outlookGraphBaseURL
			googleCalendarAPIBaseURL, outlookGraphBaseURL = f.server.URL, f.server.URL
			t.Cleanup(func() { googleCalendarAPIBaseURL, outlookGraphBaseURL = previousGoogle, previousGraph })
			account := f.accounts["alice"]
			expires := time.Now().Add(time.Hour)
			if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, test.oauthProvider, "subject", "alice-access", "alice-refresh", "Bearer", &expires, test.scopes); err != nil {
				t.Fatal(err)
			}
			discover := func(form url.Values) string {
				t.Helper()
				req := httptest.NewRequest(http.MethodPost, "/api/accounts/"+account.ID+"/calendar/discover", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.SetPathValue("id", account.ID)
				req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "alice"}))
				rec := httptest.NewRecorder()
				f.h.handleUserDiscoverCalendars(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
				}
				return rec.Body.String()
			}

			body := discover(url.Values{})
			if strings.Contains(body, "data-calendar-create-form") != test.allowed {
				t.Fatalf("panel after discovery: %q", body)
			}
			body = discover(url.Values{"create_name": {"Family"}})
			if !test.allowed {
				if len(api.created) != 0 || !strings.Contains(body, "Calendar creation failed") || !strings.Contains(body, "reconnect") {
					t.Fatalf("an older grant created %v: %q", api.created, body)
				}
				return
			}
			if len(api.created) != 1 || api.created[0] != "Family" || api.tokens[0] != "Bearer alice-access" {
				t.Fatalf("created %v with %v", api.created, api.tokens)
			}
			if !strings.Contains(body, "Created the calendar &#34;Family&#34;") {
				t.Fatalf("create: %q", body)
			}
			data, err := f.h.userEditData(t.Context(), "alice", account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if data.CalendarSync.CreateAccess != models.CalendarCreateAllowed || len(data.CalendarSync.Sources) != 2 {
				t.Fatalf("calendar sync = %+v", data.CalendarSync)
			}
			for _, source := range data.CalendarSync.Sources {
				if !source.IsSelected {
					t.Fatalf("source %s is not selected", source.Name)
				}
			}
		})
	}
}
