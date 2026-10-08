package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// A reservation keeps newer authenticated guest intent across uncertain writes.
// Authentication is performed by the caller; diagnostic receipts alone never
// authorize a provider operation.
type UserCalendarIncomingResponseClaim struct {
	message         *UserCalendarIncomingMessageSnapshot
	event           *UserCalendarEventSnapshot
	sequence        int
	stamp, response string
}

func calendarIncomingEventUniqueTx(ctx context.Context, tx *sql.Tx, event *UserCalendarEventSnapshot) error {
	e := event.Event()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_events e JOIN calendar_sources s ON s.id=e.source_id AND s.user_id=e.user_id WHERE e.user_id=? AND s.account_id=? AND s.provider='caldav' AND s.is_selected=1 AND s.is_deleted=0 AND e.is_deleted=0 AND e.ical_uid=? AND e.series_remote_id=''`, e.UserID, event.Source().AccountID, e.ICalUID).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrCalendarIncomingChanged
	}
	return nil
}

func (db *DB) ValidateUserCalendarIncomingAssociation(ctx context.Context, m *UserCalendarIncomingMessageSnapshot, event *UserCalendarEventSnapshot, guard func(*sql.Tx, string) error) error {
	if m == nil || event == nil || guard == nil || m.receipt.Event != event.Event().ID || m.receipt.UID != event.Event().ICalUID || m.receipt.Attendee != m.candidate.From || event.Source().UserID != m.candidate.UserID || event.Source().AccountID != m.candidate.AccountID {
		return ErrCalendarIncomingChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, m, guard); err != nil {
		return err
	}
	if err := validateUserCalendarEventTx(ctx, tx, event, guard); err != nil {
		return err
	}
	if err := calendarIncomingEventUniqueTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) calendarIncomingResponseGuardTx(ctx context.Context, tx *sql.Tx, claim *UserCalendarIncomingResponseClaim, guard func(*sql.Tx, string) error) error {
	if claim == nil || claim.message == nil || claim.event == nil || guard == nil {
		return ErrCalendarIncomingChanged
	}
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, claim.message, guard); err != nil {
		return err
	}
	if err := calendarIncomingEventUniqueTx(ctx, tx, claim.event); err != nil {
		return err
	}
	e := claim.event.Event()
	var seq int
	var stamp, response string
	err := tx.QueryRowContext(ctx, `SELECT sequence,stamp,response FROM calendar_incoming_responses WHERE user_id=? AND source_id=? AND ical_uid=? AND attendee=?`, e.UserID, e.SourceID, e.ICalUID, claim.message.candidate.From).Scan(&seq, &stamp, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCalendarIncomingChanged
	}
	if err != nil {
		return err
	}
	if seq != claim.sequence || stamp != claim.stamp || response != claim.response {
		return ErrCalendarIncomingChanged
	}
	return nil
}

func (db *DB) ReserveUserCalendarIncomingResponse(ctx context.Context, m *UserCalendarIncomingMessageSnapshot, event *UserCalendarEventSnapshot, sequence int, stamp time.Time, response string, guard func(*sql.Tx, string) error) (*UserCalendarIncomingResponseClaim, error) {
	if m == nil || event == nil || guard == nil || sequence < 0 || stamp.IsZero() || (response != "ACCEPTED" && response != "TENTATIVE" && response != "DECLINED") {
		return nil, ErrCalendarIncomingChanged
	}
	e, source := event.Event(), event.Source()
	if source.Provider != CalendarSourceProviderCalDAV || source.UserID != m.candidate.UserID || source.AccountID != m.candidate.AccountID || e.SeriesRemoteID != "" || calendarPublicationHasRecurrence(e.RecurrenceJSON) || e.ICalUID == "" || e.AccountEmail == "" || !strings.EqualFold(e.OrganizerEmail, e.AccountEmail) || m.receipt.Event != e.ID || m.receipt.UID != e.ICalUID || m.receipt.Attendee != m.candidate.From || m.receipt.Reason != "processing" || m.receipt.State != "retry" {
		return nil, ErrCalendarIncomingChanged
	}
	role := strings.ToLower(strings.TrimSpace(source.AccessRole))
	if role != "owner" && role != "writer" && role != "" && role != "unknown" {
		return nil, ErrCalendarIncomingChanged
	}
	claim := &UserCalendarIncomingResponseClaim{message: m, event: event, sequence: sequence, stamp: stamp.UTC().Format(time.RFC3339), response: response}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := db.validateUserCalendarIncomingMessageTx(ctx, tx, m, guard); err != nil {
		return nil, err
	}
	if err := validateUserCalendarEventTx(ctx, tx, event, guard); err != nil {
		return nil, err
	}
	if err := calendarIncomingEventUniqueTx(ctx, tx, event); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO calendar_incoming_responses(user_id,source_id,ical_uid,attendee,sequence,stamp,response) VALUES(?,?,?,?,?,?,?) ON CONFLICT(source_id,ical_uid,attendee) DO UPDATE SET sequence=excluded.sequence,stamp=excluded.stamp,response=excluded.response,updated_at=CURRENT_TIMESTAMP WHERE calendar_incoming_responses.sequence<excluded.sequence OR (calendar_incoming_responses.sequence=excluded.sequence AND calendar_incoming_responses.stamp<excluded.stamp)`, e.UserID, e.SourceID, e.ICalUID, m.candidate.From, sequence, claim.stamp, response)
	if err != nil {
		return nil, err
	}
	if err := db.calendarIncomingResponseGuardTx(ctx, tx, claim, guard); err != nil {
		return nil, err
	}
	if err := validateUserCalendarEventTx(ctx, tx, event, guard); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

func (db *DB) ValidateUserCalendarIncomingResponse(ctx context.Context, claim *UserCalendarIncomingResponseClaim, guard func(*sql.Tx, string) error) error {
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.calendarIncomingResponseGuardTx(ctx, tx, claim, guard); err != nil {
		return err
	}
	if err := validateUserCalendarEventTx(ctx, tx, claim.event, guard); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) PublishUserCalendarIncomingResponse(ctx context.Context, claim *UserCalendarIncomingResponseClaim, event CalendarEvent, guard func(*sql.Tx, string) error) error {
	if claim == nil || claim.event == nil || claim.message == nil || guard == nil {
		return ErrCalendarIncomingChanged
	}
	var guests []struct{ Email, Status string }
	if json.Unmarshal([]byte(event.AttendeesJSON), &guests) != nil {
		return ErrCalendarIncomingChanged
	}
	n := 0
	for _, g := range guests {
		if strings.EqualFold(g.Email, claim.message.candidate.From) {
			if g.Status != claim.response {
				return ErrCalendarIncomingChanged
			}
			n++
		}
	}
	if n != 1 {
		return ErrCalendarIncomingChanged
	}
	return db.publishUserCalendarEvent(ctx, claim.event, calendarPublishIncomingResponse, event, func(tx *sql.Tx, account string) error {
		if err := guard(tx, account); err != nil {
			return err
		}
		return db.calendarIncomingResponseGuardTx(ctx, tx, claim, guard)
	})
}
