package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

func TestCalendarSourceGoneReadsEachProvidersNotFound(t *testing.T) {
	for _, test := range []struct {
		err  error
		gone bool
	}{
		{outlookAPIError{Status: http.StatusNotFound}, true},
		{fmt.Errorf("refresh: %w", googleAPIError{Status: http.StatusGone}), true},
		{errors.Join(errors.New("other"), calDAVDiscoveryError{status: http.StatusNotFound}), true},
		{calendarCreateProviderError{Status: http.StatusNotFound}, true},
		{outlookAPIError{Status: http.StatusUnauthorized}, false},
		{googleAPIError{Status: http.StatusTooManyRequests}, false},
		{errors.New("timeout"), false},
		{nil, false},
	} {
		if got := calendarSourceGone(test.err); got != test.gone {
			t.Fatalf("%v gone = %v", test.err, got)
		}
	}
}

func TestCalendarListDueSpacesTheReads(t *testing.T) {
	h := &Handler{}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if h.calendarListDue("a", now, false) {
		t.Fatal("an account first seen read its list at once")
	}
	if h.calendarListDue("a", now.Add(calendarListInterval-time.Minute), false) || !h.calendarListDue("a", now.Add(calendarListInterval), false) {
		t.Fatal("the hourly read is off")
	}
	after := now.Add(calendarListInterval)
	if h.calendarListDue("a", after.Add(calendarListGoneInterval-time.Second), true) || !h.calendarListDue("a", after.Add(calendarListGoneInterval), true) {
		t.Fatal("a calendar not found didn't read the list within its spacing")
	}
	if !h.calendarListDue("b", now, true) {
		t.Fatal("a calendar not found on an account first seen waited")
	}
}

// fakeCalendarListAPI lists whichever Google or Outlook calendars it is set
// to.
type fakeCalendarListAPI struct {
	mu    sync.Mutex
	names []string
	reads int
}

func (f *fakeCalendarListAPI) set(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = names
}

func (f *fakeCalendarListAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/me/calendars") || strings.HasSuffix(r.URL.Path, "/calendarList") {
		f.reads++
	}
	w.Header().Set("Content-Type", "application/json")
	var items []string
	for i, name := range f.names {
		if strings.HasSuffix(r.URL.Path, "/me/calendars") {
			items = append(items, fmt.Sprintf(`{"id":%q,"name":%q,"isDefaultCalendar":%t,"canEdit":true}`, name, name, i == 0))
		} else {
			items = append(items, fmt.Sprintf(`{"id":%q,"summary":%q,"primary":%t,"accessRole":"owner"}`, name, name, i == 0))
		}
	}
	if strings.HasSuffix(r.URL.Path, "/me/calendars") {
		fmt.Fprint(w, `{"value":[`+strings.Join(items, ",")+`]}`)
		return
	}
	fmt.Fprint(w, `{"items":[`+strings.Join(items, ",")+`]}`)
}

type calendarChoice struct {
	selected, deleted bool
}

func calendarChoices(sources []storage.CalendarSource) map[string]calendarChoice {
	out := map[string]calendarChoice{}
	for _, source := range sources {
		out[source.RemoteID] = calendarChoice{source.IsSelected, source.IsDeleted}
	}
	return out
}

func wantCalendarChoices(t *testing.T, step string, got map[string]calendarChoice, want map[string]calendarChoice) {
	t.Helper()
	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(got) != len(want) {
		t.Fatalf("%s: calendars %v, want %v", step, got, want)
	}
	for _, key := range keys {
		if got[key] != want[key] {
			t.Fatalf("%s: %s = %+v, want %+v (all %v)", step, key, got[key], want[key], got)
		}
	}
}

