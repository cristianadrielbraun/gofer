package storage

import (
	"context"
	"database/sql"
	"fmt"
)

const calendarMeetDraftSchema = `CREATE TABLE IF NOT EXISTS calendar_meet_drafts (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
 draft_id TEXT NOT NULL,
 remote_id TEXT NOT NULL,
 conference_json TEXT NOT NULL DEFAULT '',
 used_by TEXT NOT NULL DEFAULT '',
 cleanup_pending INTEGER NOT NULL DEFAULT 1,
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 PRIMARY KEY(user_id,source_id,draft_id)
);`

func migrateV103ToV104(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(calendarMeetDraftSchema); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 104); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarMeetDraft struct{ UserID, SourceID, DraftID, RemoteID, ConferenceJSON, UsedBy string }

func (db *DB) BeginCalendarMeetDraft(ctx context.Context, userID, sourceID, draftID, remoteID string) (CalendarMeetDraft, error) {
	_, err := db.Write().ExecContext(ctx, `INSERT INTO calendar_meet_drafts(user_id,source_id,draft_id,remote_id)
 SELECT s.user_id,s.id,?,? FROM calendar_sources s JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id
 WHERE s.user_id=? AND s.id=? AND s.provider='gmail' AND s.is_selected=1 AND s.is_deleted=0 AND a.is_deleting=0
 ON CONFLICT(user_id,source_id,draft_id) DO NOTHING`, draftID, remoteID, userID, sourceID)
	if err != nil {
		return CalendarMeetDraft{}, err
	}
	return db.GetCalendarMeetDraft(ctx, userID, sourceID, draftID)
}

func (db *DB) GetCalendarMeetDraft(ctx context.Context, userID, sourceID, draftID string) (CalendarMeetDraft, error) {
	var d CalendarMeetDraft
	err := db.Read().QueryRowContext(ctx, `SELECT d.user_id,d.source_id,d.draft_id,d.remote_id,d.conference_json,d.used_by FROM calendar_meet_drafts d
 JOIN calendar_sources s ON s.id=d.source_id AND s.user_id=d.user_id JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id
 WHERE d.user_id=? AND d.source_id=? AND d.draft_id=? AND s.is_selected=1 AND s.is_deleted=0 AND a.is_deleting=0`, userID, sourceID, draftID).Scan(&d.UserID, &d.SourceID, &d.DraftID, &d.RemoteID, &d.ConferenceJSON, &d.UsedBy)
	return d, err
}

func (db *DB) CompleteCalendarMeetDraft(ctx context.Context, d CalendarMeetDraft, conference string) error {
	result, err := db.Write().ExecContext(ctx, `UPDATE calendar_meet_drafts SET conference_json=? WHERE user_id=? AND source_id=? AND draft_id=? AND remote_id=? AND (conference_json='' OR conference_json=?)`, conference, d.UserID, d.SourceID, d.DraftID, d.RemoteID, conference)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrCalendarCreateConflict
	}
	return nil
}

// A prepared conference belongs to only one final event, including safe retries.
func (db *DB) BindCalendarMeetDraft(ctx context.Context, d CalendarMeetDraft, target string) error {
	if d.ConferenceJSON == "" || target == "" {
		return fmt.Errorf("Meet link is not ready")
	}
	result, err := db.Write().ExecContext(ctx, `UPDATE calendar_meet_drafts SET used_by=? WHERE user_id=? AND source_id=? AND draft_id=? AND conference_json=? AND (used_by='' OR used_by=?)`, target, d.UserID, d.SourceID, d.DraftID, d.ConferenceJSON, target)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrCalendarCreateConflict
	}
	return nil
}

func (db *DB) FinishCalendarMeetDraftCleanup(ctx context.Context, d CalendarMeetDraft) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE calendar_meet_drafts SET cleanup_pending=0 WHERE user_id=? AND source_id=? AND draft_id=?`, d.UserID, d.SourceID, d.DraftID)
	return err
}

func (db *DB) ListCalendarMeetDraftCleanup(ctx context.Context) ([]CalendarMeetDraft, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT user_id,source_id,draft_id,remote_id,conference_json,used_by FROM calendar_meet_drafts WHERE cleanup_pending=1 AND (conference_json<>'' OR created_at<datetime('now','-1 hour')) ORDER BY created_at LIMIT 8`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CalendarMeetDraft
	for rows.Next() {
		var d CalendarMeetDraft
		if err := rows.Scan(&d.UserID, &d.SourceID, &d.DraftID, &d.RemoteID, &d.ConferenceJSON, &d.UsedBy); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (db *DB) PruneCalendarMeetDrafts(ctx context.Context) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM calendar_meet_drafts WHERE cleanup_pending=0 AND created_at<datetime('now','-7 days')`)
	return err
}

// Cleanup also works for a calendar the user deselected after preparation.
func (db *DB) CalendarMeetDraftSource(ctx context.Context, d CalendarMeetDraft) (CalendarSource, error) {
	var s CalendarSource
	err := db.Read().QueryRowContext(ctx, `SELECT s.id,s.user_id,s.account_id,s.provider,s.remote_id FROM calendar_sources s JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id WHERE s.id=? AND s.user_id=? AND s.provider='gmail' AND s.is_deleted=0 AND a.is_deleting=0`, d.SourceID, d.UserID).Scan(&s.ID, &s.UserID, &s.AccountID, &s.Provider, &s.RemoteID)
	return s, err
}
