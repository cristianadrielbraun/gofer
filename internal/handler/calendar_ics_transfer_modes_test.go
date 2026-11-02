package handler

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

// calendarTransferSetup is a Handler whose user alice syncs one writable
// Google or Microsoft calendar "cal", with sourceID its id in Gofer.
type calendarTransferSetup struct {
	h         *Handler
	sourceID  string
	serverURL string
}

// sharedCalendarTransferSetup is the shared store, as Open and Personal
// mode run.
func sharedCalendarTransferSetup(t *testing.T, provider, token string) calendarTransferSetup {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('alice', 'alice', 'alice', 'Alice');
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, auth_method)
		VALUES ('account', 'alice', ?, 'subject', 'alice@example.com', 'Alice', 'oauth2');`, provider); err != nil {
		t.Fatal(err)
	}
	client := &oauth2.Config{}
	credentials := mailauth.New(&mailauth.Config{GoogleClient: client, MicrosoftClient: client}, db, testMailboxCredentialKey)
	expires := time.Now().Add(time.Hour)
	oauthProvider, scope := providers.OAuthGoogle, mailauth.GoogleCalendarReadOnlyScope+" "+mailauth.GoogleCalendarEventsScope
	if provider == providers.ProviderOutlook {
		oauthProvider, scope = providers.OAuthMicrosoft, "https://graph.microsoft.com/Calendars.ReadWrite"
	}
	if err := credentials.UpsertOAuthAccount(t.Context(), "account", oauthProvider, "subject", token, "refresh", "Bearer", &expires, scope); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "alice", "account", provider, []storage.CalendarSource{{RemoteID: "cal", Name: "Evenings", AccessRole: "owner", IsPrimary: true, IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListSelectedCalendarSources(t.Context(), "alice")
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %v, %v", sources, err)
	}
	return calendarTransferSetup{h: &Handler{db: db, mailboxAuth: credentials}, sourceID: sources[0].ID}
}

// managedCalendarTransferSetup is the per-user stores, as Managed mode
// runs, where each provider request passes the calendar's endpoint guard.
func managedCalendarTransferSetup(t *testing.T, provider string, api http.Handler) calendarTransferSetup {
	t.Helper()
	f := newUserContactPushFixture(t, provider, api)
	oauthProvider, scope := "google", mailauth.GoogleCalendarReadOnlyScope+" "+mailauth.GoogleCalendarEventsScope
	if provider == providers.ProviderOutlook {
		oauthProvider, scope = "microsoft", "https://graph.microsoft.com/Calendars.ReadWrite"
	}
	expires := time.Now().Add(time.Hour)
	account := f.accounts["alice"]
	if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, oauthProvider, "subject", "alice-access", "alice-refresh", "Bearer", &expires, scope); err != nil {
		t.Fatal(err)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		return db.ReplaceCalendarSources(t.Context(), "alice", account.ID, provider, []storage.CalendarSource{{ID: "same-source", RemoteID: "cal", Name: "Evenings", AccessRole: "owner", IsPrimary: true, IsSelected: true}})
	}); err != nil {
		t.Fatal(err)
	}
	return calendarTransferSetup{h: f.h, sourceID: "same-source", serverURL: f.server.URL}
}

// fakeCalDAVCalendar is a CalDAV calendar at /cal/ that refuses a second
// event with a UID it has, as servers do.
type fakeCalDAVCalendar struct {
	mu                 sync.Mutex
	username, password string
	resources          map[string]string
}

func (f *fakeCalDAVCalendar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if username, password, ok := r.BasicAuth(); !ok || username != f.username || password != f.password || !strings.HasPrefix(r.URL.Path, "/cal/") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		uid := regexp.MustCompile(`(?m)^UID:(.*)\r$`).FindStringSubmatch(string(data))
		for _, existing := range f.resources {
			if r.Header.Get("If-None-Match") != "*" || len(uid) != 2 || strings.Contains(existing, "UID:"+uid[1]+"\r\n") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		f.resources[r.URL.Path] = string(data)
		w.WriteHeader(http.StatusCreated)
	case "REPORT":
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
		for path, data := range f.resources {
			var escaped strings.Builder
			_ = xml.EscapeText(&escaped, []byte(data))
			fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, path, escaped.String())
		}
		io.WriteString(w, `</d:multistatus>`)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// sharedCalDAVTransferSetup is the shared store with a CalDAV calendar.
func sharedCalDAVTransferSetup(t *testing.T, api *fakeCalDAVCalendar) calendarTransferSetup {
	t.Helper()
	server := httptest.NewTLSServer(api)
	t.Cleanup(server.Close)
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	t.Cleanup(func() { davHTTPTransport = prior })
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('alice', 'alice', 'alice', 'Alice');
		INSERT INTO accounts (id, user_id, provider, email_address, display_name, email_sync_enabled) VALUES ('account', 'alice', 'imap', 'alice@example.com', 'Alice', 0);`); err != nil {
		t.Fatal(err)
	}
	store, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCalDAVConfig(t.Context(), "alice", "account", server.URL+"/", api.username, api.password, false); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "alice", "account", "caldav", []storage.CalendarSource{{ID: "dav-source", RemoteID: server.URL + "/cal/", Name: "Evenings", AccessRole: "owner", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	return calendarTransferSetup{h: &Handler{db: db, accountStore: store}, sourceID: "dav-source"}
}

// managedCalDAVTransferSetup is the per-user stores with a CalDAV calendar.
func managedCalDAVTransferSetup(t *testing.T, api *fakeCalDAVCalendar) calendarTransferSetup {
	t.Helper()
	f := newUserContactPushFixture(t, "carddav", api)
	account := f.accounts["alice"]
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(local *config.AccountStore, db *storage.DB) error {
		if err := local.SaveCalDAVConfig(t.Context(), "alice", account.ID, f.server.URL+"/", api.username, api.password, false); err != nil {
			return err
		}
		return db.ReplaceCalendarSources(t.Context(), "alice", account.ID, "caldav", []storage.CalendarSource{{ID: "same-source", RemoteID: f.server.URL + "/cal/", Name: "Evenings", AccessRole: "owner", IsSelected: true}})
	}); err != nil {
		t.Fatal(err)
	}
	return calendarTransferSetup{h: f.h, sourceID: "same-source"}
}