// A per-user account's calendar list read in the background removes the
// calendar the provider deleted (the list of calendars leaves it out), adds a
// new one unticked, and keeps the other choices; a list that comes back
// empty changes nothing.
func TestUserCalendarListRefreshInTheBackground(t *testing.T) {
	api := &fakeCalendarListAPI{}
	f := newUserContactPushFixture(t, "outlook", api)
	previous := outlookGraphBaseURL
	outlookGraphBaseURL = f.server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previous })
	account := f.accounts["alice"]
	expires := time.Now().Add(time.Hour)
	if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, "microsoft", "subject", "alice-access", "alice-refresh", "Bearer", &expires, "https://graph.microsoft.com/Calendars.ReadWrite"); err != nil {
		t.Fatal(err)
	}
	sources := func() map[string]calendarChoice {
		var out map[string]calendarChoice
		if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
			list, err := db.ListCalendarSourcesForAccount(t.Context(), "alice", account.ID)
			out = calendarChoices(list)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	api.set("main", "club", "holidays")
	if _, err := f.h.runUserCalendarDiscovery(t.Context(), "alice", account.ID, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	// The owner ticks club too.
	if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sources SET is_selected = 1 WHERE remote_id = 'club'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	api.set("main", "holidays", "new")
	if _, err := f.h.runUserCalendarDiscovery(t.Context(), "alice", account.ID, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	wantCalendarChoices(t, "after the background read", sources(), map[string]calendarChoice{
		"main": {selected: true}, "holidays": {}, "new": {},
	})

	api.set()
	if _, err := f.h.runUserCalendarDiscovery(t.Context(), "alice", account.ID, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	wantCalendarChoices(t, "after an empty list", sources(), map[string]calendarChoice{
		"main": {selected: true}, "holidays": {}, "new": {},
	})
}

// The shared store reads a Google account's calendar list again the same
// way.
func TestSharedCalendarListRefresh(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, auth_method)
		VALUES ('gmail-account', 'default', 'gmail', 'google-subject', 'person@gmail.com', 'Person', 'oauth2');`); err != nil {
		t.Fatal(err)
	}
	credentials := mailauth.New(&mailauth.Config{GoogleClient: &oauth2.Config{}}, db, testMailboxCredentialKey)
	expires := time.Now().Add(time.Hour)
	if err := credentials.UpsertOAuthAccount(t.Context(), "gmail-account", providers.OAuthGoogle, "google-subject", "token", "refresh", "Bearer", &expires, mailauth.GoogleCalendarReadOnlyScope); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, mailboxAuth: credentials}
	api := &fakeCalendarListAPI{}
	server := httptest.NewServer(api)
	defer server.Close()
	previous := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	t.Cleanup(func() { googleCalendarAPIBaseURL = previous })
	if err := db.ReplaceCalendarSources(t.Context(), "default", "gmail-account", "gmail", []storage.CalendarSource{
		{RemoteID: "main", Name: "main", IsPrimary: true, IsSelected: true},
		{RemoteID: "club", Name: "club", IsSelected: true},
	}); err != nil {
		t.Fatal(err)
	}
	list := func() map[string]calendarChoice {
		sources, err := db.ListCalendarSourcesForAccount(t.Context(), "default", "gmail-account")
		if err != nil {
			t.Fatal(err)
		}
		return calendarChoices(sources)
	}
	source := storage.CalendarSource{UserID: "default", AccountID: "gmail-account", Provider: "gmail", RemoteID: "club"}

	api.set("main", "new")
	if err := h.refreshCalendarList(t.Context(), source, map[string]calendarCredentials{}); err != nil {
		t.Fatal(err)
	}
	wantCalendarChoices(t, "after the read", list(), map[string]calendarChoice{"main": {selected: true}, "new": {}})

	api.set()
	if err := h.refreshCalendarList(t.Context(), source, map[string]calendarCredentials{}); err != nil {
		t.Fatal(err)
	}
	wantCalendarChoices(t, "after an empty list", list(), map[string]calendarChoice{"main": {selected: true}, "new": {}})
}

// A calendar notification also reads the account's calendar list, so a
// calendar made in Outlook shows up with its first event; more notifications
// within the minute don't read it again.
func TestOutlookCalendarNotificationReadsTheCalendarList(t *testing.T) {
	api := &fakeCalendarListAPI{}
	f := newUserContactPushFixture(t, "outlook", api)
	previous := outlookGraphBaseURL
	outlookGraphBaseURL = f.server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previous })
	account := f.accounts["alice"]
	expires := time.Now().Add(time.Hour)
	if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, "microsoft", "subject", "alice-access", "alice-refresh", "Bearer", &expires, "https://graph.microsoft.com/Calendars.ReadWrite"); err != nil {
		t.Fatal(err)
	}
	api.set("main")
	if _, err := f.h.runUserCalendarDiscovery(t.Context(), "alice", account.ID, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.names, api.reads = []string{"main", "new"}, 0
	api.mu.Unlock()

	f.h.OutlookServiceChanged(t.Context(), mail.OutlookPushCalendar, []string{account.ID})
	f.h.OutlookServiceChanged(t.Context(), mail.OutlookPushCalendar, []string{account.ID})
	api.mu.Lock()
	reads := api.reads
	api.mu.Unlock()
	if reads != 1 {
		t.Fatalf("calendar list read %d times, want once", reads)
	}
	if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
		others, err := db.ListUnselectedCalendarSources(t.Context(), "alice")
		if err == nil && (len(others) != 1 || others[0].Name != "new") {
			t.Fatalf("offered calendars %+v", others)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
