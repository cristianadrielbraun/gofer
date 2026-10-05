package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func teamsDraftDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "teams.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner','owner','owner'),('other','other','other'); INSERT INTO accounts(id,user_id,provider,email_address) VALUES('account','owner','outlook','owner@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "owner", "account", "outlook", []CalendarSource{{ID: "source", RemoteID: "calendar", IsSelected: true}}); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestCalendarTeamsDraftOwnershipReservationExpiryAndCleanup(t *testing.T) {
	db := teamsDraftDB(t)
	ctx := t.Context()
	if _, err := db.BeginCalendarTeamsDraft(ctx, "other", "source", "draft"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign draft allocated")
	}
	d, err := db.BeginCalendarTeamsDraft(ctx, "owner", "source", "draft")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.BindCalendarTeamsDraft(ctx, d, "create:first"); err == nil {
		t.Fatal("unready draft bound")
	}
	if err := db.SetCalendarTeamsDraftRemote(ctx, d, "remote"); err != nil {
		t.Fatal(err)
	}
	d.RemoteID = "remote"
	if err := db.SetCalendarTeamsDraftRemote(ctx, d, "different"); err == nil {
		t.Fatal("draft identity replaced")
	}
	if err := db.CompleteCalendarTeamsDraft(ctx, d, `{"url":"https://teams.live.com/meet/123"}`); err != nil {
		t.Fatal(err)
	}
	d, _ = db.GetCalendarTeamsDraft(ctx, "owner", "source", "draft")
	if err := db.BindCalendarTeamsDraft(ctx, d, "create:first"); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCalendarTeamsDraft(ctx, d, "create:first"); err != nil {
		t.Fatal("safe retry blocked", err)
	}
	if err := db.BindCalendarTeamsDraft(ctx, d, "create:second"); err == nil {
		t.Fatal("draft reused across events")
	}
	if err := db.AbandonCalendarTeamsDraft(ctx, d); err != nil {
		t.Fatal(err)
	}
	stored, _ := db.GetCalendarTeamsDraft(ctx, "owner", "source", "draft")
	if stored.State != "saving" {
		t.Fatal("in-flight save abandoned")
	}
	pending, err := db.ListCalendarTeamsDraftCleanup(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal("in-flight save queued for deletion")
	}
	if err := db.FinishCalendarTeamsDraft(ctx, d, "saved"); err != nil {
		t.Fatal(err)
	}
	active, _ := db.BeginCalendarTeamsDraft(ctx, "owner", "source", "abandoned")
	if err := db.AbandonCalendarTeamsDraft(ctx, active); err != nil {
		t.Fatal(err)
	}
	again, _ := db.BeginCalendarTeamsDraft(ctx, "owner", "source", "abandoned")
	if again.State != "abandoned" {
		t.Fatal("late preparation bypassed discard tombstone")
	}
	expired, _ := db.BeginCalendarTeamsDraft(ctx, "owner", "source", "expired")
	if _, err := db.Write().Exec(`UPDATE calendar_teams_drafts SET updated_at=datetime('now','-2 hours') WHERE draft_id='expired'`); err != nil {
		t.Fatal(err)
	}
	if err := db.BindCalendarTeamsDraft(ctx, expired, "create:expired"); err == nil {
		t.Fatal("expired draft usable")
	}
	pending, err = db.ListCalendarTeamsDraftCleanup(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatalf("abandoned=%d err=%v", len(pending), err)
	}
	for _, d := range pending {
		if err := db.FinishCalendarTeamsDraft(ctx, d, "cleaned"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Write().Exec(`UPDATE calendar_sources SET is_selected=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCalendarTeamsDraft(ctx, "owner", "source", "draft"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deselected draft usable")
	}
	if source, err := db.CalendarTeamsDraftSource(ctx, d); err != nil || source.RemoteID != "calendar" {
		t.Fatal("deselection prevented cleanup")
	}
	if _, err := db.Write().Exec(`UPDATE calendar_teams_drafts SET updated_at=datetime('now','-8 days')`); err != nil {
		t.Fatal(err)
	}
	if err := db.PruneCalendarTeamsDrafts(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT count(*) FROM calendar_teams_drafts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("finished drafts not pruned")
	}
}
func TestCalendarTeamsDraftMigrationFrom104(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`DROP TABLE calendar_teams_drafts;DELETE FROM schema_version;INSERT INTO schema_version(version) VALUES(104)`); err != nil {
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
		t.Fatal("migration did not finish")
	}
	if _, err := db.Read().Exec(`SELECT * FROM calendar_teams_drafts`); err != nil {
		t.Fatal(err)
	}
}
