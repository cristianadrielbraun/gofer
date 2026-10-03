package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarSeriesUpdateStorageOwnershipAndScope(t *testing.T) {
	testCalendarSeriesStorageOwnershipAndScope(t, false)
}

func TestCalendarSeriesDeleteStorageOwnershipAndScope(t *testing.T) {
	testCalendarSeriesStorageOwnershipAndScope(t, true)
}

func testCalendarSeriesStorageOwnershipAndScope(t *testing.T, deleted bool) {
	db, err := New(filepath.Join(t.TempDir(), "series.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	complete := db.CompleteCalendarSeriesUpdate
	if deleted {
		complete = db.CompleteCalendarSeriesDelete
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES ('owner','owner','owner'),('foreign','foreign','foreign');
		INSERT INTO accounts(id,user_id,provider,email_address) VALUES ('account','owner','gmail','owner@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "gmail", []CalendarSource{
		{ID: "source", RemoteID: "work", IsSelected: true}, {ID: "other-source", RemoteID: "home", IsSelected: true},
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, source := range []string{"source", "other-source"} {
		if err := db.ReplaceCalendarEvents(t.Context(), "owner", source, []CalendarEvent{
			{ID: source + "-instance", RemoteID: "instance", SeriesRemoteID: "master", ETag: `"v1"`, StartAt: &start, EndAt: &end},
			{ID: source + "-keep", RemoteID: "keep", SeriesRemoteID: "other-master", ETag: `"k1"`, StartAt: &start, EndAt: &end},
		}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	master := CalendarEvent{RemoteID: "master", ETag: `"master-v2"`, RecurrenceJSON: `["RRULE:FREQ=DAILY"]`}
	for _, tc := range []struct{ user, source, version string }{
		{"foreign", "source", `"v1"`}, {"owner", "other-source", `"v1"`}, {"owner", "source", `"stale"`},
	} {
		if err := complete(t.Context(), tc.user, "source-instance", tc.source, tc.version, master); !errors.Is(err, ErrCalendarUpdateConflict) {
			t.Fatalf("unscoped/stale update: %v", err)
		}
	}
	if err := db.SetCalendarSourceSelection(t.Context(), "owner", "account", []string{"other-source"}); err != nil {
		t.Fatal(err)
	}
	if err := complete(t.Context(), "owner", "source-instance", "source", `"v1"`, master); !errors.Is(err, ErrCalendarUpdateConflict) {
		t.Fatalf("deselected source: %v", err)
	}
	if err := db.SetCalendarSourceSelection(t.Context(), "owner", "account", []string{"source", "other-source"}); err != nil {
		t.Fatal(err)
	}
	if err := complete(t.Context(), "owner", "source-instance", "source", `"v1"`, master); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source-keep", "other-source-instance", "other-source-keep"} {
		if _, err := db.GetCalendarEvent(t.Context(), "owner", id); err != nil {
			t.Fatalf("unrelated event invalidated: %s %v", id, err)
		}
	}
	if err := complete(t.Context(), "owner", "source-instance", "source", `"v1"`, master); !errors.Is(err, ErrCalendarUpdateConflict) {
		t.Fatalf("replay accepted: %v", err)
	}
}
