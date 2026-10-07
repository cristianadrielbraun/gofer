package storage

import (
	"context"
	"database/sql"
)

// Native RSVP dispatch must retain the exact nonce between reservation and
// provider work, independently of its event/service authorization.
func (db *DB) ValidateUserCalendarResponseClaim(ctx context.Context, claim *UserCalendarResponseClaim, guard func(*sql.Tx, string) error) error {
	if claim == nil || claim.snapshot == nil || claim.id == "" || guard == nil {
		return ErrCalendarEventChanged
	}
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateUserCalendarEventTx(ctx, tx, claim.snapshot, guard); err != nil {
		return err
	}
	if err := calendarResponseClaimMatchesTx(ctx, tx, claim); err != nil {
		return err
	}
	if err := guard(tx, claim.snapshot.source.AccountID); err != nil {
		return err
	}
	return tx.Commit()
}

// The shared publication transaction calls this guard before and after event
// mutation, so writer waits or triggers cannot publish a replaced reservation.
func (db *DB) PublishUserCalendarResponse(ctx context.Context, claim *UserCalendarResponseClaim, event CalendarEvent, guard func(*sql.Tx, string) error) error {
	if claim == nil || claim.snapshot == nil || claim.id == "" || guard == nil || claim.scope == "series" || event.ResponseStatus != claim.response {
		return ErrCalendarEventChanged
	}
	return db.PublishUserCalendarEvent(ctx, claim.snapshot, CalendarPublishResponse, event, func(tx *sql.Tx, account string) error {
		if account != claim.snapshot.source.AccountID {
			return ErrAccountRoute
		}
		if err := guard(tx, account); err != nil {
			return err
		}
		return calendarResponseClaimMatchesTx(ctx, tx, claim)
	})
}
