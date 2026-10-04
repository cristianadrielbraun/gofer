package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const calendarIncomingSchema = `
CREATE TABLE IF NOT EXISTS calendar_incoming_messages (
 message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
 state TEXT NOT NULL CHECK(state IN ('complete','ignored','retry')),
 next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 attempts INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS calendar_incoming_responses (
 user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 source_id TEXT NOT NULL REFERENCES calendar_sources(id) ON DELETE CASCADE,
 ical_uid TEXT NOT NULL, attendee TEXT NOT NULL,
 sequence INTEGER NOT NULL, stamp TEXT NOT NULL,
 response TEXT NOT NULL CHECK(response IN ('ACCEPTED','TENTATIVE','DECLINED')),
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 PRIMARY KEY(source_id,ical_uid,attendee)
);`

func migrateV101ToV102(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(calendarIncomingSchema); err != nil {
		return err
	}
	// Very old migration fixtures may not have a mail table yet.
	if exists, err := tableExistsTx(tx, "messages"); err != nil {
		return err
	} else if exists {
		accountColumn, err := columnExistsTx(tx, "messages", "account_id")
		if err != nil {
			return err
		}
		senderColumn, err := columnExistsTx(tx, "messages", "from_email")
		if err != nil {
			return err
		}
		if accountColumn && senderColumn {
			if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_calendar_sender ON messages(account_id,lower(from_email),id)`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 102); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarIncomingMessage struct {
	ID                      int64
	UserID, AccountID, From string
}

// Only inspect mail from guests on existing meetings in this same mailbox.
// No subject heuristics, no bulk download of unrelated mail, no Sent/Trash.
func (db *DB) ListCalendarIncomingMessages(ctx context.Context, limit int) ([]CalendarIncomingMessage, error) {
	if limit < 1 || limit > 50 {
		return nil, fmt.Errorf("invalid incoming reply batch size")
	}
	rows, err := db.Read().QueryContext(ctx, `WITH invited AS (
 SELECT DISTINCT a.id AS account_id,a.user_id,lower(json_extract(guest.value,'$.email')) AS email
 FROM calendar_events e JOIN calendar_sources s ON s.id=e.source_id
 JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id AND e.user_id=a.user_id
 JOIN json_each(CASE WHEN json_valid(e.attendees_json) THEN e.attendees_json ELSE '[]' END) guest
 WHERE a.is_deleting=0 AND a.email_sync_enabled=1
 AND s.provider='caldav' AND s.is_selected=1 AND s.is_deleted=0 AND e.is_deleted=0
 AND e.series_remote_id='' AND lower(e.organizer_email)=lower(a.email_address)
 ) SELECT m.id,invited.user_id,invited.account_id,lower(m.from_email)
 FROM invited JOIN messages m ON m.account_id=invited.account_id AND lower(m.from_email)=invited.email
 LEFT JOIN calendar_incoming_messages seen ON seen.message_id=m.id
 WHERE (seen.message_id IS NULL OR (seen.state='retry' AND seen.next_attempt_at<=CURRENT_TIMESTAMP))
 AND EXISTS (SELECT 1 FROM message_folder_state ms JOIN folders f ON f.id=ms.folder_id AND f.account_id=invited.account_id
   WHERE ms.message_id=m.id AND ms.is_deleted=0 AND ms.is_draft=0 AND f.role NOT IN ('sent','trash','junk','spam','drafts'))
 ORDER BY CASE WHEN seen.message_id IS NULL THEN 0 ELSE 1 END,m.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []CalendarIncomingMessage
	for rows.Next() {
		var m CalendarIncomingMessage
		if err := rows.Scan(&m.ID, &m.UserID, &m.AccountID, &m.From); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (db *DB) FinishCalendarIncomingMessage(ctx context.Context, m CalendarIncomingMessage, state, note string) error {
	return db.FinishCalendarIncomingDelivery(ctx, m, state, note, "")
}

func (db *DB) FinishCalendarIncomingDelivery(ctx context.Context, m CalendarIncomingMessage, state, note, reason string) error {
	if state != "complete" && state != "ignored" && state != "retry" {
		return fmt.Errorf("invalid incoming reply state")
	}
	_, err := db.Write().ExecContext(ctx, `INSERT INTO calendar_incoming_messages(message_id,state,next_attempt_at,attempts,last_error,reason_code)
 SELECT m.id,?,CASE WHEN ?='retry' THEN datetime('now','+2 minutes') ELSE CURRENT_TIMESTAMP END,1,?,? FROM messages m JOIN accounts a ON a.id=m.account_id
 WHERE m.id=? AND a.id=? AND a.user_id=? AND a.is_deleting=0
 ON CONFLICT(message_id) DO UPDATE SET state=excluded.state, next_attempt_at=excluded.next_attempt_at,
 attempts=calendar_incoming_messages.attempts+1,last_error=excluded.last_error,reason_code=excluded.reason_code,updated_at=CURRENT_TIMESTAMP`, state, state, note, reason, m.ID, m.AccountID, m.UserID)
	return err
}

func (db *DB) FindCalendarIncomingEvent(ctx context.Context, userID, accountID, uid string) (CalendarEvent, error) {
	rows, err := db.Read().QueryContext(ctx, calendarEventSelect+` AND account.id=? AND source.provider='caldav' AND event.ical_uid=? AND event.series_remote_id='' LIMIT 2`, userID, accountID, uid)
	if err != nil {
		return CalendarEvent{}, err
	}
	defer rows.Close()
	var matches []CalendarEvent
	for rows.Next() {
		e, err := scanCalendarEvent(rows)
		if err != nil {
			return CalendarEvent{}, err
		}
		matches = append(matches, e)
	}
	if err := rows.Err(); err != nil {
		return CalendarEvent{}, err
	}
	if len(matches) != 1 {
		return CalendarEvent{}, sql.ErrNoRows
	}
	return matches[0], nil
}

// Reserve the newest authenticated reply before PUT. An ambiguous write may
// retry the identical response, but older mail cannot undo newer intent even
// after restart. The source gate serializes the resource's network writes.
func (db *DB) ReserveCalendarIncomingResponse(ctx context.Context, e CalendarEvent, attendee string, sequence int, stamp time.Time, response string) (bool, error) {
	if e.ICalUID == "" || attendee == "" || sequence < 0 || stamp.IsZero() || (response != "ACCEPTED" && response != "TENTATIVE" && response != "DECLINED") {
		return false, fmt.Errorf("invalid calendar reply reservation")
	}
	value := stamp.UTC().Format(time.RFC3339)
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO calendar_incoming_responses(user_id,source_id,ical_uid,attendee,sequence,stamp,response)
 SELECT s.user_id,s.id,?,?,?,?,? FROM calendar_sources s JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id
 WHERE s.id=? AND s.user_id=? AND s.provider='caldav' AND s.is_selected=1 AND s.is_deleted=0 AND a.is_deleting=0
 ON CONFLICT(source_id,ical_uid,attendee) DO UPDATE SET sequence=excluded.sequence,stamp=excluded.stamp,response=excluded.response,updated_at=CURRENT_TIMESTAMP
 WHERE calendar_incoming_responses.sequence<excluded.sequence OR (calendar_incoming_responses.sequence=excluded.sequence AND calendar_incoming_responses.stamp<excluded.stamp)`, e.ICalUID, attendee, sequence, value, response, e.SourceID, e.UserID)
	if err != nil {
		return false, err
	}
	var seq int
	var timestamp, status string
	err = tx.QueryRowContext(ctx, `SELECT sequence,stamp,response FROM calendar_incoming_responses WHERE user_id=? AND source_id=? AND ical_uid=? AND attendee=?`, e.UserID, e.SourceID, e.ICalUID, attendee).Scan(&seq, &timestamp, &status)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return seq == sequence && timestamp == value && status == response, nil
}
