package storage

import (
	"context"
	"database/sql"
)

const calendarTeamsDraftSchema = `CREATE TABLE IF NOT EXISTS calendar_teams_drafts (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
 draft_id TEXT NOT NULL,
 remote_id TEXT NOT NULL DEFAULT '',
 meeting_json TEXT NOT NULL DEFAULT '',
 used_by TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','saving','saved','abandoned','cleaned')),
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 PRIMARY KEY(user_id,source_id,draft_id)
);`

func migrateV104ToV105(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(calendarTeamsDraftSchema); err != nil {
		return err
	}
	if err = markSchemaVersion(tx, 105); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarTeamsDraft struct{ UserID, SourceID, DraftID, RemoteID, MeetingJSON, UsedBy, State string }

func scanCalendarTeamsDraft(row interface{ Scan(...any) error }) (CalendarTeamsDraft, error) {
	var d CalendarTeamsDraft
	err := row.Scan(&d.UserID, &d.SourceID, &d.DraftID, &d.RemoteID, &d.MeetingJSON, &d.UsedBy, &d.State)
	return d, err
}
func (db *DB) BeginCalendarTeamsDraft(ctx context.Context, user, source, id string) (CalendarTeamsDraft, error) {
	_, err := db.Write().ExecContext(ctx, `INSERT INTO calendar_teams_drafts(user_id,source_id,draft_id)
 SELECT s.user_id,s.id,? FROM calendar_sources s JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id
 WHERE s.user_id=? AND s.id=? AND s.provider='outlook' AND s.is_selected=1 AND s.is_deleted=0 AND a.is_deleting=0
 ON CONFLICT(user_id,source_id,draft_id) DO NOTHING`, id, user, source)
	if err != nil {
		return CalendarTeamsDraft{}, err
	}
	return db.GetCalendarTeamsDraft(ctx, user, source, id)
}
func (db *DB) GetCalendarTeamsDraft(ctx context.Context, user, source, id string) (CalendarTeamsDraft, error) {
	return scanCalendarTeamsDraft(db.Read().QueryRowContext(ctx, `SELECT d.user_id,d.source_id,d.draft_id,d.remote_id,d.meeting_json,d.used_by,d.state FROM calendar_teams_drafts d
 JOIN calendar_sources s ON s.id=d.source_id AND s.user_id=d.user_id JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id
 WHERE d.user_id=? AND d.source_id=? AND d.draft_id=? AND s.is_selected=1 AND s.is_deleted=0 AND a.is_deleting=0`, user, source, id))
}
func teamsDraftChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrCalendarCreateConflict
	}
	return nil
}
func (db *DB) SetCalendarTeamsDraftRemote(ctx context.Context, d CalendarTeamsDraft, id string) error {
	return teamsDraftChanged(db.Write().ExecContext(ctx, `UPDATE calendar_teams_drafts SET remote_id=?,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND source_id=? AND draft_id=? AND state='active' AND (remote_id='' OR remote_id=?)`, id, d.UserID, d.SourceID, d.DraftID, id))
}
func (db *DB) CompleteCalendarTeamsDraft(ctx context.Context, d CalendarTeamsDraft, meeting string) error {
	return teamsDraftChanged(db.Write().ExecContext(ctx, `UPDATE calendar_teams_drafts SET meeting_json=?,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND source_id=? AND draft_id=? AND remote_id=? AND state='active' AND (meeting_json='' OR meeting_json=?)`, meeting, d.UserID, d.SourceID, d.DraftID, d.RemoteID, meeting))
}

// Saving reservations survive disconnects. Expired reservations are inspected
// at the provider before cleanup can decide whether they were ever finalized.
func (db *DB) BindCalendarTeamsDraft(ctx context.Context, d CalendarTeamsDraft, target string) error {
	if d.MeetingJSON == "" || d.RemoteID == "" || target == "" {
		return ErrCalendarCreateConflict
	}
	return teamsDraftChanged(db.Write().ExecContext(ctx, `UPDATE calendar_teams_drafts SET used_by=?,state='saving',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND source_id=? AND draft_id=? AND meeting_json=? AND ((state='active' AND used_by='' AND updated_at>=datetime('now','-1 hour')) OR (state IN ('saving','saved') AND used_by=?))`, target, d.UserID, d.SourceID, d.DraftID, d.MeetingJSON, target))
}

// Call only while holding the source gate, after a provider read confirms that
// the expired reservation is still the original private, guest-free draft.
func (db *DB) ExpireCalendarTeamsReservation(ctx context.Context, d CalendarTeamsDraft) error {
	return teamsDraftChanged(db.Write().ExecContext(ctx, `UPDATE calendar_teams_drafts SET state='abandoned' WHERE user_id=? AND source_id=? AND draft_id=? AND state='saving' AND used_by=? AND updated_at<datetime('now','-1 hour')`, d.UserID, d.SourceID, d.DraftID, d.UsedBy))
}
func (db *DB) AbandonCalendarTeamsDraft(ctx context.Context, d CalendarTeamsDraft) error {
	_, err := db.Write().ExecContext(ctx, `UPDATE calendar_teams_drafts SET state='abandoned',updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND source_id=? AND draft_id=? AND state='active' AND used_by=''`, d.UserID, d.SourceID, d.DraftID)
	return err
}
func (db *DB) FinishCalendarTeamsDraft(ctx context.Context, d CalendarTeamsDraft, state string) error {
	if state != "saved" && state != "cleaned" {
		return ErrCalendarCreateConflict
	}
	expected := "saving"
	if state == "cleaned" {
		expected = "abandoned"
	}
	return teamsDraftChanged(db.Write().ExecContext(ctx, `UPDATE calendar_teams_drafts SET state=?,updated_at=CURRENT_TIMESTAMP WHERE user_id=? AND source_id=? AND draft_id=? AND state=?`, state, d.UserID, d.SourceID, d.DraftID, expected))
}
func (db *DB) ListCalendarTeamsDraftCleanup(ctx context.Context) ([]CalendarTeamsDraft, error) {
	if _, err := db.Write().ExecContext(ctx, `UPDATE calendar_teams_drafts SET state='abandoned' WHERE state='active' AND used_by='' AND updated_at<datetime('now','-1 hour')`); err != nil {
		return nil, err
	}
	rows, err := db.Read().QueryContext(ctx, `SELECT user_id,source_id,draft_id,remote_id,meeting_json,used_by,state FROM calendar_teams_drafts WHERE state='abandoned' OR (state='saving' AND updated_at<datetime('now','-1 hour')) ORDER BY updated_at LIMIT 8`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CalendarTeamsDraft
	for rows.Next() {
		d, err := scanCalendarTeamsDraft(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func (db *DB) CalendarTeamsDraftSource(ctx context.Context, d CalendarTeamsDraft) (CalendarSource, error) {
	var s CalendarSource
	err := db.Read().QueryRowContext(ctx, `SELECT s.id,s.user_id,s.account_id,s.provider,s.remote_id FROM calendar_sources s JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id WHERE s.id=? AND s.user_id=? AND s.provider='outlook' AND s.is_deleted=0 AND a.is_deleting=0`, d.SourceID, d.UserID).Scan(&s.ID, &s.UserID, &s.AccountID, &s.Provider, &s.RemoteID)
	return s, err
}
func (db *DB) PruneCalendarTeamsDrafts(ctx context.Context) error {
	_, err := db.Write().ExecContext(ctx, `DELETE FROM calendar_teams_drafts WHERE state IN ('saved','cleaned') AND updated_at<datetime('now','-7 days')`)
	return err
}
