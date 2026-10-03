package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarUpdateStorageScopedConditionalAndInPlace(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "edit.db"))
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
	event := CalendarEvent{ID: "event", RemoteID: "remote", ETag: `"v1"`, ICalUID: "same-uid", Summary: "Original", StartAt: &start, EndAt: &end}
	if err := db.ReplaceCalendarEvents(t.Context(), "owner", "source", []CalendarEvent{event}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	event.ETag, event.Summary = `"v2"`, "Updated"
	for _, user := range []string{"foreign", "owner"} {
		version := `"v1"`
		if user == "owner" {
			version = `"stale"`
		}
		if err := db.CompleteCalendarUpdate(t.Context(), user, "event", "source", version, event); !errors.Is(err, ErrCalendarUpdateConflict) {
			t.Fatalf("unauthorized/stale write: %v", err)
		}
	}
	if err := db.CompleteCalendarUpdate(t.Context(), "owner", "event", "source", `"v1"`, event); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetCalendarEvent(t.Context(), "owner", "event")
	if err != nil || updated.Summary != "Updated" || updated.ICalUID != "same-uid" || updated.ETag != `"v2"` {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	event.AllDay, event.StartDate, event.EndDate = true, "2026-10-03", "2026-10-05"
	event.ETag = `"v3"`
	if err := db.CompleteCalendarUpdate(t.Context(), "owner", "event", "source", `"v2"`, event); err != nil {
		t.Fatal(err)
	}
	updated, err = db.GetCalendarEvent(t.Context(), "owner", "event")
	if err != nil || !updated.AllDay || updated.StartAt != nil || updated.EndAt != nil || updated.EndDate != "2026-10-05" {
		t.Fatalf("all-day=%#v err=%v", updated, err)
	}
	if err := db.SetCalendarSourceSelection(t.Context(), "owner", "account", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteCalendarUpdate(t.Context(), "owner", "event", "source", `"v3"`, event); !errors.Is(err, ErrCalendarUpdateConflict) {
		t.Fatalf("deselected write: %v", err)
	}
	event.ETag, event.RecurrenceJSON = `"v4"`, `["RRULE:FREQ=DAILY;COUNT=3"]`
	if err := db.CompleteCalendarSeriesConversion(t.Context(), "owner", "event", "source", `"v3"`, event); !errors.Is(err, ErrCalendarUpdateConflict) {
		t.Fatalf("deselected series conversion: %v", err)
	}
	if err := db.SetCalendarSourceSelection(t.Context(), "owner", "account", []string{"source"}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"foreign", "owner"} {
		version := `"v3"`
		if owner == "owner" {
			version = `"stale"`
		}
		if err := db.CompleteCalendarSeriesConversion(t.Context(), owner, "event", "source", version, event); !errors.Is(err, ErrCalendarUpdateConflict) {
			t.Fatalf("foreign/stale series conversion: %v", err)
		}
	}
	if err := db.CompleteCalendarUpdate(t.Context(), "owner", "event", "source", `"v3"`, event); err == nil {
		t.Fatal("single-event update accepted a recurring master")
	}
	if err := db.CompleteCalendarSeriesConversion(t.Context(), "owner", "event", "source", `"v3"`, event); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCalendarEvent(t.Context(), "owner", "event"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("converted master must not remain visible as a single event")
	}
	if err := db.CompleteCalendarSeriesConversion(t.Context(), "owner", "event", "source", `"v3"`, event); !errors.Is(err, ErrCalendarUpdateConflict) {
		t.Fatal("series conversion was replayed over its tombstone")
	}
}
