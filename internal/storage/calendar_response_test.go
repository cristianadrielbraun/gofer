package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarResponseStorageAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "response.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES ('owner','owner','owner'),('foreign','foreign','foreign'); INSERT INTO accounts(id,user_id,provider,email_address) VALUES ('account','owner','gmail','owner@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "gmail", []CalendarSource{{ID: "source", RemoteID: "calendar", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	start, end := time.Now(), time.Now().Add(time.Hour)
	event := CalendarEvent{ID: "event", RemoteID: "remote", ICalUID: "uid", ETag: `"v1"`, Summary: "Original", StartAt: &start, EndAt: &end, SeriesRemoteID: "master", AttendeesJSON: `[{"email":"owner@example.com","responseStatus":"needsAction"}]`, ResponseStatus: "needsAction"}
	other := event
	other.ID, other.RemoteID = "other", "other-remote"
	if err := db.ReplaceCalendarEvents(t.Context(), "owner", "source", []CalendarEvent{event, other}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	event, err = db.GetCalendarEvent(t.Context(), "owner", "event")
	if err != nil || event.ResponseStatus != "needsAction" {
		t.Fatal("response did not round-trip through sync", err)
	}
	if _, err := db.Write().Exec(`DROP TABLE calendar_response_requests; ALTER TABLE calendar_events DROP COLUMN response_status; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(98)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV98ToV99(db.Write()); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	_ = db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version)
	if version != 99 {
		t.Fatal("migration did not update schema")
	}
	legacy, err := db.GetCalendarEvent(t.Context(), "owner", "event")
	if err != nil || legacy.Summary != "Original" || legacy.ResponseStatus != "" {
		t.Fatal("migration damaged existing event", err)
	}
	if err := db.BeginCalendarResponse(t.Context(), "foreign", "source", "remote", `"v1"`, "accepted"); err == nil {
		t.Fatal("foreign response was reserved")
	}
	if err := db.BeginCalendarResponse(t.Context(), "owner", "source", "remote", `"v1"`, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{"accepted", "declined"} {
		if err := db.BeginCalendarResponse(t.Context(), "owner", "source", "remote", `"v1"`, answer); !errors.Is(err, ErrCalendarResponsePending) {
			t.Fatal("restart lost duplicate protection", err)
		}
	}
	saved := event
	saved.ETag, saved.ResponseStatus, saved.AttendeesJSON = `"v2"`, "accepted", `[{"email":"owner@example.com","responseStatus":"accepted"}]`
	for _, mode := range []string{"foreign", "stale", "wrong-parent"} {
		before, after := event, saved
		if mode == "foreign" {
			before.UserID = "foreign"
		} else if mode == "stale" {
			before.ETag = `"old"`
		} else {
			after.SeriesRemoteID = "different"
		}
		if err := db.CompleteCalendarResponse(t.Context(), before, after); !errors.Is(err, ErrCalendarUpdateConflict) {
			t.Fatal("unsafe cache write", mode, err)
		}
	}
	if err := db.CompleteCalendarResponse(t.Context(), event, saved); err != nil {
		t.Fatal(err)
	}
	saved, err = db.GetCalendarEvent(t.Context(), "owner", "event")
	if err != nil || saved.ResponseStatus != "accepted" || saved.Summary != "Original" || saved.SeriesRemoteID != "master" || saved.ICalUID != "uid" {
		t.Fatal("response modified meeting details", err)
	}
	other, err = db.GetCalendarEvent(t.Context(), "owner", "other")
	if err != nil || other.ETag != `"v1"` || other.ResponseStatus != "" {
		t.Fatal("response changed another occurrence", err)
	}
	updated := saved
	updated.ETag, updated.Summary, updated.Description = `"v3"`, "Provider's changed title", "New provider details"
	if err := db.CompleteCalendarResponse(t.Context(), saved, updated); err != nil {
		t.Fatal(err)
	}
	updated, err = db.GetCalendarEvent(t.Context(), "owner", "event")
	if err != nil || updated.Summary != "Provider's changed title" || updated.Description != "New provider details" {
		t.Fatal("new version was attached to stale meeting details", err)
	}
	if err := db.ReleaseCalendarResponse(t.Context(), "foreign", "source", "remote", `"v1"`); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCalendarResponse(t.Context(), "owner", "source", "remote", `"v1"`, "accepted"); !errors.Is(err, ErrCalendarResponsePending) {
		t.Fatal("foreign user released reservation")
	}
	if err := db.ReleaseCalendarResponse(t.Context(), "owner", "source", "remote", `"v1"`); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCalendarResponse(t.Context(), "owner", "source", "remote", `"v1"`, "accepted"); err != nil {
		t.Fatal("definite failure cannot retry", err)
	}
	if err := db.SetCalendarSourceSelection(t.Context(), "owner", "account", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.BeginCalendarResponse(t.Context(), "owner", "source", "remote", `"v3"`, "accepted"); err == nil {
		t.Fatal("deselected source accepted response")
	}
}
