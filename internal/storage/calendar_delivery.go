package storage

import (
	"context"
	"database/sql"
	"encoding/json"
)

func migrateV102ToV103(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, column := range []string{"event_id", "ical_uid", "attendee", "reason_code"} {
		exists, err := columnExistsTx(tx, "calendar_incoming_messages", column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec(`ALTER TABLE calendar_incoming_messages ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_calendar_incoming_event ON calendar_incoming_messages(event_id,attendee,message_id)`); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 103); err != nil {
		return err
	}
	return tx.Commit()
}

type CalendarNotificationDelivery struct {
	ID, Status, Method, Calendar string
	Recipients                   []string
}

// Only organizer notifications for this owned, selected event. Never expose
// ordinary outgoing mail, attendee replies, MIME bodies or account secrets.
func (db *DB) ListCalendarNotificationDeliveries(ctx context.Context, userID, eventID string) ([]CalendarNotificationDelivery, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT os.id,os.status,os.envelope_recipients,
 json_extract(os.message_json,'$.calendar_notification.Method'),json_extract(os.message_json,'$.calendar_notification.Calendar')
 FROM outgoing_sends os JOIN accounts a ON a.id=os.account_id
 JOIN calendar_sources s ON s.account_id=a.id AND s.user_id=a.user_id
 JOIN calendar_events e ON e.source_id=s.id AND e.user_id=a.user_id
 WHERE a.user_id=? AND e.id=? AND a.is_deleting=0 AND s.is_selected=1 AND s.is_deleted=0 AND e.is_deleted=0
 AND s.provider='caldav' AND lower(e.organizer_email)=lower(a.email_address)
 AND lower(os.envelope_from)=lower(a.email_address)
 AND json_extract(CASE WHEN json_valid(os.message_json) THEN os.message_json ELSE '{}' END,'$.calendar_notification.UserID')=a.user_id
 AND json_extract(CASE WHEN json_valid(os.message_json) THEN os.message_json ELSE '{}' END,'$.calendar_notification.SourceID')=s.id
 AND json_extract(CASE WHEN json_valid(os.message_json) THEN os.message_json ELSE '{}' END,'$.calendar_notification.ResourceID')=e.remote_id
 ORDER BY os.created_at DESC,os.rowid DESC LIMIT 30`, userID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CalendarNotificationDelivery
	for rows.Next() {
		var d CalendarNotificationDelivery
		var recipients string
		if err := rows.Scan(&d.ID, &d.Status, &recipients, &d.Method, &d.Calendar); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(recipients), &d.Recipients); err != nil {
			continue
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

type CalendarIncomingDelivery struct{ Attendee, State, Reason string }

func (db *DB) ListCalendarIncomingDeliveries(ctx context.Context, userID, eventID string) ([]CalendarIncomingDelivery, error) {
	rows, err := db.Read().QueryContext(ctx, `SELECT receipt.attendee,receipt.state,receipt.reason_code
 FROM calendar_incoming_messages receipt JOIN messages m ON m.id=receipt.message_id
 JOIN accounts a ON a.id=m.account_id JOIN calendar_events e ON e.id=receipt.event_id AND e.user_id=a.user_id
 JOIN calendar_sources s ON s.id=e.source_id AND s.account_id=a.id AND s.user_id=a.user_id
 WHERE a.user_id=? AND e.id=? AND receipt.ical_uid=e.ical_uid AND a.is_deleting=0 AND e.is_deleted=0
 AND s.is_selected=1 AND s.is_deleted=0 AND s.provider='caldav' AND lower(e.organizer_email)=lower(a.email_address)
 AND receipt.message_id=(SELECT max(newer.message_id) FROM calendar_incoming_messages newer
 WHERE newer.event_id=receipt.event_id AND newer.ical_uid=receipt.ical_uid AND newer.attendee=receipt.attendee)
 ORDER BY receipt.message_id DESC LIMIT 30`, userID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CalendarIncomingDelivery
	for rows.Next() {
		var d CalendarIncomingDelivery
		if err := rows.Scan(&d.Attendee, &d.State, &d.Reason); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// Linking unverified mail only creates a diagnostic. It never updates RSVP.
func (db *DB) BeginCalendarIncomingDelivery(ctx context.Context, c CalendarIncomingMessage, e CalendarEvent, attendee string) error {
	result, err := db.Write().ExecContext(ctx, `INSERT INTO calendar_incoming_messages(message_id,state,event_id,ical_uid,attendee,reason_code,next_attempt_at)
 SELECT m.id,'retry',e.id,e.ical_uid,?,'processing',datetime('now','+2 minutes')
 FROM messages m JOIN accounts a ON a.id=m.account_id JOIN calendar_events e ON e.user_id=a.user_id
 JOIN calendar_sources s ON s.id=e.source_id AND s.account_id=a.id AND s.user_id=a.user_id
 WHERE m.id=? AND a.id=? AND a.user_id=? AND e.id=? AND e.ical_uid=? AND lower(m.from_email)=?
 AND a.is_deleting=0 AND e.is_deleted=0 AND s.is_selected=1 AND s.is_deleted=0
 ON CONFLICT(message_id) DO UPDATE SET state='retry',event_id=excluded.event_id,ical_uid=excluded.ical_uid,
 attendee=excluded.attendee,reason_code='processing',next_attempt_at=excluded.next_attempt_at,updated_at=CURRENT_TIMESTAMP`, attendee, c.ID, c.AccountID, c.UserID, e.ID, e.ICalUID, attendee)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
