package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// davAccountTestServer is a CalDAV and CardDAV server with two calendars and
// one address book for the login dav-user / dav-pass.
func davAccountTestServer(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, _ := r.BasicAuth()
		if username != "dav-user" || password != "dav-pass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != "PROPFIND" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(207)
		if r.Header.Get("Depth") == "0" {
			fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal><c:calendar-home-set><d:href>/calendars/</d:href></c:calendar-home-set><card:addressbook-home-set><d:href>/books/</d:href></card:addressbook-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/books/") {
			fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:response><d:href>/books/family/</d:href><d:propstat><d:prop><d:displayname>Family</d:displayname><d:resourcetype><d:collection/><card:addressbook/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
			return
		}
		calendar := func(path, name string) string {
			return `<d:response><d:href>` + path + `</d:href><d:propstat><d:prop><d:displayname>` + name + `</d:displayname><d:resourcetype><d:collection/><c:calendar/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
		}
		fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+calendar("/calendars/home/", "Home")+calendar("/calendars/club/", "Club")+`</d:multistatus>`)
	})
}

func davAccountRequest(path string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "alice"}))
}

func aliceDAVAccounts(t *testing.T, f *userContactPushFixture) []string {
	t.Helper()
	var ids []string
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		accounts, err := db.GetAccounts(t.Context(), "alice")
		for _, account := range accounts {
			if account.Provider == providers.ProviderDAV {
				ids = append(ids, account.ID)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// A calendar-only account lists its server's calendars to choose from, is
// created with the ones ticked, signing in with its own login, and edits
// only its server, login and calendars.
func TestCalendarOnlyAccount(t *testing.T) {
	f := newUserContactPushFixture(t, "carddav", davAccountTestServer(t))
	f.h.userAccountHooks = UserAccountHooks{Created: func(context.Context, string) error { return nil }}
	form := url.Values{"service": {"calendar"}, "display_name": {"Home server"}, "server_url": {f.server.URL}, "username": {"dav-user"}, "password": {"wrong"}}

	rec := httptest.NewRecorder()
	f.h.handleUserFindDAVItems(rec, davAccountRequest("/api/accounts/dav/find", form))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "CalDAV") {
		t.Fatalf("wrong password: %d %q", rec.Code, rec.Body.String())
	}
	form.Set("password", "dav-pass")
	rec = httptest.NewRecorder()
	f.h.handleUserFindDAVItems(rec, davAccountRequest("/api/accounts/dav/find", form))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ">Home<") || !strings.Contains(rec.Body.String(), ">Club<") {
		t.Fatalf("find: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	f.h.handleUserCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("created with no calendar chosen: %d", rec.Code)
	}
	if ids := aliceDAVAccounts(t, f); len(ids) != 0 {
		t.Fatal("finding or a refused create made an account")
	}

	form["item"] = []string{f.server.URL + "/calendars/club/"}
	rec = httptest.NewRecorder()
	f.h.handleUserCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	ids := aliceDAVAccounts(t, f)
	if len(ids) != 1 || rec.Header().Get("X-Gofer-Account-ID") != ids[0] {
		t.Fatalf("created %v, answered %q", ids, rec.Header().Get("X-Gofer-Account-ID"))
	}
	data, err := f.h.userEditData(t.Context(), "alice", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if data.DisplayName != "Home server" || data.EmailAddress != "" || data.EmailSyncEnabled || data.Username != "dav-user" {
		t.Fatalf("account = %+v", data)
	}
	cal := data.CalendarSync
	if cal.CalDAVBaseURL == "" || !cal.CalDAVUseAccountCredentials || cal.SourceCount != 2 || cal.SelectedSourceCount != 1 || davSelectedCalendar(data) != "Club" {
		t.Fatalf("calendars = %+v", cal)
	}

	// Edit keeps the password when it is left blank, and saves the new choice.
	edit := url.Values{"display_name": {"Renamed"}, "server_url": {f.server.URL}, "username": {"dav-user"}, "item": {f.server.URL + "/calendars/home/"}}
	r := davAccountRequest("/api/accounts/"+ids[0]+"/dav", edit)
	r.SetPathValue("id", ids[0])
	rec = httptest.NewRecorder()
	f.h.handleUserUpdateDAVAccount(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update: %d %q", rec.Code, rec.Body.String())
	}
	if data, err = f.h.userEditData(t.Context(), "alice", ids[0]); err != nil {
		t.Fatal(err)
	}
	if data.DisplayName != "Renamed" || data.CalendarSync.SelectedSourceCount != 1 || davSelectedCalendar(data) != "Home" {
		t.Fatalf("after editing: %q, %+v", data.DisplayName, data.CalendarSync)
	}
	if dialog := davAccountDialogData(*data); dialog.Service != "calendar" || len(dialog.Items) != 2 {
		t.Fatalf("edit dialog = %+v", dialog)
	}
}

func davSelectedCalendar(data *models.EditAccountData) string {
	for _, source := range data.CalendarSync.Sources {
		if source.IsSelected {
			return source.Name
		}
	}
	return ""
}

// A contacts-only account is created with the address books ticked and the
// default one chosen, signing in with its own login.
func TestContactsOnlyAccount(t *testing.T) {
	f := newUserContactPushFixture(t, "carddav", davAccountTestServer(t))
	f.h.userAccountHooks = UserAccountHooks{Created: func(context.Context, string) error { return nil }}
	book := f.server.URL + "/books/family/"
	form := url.Values{"service": {"contacts"}, "server_url": {f.server.URL}, "username": {"dav-user"}, "password": {"dav-pass"}, "item": {book}, "default_item": {book}}
	rec := httptest.NewRecorder()
	f.h.handleUserCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	ids := aliceDAVAccounts(t, f)
	if len(ids) != 1 {
		t.Fatalf("created %v", ids)
	}
	data, err := f.h.userEditData(t.Context(), "alice", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	contacts := data.ContactSync
	server, _ := url.Parse(f.server.URL)
	if data.DisplayName != server.Hostname() || !contacts.Enabled || contacts.Username != "dav-user" || contacts.HasPassword || len(contacts.AddressBooks) != 1 || contacts.AddressBooks[0].URL != book || !contacts.AddressBooks[0].Default {
		t.Fatalf("account %q, contacts = %+v", data.DisplayName, contacts)
	}
	snapshot, err := f.h.userAccounts.SnapshotServices(t.Context(), "alice", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if password, err := snapshot.ContactSyncPassword(""); err != nil || password != "dav-pass" {
		t.Fatalf("contacts sign in with %q, %v", password, err)
	}
	if dialog := davAccountDialogData(*data); dialog.Service != "contacts" || len(dialog.Items) != 1 || !dialog.Items[0].Default {
		t.Fatalf("edit dialog = %+v", dialog)
	}
}

// A login that is an address must be at an allowed domain, and is refused
// before the server is asked; a plain username has no domain to check.
func TestDAVAccountLoginKeepsToTheAllowedDomains(t *testing.T) {
	asked := 0
	dav := davAccountTestServer(t)
	f := newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		dav.ServeHTTP(w, r)
	}))
	f.h.userAccountHooks = UserAccountHooks{Created: func(context.Context, string) error { return nil }}
	if _, err := f.h.mailAllowlistDB().AddMailAllowlistEntry(t.Context(), storage.MailAllowlistDomain, "example.test", "admin"); err != nil {
		t.Fatal(err)
	}
	book := f.server.URL + "/books/family/"
	form := func(username string) url.Values {
		return url.Values{"service": {"contacts"}, "server_url": {f.server.URL}, "username": {username}, "password": {"dav-pass"}, "item": {book}, "default_item": {book}}
	}
	rec := httptest.NewRecorder()
	f.h.handleUserFindDAVItems(rec, davAccountRequest("/api/accounts/dav/find", form("dav-user@example.org")))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "example.org") {
		t.Fatalf("find: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	f.h.handleUserCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form("dav-user@example.org")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	if asked != 0 || len(aliceDAVAccounts(t, f)) != 0 {
		t.Fatalf("refused login asked the server %d times, accounts %v", asked, aliceDAVAccounts(t, f))
	}
	rec = httptest.NewRecorder()
	f.h.handleUserCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form("dav-user")))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("create with a plain username: %d %q", rec.Code, rec.Body.String())
	}
}

// An account imported from a profile saves only with its password, which
// starts it syncing with the calendars ticked in its dialog.
func TestImportedCalendarOnlyAccountTakesItsPassword(t *testing.T) {
	f := newUserContactPushFixture(t, "carddav", davAccountTestServer(t))
	f.h.userAccountHooks = UserAccountHooks{Created: func(context.Context, string) error { return nil }}
	home, club := f.server.URL+"/calendars/home/", f.server.URL+"/calendars/club/"
	form := url.Values{"service": {"calendar"}, "server_url": {f.server.URL}, "username": {"dav-user"}, "password": {"dav-pass"}, "item": {home}}
	rec := httptest.NewRecorder()
	f.h.handleUserCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form))
	id := rec.Header().Get("X-Gofer-Account-ID")
	if rec.Code != http.StatusNoContent || id == "" {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", id, func(_ *config.AccountStore, db *storage.DB) error {
		return db.SetImportedAccountPending(t.Context(), "alice", id, storage.ImportedAccountPending{Calendars: []string{club}, Needs: []string{storage.ImportedLoginMail}})
	}); err != nil {
		t.Fatal(err)
	}
	data, err := f.h.userEditData(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	dialog := davAccountDialogData(*data)
	if !dialog.NeedsPassword {
		t.Fatal("imported account's dialog doesn't ask for its password")
	}
	for _, item := range dialog.Items {
		if item.ID == club && !item.Checked {
			t.Fatal("the calendar the profile synced starts unticked")
		}
	}

	update := func(password string) *httptest.ResponseRecorder {
		edit := url.Values{"server_url": {f.server.URL}, "username": {"dav-user"}, "password": {password}, "item": {home}}
		r := davAccountRequest("/api/accounts/"+id+"/dav", edit)
		r.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		f.h.handleUserUpdateDAVAccount(rec, r)
		return rec
	}
	if rec := update(""); rec.Code != http.StatusBadRequest {
		t.Fatalf("saved without its password: %d", rec.Code)
	}
	if rec := update("dav-pass"); rec.Code != http.StatusNoContent {
		t.Fatalf("update: %d %q", rec.Code, rec.Body.String())
	}
	if data, err = f.h.userEditData(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if data.ReconnectPending || data.CalendarSync.SelectedSourceCount != 1 || davSelectedCalendar(data) != "Home" {
		t.Fatalf("after its password: pending %v, %+v", data.ReconnectPending, data.CalendarSync)
	}
}
