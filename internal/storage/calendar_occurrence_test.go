package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarOccurrenceStorageIdentityAndResourceVersions(t *testing.T) {
	for _, provider := range []string{"caldav", "gmail"} {
		for _, deleting := range []bool{false, true} {
			db, err := New(filepath.Join(t.TempDir(), "occurrence.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			_, err = db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'); INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','imap','owner@example.com')`)
			if err != nil {
				t.Fatal(err)
			}
			if provider == "gmail" {
				if _, err := db.Write().Exec(`UPDATE accounts SET provider='gmail' WHERE id='account'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", provider, []CalendarSource{{ID: "source", RemoteID: "remote-source", IsSelected: true}}); err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			event := CalendarEvent{ID: "event", RemoteID: "instance", SeriesRemoteID: "series", ICalUID: "uid", ETag: `"v1"`, StartAt: &start, EndAt: &end, Summary: "Before"}
			sibling, unrelated, older := event, event, event
			sibling.ID, sibling.RemoteID, sibling.Summary = "sibling", "sibling", "Unchanged"
			unrelated.ID, unrelated.RemoteID, unrelated.SeriesRemoteID = "other", "other", "other-series"
			older.ID, older.RemoteID, older.ETag = "old", "old", `"older"`
			if err := db.ReplaceCalendarEvents(t.Context(), "owner", "source", []CalendarEvent{event, sibling, unrelated, older}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			event.ETag, event.Summary = `"v2"`, "Changed"
			mutate := db.CompleteCalendarOccurrenceUpdate
			if deleting {
				mutate = db.CompleteCalendarOccurrenceDelete
			}
			for _, parent := range []string{"other-series", ""} {
				wrong := event
				wrong.SeriesRemoteID = parent
				if err := mutate(t.Context(), "owner", "event", "source", `"v1"`, wrong); err == nil {
					t.Fatal("accepted wrong parent")
				}
			}
			if err := mutate(t.Context(), "foreign", "event", "source", `"v1"`, event); !errors.Is(err, ErrCalendarUpdateConflict) {
				t.Fatalf("foreign write: %v", err)
			}
			if err := mutate(t.Context(), "owner", "event", "source", `"old"`, event); !errors.Is(err, ErrCalendarUpdateConflict) {
				t.Fatalf("stale write: %v", err)
			}
			if err := mutate(t.Context(), "owner", "event", "source", `"v1"`, event); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"sibling", "other", "old"} {
				got, err := db.GetCalendarEvent(t.Context(), "owner", id)
				if err != nil {
					t.Fatal("occurrence mutation removed another event")
				}
				want := `"v1"`
				if id == "sibling" && provider == "caldav" {
					want = `"v2"`
				}
				if id == "old" {
					want = `"older"`
				}
				if got.ETag != want || id == "sibling" && got.Summary != "Unchanged" {
					t.Fatalf("incorrect sibling update: %+v", got)
				}
			}
		}
	}
}
