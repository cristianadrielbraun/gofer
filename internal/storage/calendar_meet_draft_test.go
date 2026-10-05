package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestCalendarMeetDraftOwnershipRetryBindingAndCleanup(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "meet-draft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'),('other','other','other'); INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','gmail','owner@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "gmail", []CalendarSource{{ID: "source", RemoteID: "primary", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginCalendarMeetDraft(t.Context(), "other", "source", "draft", "wrong"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign draft: %v", err)
	}
	d, err := db.BeginCalendarMeetDraft(t.Context(), "owner", "source", "draft", "remote")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := db.BeginCalendarMeetDraft(t.Context(), "owner", "source", "draft", "different")
	if err != nil || retry.RemoteID != "remote" {
		t.Fatal("retry replaced resource identity")
	}
	if err := db.BindCalendarMeetDraft(t.Context(), d, "event:one"); err == nil {
		t.Fatal("pending conference consumed")
	}
	if pending, err := db.ListCalendarMeetDraftCleanup(t.Context()); err != nil || len(pending) != 0 {
		t.Fatal("active preparation cleaned too early")
	}
	if err := db.CompleteCalendarMeetDraft(t.Context(), d, `{"signature":"signed"}`); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteCalendarMeetDraft(t.Context(), d, `{"signature":"changed"}`); !errors.Is(err, ErrCalendarCreateConflict) {
		t.Fatal("ready link replaced")
	}
	d, err = db.GetCalendarMeetDraft(t.Context(), "owner", "source", "draft")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := db.BindCalendarMeetDraft(t.Context(), d, "event:one"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.BindCalendarMeetDraft(t.Context(), d, "event:two"); !errors.Is(err, ErrCalendarCreateConflict) {
		t.Fatal("conference reused across events")
	}
	if err := db.SetCalendarSourceSelection(t.Context(), "owner", "account", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCalendarMeetDraft(t.Context(), "owner", "source", "draft"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deselected conference usable")
	}
	if source, err := db.CalendarMeetDraftSource(t.Context(), d); err != nil || source.RemoteID != "primary" {
		t.Fatal("deselection prevented cleanup")
	}
	pending, err := db.ListCalendarMeetDraftCleanup(t.Context())
	if err != nil || len(pending) != 1 {
		t.Fatal("ready resource cleanup not durable")
	}
	if err := db.FinishCalendarMeetDraftCleanup(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE calendar_meet_drafts SET created_at=datetime('now','-8 days')`); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneCalendarMeetDrafts(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_meet_drafts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("cleaned abandoned drafts not pruned")
	}
}

func TestCalendarMeetDraftMigrationFrom103(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`DROP TABLE calendar_meet_drafts;DELETE FROM schema_version;INSERT INTO schema_version(version) VALUES(103)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.Read().QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != CurrentSchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if _, err := db.Read().Exec(`SELECT * FROM calendar_meet_drafts`); err != nil {
		t.Fatal(err)
	}
}
