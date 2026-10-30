package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// emptyDAVServer is a CalDAV and CardDAV server for dav-user / dav-pass that
// starts with no calendars or address books and makes the ones asked for,
// unless refuse is set.
type emptyDAVServer struct {
	mu          sync.Mutex
	collections map[string]string // path -> display name
	methods     map[string]string // path -> method that made it
	bodies      []string
	refuse      bool
}

func newEmptyDAVServer() *emptyDAVServer {
	return &emptyDAVServer{collections: map[string]string{}, methods: map[string]string{}}
}

func (s *emptyDAVServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	username, password, _ := r.BasicAuth()
	if username != "dav-user" || password != "dav-pass" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case "MKCALENDAR", "MKCOL":
		body, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(body))
		if s.refuse {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "creating collections is turned off")
			return
		}
		home := "/calendars/"
		if r.Method == "MKCOL" {
			home = "/books/"
		}
		if !strings.HasPrefix(r.URL.Path, home) || !strings.HasSuffix(r.URL.Path, "/") {
			w.WriteHeader(http.StatusConflict)
			return
		}
		_, name, _ := strings.Cut(string(body), "<d:displayname>")
		name, _, _ = strings.Cut(name, "</d:displayname>")
		s.collections[r.URL.Path] = name
		s.methods[r.URL.Path] = r.Method
		w.WriteHeader(http.StatusCreated)
		return
	case "PROPFIND":
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(207)
	if r.Header.Get("Depth") == "0" {
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:current-user-principal><d:href>/principal/</d:href></d:current-user-principal><c:calendar-home-set><d:href>/calendars/</d:href></c:calendar-home-set><card:addressbook-home-set><d:href>/books/</d:href></card:addressbook-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path)
		return
	}
	fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:card="urn:ietf:params:xml:ns:carddav"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, r.URL.Path)
	for path, name := range s.collections {
		if !strings.HasPrefix(path, r.URL.Path) {
			continue
		}
		kind := `<c:calendar/>`
		if strings.HasPrefix(path, "/books/") {
			kind = `<card:addressbook/>`
		}
		fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:displayname>%s</d:displayname><d:resourcetype><d:collection/>%s</d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, path, name, kind)
	}
	fmt.Fprint(w, `</d:multistatus>`)
}

// A server with an empty calendar home is signed in to, not taken for no
// server; a calendar is made there with MKCALENDAR and comes back ticked.
func TestCalendarOnlyAccountCreatesCalendar(t *testing.T) {
	dav := newEmptyDAVServer()
	f := newUserContactPushFixture(t, "carddav", dav)
	form := url.Values{"service": {"calendar"}, "server_url": {f.server.URL}, "username": {"dav-user"}, "password": {"dav-pass"}}

	rec := httptest.NewRecorder()
	f.h.handleUserFindDAVItems(rec, davAccountRequest("/api/accounts/dav/find", form))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No calendars on this server yet") || strings.Contains(rec.Body.String(), "data-dav-items-found") {
		t.Fatalf("find on an empty server: %d %q", rec.Code, rec.Body.String())
	}

	form.Set("create_name", "Book club & friends")
	rec = httptest.NewRecorder()
	f.h.handleUserFindDAVItems(rec, davAccountRequest("/api/accounts/dav/find", form))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-dav-items-found") || !strings.Contains(rec.Body.String(), "Book club &amp; friends") {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	if len(dav.collections) != 1 {
		t.Fatalf("collections = %v, want one", dav.collections)
	}
	for path, method := range dav.methods {
		if method != "MKCALENDAR" || !strings.HasPrefix(path, "/calendars/") {
			t.Fatalf("made %s with %s", path, method)
		}
		if !strings.Contains(rec.Body.String(), `checked name="item" value="`+f.server.URL+path+`"`) {
			t.Fatalf("new calendar %s is not ticked: %q", path, rec.Body.String())
		}
	}
	if !strings.Contains(dav.bodies[0], "Book club &amp; friends") {
		t.Fatalf("MKCALENDAR body doesn't escape the name: %q", dav.bodies[0])
	}
}

