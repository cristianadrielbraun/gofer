package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarSourceSyncRequest(owner, sourceID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/calendar/sources/"+sourceID+"/sync", nil)
	r.SetPathValue("id", sourceID)
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: owner}))
}

func calendarSourceIDs(t *testing.T, sources []storage.CalendarSource) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, source := range sources {
		ids[source.RemoteID] = source.ID
	}
	return ids
}

// The shared store adds a calendar the sidebar offers to those its account
// syncs; one already synced, or someone else's, isn't offered.
func TestCalendarSourceSyncForTheSharedStore(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name) VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO users (id, username, username_normalized, name) VALUES ('other', 'other', 'other', 'Other');
		INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('out-1', 'default', 'outlook', 'out-1@example.test');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "default", "out-1", "outlook", []storage.CalendarSource{
		{RemoteID: "main", Name: "Main", IsPrimary: true, IsSelected: true},
		{RemoteID: "club", Name: "Club"},
	}); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListCalendarSourcesForAccount(t.Context(), "default", "out-1")
	if err != nil {
		t.Fatal(err)
	}
	ids := calendarSourceIDs(t, sources)
	h := &Handler{db: db}

	for owner, id := range map[string]string{"default": ids["main"], "other": ids["club"]} {
		rec := httptest.NewRecorder()
		h.handleCalendarSourceSync(rec, calendarSourceSyncRequest(owner, id))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s syncing %s: %d %s", owner, id, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.handleCalendarSourceSync(rec, calendarSourceSyncRequest("default", ids["club"]))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	selected, err := db.ListSelectedCalendarSources(t.Context(), "default")
	if err != nil || len(selected) != 2 {
		t.Fatalf("selected %+v, %v", selected, err)
	}
	others, err := db.ListUnselectedCalendarSources(t.Context(), "default")
	if err != nil || len(others) != 0 {
		t.Fatalf("still offered %+v, %v", others, err)
	}
}

// A per-user account does the same through its own store.
func TestCalendarSourceSyncForThePerUserStores(t *testing.T) {
	f := newUserContactPushFixture(t, "outlook", http.NotFoundHandler())
	account := f.accounts["alice"]
	var ids map[string]string
	if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
		if err := db.ReplaceCalendarSources(t.Context(), "alice", account.ID, "outlook", []storage.CalendarSource{
			{RemoteID: "main", Name: "Main", IsPrimary: true, IsSelected: true},
			{RemoteID: "club", Name: "Club"},
		}); err != nil {
			return err
		}
		sources, err := db.ListCalendarSourcesForAccount(t.Context(), "alice", account.ID)
		ids = calendarSourceIDs(t, sources)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	f.h.handleCalendarSourceSync(rec, calendarSourceSyncRequest("bob", ids["club"]))
	if rec.Code == http.StatusNoContent {
		t.Fatal("bob synced alice's calendar")
	}
	rec = httptest.NewRecorder()
	f.h.handleCalendarSourceSync(rec, calendarSourceSyncRequest("alice", ids["club"]))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sync: %d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if err := f.h.userAccounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
		selected, err := db.ListSelectedCalendarSources(t.Context(), "alice")
		if err == nil && len(selected) != 2 {
			t.Fatalf("selected %+v", selected)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
