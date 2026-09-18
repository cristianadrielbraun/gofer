package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

func TestDiscoverGoogleCalendarsPaginatesAndNormalizes(t *testing.T) {
	var pageTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me/calendarList" {
			t.Fatalf("path = %q, want calendar list endpoint", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer calendar-token" {
			t.Fatalf("authorization = %q, want bearer token", r.Header.Get("Authorization"))
		}
		pageTokens = append(pageTokens, r.URL.Query().Get("pageToken"))
		w.Header().Set("Content-Type", "application/json")
		if len(pageTokens) == 1 {
			_, _ = fmt.Fprint(w, `{"items":[{"id":"primary","summary":"Primary","timeZone":"Europe/Prague","accessRole":"owner","primary":true},{"id":"deleted","summary":"Deleted","deleted":true}],"nextPageToken":"page-2"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"items":[{"id":"team","summary":"Team","summaryOverride":"My Team","description":"Team events","backgroundColor":"#123456","accessRole":"reader"}]}`)
	}))
	defer server.Close()

	previousBase := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	t.Cleanup(func() { googleCalendarAPIBaseURL = previousBase })

	calendars, err := discoverGoogleCalendars(t.Context(), "calendar-token")
	if err != nil {
		t.Fatalf("discoverGoogleCalendars() error = %v", err)
	}
	if len(calendars) != 2 {
		t.Fatalf("discovered calendars = %#v, want two active calendars", calendars)
	}
	if calendars[0].RemoteID != "primary" || calendars[0].Name != "Primary" || !calendars[0].Primary {
		t.Fatalf("primary calendar = %#v", calendars[0])
	}
	if calendars[1].RemoteID != "team" || calendars[1].Name != "My Team" || calendars[1].Color != "#123456" {
		t.Fatalf("team calendar = %#v", calendars[1])
	}
	if strings.Join(pageTokens, ",") != ",page-2" {
		t.Fatalf("page tokens = %#v, want first page followed by page-2", pageTokens)
	}
}

func TestListGoogleCalendarEventsPaginatesAndNormalizes(t *testing.T) {
	var pageTokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendars/primary/events" {
			t.Fatalf("path = %q, want calendar events endpoint", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer calendar-token" {
			t.Fatalf("authorization = %q, want bearer token", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("singleEvents") != "true" || r.URL.Query().Get("orderBy") != "startTime" {
			t.Fatalf("event query = %v, want expanded chronological events", r.URL.Query())
		}
		pageTokens = append(pageTokens, r.URL.Query().Get("pageToken"))
		w.Header().Set("Content-Type", "application/json")
		if len(pageTokens) == 1 {
			_, _ = fmt.Fprint(w, `{"items":[{"id":"timed","iCalUID":"timed@example.com","status":"confirmed","summary":"Planning","start":{"dateTime":"2026-09-03T09:00:00+02:00","timeZone":"Europe/Prague"},"end":{"dateTime":"2026-09-03T10:00:00+02:00","timeZone":"Europe/Prague"},"organizer":{"displayName":"Person","email":"person@example.com"},"nextSyncToken":"ignored-on-page"}],"nextPageToken":"page-2"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"items":[{"id":"all-day","status":"confirmed","summary":"Holiday","start":{"date":"2026-09-04"},"end":{"date":"2026-09-05"}}],"nextSyncToken":"sync-2"}`)
	}))
	defer server.Close()

	previousBase := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	t.Cleanup(func() { googleCalendarAPIBaseURL = previousBase })

	page, err := listGoogleCalendarEvents(t.Context(), "calendar-token", "primary", calendar.EventQuery{
		WindowStart: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.FixedZone("CEST", 2*60*60)),
		WindowEnd:   time.Date(2026, time.October, 1, 0, 0, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	if err != nil {
		t.Fatalf("listGoogleCalendarEvents() error = %v", err)
	}
	if len(page.Events) != 2 || page.NextSyncCursor.Value != "sync-2" {
		t.Fatalf("event page = %#v, want two events and final sync cursor", page)
	}
	if page.Events[0].StartAt == nil || page.Events[0].StartAt.Hour() != 7 || page.Events[0].EndAt == nil {
		t.Fatalf("timed event normalization = %#v", page.Events[0])
	}
	if !page.Events[1].AllDay || page.Events[1].StartDate != "2026-09-04" || page.Events[1].EndDate != "2026-09-05" {
		t.Fatalf("all-day event normalization = %#v", page.Events[1])
	}
	if page.Events[0].OrganizerEmail != "person@example.com" {
		t.Fatalf("organizer normalization = %#v", page.Events[0])
	}
	if strings.Join(pageTokens, ",") != ",page-2" {
		t.Fatalf("page tokens = %#v, want first page followed by page-2", pageTokens)
	}
}

func TestHandleDiscoverAccountCalendarsStoresSourcesAndRendersStatus(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name)
		VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, auth_method)
		VALUES ('gmail-account', 'default', 'gmail', 'google-subject', 'person@gmail.com', 'Person', 'oauth2');`); err != nil {
		t.Fatalf("insert Gmail account: %v", err)
	}

	mailCredentials := mailauth.New(&mailauth.Config{GoogleClient: &oauth2.Config{}}, db, testMailboxCredentialKey)
	expiresAt := time.Now().Add(time.Hour)
	if err := mailCredentials.UpsertOAuthAccount(t.Context(), "gmail-account", providers.OAuthGoogle, "google-subject", "calendar-token", "refresh-token", "Bearer", &expiresAt, mailauth.GoogleCalendarReadOnlyScope); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	h := &Handler{db: db, accountStore: accountStore, mailboxAuth: mailCredentials}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me/calendarList" {
			t.Fatalf("path = %q, want calendar list endpoint", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"items":[{"id":"primary","summary":"Primary","timeZone":"Europe/Prague","accessRole":"owner","primary":true}]}`)
	}))
	defer server.Close()
	previousBase := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	t.Cleanup(func() { googleCalendarAPIBaseURL = previousBase })

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/gmail-account/calendar/discover", nil)
	req.SetPathValue("id", "gmail-account")
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "default", Username: "default"}))
	rec := httptest.NewRecorder()
	h.handleDiscoverAccountCalendars(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q, want success", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Discovered 1 Google calendar source(s)") {
		t.Fatalf("response body = %q, want discovery status", rec.Body.String())
	}
	sources, err := db.ListCalendarSourcesForAccount(t.Context(), "default", "gmail-account")
	if err != nil {
		t.Fatalf("ListCalendarSourcesForAccount() error = %v", err)
	}
	if len(sources) != 1 || sources[0].RemoteID != "primary" || !sources[0].IsPrimary || !sources[0].IsSelected {
		t.Fatalf("stored Calendar sources = %#v", sources)
	}

	accounts, err := db.GetAccounts(t.Context(), "default")
	if err != nil {
		t.Fatalf("GetAccounts() error = %v", err)
	}
	if len(accounts) != 1 || !accounts[0].CalendarSyncEnabled {
		t.Fatalf("accounts after discovery = %#v, want Calendar enabled", accounts)
	}
}
