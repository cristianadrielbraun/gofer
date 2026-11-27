package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// fakeGoogleCalendarList lists two Google calendars and lets their colors
// be changed in the account's calendar list, recording each change. answer,
// when set, is the status Google answers a change with instead.
type fakeGoogleCalendarList struct {
	mu      sync.Mutex
	colors  map[string]string
	changes []string
	answer  int
	scope   string
}

func (f *fakeGoogleCalendarList) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	const list = "/calendar/v3/users/me/calendarList"
	switch {
	case r.URL.Path == "/token":
		fmt.Fprintf(w, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600,"scope":%q}`, f.scope)
	case r.Method == http.MethodGet && r.URL.Path == list:
		fmt.Fprintf(w, `{"items":[{"id":"alice@example.com","summary":"Alice","primary":true,"accessRole":"owner","backgroundColor":%q},{"id":"club@group.calendar.google.com","summary":"Book club","accessRole":"reader","backgroundColor":%q}]}`,
			f.colors["alice@example.com"], f.colors["club@group.calendar.google.com"])
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, list+"/"):
		body, _ := io.ReadAll(r.Body)
		id := strings.TrimPrefix(r.URL.Path, list+"/")
		f.changes = append(f.changes, id+"?"+r.URL.RawQuery+" "+strings.TrimSpace(string(body)))
		if f.answer != 0 {
			w.WriteHeader(f.answer)
			fmt.Fprintf(w, `{"error":{"code":%d,"message":"No"}}`, f.answer)
			return
		}
		var change struct{ BackgroundColor string }
		_ = json.Unmarshal(body, &change)
		f.colors[id] = change.BackgroundColor
		fmt.Fprintf(w, `{"id":%q,"backgroundColor":%q}`, id, change.BackgroundColor)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// A color picked for a Google calendar goes to the account's Google calendar
// list when Gofer has full calendar access. When it hasn't, or Google
// refuses, Gofer keeps the color itself, and syncing the calendar list
// again leaves it alone. A failure on Google's side changes nothing.
func TestCalendarColorOnGoogle(t *testing.T) {
	api := &fakeGoogleCalendarList{colors: map[string]string{"alice@example.com": "#9fe1e7", "club@group.calendar.google.com": "#16a765"}}
	f := newUserContactPushFixture(t, "gmail", api)
	previous := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = f.server.URL + "/calendar/v3"
	t.Cleanup(func() { googleCalendarAPIBaseURL = previous })
	account := f.accounts["alice"]
	grant := func(scopes string) {
		t.Helper()
		api.mu.Lock()
		api.scope = scopes
		api.mu.Unlock()
		expires := time.Now().Add(time.Hour)
		if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, "google", "subject", "alice-access", "alice-refresh", "Bearer", &expires, scopes); err != nil {
			t.Fatal(err)
		}
	}
	grant("https://www.googleapis.com/auth/calendar")
	discover := func() {
		t.Helper()
		if result, err := f.h.runUserCalendarDiscovery(t.Context(), "alice", account.ID, nil, nil, false); err != nil || result.failed {
			t.Fatal(err, result.message)
		}
	}
	discover()
	sources := func() map[string]storage.CalendarSource {
		t.Helper()
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
	if err := f.h.userAccounts.SetCalendarSourcesSelected(t.Context(), "alice", account.ID, []string{sources()["alice@example.com"].ID, sources()["club@group.calendar.google.com"].ID}); err != nil {
		t.Fatal(err)
	}
	pick := func(remoteID, color string) (*httptest.ResponseRecorder, calendarColorResult) {
		t.Helper()
		id := sources()[remoteID].ID
		req := httptest.NewRequest(http.MethodPost, "/api/calendar/sources/"+id+"/color", strings.NewReader(fmt.Sprintf(`{"color":%q}`, color)))
		req.SetPathValue("id", id)
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "alice"}))
		rec := httptest.NewRecorder()
		f.h.handleCalendarSourceColor(rec, req)
		var result calendarColorResult
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return rec, result
	}
	colors := func(remoteID string) string {
		t.Helper()
		source := sources()[remoteID]
		return source.Color + " " + source.ChosenColor
	}

	// With full calendar access the color goes to Google, with text that
	// reads on it, and becomes the calendar's own.
	if rec, result := pick("club@group.calendar.google.com", "#2563EB"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#2563eb"}) {
		t.Fatalf("pick on Google: %d %s", rec.Code, rec.Body.String())
	}
	if want := `club@group.calendar.google.com?colorRgbFormat=true {"backgroundColor":"#2563eb","foregroundColor":"#ffffff"}`; strings.Join(api.changes, "; ") != want {
		t.Fatalf("Google saw %q, want %q", api.changes, want)
	}
	if got := colors("club@group.calendar.google.com"); got != "#2563eb " {
		t.Fatalf("after Google took it, color = %q", got)
	}
	discover()
	if got := colors("club@group.calendar.google.com"); got != "#2563eb " {
		t.Fatalf("after syncing the list, color = %q", got)
	}

	// Google refusing it keeps the color in Gofer, and the list's next
	// sync, still with Google's color, leaves it alone.
	api.answer = http.StatusForbidden
	if rec, result := pick("alice@example.com", "#facc15"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#facc15", InGoferOnly: true, Reason: calendarColorGoogleAccess}) {
		t.Fatalf("pick Google refused: %d %s", rec.Code, rec.Body.String())
	}
	discover()
	if got := colors("alice@example.com"); got != "#9fe1e7 #facc15" {
		t.Fatalf("kept in Gofer, after syncing the list, color = %q", got)
	}

	// A failure on Google's side is no refusal: nothing changes.
	api.answer = http.StatusInternalServerError
	if rec, _ := pick("alice@example.com", "#ef4444"); rec.Code != http.StatusBadGateway {
		t.Fatalf("pick while Google fails: %d %s", rec.Code, rec.Body.String())
	}
	if got := colors("alice@example.com"); got != "#9fe1e7 #facc15" {
		t.Fatalf("after Google failed, color = %q", got)
	}

	// Without full calendar access Gofer doesn't ask Google at all.
	api.answer, api.changes = 0, nil
	grant("https://www.googleapis.com/auth/calendar.readonly https://www.googleapis.com/auth/calendar.events https://www.googleapis.com/auth/calendar.app.created")
	if rec, result := pick("club@group.calendar.google.com", "#ef4444"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#ef4444", InGoferOnly: true, Reason: calendarColorGoogleAccess}) {
		t.Fatalf("pick without full access: %d %s", rec.Code, rec.Body.String())
	}
	if len(api.changes) != 0 {
		t.Fatalf("Google was asked without full access: %v", api.changes)
	}
	if got := colors("club@group.calendar.google.com"); got != "#2563eb #ef4444" {
		t.Fatalf("kept in Gofer, color = %q", got)
	}

	// An empty color goes back to Google's.
	if rec, result := pick("club@group.calendar.google.com", ""); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#2563eb"}) {
		t.Fatalf("back to Google's color: %d %s", rec.Code, rec.Body.String())
	}
	if got := colors("club@group.calendar.google.com"); got != "#2563eb " {
		t.Fatalf("back to Google's, color = %q", got)
	}

	if rec, _ := pick("club@group.calendar.google.com", "blue"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a color that isn't #rrggbb: %d", rec.Code)
	}
}

// fakeOutlookCalendarColors lists two Outlook calendars with their exact
// colors and lets the colors be changed, recording each change. answer,
// when set, is the status Microsoft answers a change with instead, and
// ignore leaves the color as it was, as Microsoft does with a property it
// doesn't let apps change.
type fakeOutlookCalendarColors struct {
	mu      sync.Mutex
	colors  map[string]string
	changes []string
	answer  int
	ignore  bool
}

func (f *fakeOutlookCalendarColors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	id := strings.TrimPrefix(r.URL.Path, "/me/calendars/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/me/calendars":
		fmt.Fprintf(w, `{"value":[{"id":"main","name":"Calendar","isDefaultCalendar":true,"canEdit":true,"color":"auto","hexColor":%q},{"id":"club","name":"Book club","canEdit":true,"color":"auto","hexColor":%q}]}`, f.colors["main"], f.colors["club"])
	case r.Method == http.MethodPatch && id != r.URL.Path:
		body, _ := io.ReadAll(r.Body)
		f.changes = append(f.changes, id+" "+strings.TrimSpace(string(body)))
		if f.answer != 0 {
			w.WriteHeader(f.answer)
			fmt.Fprintf(w, `{"error":{"code":"ErrorAccessDenied","message":"No"}}`)
			return
		}
		var change struct{ HexColor string }
		_ = json.Unmarshal(body, &change)
		if !f.ignore {
			f.colors[id] = change.HexColor
		}
		fmt.Fprintf(w, `{"id":%q,"color":"auto","hexColor":%q}`, id, f.colors[id])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// An Outlook calendar takes the exact color picked. When the account's
// sign-in can't change calendars, or Microsoft refuses the color or keeps
// its own, Gofer keeps the color itself until Outlook's changes; a failure
// on Microsoft's side changes nothing.
func TestCalendarColorOnOutlook(t *testing.T) {
	api := &fakeOutlookCalendarColors{colors: map[string]string{"main": "", "club": "#abebdc"}}
	f := newUserContactPushFixture(t, "outlook", api)
	previous := outlookGraphBaseURL
	outlookGraphBaseURL = f.server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previous })
	account := f.accounts["alice"]
	grant := func(scopes string) {
		t.Helper()
		expires := time.Now().Add(time.Hour)
		if err := f.h.userCredentials.UpsertForUser(t.Context(), "alice", account.ID, "microsoft", "subject", "alice-access", "alice-refresh", "Bearer", &expires, scopes); err != nil {
			t.Fatal(err)
		}
	}
	grant("https://graph.microsoft.com/Calendars.ReadWrite")
	discover := func() {
		t.Helper()
		if result, err := f.h.runUserCalendarDiscovery(t.Context(), "alice", account.ID, nil, nil, false); err != nil || result.failed {
			t.Fatal(err, result.message)
		}
	}
	discover()
	sources := func() map[string]storage.CalendarSource {
		t.Helper()
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
	if err := f.h.userAccounts.SetCalendarSourcesSelected(t.Context(), "alice", account.ID, []string{sources()["main"].ID, sources()["club"].ID}); err != nil {
		t.Fatal(err)
	}
	pick := func(remoteID, color string) (*httptest.ResponseRecorder, calendarColorResult) {
		t.Helper()
		id := sources()[remoteID].ID
		req := httptest.NewRequest(http.MethodPost, "/api/calendar/sources/"+id+"/color", strings.NewReader(fmt.Sprintf(`{"color":%q}`, color)))
		req.SetPathValue("id", id)
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "alice"}))
		rec := httptest.NewRecorder()
		f.h.handleCalendarSourceColor(rec, req)
		var result calendarColorResult
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return rec, result
	}
	colors := func(remoteID string) string {
		t.Helper()
		source := sources()[remoteID]
		return source.Color + " " + source.ChosenColor
	}
	if got := colors("club"); got != "#abebdc " {
		t.Fatalf("a custom Outlook color reads as %q", got)
	}

	// The exact color goes to Microsoft and becomes the calendar's own,
	// which reading the list again keeps.
	if rec, result := pick("club", "#D946EF"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#d946ef"}) {
		t.Fatalf("pick on Outlook: %d %s", rec.Code, rec.Body.String())
	}
	if want := `club {"hexColor":"#d946ef"}`; strings.Join(api.changes, "; ") != want {
		t.Fatalf("Microsoft saw %q, want %q", api.changes, want)
	}
	discover()
	if got := colors("club"); got != "#d946ef " {
		t.Fatalf("after syncing the list, color = %q", got)
	}

	// Microsoft keeping its own color, or refusing, keeps the color in
	// Gofer, which syncing leaves until Outlook's color changes.
	api.ignore = true
	if rec, result := pick("club", "#ef4444"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#ef4444", InGoferOnly: true, Reason: calendarColorRefused}) {
		t.Fatalf("pick Microsoft ignored: %d %s", rec.Code, rec.Body.String())
	}
	api.ignore = false
	for _, status := range []int{http.StatusForbidden, http.StatusBadRequest} {
		api.answer = status
		if rec, result := pick("main", "#c00000"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#c00000", InGoferOnly: true, Reason: calendarColorRefused}) {
			t.Fatalf("pick Microsoft refused with %d: %d %s", status, rec.Code, rec.Body.String())
		}
	}
	api.answer = 0
	discover()
	if got := colors("main"); got != "#2563eb #c00000" {
		t.Fatalf("kept in Gofer, color = %q", got)
	}
	api.colors["main"] = "#f7630c"
	discover()
	if got := colors("main"); got != "#f7630c " {
		t.Fatalf("after Outlook's color changed, color = %q", got)
	}

	// A failure on Microsoft's side is no refusal, and a calendar gone from
	// Outlook is gone.
	api.answer = http.StatusInternalServerError
	if rec, _ := pick("main", "#ffc000"); rec.Code != http.StatusBadGateway || colors("main") != "#f7630c " {
		t.Fatalf("pick while Microsoft fails: %d %s, color %q", rec.Code, rec.Body.String(), colors("main"))
	}
	api.answer = http.StatusNotFound
	if rec, _ := pick("main", "#ffc000"); rec.Code != http.StatusNotFound {
		t.Fatalf("pick on a calendar gone from Outlook: %d %s", rec.Code, rec.Body.String())
	}

	// A sign-in that can't change calendars keeps the color in Gofer without
	// asking Microsoft.
	api.answer, api.changes = 0, nil
	grant("https://graph.microsoft.com/Calendars.Read")
	if rec, result := pick("main", "#ffc000"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#ffc000", InGoferOnly: true, Reason: calendarColorReconnect}) {
		t.Fatalf("pick without write access: %d %s", rec.Code, rec.Body.String())
	}
	if len(api.changes) != 0 {
		t.Fatalf("Microsoft was asked without write access: %v", api.changes)
	}
}

// fakeCalDAVCalendarColors keeps CalDAV calendars' calendar-color and
// answers PROPPATCH and PROPFIND for it, as answer says: "" takes the color,
// "property" refuses the property in a 207, "ignore" says yes and keeps
// nothing, or a status answers the whole request.
type fakeCalDAVCalendarColors struct {
	mu      sync.Mutex
	colors  map[string]string
	answer  string
	changes []string
}

func (f *fakeCalDAVCalendarColors) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	switch r.Method {
	case "PROPPATCH":
		f.changes = append(f.changes, r.URL.Path+" "+string(body))
		if status, err := strconv.Atoi(f.answer); err == nil {
			w.WriteHeader(status)
			return
		}
		state := "200 OK"
		if f.answer == "property" {
			state = "403 Forbidden"
		}
		if f.answer == "" {
			start := strings.Index(string(body), "<a:calendar-color>") + len("<a:calendar-color>")
			end := strings.Index(string(body), "</a:calendar-color>")
			// Apple's apps keep the alpha too.
			f.colors[r.URL.Path] = strings.ToUpper(string(body)[start:end]) + "FF"
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/"><d:response><d:href>%s</d:href><d:propstat><d:prop><a:calendar-color/></d:prop><d:status>HTTP/1.1 %s</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, state)
	case "PROPFIND":
		if r.Header.Get("Depth") != "0" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:a="http://apple.com/ns/ical/"><d:response><d:href>%s</d:href><d:propstat><d:prop><a:calendar-color>%s</a:calendar-color></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, r.URL.Path, f.colors[r.URL.Path])
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// A CalDAV calendar's color goes to its server and is read back. A server
// that refuses it, in a 207 or as a whole, or says yes but doesn't keep it,
// leaves the color kept in Gofer; a failure on the server changes nothing,
// and a calendar gone from it is gone.
func TestCalendarColorOnCalDAV(t *testing.T) {
	dav := &fakeCalDAVCalendarColors{colors: map[string]string{"/calendars/home/": "#16A765FF"}}
	server := httptest.NewServer(dav)
	defer server.Close()
	allowLocalDAVServers(t)
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO accounts (id, user_id, provider, email_address, display_name, auth_method) VALUES ('imap-account', 'default', 'imap', 'robin@example.com', 'Robin', 'plain');`); err != nil {
		t.Fatal(err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SaveCalDAVConfig(t.Context(), "default", "imap-account", server.URL, "dav-user", "dav-pass", false); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "default", "imap-account", "caldav", []storage.CalendarSource{
		{RemoteID: server.URL + "/calendars/home/", Name: "Home", Color: "#16a765", IsPrimary: true, IsSelected: true},
	}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, accountStore: accountStore}
	home := func() storage.CalendarSource {
		t.Helper()
		sources, err := db.ListCalendarSourcesForAccount(t.Context(), "default", "imap-account")
		if err != nil || len(sources) != 1 {
			t.Fatalf("sources = %+v, %v", sources, err)
		}
		return sources[0]
	}
	pick := func(color string) (*httptest.ResponseRecorder, calendarColorResult) {
		t.Helper()
		id := home().ID
		req := httptest.NewRequest(http.MethodPost, "/api/calendar/sources/"+id+"/color", strings.NewReader(fmt.Sprintf(`{"color":%q}`, color)))
		req.SetPathValue("id", id)
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "default", Username: "default"}))
		rec := httptest.NewRecorder()
		h.handleCalendarSourceColor(rec, req)
		var result calendarColorResult
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return rec, result
	}

	if rec, result := pick("#2563EB"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#2563eb"}) {
		t.Fatalf("pick on CalDAV: %d %s", rec.Code, rec.Body.String())
	}
	if len(dav.changes) != 1 || !strings.Contains(dav.changes[0], "<a:calendar-color>#2563eb</a:calendar-color>") || dav.colors["/calendars/home/"] != "#2563EBFF" {
		t.Fatalf("the server saw %v and keeps %q", dav.changes, dav.colors["/calendars/home/"])
	}
	if source := home(); source.Color != "#2563eb" || source.ChosenColor != "" {
		t.Fatalf("taken by the server: %+v", source)
	}

	for _, answer := range []string{"property", "ignore", "403", "405"} {
		dav.answer = answer
		if rec, result := pick("#ef4444"); rec.Code != http.StatusOK || result != (calendarColorResult{Color: "#ef4444", InGoferOnly: true, Reason: calendarColorRefused}) {
			t.Fatalf("pick the server answers %q: %d %s", answer, rec.Code, rec.Body.String())
		}
		if source := home(); source.Color != "#2563eb" || source.ChosenColor != "#ef4444" {
			t.Fatalf("kept in Gofer after %q: %+v", answer, source)
		}
	}

	dav.answer = "500"
	if rec, _ := pick("#ffc000"); rec.Code != http.StatusBadGateway || home().ChosenColor != "#ef4444" {
		t.Fatalf("pick while the server fails: %d %s, %+v", rec.Code, rec.Body.String(), home())
	}
	dav.answer = "404"
	if rec, _ := pick("#ffc000"); rec.Code != http.StatusNotFound {
		t.Fatalf("pick on a calendar gone from the server: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCalendarTextColorReadsOnTheColor(t *testing.T) {
	for color, want := range map[string]string{"#2563eb": "#ffffff", "#111827": "#ffffff", "#facc15": "#000000", "#9fe1e7": "#000000", "#zzzzzz": "#000000"} {
		if got := calendarTextColor(color); got != want {
			t.Errorf("calendarTextColor(%q) = %q, want %q", color, got, want)
		}
	}
}
