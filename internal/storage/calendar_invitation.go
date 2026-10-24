package storage

import (
	"context"
	"database/sql"
	"fmt"
)

// Invitations Gofer adds to a CalDAV calendar by itself: each email it read
// for them, so it is read once, in the order the emails came.
const calendarInvitationSchema = `
CREATE TABLE IF NOT EXISTS calendar_invitation_messages (
 message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
 state TEXT NOT NULL CHECK(state IN ('complete','ignored','retry')),
 next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
 attempts INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

// CalendarInvitationWindow is how old an email can be for Gofer to add its
// invitation by itself; older ones wait for the person to add them.
const CalendarInvitationWindow = "-7 days"

// calendarInvitationSeenSQL marks the recent emails as read: those that came
// before Gofer added invitations by itself are left for the person.
const calendarInvitationSeenSQL = `INSERT OR IGNORE INTO calendar_invitation_messages(message_id,state,last_error)
 SELECT id,'ignored','received before Gofer added invitations by itself' FROM messages
 WHERE substr(date_received,1,19)>=strftime('%Y-%m-%d %H:%M:%S','now','` + CalendarInvitationWindow + `')`

// migrateV112ToV113 adds the emails Gofer read for invitations.
func migrateV112ToV113(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 113 {
		return nil
	}
	if _, err := tx.Exec(calendarInvitationSchema); err != nil {
		return err
	}
	if exists, err := columnExistsTx(tx, "messages", "date_received"); err != nil {
		return err
	} else if exists {
		if _, err := tx.Exec(calendarInvitationSeenSQL); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 113); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarInvitationMessage struct {
	ID                int64
	UserID, AccountID string
	// Retry is set when an earlier attempt failed; Due when it may run again.
	Retry, Due bool
	Attempts   int
}

// ListCalendarInvitationMessages returns recent emails, oldest first, that
// may have an invitation for a CalDAV calendar of their account: received in
// the inbox or another ordinary folder, from someone else, and not read for
// one yet. An email to retry stays in its place, so a later cancellation
// can't be read before the invitation it cancels. An empty user or account
// means all.
func (db *DB) ListCalendarInvitationMessages(ctx context.Context, userID, accountID string, maxSize int64, limit int) ([]CalendarInvitationMessage, error) {
	if limit < 1 || limit > 50 {
		return nil, fmt.Errorf("invalid invitation batch size")
	}
	rows, err := db.Read().QueryContext(ctx, `SELECT m.id,a.user_id,a.id,seen.message_id IS NOT NULL,COALESCE(seen.next_attempt_at<=CURRENT_TIMESTAMP,1),COALESCE(seen.attempts,0)
 FROM messages m JOIN accounts a ON a.id=m.account_id
 LEFT JOIN calendar_invitation_messages seen ON seen.message_id=m.id
 WHERE (?='' OR a.user_id=?) AND (?='' OR a.id=?)
 AND a.is_deleting=0 AND a.email_sync_enabled=1
 AND (seen.message_id IS NULL OR seen.state='retry')
 AND substr(m.date_received,1,19)>=strftime('%Y-%m-%d %H:%M:%S','now','`+CalendarInvitationWindow+`')
 AND m.size_bytes<=? AND lower(m.from_email)<>lower(a.email_address)
 AND EXISTS (SELECT 1 FROM calendar_sources s WHERE s.account_id=a.id AND s.user_id=a.user_id AND s.provider IN ('caldav','jmap') AND s.is_selected=1 AND s.is_deleted=0)
 AND EXISTS (SELECT 1 FROM message_folder_state ms JOIN folders f ON f.id=ms.folder_id AND f.account_id=a.id
   WHERE ms.message_id=m.id AND ms.is_deleted=0 AND ms.is_draft=0 AND f.role NOT IN ('sent','trash','junk','spam','drafts'))
 ORDER BY substr(m.date_received,1,19),m.id LIMIT ?`, userID, userID, accountID, accountID, maxSize, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []CalendarInvitationMessage
	for rows.Next() {
		var m CalendarInvitationMessage
		if err := rows.Scan(&m.ID, &m.UserID, &m.AccountID, &m.Retry, &m.Due, &m.Attempts); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// FinishCalendarInvitationMessage records what reading an email for its
// invitation came to; a retry waits a while.
func (db *DB) FinishCalendarInvitationMessage(ctx context.Context, m CalendarInvitationMessage, state, note string) error {
	if state != "complete" && state != "ignored" && state != "retry" {
		return fmt.Errorf("invalid invitation state")
	}
	_, err := db.Write().ExecContext(ctx, `INSERT INTO calendar_invitation_messages(message_id,state,next_attempt_at,attempts,last_error)
 SELECT m.id,?,CASE WHEN ?='retry' THEN datetime('now','+2 minutes') ELSE CURRENT_TIMESTAMP END,1,? FROM messages m JOIN accounts a ON a.id=m.account_id
 WHERE m.id=? AND a.id=? AND a.user_id=? AND a.is_deleting=0
 ON CONFLICT(message_id) DO UPDATE SET state=excluded.state,next_attempt_at=excluded.next_attempt_at,
 attempts=calendar_invitation_messages.attempts+1,last_error=excluded.last_error,updated_at=CURRENT_TIMESTAMP`, state, state, note, m.ID, m.AccountID, m.UserID)
	return err
}
