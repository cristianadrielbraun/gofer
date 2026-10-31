package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
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
			if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
				selected, err := db.ListSelectedCalendarSources(t.Context(), "alice")
				for _, source := range selected {
					if source.CreatedInGofer != (source.RemoteID == "made-by-gofer") {
						t.Fatalf("%s created in Gofer = %v", source.RemoteID, source.CreatedInGofer)
					}
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, source := range data.CalendarSync.Sources {
				if !source.IsSelected {
					t.Fatalf("source %s is not selected", source.Name)
				}
			}
		})
	}
}

// fakeOutlookCalendars lists Outlook calendars by id and name and lets them
// be renamed and deleted, recording each change.
type fakeOutlookCalendars struct {
	mu      sync.Mutex
	ids     []string
	names   map[string]string
	changes []string
}

func (f *fakeOutlookCalendars) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	id := strings.TrimPrefix(r.URL.Path, "/me/calendars/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/me/calendars":
		var items []string
		for i, id := range f.ids {
			items = append(items, fmt.Sprintf(`{"id":%q,"name":%q,"isDefaultCalendar":%t,"canEdit":true}`, id, f.names[id], i == 0))
		}
		fmt.Fprint(w, `{"value":[`+strings.Join(items, ",")+`]}`)
	case r.Method == http.MethodPatch && id != r.URL.Path:
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.names[id] = body.Name
		f.changes = append(f.changes, "rename "+id+" "+body.Name)
		fmt.Fprintf(w, `{"id":%q,"name":%q}`, id, body.Name)
	case r.Method == http.MethodDelete && id != r.URL.Path:
		for i, existing := range f.ids {
			if existing == id {
				f.ids = append(f.ids[:i], f.ids[i+1:]...)
			}
		}
		f.changes = append(f.changes, "delete "+id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// A per-user Outlook calendar is renamed and deleted on Microsoft's side,
// with Gofer's list following; the account's main calendar isn't deleted.
func TestUserCalendarRenameAndDeleteOnOutlook(t *testing.T) {
	api := &fakeOutlookCalendars{ids: []string{"main", "club"}, names: map[string]string{"main": "Calendar", "club": "Club"}}
	f := newUserContactPushFixture(t, "outlook", api)
	previous := outlookGraphBaseURL
	outlookGraphBaseURL = f.server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previous })
	account := f.accounts["alice"]
	expires := time.Now().Add(time.Hour)
	if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, "microsoft", "subject", "alice-access", "alice-refresh", "Bearer", &expires, "https://graph.microsoft.com/Calendars.ReadWrite"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.runUserCalendarDiscovery(t.Context(), "alice", account.ID, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	sources := func() map[string]storage.CalendarSource {
		out := map[string]storage.CalendarSource{}
		if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
			list, err := db.ListCalendarSourcesForAccount(t.Context(), "alice", account.ID)
			for _, source := range list {
				out[source.RemoteID] = source
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Club is synced too.
	if err := f.h.userAccounts.SetCalendarSourcesSelected(t.Context(), "alice", account.ID, []string{sources()["main"].ID, sources()["club"].ID}); err != nil {
		t.Fatal(err)
	}
	manage := func(method, id, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/calendar/sources/"+id, strings.NewReader(body))
		req.SetPathValue("id", id)
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "alice"}))
		rec := httptest.NewRecorder()
		f.h.handleCalendarSourceManage(rec, req)
		return rec
	}

	club := sources()["club"].ID
	if rec := manage(http.MethodPatch, club, `{"name":"Book club"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	if sources()["club"].Name != "Book club" {
		t.Fatalf("renamed source = %+v", sources()["club"])
	}
	if rec := manage(http.MethodDelete, sources()["main"].ID, ""); rec.Code != http.StatusConflict {
		t.Fatalf("deleting the main calendar: %d %s", rec.Code, rec.Body.String())
	}
	if rec := manage(http.MethodDelete, club, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := sources()["club"]; ok {
		t.Fatal("the deleted calendar is still listed")
	}
	if strings.Join(api.changes, "; ") != "rename club Book club; delete club" {
		t.Fatalf("Microsoft saw %v", api.changes)
	}
}

// Google hides a calendar Gofer didn't make behind a 404, which is a
// refusal rather than a calendar already gone; Microsoft's 404 is gone.
func TestCalendarManageErrorReadsGoogleHiddenCalendars(t *testing.T) {
	for _, test := range []struct {
		provider, name string
		err            error
		want           string
	}{
		{"gmail", "New name", googleAPIError{Status: http.StatusNotFound}, "only the calendars it created"},
		{"gmail", "", googleAPIError{Status: http.StatusNotFound}, "only the calendars it created"},
		{"gmail", "", googleAPIError{Status: http.StatusForbidden}, "only the calendars it created"},
		{"outlook", "", outlookAPIError{Status: http.StatusNotFound}, ""},
		{"outlook", "New name", outlookAPIError{Status: http.StatusForbidden}, "Microsoft didn't allow"},
	} {
		err := calendarManageError(test.provider, test.name, test.err)
		if (test.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), test.want)) {
			t.Fatalf("%s %q %v = %v", test.provider, test.name, test.err, err)
		}
	}
}

// A rename or removal just for the account changes its own Google calendar
// list, not the calendar; without full calendar access Google refuses it.
func TestGoogleCalendarListEntryChangesOnlyTheAccountsList(t *testing.T) {
	var seen []string
	refuse := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		switch {
		case refuse:
			http.Error(w, `{"error":{"code":403,"message":"Insufficient Permission"}}`, http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/gone"):
			http.Error(w, `{"error":{"code":404,"message":"Not Found"}}`, http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer server.Close()
	prior := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	defer func() { googleCalendarAPIBaseURL = prior }()

	shared := storage.CalendarSource{Provider: "gmail", RemoteID: "team@group.calendar.google.com"}
	if err := manageCalendarSource(t.Context(), shared, calendarManageLogin{token: "t"}, calendarChange{name: "Team (mine)", personal: true}); err != nil {
		t.Fatal(err)
	}
	if err := manageCalendarSource(t.Context(), shared, calendarManageLogin{token: "t"}, calendarChange{personal: true}); err != nil {
		t.Fatal(err)
	}
	if err := manageGoogleCalendarListEntry(t.Context(), storage.CalendarSource{RemoteID: "gone"}, "t", ""); err != nil {
		t.Fatalf("removing a calendar already gone from the list: %v", err)
	}
	want := `PATCH /users/me/calendarList/team@group.calendar.google.com {"summaryOverride":"Team (mine)"}; DELETE /users/me/calendarList/team@group.calendar.google.com ; DELETE /users/me/calendarList/gone `
	if got := strings.Join(seen, "; "); got != want {
		t.Fatalf("Google saw %q, want %q", got, want)
	}
	// Renaming a calendar the account owns drops its own name for it too,
	// which Google would otherwise keep showing.
	seen = nil
	family := storage.CalendarSource{Provider: "gmail", RemoteID: "family1@group.calendar.google.com"}
	if err := manageCalendarSource(t.Context(), family, calendarManageLogin{token: "t"}, calendarChange{name: "Home"}); err != nil {
		t.Fatal(err)
	}
	want = `PATCH /calendars/family1@group.calendar.google.com {"summary":"Home"}; PATCH /users/me/calendarList/family1@group.calendar.google.com {"summaryOverride":null}`
	if got := strings.Join(seen, "; "); got != want {
		t.Fatalf("Google saw %q, want %q", got, want)
	}
	refuse = true
	if err := manageGoogleCalendarListEntry(t.Context(), shared, "t", "Team"); !errors.Is(err, errGoogleCalendarListNeedsAccess) {
		t.Fatalf("without full access = %v", err)
	}
	if err := manageCalendarSource(t.Context(), storage.CalendarSource{Provider: "outlook"}, calendarManageLogin{}, calendarChange{name: "x", personal: true}); err == nil {
		t.Fatal("an Outlook calendar was renamed just for the account")
	}
}

// Google's family calendar can be renamed but not deleted.
func TestSyncedCalendarSourceRefusesDeletingTheFamilyCalendar(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('alice', 'alice', 'alice', 'Alice');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'alice', 'gmail', 'alice@gmail.com');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "alice", "acc", "gmail", []storage.CalendarSource{{
		RemoteID: "family01234567890@group.calendar.google.com", Name: "Family", AccessRole: "owner", IsSelected: true,
	}}); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListSelectedCalendarSources(t.Context(), "alice")
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %v, %v", sources, err)
	}
	if _, err := syncedCalendarSource(t.Context(), db, "alice", sources[0].ID, calendarChange{}); !errors.As(err, new(errCalendarUndeletable)) || !strings.Contains(err.Error(), "family calendar") {
		t.Fatalf("delete = %v", err)
	}
	if _, err := syncedCalendarSource(t.Context(), db, "alice", sources[0].ID, calendarChange{name: "Home"}); err != nil {
		t.Fatalf("rename = %v", err)
	}
}
