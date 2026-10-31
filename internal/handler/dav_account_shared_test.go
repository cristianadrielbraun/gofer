package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// newSharedDAVAccountHandler is the handler of open and personal mode, with
// the shared store, for alice, and a CalDAV and CardDAV server.
func newSharedDAVAccountHandler(t *testing.T) (*Handler, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(davAccountTestServer(t))
	t.Cleanup(server.Close)
	allowLocalDAVServers(t)
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice')`); err != nil {
		t.Fatal(err)
	}
	accounts, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{db: db, accountStore: accounts, contactSyncRunning: map[string]struct{}{}, contactSyncQueue: make(chan struct{}, 1)}, server
}

func sharedDAVAccounts(t *testing.T, h *Handler) []string {
	t.Helper()
	accounts, err := h.db.GetAccounts(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, account := range accounts {
		if account.Provider == providers.ProviderDAV {
			ids = append(ids, account.ID)
		}
	}
	return ids
}

// Open and personal mode serve the dialog of calendar-only and contacts-only
// accounts, as managed mode does.
func TestSharedModeRoutesDAVAccounts(t *testing.T) {
	h, _ := newSharedDAVAccountHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	for path, want := range map[string]string{
		"/api/accounts/dav/find":       "POST /api/accounts/dav/find",
		"/api/accounts/dav":            "POST /api/accounts/dav",
		"/api/accounts/acc_x/dav/find": "POST /api/accounts/{id}/dav/find",
		"/api/accounts/acc_x/dav":      "POST /api/accounts/{id}/dav",
		"/api/accounts/acc_x/mail":     "POST /api/accounts/{id}/mail",
	} {
		if _, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, path, nil)); pattern != want {
			t.Errorf("POST %s goes to %q, want %q", path, pattern, want)
		}
	}
}

// In open and personal mode a calendar-only account is found, created with
// the calendars ticked, edited in its own dialog, and can't turn on mail.
// Two of them, which have no address to tell them apart, stay two accounts.
func TestSharedModeCalendarOnlyAccount(t *testing.T) {
	h, server := newSharedDAVAccountHandler(t)
	form := url.Values{"service": {"calendar"}, "display_name": {"Home server"}, "server_url": {server.URL}, "username": {"dav-user"}, "password": {"dav-pass"}}

	rec := httptest.NewRecorder()
	h.handleFindDAVItems(rec, davAccountRequest("/api/accounts/dav/find", form))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ">Home<") || !strings.Contains(rec.Body.String(), ">Club<") {
		t.Fatalf("find: %d %q", rec.Code, rec.Body.String())
	}
	form["item"] = []string{server.URL + "/calendars/club/"}
	for range 2 {
		rec = httptest.NewRecorder()
		h.handleCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
		}
	}
	ids := sharedDAVAccounts(t, h)
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("created %v", ids)
	}
	data, err := h.accountEditData(t.Context(), "alice", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	cal := data.CalendarSync
	if data.DisplayName != "Home server" || data.EmailAddress != "" || data.EmailSyncEnabled || data.Username != "dav-user" ||
		cal.CalDAVBaseURL == "" || !cal.CalDAVUseAccountCredentials || cal.SourceCount != 2 || cal.SelectedSourceCount != 1 || davSelectedCalendar(data) != "Club" {
		t.Fatalf("account = %+v", data)
	}
	if creds := h.calendarCredentialsForSource(t.Context(), "alice", storage.CalendarSource{AccountID: ids[0], Provider: storage.CalendarSourceProviderCalDAV}); creds.err != nil || creds.username != "dav-user" || creds.password != "dav-pass" {
		t.Fatalf("calendars sign in with %q, %v", creds.password, creds.err)
	}

	r := davAccountRequest("/api/accounts/"+ids[0]+"/edit", nil)
	r.Method = http.MethodGet
	r.SetPathValue("id", ids[0])
	rec = httptest.NewRecorder()
	h.handleGetEditAccount(rec, r)
	if !strings.Contains(rec.Body.String(), "edit-dav-account-dialog") {
		t.Fatalf("edit opens %d %q", rec.Code, rec.Body.String())
	}

	edit := url.Values{"display_name": {"Renamed"}, "server_url": {server.URL}, "username": {"dav-user"}, "item": {server.URL + "/calendars/home/"}}
	r = davAccountRequest("/api/accounts/"+ids[0]+"/dav", edit)
	r.SetPathValue("id", ids[0])
	rec = httptest.NewRecorder()
	h.handleUpdateDAVAccount(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update: %d %q", rec.Code, rec.Body.String())
	}
	if data, err = h.accountEditData(t.Context(), "alice", ids[0]); err != nil {
		t.Fatal(err)
	}
	if data.DisplayName != "Renamed" || data.CalendarSync.SelectedSourceCount != 1 || davSelectedCalendar(data) != "Home" {
		t.Fatalf("after editing: %q, %+v", data.DisplayName, data.CalendarSync)
	}

	r = davAccountRequest("/api/accounts/"+ids[0]+"/services", url.Values{"service": {"email"}, "enabled": {"true"}})
	r.SetPathValue("id", ids[0])
	rec = httptest.NewRecorder()
	h.handleUpdateAccountService(rec, r)
	if rec.Code != http.StatusConflict || h.db.IsEmailSyncEnabled(t.Context(), ids[0]) {
		t.Fatalf("turning on mail: %d %q", rec.Code, rec.Body.String())
	}
}

