package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type managedDAVBlock struct {
	started   chan struct{}
	cancelled chan struct{}
	abort     chan struct{}
}

type managedDAVFixture struct {
	mu              sync.Mutex
	calls           map[string]int
	failingOwner    string
	blockedCalendar *managedDAVBlock
}

func (a *managedDAVFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner, password, ok := r.BasicAuth()
	if !ok || (owner != "alice" && owner != "bob") || password != owner+"-dav-secret" || !strings.Contains(r.URL.Path, "/"+owner+"/") {
		http.Error(w, "wrong owner credentials", 401)
		return
	}
	if r.Method != "REPORT" {
		http.Error(w, "expected DAV REPORT", 400)
		return
	}
	// Drain REPORT before waiting: a server handler with an unread request body
	// cannot reliably observe a peer closing its connection while stalled.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		http.Error(w, "invalid fixture REPORT", http.StatusBadRequest)
		return
	}
	calendar := strings.HasPrefix(r.URL.Path, "/calendars/")
	a.mu.Lock()
	a.calls[owner+"|"+r.URL.Path]++
	failing := a.failingOwner == owner
	block := a.blockedCalendar
	a.mu.Unlock()
	if block != nil && owner == "alice" && calendar {
		select {
		case block.started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			select {
			case block.cancelled <- struct{}{}:
			default:
			}
		case <-block.abort:
			http.Error(w, "fixture cleanup", http.StatusServiceUnavailable)
		}
		return
	}
	if failing {
		http.Error(w, "temporary fixture provider outage", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	if calendar {
		start := time.Now().UTC().Add(time.Hour).Truncate(time.Hour)
		ics := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Fixture//EN\r\nBEGIN:VEVENT\r\nUID:same-native-event\r\nDTSTART:%s\r\nDTEND:%s\r\nSUMMARY:%s native appointment\r\nDESCRIPTION:%s native notes\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", start.Format("20060102T150405Z"), start.Add(time.Hour).Format("20060102T150405Z"), owner, owner)
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>%sevent.ics</d:href><d:propstat><d:prop><d:getetag>"native-calendar-1"</d:getetag><c:calendar-data>`, r.URL.Path)
		xml.EscapeText(w, []byte(ics))
		fmt.Fprint(w, `</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	} else {
		vcard := fmt.Sprintf("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:same-native-card\r\nFN:%s native friend\r\nEMAIL:%s-friend@example.test\r\nEND:VCARD\r\n", owner, owner)
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav"><d:sync-token>%s-native-cursor</d:sync-token><d:response><d:href>%sperson.vcf</d:href><d:propstat><d:prop><d:getetag>"native-card-1"</d:getetag><c:address-data>`, owner, r.URL.Path)
		xml.EscapeText(w, []byte(vcard))
		fmt.Fprint(w, `</c:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
	}
}

func TestManagedApplicationNativeDAVWorkersHTTPIsolationAndRestart(t *testing.T) {
	api := &managedDAVFixture{calls: make(map[string]int)}
	server := httptest.NewTLSServer(api)
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	// Trust only this local TLS fixture's certificate; requests still use the
	// production DAV URLs, Basic authentication, HTTP client and XML parsers.
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	f := newManagedMailFixture(t, func(db *storage.DB, accounts *config.AccountStore, ids map[string]string) {
		for _, owner := range []string{"alice", "bob"} {
			book := server.URL + "/books/" + owner + "/"
			if err := accounts.SaveContactSyncConfig(t.Context(), owner, ids[owner], models.ContactSyncConfig{Provider: "carddav", Enabled: true, Username: owner, BaseURL: server.URL, AddressBooks: []models.ContactAddressBook{{ID: owner + "-book", URL: book, Default: true}}}, owner+"-dav-secret"); err != nil {
				t.Fatal(err)
			}
			if err := accounts.SaveCalDAVConfig(t.Context(), owner, ids[owner], server.URL+"/calendars/"+owner+"/", owner, owner+"-dav-secret", false); err != nil {
				t.Fatal(err)
			}
			if err := db.ReplaceCalendarSources(t.Context(), owner, ids[owner], "caldav", []storage.CalendarSource{{ID: owner + "-calendar", RemoteID: server.URL + "/calendars/" + owner + "/", Name: owner + " native calendar", IsPrimary: true, IsSelected: true, AccessRole: "owner"}}); err != nil {
				t.Fatal(err)
			}
		}
	})
	assertImported := func() bool {
		for _, owner := range []string{"alice", "bob"} {
			var cards, events int
			if err := f.app.storage.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
				return db.Read().QueryRow(`SELECT (SELECT count(*) FROM contact_cards WHERE user_id=? AND provider='carddav'),(SELECT count(*) FROM calendar_events WHERE user_id=? AND summary=?)`, owner, owner, owner+" native appointment").Scan(&cards, &events)
			}); err != nil {
				t.Fatal(err)
			}
			if cards != 1 || events != 1 {
				return false
			}
		}
		return true
	}
	// Startup scheduling, rather than direct provider/handler calls, performs
	// the first imports against source credentials copied by the real migration.
	awaitManagedCondition(t, assertImported)
	for _, owner := range []string{"alice", "bob"} {
		response := f.form(owner, "POST", "/api/settings/contacts/accounts/sync", url.Values{"account_id": {f.accounts[owner]}})
		if response.Code != 200 || response.Header().Get("X-Gofer-Status") == "error" {
			t.Fatal("contact HTTP sync", owner, response.Code, response.Body.String())
		}
		response = f.form(owner, "POST", "/api/calendar/sync", url.Values{"account_id": {f.accounts[owner]}})
		if response.Code != 200 || response.Header().Get("X-Gofer-Status") == "error" {
			t.Fatal("calendar HTTP sync", owner, response.Code, response.Body.String())
		}
		other := "alice"
		if owner == "alice" {
			other = "bob"
		}
		for path, expected := range map[string]string{"/contacts": owner + " native friend", "/calendar": owner + " native appointment"} {
			response = managedRequest(f.app, "GET", path, f.tokens[owner], "")
			if response.Code != 200 || !strings.Contains(response.Body.String(), expected) || strings.Contains(response.Body.String(), other+" native friend") || strings.Contains(response.Body.String(), other+" native appointment") {
				t.Fatal("DAV rendering mixed owners", owner, path, response.Code)
			}
		}
	}
	api.mu.Lock()
	for _, owner := range []string{"alice", "bob"} {
		if api.calls[owner+"|/books/"+owner+"/"] < 2 || api.calls[owner+"|/calendars/"+owner+"/"] < 2 {
			api.mu.Unlock()
			t.Fatal("startup and manual HTTP did not reach native DAV", owner)
		}
	}
	api.mu.Unlock()
	// One tenant's provider outage must be visible as an error, preserve the
	// last imported data, and leave another tenant's live sync usable.
	api.mu.Lock()
	api.failingOwner = "alice"
	api.mu.Unlock()
	if response := f.form("alice", "POST", "/api/settings/contacts/accounts/sync", url.Values{"account_id": {f.accounts["alice"]}}); response.Header().Get("X-Gofer-Status") != "error" || !strings.Contains(response.Body.String(), "503") {
		t.Fatal("contact outage was hidden", response.Code, response.Body.String())
	}
	if response := f.form("alice", "POST", "/api/calendar/sync", url.Values{"account_id": {f.accounts["alice"]}}); response.Header().Get("X-Gofer-Status") != "error" {
		t.Fatal("calendar outage was hidden", response.Code)
	}
	for _, path := range []string{"/api/settings/contacts/accounts/sync", "/api/calendar/sync"} {
		if response := f.form("bob", "POST", path, url.Values{"account_id": {f.accounts["bob"]}}); response.Code != http.StatusOK || response.Header().Get("X-Gofer-Status") == "error" {
			t.Fatal("foreign provider outage blocked Bob", path, response.Code)
		}
	}
	if !assertImported() {
		t.Fatal("provider outage removed retained data")
	}
	api.mu.Lock()
	api.failingOwner = ""
	api.mu.Unlock()
	for _, path := range []string{"/api/settings/contacts/accounts/sync", "/api/calendar/sync"} {
		if response := f.form("alice", "POST", path, url.Values{"account_id": {f.accounts["alice"]}}); response.Code != http.StatusOK || response.Header().Get("X-Gofer-Status") == "error" {
			t.Fatal("provider recovery failed", path, response.Code)
		}
	}
	if !assertImported() {
		t.Fatal("provider recovery duplicated or removed rows")
	}
	if err := storage.VerifyUserStorageRuntimeBoundary(t.Context(), f.path); err != nil {
		t.Fatal(err)
	}
	block := &managedDAVBlock{started: make(chan struct{}, 1), cancelled: make(chan struct{}, 1), abort: make(chan struct{})}
	t.Cleanup(func() { close(block.abort) })
	api.mu.Lock()
	api.blockedCalendar = block
	api.mu.Unlock()
	requestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		requestDone <- f.form("alice", "POST", "/api/calendar/sync", url.Values{"account_id": {f.accounts["alice"]}})
	}()
	select {
	case <-block.started:
	case <-time.After(20 * time.Second):
		t.Fatal("calendar request did not reach blocked native provider")
	}
	// With the application cache limited to one DB, another owner's actual
	// HTTP read/write must complete while Alice's provider is stalled.
	response := managedRequest(f.app, "PATCH", "/api/settings/ui", f.tokens["bob"], `{"theme":"dark"}`)
	if response.Code != http.StatusOK {
		t.Fatal("blocked provider retained a database lease", response.Code)
	}
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown returned before joining blocked HTTP request")
	}
	select {
	case <-block.cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel native provider request")
	}
	api.mu.Lock()
	api.blockedCalendar = nil
	api.mu.Unlock()
	app, err := newManagedApplication(t.Context(), f.path, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.app = app
	if !assertImported() {
		t.Fatal("DAV rows lost across restart")
	}
}
