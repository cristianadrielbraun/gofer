package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func TestCalendarEventParticipants(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		want      []views.CalendarEventParticipant
	}{
		{"Google", `[{"displayName":"Alice","email":"alice@example.com","responseStatus":"accepted","optional":true}]`, []views.CalendarEventParticipant{{Name: "Alice", Email: "alice@example.com", ResponseStatus: "accepted", Optional: true}}},
		{"Graph", `[{"emailAddress":{"name":"Bob","address":"bob@example.com"},"status":{"response":"tentativelyAccepted"},"type":"optional"}]`, []views.CalendarEventParticipant{{Name: "Bob", Email: "bob@example.com", ResponseStatus: "tentativelyAccepted", Optional: true}}},
		{"CalDAV", `[{"name":"Carol","email":"carol@example.com","status":"DECLINED","role":"OPT-PARTICIPANT"}]`, []views.CalendarEventParticipant{{Name: "Carol", Email: "carol@example.com", ResponseStatus: "DECLINED", Optional: true}}},
		{"email only", `[{"email":"guest@example.com"},{}]`, []views.CalendarEventParticipant{{Email: "guest@example.com"}}},
		{"invalid JSON", `{bad`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := calendarEventParticipants(test.raw); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("guests = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestCalendarDescriptionText(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{" Plain\ntext ", "Plain\ntext"},
		{`Visit <https://example.com/path>`, `Visit <https://example.com/path>`},
		{`<p>Notes &amp; actions</p><script>secret()</script><style>.hidden{}</style><img src="https://tracker.example/pixel"><p>Next</p>`, "Notes & actions Next"},
		{`<div>` + strings.Repeat("Complete text ", 30) + `lastword</div>`, strings.Repeat("Complete text ", 30) + "lastword"},
	} {
		if got := calendarDescriptionText(test.raw); got != test.want {
			t.Errorf("description = %q, want %q", got, test.want)
		}
	}
}

func TestHandleCalendarEventReadsOnlyVisibleOwnedCache(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized) VALUES ('detail-user', 'detail-user', 'detail-user'), ('foreign-user', 'foreign-user', 'foreign-user');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('detail-account', 'detail-user', 'gmail', 'detail@example.com');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "detail-user", "detail-account", "gmail", []storage.CalendarSource{
		{ID: "detail-source", RemoteID: "primary", Name: "Work", IsSelected: true},
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.October, 3, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	if err := db.ReplaceCalendarEvents(t.Context(), "detail-user", "detail-source", []storage.CalendarEvent{
		{ID: "detail-event", RemoteID: "remote-event", Summary: "Planning", Description: `<p>Full description</p><script>secret()</script>`, OrganizerName: "Organizer", OrganizerEmail: "organizer@example.com", AttendeesJSON: `[{"email":"guest@example.com","responseStatus":"accepted"}]`, StartAt: &start, EndAt: &end},
	}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db} // No credentials or provider clients: details must be cache-only.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/calendar/events/{id}", h.handleCalendarEvent)
	request := func(userID string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/calendar/events/detail-event", nil)
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: userID}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := request("detail-user")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("details status = %d, headers = %v", w.Code, w.Header())
	}
	for _, expected := range []string{"Planning", "Full description", "Work", "organizer@example.com", "guest@example.com", "Accepted"} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Errorf("details missing %q", expected)
		}
	}
	if strings.Contains(w.Body.String(), "secret()") {
		t.Fatal("provider script included in details")
	}
	if w := request("foreign-user"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "Planning") {
		t.Fatal("foreign user could retrieve cached event details")
	}
	if err := db.SetCalendarSourceSelection(t.Context(), "detail-user", "detail-account", nil); err != nil {
		t.Fatal(err)
	}
	if w := request("detail-user"); w.Code != http.StatusNotFound {
		t.Fatal("deselected calendar details remain visible")
	}
}
