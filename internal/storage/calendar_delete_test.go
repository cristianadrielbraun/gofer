package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarDeleteStorageConditionalAndScoped(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES ('owner','owner','owner'),('foreign','foreign','foreign');
		INSERT INTO accounts(id,user_id,provider,email_address) VALUES ('account','owner','gmail','owner@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "gmail", []CalendarSource{{ID: "source", RemoteID: "remote-source", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	if err := db.ReplaceCalendarEvents(t.Context(), "owner", "source", []CalendarEvent{
		{ID: "event", RemoteID: "remote", ETag: `"v1"`, StartAt: &start, EndAt: &end},
		{ID: "keep", RemoteID: "keep", ETag: `"v1"`, StartAt: &start, EndAt: &end},
	}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ user, source, version string }{
		{"foreign", "source", `"v1"`}, {"owner", "foreign", `"v1"`}, {"owner", "source", `"old"`}, {"owner", "source", ""},
	} {
		if err := db.CompleteCalendarDelete(t.Context(), tc.user, "event", tc.source, tc.version); err == nil {
			t.Fatal("unowned or unversioned deletion accepted")
		}
	}
	for _, mutation := range []string{
		`UPDATE calendar_sources SET is_selected=0 WHERE id='source'`,
		`UPDATE calendar_sources SET is_deleted=1 WHERE id='source'`,
		`UPDATE accounts SET is_deleting=1 WHERE id='account'`,
		`UPDATE accounts SET user_id='foreign' WHERE id='account'`,
	} {
		if _, err := db.Write().Exec(mutation); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteCalendarDelete(t.Context(), "owner", "event", "source", `"v1"`); !errors.Is(err, ErrCalendarUpdateConflict) {
			t.Fatalf("invalid calendar allowed deletion: %v", err)
		}
		if _, err := db.Write().Exec(`UPDATE calendar_sources SET is_selected=1,is_deleted=0 WHERE id='source'; UPDATE accounts SET is_deleting=0,user_id='owner' WHERE id='account'`); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompleteCalendarDelete(t.Context(), "owner", "event", "source", `"v1"`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCalendarEvent(t.Context(), "owner", "event"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleted event still visible")
	}
	events, err := db.ListCalendarEvents(t.Context(), "owner", start.Add(-time.Hour), end.Add(time.Hour))
	if err != nil || len(events) != 1 || events[0].ID != "keep" {
		t.Fatalf("remaining events=%#v err=%v", events, err)
	}
	if err := db.CompleteCalendarDelete(t.Context(), "owner", "event", "source", `"v1"`); !errors.Is(err, ErrCalendarUpdateConflict) {
		t.Fatal("deleted event accepted twice")
	}
}