// In open and personal mode a contacts-only account is created with the
// address books ticked, signing in with its own login.
func TestSharedModeContactsOnlyAccount(t *testing.T) {
	h, server := newSharedDAVAccountHandler(t)
	book := server.URL + "/books/family/"
	form := url.Values{"service": {"contacts"}, "server_url": {server.URL}, "username": {"dav-user"}, "password": {"dav-pass"}, "item": {book}, "default_item": {book}}
	rec := httptest.NewRecorder()
	h.handleCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	ids := sharedDAVAccounts(t, h)
	if len(ids) != 1 {
		t.Fatalf("created %v", ids)
	}
	data, err := h.accountEditData(t.Context(), "alice", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	contacts := data.ContactSync
	if !contacts.Enabled || contacts.Username != "dav-user" || len(contacts.AddressBooks) != 1 || contacts.AddressBooks[0].URL != book || !contacts.AddressBooks[0].Default {
		t.Fatalf("contacts = %+v", contacts)
	}
	if _, password, err := h.cardDAVConfig(t.Context(), "alice", ids[0]); err != nil || password != "dav-pass" {
		t.Fatalf("contacts sign in with %q, %v", password, err)
	}
	if synced, err := h.contactSyncAccounts(t.Context(), "alice", ids[0]); err != nil || len(synced) != 1 || synced[0].Provider != providers.ProviderCardDAV {
		t.Fatalf("contacts sync %+v, %v", synced, err)
	}
}

// Someone else's account can't be edited through the dialog.
func TestSharedModeDAVAccountIsTheOwners(t *testing.T) {
	h, server := newSharedDAVAccountHandler(t)
	if _, err := h.db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('bob','bob','bob')`); err != nil {
		t.Fatal(err)
	}
	account, err := h.accountStore.CreateAccount(context.Background(), "bob", &models.CreateAccountRequest{Provider: providers.ProviderDAV, DisplayName: "Bob", Username: "dav-user", Password: "dav-pass"})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"server_url": {server.URL}, "username": {"dav-user"}, "item": {server.URL + "/calendars/home/"}}
	for _, path := range []string{"/dav/find", "/dav"} {
		r := davAccountRequest("/api/accounts/"+account.ID+path, form)
		r.SetPathValue("id", account.ID)
		rec := httptest.NewRecorder()
		if path == "/dav" {
			h.handleUpdateDAVAccount(rec, r)
		} else {
			h.handleFindDAVItems(rec, r)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("alice reached bob's account at %s: %d %q", path, rec.Code, rec.Body.String())
		}
	}
}
