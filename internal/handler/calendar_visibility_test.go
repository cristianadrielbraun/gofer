package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestHandleCalendarVisibilityValidatesAndScopesChanges(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized) VALUES ('visible', 'visible', 'visible'), ('foreign', 'foreign', 'foreign');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('account', 'visible', 'gmail', 'visible@example.com');
		INSERT INTO calendar_sources (id, user_id, account_id, provider, remote_id, name) VALUES ('work', 'visible', 'account', 'gmail', 'primary', 'Work');`); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db} // No provider clients or credentials required.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/calendar/sources/{id}/visibility", h.handleCalendarVisibility)
	for _, test := range []struct {
		user, source, body string
		status             int
	}{
		{"visible", "work", `{"visible":false}`, 200},
		{"visible", "work", `{"visible":true}`, 200},
		{"foreign", "work", `{"visible":false}`, 404},
		{"visible", "missing", `{"visible":false}`, 404},
		{"visible", "work", `{}`, 400},
		{"visible", "work", `{"visible":null}`, 400},
		{"visible", "work", `{"visible":"false"}`, 400},
		{"visible", "work", `{"visible":false,"is_selected":false}`, 400},
		{"visible", "work", `{"visible":false} {}`, 400},
		{"visible", "work", strings.Repeat(" ", 1025) + `{"visible":false}`, 400},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/calendar/sources/"+test.source+"/visibility", strings.NewReader(test.body))
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: test.user}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("%s %s %q: status = %d, want %d", test.user, test.source, test.body, w.Code, test.status)
		}
	}
	sources, err := db.ListSelectedCalendarSources(t.Context(), "visible")
	if err != nil || len(sources) != 1 || sources[0].IsHidden {
		t.Fatal("rejected request changed visibility or sync selection")
	}
	if mapped := calendarViewEvent(storage.CalendarEvent{SourceID: "work", SourceHidden: true}); mapped.SourceID != "work" || !mapped.SourceHidden {
		t.Fatal("source visibility missing from event presentation")
	}
}