// A CalDAV calendar imports and exports through the handlers in both
// stores, a second import adding nothing.
func TestCalDAVCalendarTransferInEachMode(t *testing.T) {
	for _, mode := range []string{"shared", "managed"} {
		t.Run(mode, func(t *testing.T) {
			api := &fakeCalDAVCalendar{username: "alice-calendar", password: "calendar-secret", resources: map[string]string{}}
			setup := sharedCalDAVTransferSetup
			if mode == "managed" {
				setup = managedCalDAVTransferSetup
			}
			s := setup(t, api)
			if first := s.importFile(t, calendarImportTestFile); first["error"] != nil || first["added"] != float64(3) {
				t.Fatalf("first import = %v", first)
			}
			if second := s.importFile(t, calendarImportTestFile); second["error"] != nil || second["added"] != float64(0) || second["existing"] != float64(3) || second["failed"] != float64(0) || len(api.resources) != 3 {
				t.Fatalf("second import = %v, %d events", second, len(api.resources))
			}
			if exported := s.export(t); strings.Count(exported, "BEGIN:VEVENT") != 4 || !strings.Contains(exported, "X-WR-CALNAME:Evenings") {
				t.Fatalf("export:\n%s", exported)
			}
		})
	}
}

// importFile posts file to the calendar's import, as the dialog does, and
// returns the stream's last line.
func (s calendarTransferSetup) importFile(t *testing.T, file string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/calendar/sources/"+s.sourceID+"/import", strings.NewReader(file))
	req.SetPathValue("id", s.sourceID)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "alice"}))
	rec := httptest.NewRecorder()
	s.h.handleCalendarSourceImport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var last map[string]any
	scanner := bufio.NewScanner(rec.Body)
	for scanner.Scan() {
		last = nil
		if err := json.Unmarshal(scanner.Bytes(), &last); err != nil {
			t.Fatalf("import line %q: %v", scanner.Text(), err)
		}
	}
	return last
}

// export downloads the calendar as the menu's link does.
func (s calendarTransferSetup) export(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/calendar/sources/"+s.sourceID+"/export", nil)
	req.SetPathValue("id", s.sourceID)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "alice"}))
	rec := httptest.NewRecorder()
	s.h.handleCalendarSourceExport(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "Evenings.ics") {
		t.Fatalf("export: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	return rec.Body.String()
}

// Google and Microsoft calendars import and export through the handlers in
// both stores, a second import adding nothing.
func TestCalendarTransferInEachMode(t *testing.T) {
	for _, provider := range []string{providers.ProviderGmail, providers.ProviderOutlook} {
		for _, mode := range []string{"shared", "managed"} {
			t.Run(provider+" "+mode, func(t *testing.T) {
				token := "token"
				if mode == "managed" {
					token = "alice-access"
				}
				var api http.Handler
				google := &fakeGoogleCalendar{token: token}
				outlook := &fakeOutlookCalendar{token: token, deleted: map[string]bool{}, changed: map[string]map[string]any{}}
				file, want, events := calendarImportTestFile, 3, func() int { return len(google.imported) }
				if provider == providers.ProviderOutlook {
					api, file, want, events = outlook, outlookImportTestFile, 2, func() int { return len(outlook.events) }
				} else {
					api = google
				}
				var setup calendarTransferSetup
				if mode == "shared" {
					server := httptest.NewServer(api)
					t.Cleanup(server.Close)
					setup = sharedCalendarTransferSetup(t, provider, token)
					setup.serverURL = server.URL
				} else {
					setup = managedCalendarTransferSetup(t, provider, api)
				}
				previousGoogle, previousGraph := googleCalendarAPIBaseURL, outlookGraphBaseURL
				googleCalendarAPIBaseURL, outlookGraphBaseURL = setup.serverURL, setup.serverURL
				t.Cleanup(func() { googleCalendarAPIBaseURL, outlookGraphBaseURL = previousGoogle, previousGraph })

				first := setup.importFile(t, file)
				if first["error"] != nil || first["added"] != float64(want) || first["calendar"] != "Evenings" {
					t.Fatalf("first import = %v", first)
				}
				added := events()
				second := setup.importFile(t, file)
				if second["error"] != nil || second["added"] != float64(0) || events() != added {
					t.Fatalf("second import = %v, %d events after %d", second, events(), added)
				}
				exported := setup.export(t)
				if strings.Count(exported, "BEGIN:VEVENT") < want || !strings.Contains(exported, "X-WR-CALNAME:Evenings") {
					t.Fatalf("export:\n%s", exported)
				}
			})
		}
	}
}
