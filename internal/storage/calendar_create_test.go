package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarCreateStorageIsDurableScopedAndDoesNotReconcileCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "create.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	if _, err := db.Write().Exec(`INSERT INTO users (id,username,username_normalized) VALUES ('owner','owner','owner'),('foreign','foreign','foreign');
		INSERT INTO accounts(id,user_id,provider,email_address) VALUES ('account','owner','gmail','owner@example.com');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "gmail", []CalendarSource{{ID: "source", RemoteID: "primary", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginCalendarCreate(t.Context(), "foreign", "source", "request", "hash"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign source request allowed")
	}
	request, err := db.BeginCalendarCreate(t.Context(), "owner", "source", "request", "hash")
	if err != nil || request.EventID != "" {
		t.Fatal("new request did not start pending")
	}
	start, end := time.Now(), time.Now().Add(time.Hour)
	result, err := db.CompleteCalendarCreate(t.Context(), "owner", "source", "request", "hash", CalendarEvent{RemoteID: "remote", Summary: "Created", StartAt: &start, EndAt: &end})
	if err != nil || result.EventID == "" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	var state string
	var last sql.NullString
	if err := db.Read().QueryRow(`SELECT state,last_success_at FROM calendar_sync_state WHERE source_id='source'`).Scan(&state, &last); err != nil || state != "pending" || last.Valid {
		t.Fatal("creation falsely marked full sync successful")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := db.BeginCalendarCreate(t.Context(), "owner", "source", "request", "hash")
	if err != nil || replayed != result {
		t.Fatalf("restart lost durable creation: %#v, %v", replayed, err)
	}
	if _, err := db.BeginCalendarCreate(t.Context(), "owner", "source", "request", "changed"); !errors.Is(err, ErrCalendarCreateConflict) {
		t.Fatal("request accepted different payload")
	}
	if _, err := db.CompleteCalendarCreate(t.Context(), "owner", "source", "wrong", "hash", CalendarEvent{RemoteID: "must-rollback", StartAt: &start, EndAt: &end}); !errors.Is(err, ErrCalendarCreateConflict) {
		t.Fatal("cache insert without matching request succeeded")
	}
	var count int
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&count)
	if count != 1 {
		t.Fatal("invalid request was not rolled back")
	}
}

func TestMigrateV97ToV98PreservesExistingCalendarData(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`DROP TABLE calendar_create_requests; DELETE FROM schema_version; INSERT INTO schema_version (version) VALUES (97);`); err != nil {
		t.Fatal(err)
	}
	if err := migrateV97ToV98(db.Write()); err != nil {
		t.Fatal(err)
	}
	if err := migrateV97ToV98(db.Write()); err != nil {
		t.Fatal(err)
	}
	var version int
	_ = db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version)
	if version != 98 {
		t.Fatal("creation migration did not mark version 98")
	}
}

func TestCalendarSeriesCreationIsDurableWithoutCachingMaster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id,username,username_normalized) VALUES ('owner','owner','owner'),('foreign','foreign','foreign');
		INSERT INTO accounts(id,user_id,provider,email_address) VALUES ('account','owner','gmail','owner@example.com');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "gmail", []CalendarSource{{ID: "source", RemoteID: "primary", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginCalendarCreate(t.Context(), "owner", "source", "request", "hash"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ user, source, request, hash, remote string }{
		{"foreign", "source", "request", "hash", "series"}, {"owner", "unknown", "request", "hash", "series"},
		{"owner", "source", "unknown", "hash", "series"}, {"owner", "source", "request", "changed", "series"}, {"owner", "source", "request", "hash", ""},
	} {
		if _, err := db.CompleteCalendarSeriesCreate(t.Context(), test.user, test.source, test.request, test.hash, test.remote); err == nil {
			t.Fatalf("invalid confirmation accepted: %+v", test)
		}
	}
	for range 2 {
		result, err := db.CompleteCalendarSeriesCreate(t.Context(), "owner", "source", "request", "hash", "series")
		if err != nil || result.RemoteID != "series" || result.EventID != "" {
			t.Fatalf("series result: %+v %v", result, err)
		}
	}
	if _, err := db.CompleteCalendarSeriesCreate(t.Context(), "owner", "source", "request", "hash", "other-series"); !errors.Is(err, ErrCalendarCreateConflict) {
		t.Fatal("overwrote a completed series identity")
	}
	var events int
	var synced sql.NullString
	_ = db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events`).Scan(&events)
	_ = db.Read().QueryRow(`SELECT last_success_at FROM calendar_sync_state WHERE source_id='source'`).Scan(&synced)
	if events != 0 || synced.Valid {
		t.Fatal("series confirmation must not cache the master or claim a full sync")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.BeginCalendarCreate(t.Context(), "owner", "source", "request", "hash")
	if err != nil || result.RemoteID != "series" || result.EventID != "" {
		t.Fatalf("restart lost confirmed series identity: %+v %v", result, err)
	}
}