// An address book is made with an extended MKCOL, ticked and made the
// default, keeping what was ticked before.
func TestContactsOnlyAccountCreatesAddressBook(t *testing.T) {
	dav := newEmptyDAVServer()
	dav.collections["/books/family/"] = "Family"
	f := newUserContactPushFixture(t, "carddav", dav)
	form := url.Values{"service": {"contacts"}, "server_url": {f.server.URL}, "username": {"dav-user"}, "password": {"dav-pass"},
		"item": {f.server.URL + "/books/family/"}, "default_item": {f.server.URL + "/books/family/"}, "create_name": {"Work"}}

	rec := httptest.NewRecorder()
	f.h.handleUserFindDAVItems(rec, davAccountRequest("/api/accounts/dav/find", form))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ">Work<") {
		t.Fatalf("create: %d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(dav.bodies[0], "<card:addressbook/>") || !strings.Contains(dav.bodies[0], "<d:displayname>Work</d:displayname>") {
		t.Fatalf("MKCOL body = %q", dav.bodies[0])
	}
	for path, method := range dav.methods {
		if method != "MKCOL" {
			t.Fatalf("made %s with %s", path, method)
		}
		if !strings.Contains(rec.Body.String(), `checked name="item" value="`+f.server.URL+path+`"`) {
			t.Fatalf("new address book is not ticked: %q", rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), `checked name="item" value="`+f.server.URL+`/books/family/"`) {
		t.Fatalf("Family lost its tick: %q", rec.Body.String())
	}
}

// A server that won't make the collection says why, and discovery stops
// there instead of trying to make one elsewhere.
func TestDAVCreateRefused(t *testing.T) {
	dav := newEmptyDAVServer()
	dav.refuse = true
	server := httptest.NewServer(dav)
	defer server.Close()
	allowLocalDAVServers(t)

	create := &davCreate{Name: "Home"}
	_, err := discoverCalDAVCalendars(context.Background(), server.URL, "dav-user", "dav-pass", "", "", create)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("err = %v", err)
	}
	if len(dav.bodies) != 1 {
		t.Fatalf("asked %d times, want once", len(dav.bodies))
	}
	create = &davCreate{Name: "Home"}
	_, err = discoverCardDAVAddressBooksCandidates(context.Background(), []string{server.URL}, "dav-user", "dav-pass", nil, false, create)
	if err == nil || !strings.Contains(err.Error(), "address book") || len(dav.bodies) != 2 {
		t.Fatalf("err = %v, asked %d times", err, len(dav.bodies))
	}
}

// A WebDAV folder that names no calendar home is still no CalDAV server,
// even though listing it works.
func TestCalDAVDiscoveryNeedsReportedHome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path)
	}))
	defer server.Close()
	allowLocalDAVServers(t)

	if _, err := discoverCalDAVCalendars(context.Background(), server.URL, "u", "p", "", "", nil); err == nil {
		t.Fatal("a plain WebDAV folder was taken for an empty CalDAV server")
	}
	if _, err := discoverCalDAVCalendars(context.Background(), server.URL, "u", "p", "", "", &davCreate{Name: "Home"}); err == nil {
		t.Fatal("a calendar was made where the server named no home")
	}
}

func TestDAVCreateFromForm(t *testing.T) {
	for _, test := range []struct {
		name, value string
		wantErr     bool
	}{
		{"none", "  ", false},
		{"plain", " Family ", false},
		{"too long", strings.Repeat("a", 101), true},
		{"line break", "Fam\nily", true},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{"create_name": {test.value}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		create, err := davCreateFromForm(r)
		if (err != nil) != test.wantErr {
			t.Fatalf("%s: err = %v", test.name, err)
		}
		if test.name == "none" && create != nil || test.name == "plain" && create.Name != "Family" {
			t.Fatalf("%s: create = %+v", test.name, create)
		}
	}
}

// Calendar sync signs in to an empty CalDAV server, keeps its settings, and
// offers to create a calendar, which is then saved selected.
func TestCalendarSyncCreatesCalDAVCalendar(t *testing.T) {
	dav := newEmptyDAVServer()
	server := httptest.NewServer(dav)
	defer server.Close()
	allowLocalDAVServers(t)
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name)
		VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO accounts (id, user_id, provider, email_address, display_name, auth_method)
		VALUES ('imap-account', 'default', 'imap', 'person@example.com', 'Person', 'plain');`); err != nil {
		t.Fatal(err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, accountStore: accountStore}
	discover := func(form url.Values) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/imap-account/calendar/discover", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", "imap-account")
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "default", Username: "default"}))
		rec := httptest.NewRecorder()
		h.handleDiscoverAccountCalendars(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	body := discover(url.Values{"caldav_url": {server.URL}, "username": {"dav-user"}, "password": {"dav-pass"}})
	if !strings.Contains(body, "This server has no calendars yet") || !strings.Contains(body, "data-calendar-create-form") {
		t.Fatalf("empty server: %q", body)
	}
	// Creating signs in with the saved settings; the form sends only the URL.
	body = discover(url.Values{"caldav_url": {server.URL}, "create_name": {"Family"}})
	if !strings.Contains(body, "Created the calendar &#34;Family&#34;") {
		t.Fatalf("create: %q", body)
	}
	sources, err := db.ListCalendarSourcesForAccount(t.Context(), "default", "imap-account")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Name != "Family" || !sources[0].IsSelected {
		t.Fatalf("sources = %#v", sources)
	}
}
