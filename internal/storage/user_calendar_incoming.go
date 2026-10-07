package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

var ErrCalendarIncomingChanged = errors.New("calendar incoming reply no longer matches current state")

// Scheduling candidates carry no native write or message-publication authority.
func (db *DB) ListAccountCalendarIncomingMessages(ctx context.Context, owner, account string, limit int, guard func(*sql.Tx, string) error) ([]CalendarIncomingMessage, error) {
	if limit < 1 || limit > 16 || guard == nil {
		return nil, ErrCalendarIncomingChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `WITH invited AS (
 SELECT DISTINCT a.id AS account_id,a.user_id,lower(CASE WHEN json_valid(guest.value) THEN json_extract(guest.value,'$.email') END) AS email
 FROM calendar_events e JOIN calendar_sources s ON s.id=e.source_id AND s.user_id=e.user_id
 JOIN accounts a ON a.id=s.account_id AND a.user_id=s.user_id
 JOIN json_each(CASE WHEN json_valid(e.attendees_json) THEN e.attendees_json ELSE '[]' END) guest
 WHERE a.id=? AND a.user_id=? AND a.is_deleting=0 AND a.email_sync_enabled=1
 AND s.provider='caldav' AND s.is_selected=1 AND s.is_deleted=0 AND e.is_deleted=0
 AND e.series_remote_id='' AND lower(e.organizer_email)=lower(a.email_address)
 ) SELECT m.id,invited.user_id,invited.account_id,lower(m.from_email)
 FROM invited JOIN messages m ON m.account_id=invited.account_id AND lower(m.from_email)=invited.email
 LEFT JOIN calendar_incoming_messages seen ON seen.message_id=m.id
 WHERE (seen.message_id IS NULL OR (seen.state='retry' AND seen.next_attempt_at<=CURRENT_TIMESTAMP))
 AND EXISTS (SELECT 1 FROM message_folder_state ms JOIN folders f ON f.id=ms.folder_id AND f.account_id=invited.account_id
 WHERE ms.message_id=m.id AND ms.is_deleted=0 AND ms.is_draft=0 AND f.role NOT IN ('sent','trash','junk','spam','drafts'))
 ORDER BY COALESCE(seen.next_attempt_at,m.created_at),m.id LIMIT ?`, account, owner, limit)
	if err != nil {
		return nil, err
	}
	var result []CalendarIncomingMessage
	for rows.Next() {
		var c CalendarIncomingMessage
		if err := rows.Scan(&c.ID, &c.UserID, &c.AccountID, &c.From); err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, c)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	if err := calendarControlOwnerTx(ctx, tx, owner, account, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

type calendarIncomingReceipt struct {
	Exists                                                   bool
	State, Next, Updated, Note, Reason, Event, UID, Attendee string
	Attempts                                                 int
}

func calendarIncomingReceiptTx(ctx context.Context, tx *sql.Tx, id int64) (calendarIncomingReceipt, error) {
	var r calendarIncomingReceipt
	err := tx.QueryRowContext(ctx, `SELECT state,CAST(next_attempt_at AS TEXT),CAST(updated_at AS TEXT),last_error,reason_code,event_id,ical_uid,attendee,attempts FROM calendar_incoming_messages WHERE message_id=?`, id).Scan(&r.State, &r.Next, &r.Updated, &r.Note, &r.Reason, &r.Event, &r.UID, &r.Attendee, &r.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	r.Exists = err == nil
	return r, err
}

// Only qualifying visible folder memberships belong to a candidate. Capture
// sender, immutable message creation identity and retrieval/membership identity;
// unrelated read flags and body cache fields do not supply reply authority.
func calendarIncomingMessageStateTx(ctx context.Context, tx *sql.Tx, c CalendarIncomingMessage) ([32]byte, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.account_id,lower(m.from_email),CAST(m.created_at AS TEXT),COALESCE(m.internet_message_id,''),ms.folder_id,ms.remote_uid,ms.is_deleted,ms.is_draft,f.account_id,f.role,f.remote_id,f.uid_validity
 FROM messages m JOIN accounts a ON a.id=m.account_id
 JOIN message_folder_state ms ON ms.message_id=m.id JOIN folders f ON f.id=ms.folder_id AND f.account_id=a.id
 WHERE m.id=? AND a.id=? AND a.user_id=? AND a.is_deleting=0 AND a.email_sync_enabled=1
 AND lower(m.from_email)=? AND ms.is_deleted=0 AND ms.is_draft=0 AND f.role NOT IN ('sent','trash','junk','spam','drafts') ORDER BY f.id`, c.ID, c.AccountID, c.UserID, c.From)
	if err != nil {
		return [32]byte{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return [32]byte{}, err
	}
	var records [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return [32]byte{}, err
		}
		records = append(records, values)
	}
	if err := rows.Err(); err != nil {
		return [32]byte{}, err
	}
	if len(records) == 0 {
		return [32]byte{}, ErrCalendarIncomingChanged
	}
	wire, err := json.Marshal(records)
	return sha256.Sum256(wire), err
}

type UserCalendarIncomingMessageSnapshot struct {
	candidate    CalendarIncomingMessage
	raw          *RawMessageSnapshot
	messageState [32]byte
	receipt      calendarIncomingReceipt
}

func (s *UserCalendarIncomingMessageSnapshot) Candidate() CalendarIncomingMessage { return s.candidate }
func (s *UserCalendarIncomingMessageSnapshot) Raw() *RawMessageSnapshot           { return s.raw }
func (s *UserCalendarIncomingMessageSnapshot) Complete() bool                     { return s.receipt.State == "complete" }

func (db *DB) SnapshotUserCalendarIncomingMessage(ctx context.Context, c CalendarIncomingMessage, guard func(*sql.Tx, string) error) (*UserCalendarIncomingMessageSnapshot, error) {
	if guard == nil || c.ID <= 0 || c.From == "" || c.From != strings.ToLower(c.From) {
		return nil, ErrCalendarIncomingChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := calendarControlOwnerTx(ctx, tx, c.UserID, c.AccountID, guard); err != nil {
		return nil, err
	}
	s := &UserCalendarIncomingMessageSnapshot{candidate: c}
	s.raw, err = db.rawMessageSnapshotTx(ctx, tx, c.UserID, c.ID)
	if err != nil {
		return nil, err
	}
	if s.raw.info.AccountID != c.AccountID {
		return nil, ErrCalendarIncomingChanged
	}
	s.messageState, err = calendarIncomingMessageStateTx(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	s.receipt, err = calendarIncomingReceiptTx(ctx, tx, c.ID)
	if err != nil {
		return nil, err
	}
	if s.receipt.Exists {
		var due bool
		if err := tx.QueryRowContext(ctx, `SELECT state='retry' AND next_attempt_at<=CURRENT_TIMESTAMP FROM calendar_incoming_messages WHERE message_id=?`, c.ID).Scan(&due); err != nil {
			return nil, err
		}
		if !due {
			return nil, ErrCalendarIncomingChanged
		}
	}
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, s, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s, nil
}

func (db *DB) validateUserCalendarIncomingMessageTx(ctx context.Context, tx *sql.Tx, s *UserCalendarIncomingMessageSnapshot, guard func(*sql.Tx, string) error) error {
	if s == nil || s.raw == nil || guard == nil {
		return ErrCalendarIncomingChanged
	}
	c := s.candidate
	if s.raw.owner != c.UserID || s.raw.id != c.ID || s.raw.info.AccountID != c.AccountID {
		return ErrCalendarIncomingChanged
	}
	if err := calendarControlOwnerTx(ctx, tx, c.UserID, c.AccountID, guard); err != nil {
		return err
	}
	if err := db.validateRawMessageTx(ctx, tx, s.raw, func() error { return guard(tx, c.AccountID) }); err != nil {
		return err
	}
	state, err := calendarIncomingMessageStateTx(ctx, tx, c)
	if err != nil {
		return err
	}
	if state != s.messageState {
		return ErrCalendarIncomingChanged
	}
	receipt, err := calendarIncomingReceiptTx(ctx, tx, c.ID)
	if err != nil {
		return err
	}
	if receipt != s.receipt {
		return ErrCalendarIncomingChanged
	}
	return nil
}

func (db *DB) ValidateUserCalendarIncomingMessage(ctx context.Context, s *UserCalendarIncomingMessageSnapshot, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, s, guard); err != nil {
		return err
	}
	return tx.Commit()
}

// Only actual raw recovery may change the raw path. The sealed retrieval
// identity, sender, folder memberships and exact receipt must still match.
func (db *DB) RefreshUserCalendarIncomingRaw(ctx context.Context, s *UserCalendarIncomingMessageSnapshot, raw *RawMessageSnapshot, guard func(*sql.Tx, string) error) (*UserCalendarIncomingMessageSnapshot, error) {
	if s == nil || !s.raw.SameIdentity(raw) {
		return nil, ErrCalendarIncomingChanged
	}
	next := *s
	next.raw = raw
	if err := db.ValidateUserCalendarIncomingMessage(ctx, &next, guard); err != nil {
		return nil, err
	}
	return &next, nil
}

// These receipts are diagnostics, never authentication or native write grants.
// A stale attempt cannot overwrite a newer one after a writer/config wait.
func (db *DB) FinishUserCalendarIncomingDelivery(ctx context.Context, s *UserCalendarIncomingMessageSnapshot, state, note, reason string, guard func(*sql.Tx, string) error) (*UserCalendarIncomingMessageSnapshot, error) {
	if s == nil || (state != "complete" && state != "ignored" && state != "retry") || len(reason) > 64 {
		return nil, ErrCalendarIncomingChanged
	}
	if len(note) > 4096 {
		note = strings.ToValidUTF8(note[:4096], "")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, s, guard); err != nil {
		return nil, err
	}
	next := *s
	next.receipt.Exists = true
	next.receipt.State, next.receipt.Note, next.receipt.Reason = state, note, reason
	next.receipt.Attempts++
	interval := "+0 seconds"
	if state == "retry" {
		interval = "+2 minutes"
	}
	if err := tx.QueryRowContext(ctx, `SELECT datetime('now',?),CURRENT_TIMESTAMP`, interval).Scan(&next.receipt.Next, &next.receipt.Updated); err != nil {
		return nil, err
	}
	if err := writeCalendarIncomingReceiptTx(ctx, tx, s.candidate.ID, next.receipt); err != nil {
		return nil, err
	}
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, &next, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}

func writeCalendarIncomingReceiptTx(ctx context.Context, tx *sql.Tx, id int64, r calendarIncomingReceipt) error {
	return teamsDraftChanged(tx.ExecContext(ctx, `INSERT INTO calendar_incoming_messages(message_id,state,next_attempt_at,attempts,last_error,reason_code,event_id,ical_uid,attendee,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(message_id) DO UPDATE SET state=excluded.state,next_attempt_at=excluded.next_attempt_at,attempts=excluded.attempts,last_error=excluded.last_error,reason_code=excluded.reason_code,event_id=excluded.event_id,ical_uid=excluded.ical_uid,attendee=excluded.attendee,updated_at=excluded.updated_at`, id, r.State, r.Next, r.Attempts, r.Note, r.Reason, r.Event, r.UID, r.Attendee, r.Updated))
}

// Association is a diagnostic step before sender verification. Neither this
// receipt nor a selected event claim authenticates the incoming mail.
func (db *DB) BeginUserCalendarIncomingDelivery(ctx context.Context, s *UserCalendarIncomingMessageSnapshot, event *UserCalendarEventSnapshot, guard func(*sql.Tx, string) error) (*UserCalendarIncomingMessageSnapshot, error) {
	if s == nil || event == nil || guard == nil {
		return nil, ErrCalendarIncomingChanged
	}
	e := event.Event()
	source := event.Source()
	c := s.candidate
	if source.Provider != CalendarSourceProviderCalDAV || source.AccountID != c.AccountID || e.UserID != c.UserID || source.UserID != c.UserID || e.ICalUID == "" || e.SeriesRemoteID != "" || e.AccountEmail == "" || !strings.EqualFold(e.OrganizerEmail, e.AccountEmail) {
		return nil, ErrCalendarIncomingChanged
	}
	var guests []struct{ Email string }
	if err := json.Unmarshal([]byte(e.AttendeesJSON), &guests); err != nil {
		return nil, ErrCalendarIncomingChanged
	}
	invited := false
	for _, g := range guests {
		if strings.EqualFold(g.Email, c.From) {
			invited = true
		}
	}
	if !invited {
		return nil, ErrCalendarIncomingChanged
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, s, guard); err != nil {
		return nil, err
	}
	if err := validateUserCalendarEventTx(ctx, tx, event, guard); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_events e JOIN calendar_sources source ON source.id=e.source_id AND source.user_id=e.user_id WHERE e.user_id=? AND source.account_id=? AND source.provider='caldav' AND source.is_selected=1 AND source.is_deleted=0 AND e.is_deleted=0 AND e.ical_uid=? AND e.series_remote_id=''`, c.UserID, c.AccountID, e.ICalUID).Scan(&count); err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, ErrCalendarIncomingChanged
	}
	next := *s
	next.receipt.Exists = true
	next.receipt.State, next.receipt.Note, next.receipt.Reason = "retry", "", "processing"
	next.receipt.Event, next.receipt.UID, next.receipt.Attendee = e.ID, e.ICalUID, c.From
	if err := tx.QueryRowContext(ctx, `SELECT datetime('now','+2 minutes'),CURRENT_TIMESTAMP`).Scan(&next.receipt.Next, &next.receipt.Updated); err != nil {
		return nil, err
	}
	if err := writeCalendarIncomingReceiptTx(ctx, tx, c.ID, next.receipt); err != nil {
		return nil, err
	}
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, &next, guard); err != nil {
		return nil, err
	}
	if err := validateUserCalendarEventTx(ctx, tx, event, guard); err != nil {
		return nil, err
	}
	// Recheck ambiguity after triggers; source/event fingerprint alone cannot
	// detect an additional matching UID under another selected source.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_events e JOIN calendar_sources source ON source.id=e.source_id AND source.user_id=e.user_id WHERE e.user_id=? AND source.account_id=? AND source.provider='caldav' AND source.is_selected=1 AND source.is_deleted=0 AND e.is_deleted=0 AND e.ical_uid=? AND e.series_remote_id=''`, c.UserID, c.AccountID, e.ICalUID).Scan(&count); err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, ErrCalendarIncomingChanged
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &next, nil
}
