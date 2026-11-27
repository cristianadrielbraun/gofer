package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestCalendarTextColorReadsOnTheColor(t *testing.T) {
	for color, want := range map[string]string{"#2563eb": "#ffffff", "#111827": "#ffffff", "#facc15": "#000000", "#9fe1e7": "#000000", "#zzzzzz": "#000000"} {
		if got := calendarTextColor(color); got != want {
			t.Errorf("calendarTextColor(%q) = %q, want %q", color, got, want)
		}
	}
}
