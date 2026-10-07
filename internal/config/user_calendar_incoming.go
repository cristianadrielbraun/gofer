package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A received message/receipt claim is distinct from an authenticated RSVP.
// It grants no provider dispatch or calendar-event publication authority.
type UserCalendarIncomingMessageSnapshot struct {
	repository *UserAccountStore
	message    *storage.UserCalendarIncomingMessageSnapshot
	service    *AccountServiceSnapshot
}

func (c *UserCalendarIncomingMessageSnapshot) Candidate() storage.CalendarIncomingMessage {
	return c.message.Candidate()
}
func (c *UserCalendarIncomingMessageSnapshot) Complete() bool { return c.message.Complete() }

func (s *UserAccountStore) ListCalendarIncomingMessages(ctx context.Context, owner, account string, limit int) ([]storage.CalendarIncomingMessage, error) {
	var result []storage.CalendarIncomingMessage
	err := s.WithAccountForUser(ctx, owner, account, func(_ *AccountStore, db *storage.DB) error {
		var err error
		result, err = db.ListAccountCalendarIncomingMessages(ctx, owner, account, limit, s.calendarControlGuard(ctx, owner))
		return err
	})
	return result, err
}

func (s *UserAccountStore) SnapshotCalendarIncomingMessage(ctx context.Context, c storage.CalendarIncomingMessage) (*UserCalendarIncomingMessageSnapshot, error) {
	snapshot := &UserCalendarIncomingMessageSnapshot{repository: s}
	err := s.WithAccountForUser(ctx, c.UserID, c.AccountID, func(_ *AccountStore, db *storage.DB) error {
		guard := func(tx *sql.Tx, account string) error {
			if account != c.AccountID {
				return storage.ErrCalendarIncomingChanged
			}
			if err := s.calendarControlGuard(ctx, c.UserID)(tx, account); err != nil {
				return err
			}
			if snapshot.service == nil {
				var err error
				snapshot.service, err = s.serviceSnapshot(ctx, tx, c.UserID, account)
				return err
			}
			return s.serviceGuard(ctx, snapshot.service, false)(tx)
		}
		var err error
		snapshot.message, err = db.SnapshotUserCalendarIncomingMessage(ctx, c, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) calendarIncomingMessageGuard(ctx context.Context, c *UserCalendarIncomingMessageSnapshot) (func(*sql.Tx, string) error, error) {
	if c == nil || c.repository != s || c.message == nil {
		return nil, storage.ErrCalendarIncomingChanged
	}
	if err := s.checkServiceSnapshot(c.service); err != nil {
		return nil, err
	}
	return func(tx *sql.Tx, account string) error {
		if account != c.service.AccountID() {
			return storage.ErrCalendarIncomingChanged
		}
		return s.serviceGuard(ctx, c.service, false)(tx)
	}, nil
}

func (s *UserAccountStore) ValidateCalendarIncomingMessage(ctx context.Context, c *UserCalendarIncomingMessageSnapshot) error {
	guard, err := s.calendarIncomingMessageGuard(ctx, c)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarIncomingMessage(ctx, c.message, guard)
	})
}

func (s *UserAccountStore) RefreshCalendarIncomingRaw(ctx context.Context, c *UserCalendarIncomingMessageSnapshot, raw *storage.RawMessageSnapshot) (*UserCalendarIncomingMessageSnapshot, error) {
	guard, err := s.calendarIncomingMessageGuard(ctx, c)
	if err != nil {
		return nil, err
	}
	next := &UserCalendarIncomingMessageSnapshot{repository: s, service: c.service}
	err = s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next.message, err = db.RefreshUserCalendarIncomingRaw(ctx, c.message, raw, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

func (s *UserAccountStore) FinishCalendarIncomingDelivery(ctx context.Context, c *UserCalendarIncomingMessageSnapshot, state, note, reason string) (*UserCalendarIncomingMessageSnapshot, error) {
	guard, err := s.calendarIncomingMessageGuard(ctx, c)
	if err != nil {
		return nil, err
	}
	next := &UserCalendarIncomingMessageSnapshot{repository: s, service: c.service}
	err = s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next.message, err = db.FinishUserCalendarIncomingDelivery(ctx, c.message, state, note, reason, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

func (s *UserAccountStore) BeginCalendarIncomingDelivery(ctx context.Context, c *UserCalendarIncomingMessageSnapshot, event *UserCalendarEventSnapshot) (*UserCalendarIncomingMessageSnapshot, error) {
	guard, err := s.calendarIncomingMessageGuard(ctx, c)
	if err != nil {
		return nil, err
	}
	if event == nil || event.repository != s || event.event == nil || event.service == nil || event.service.OwnerID() != c.service.OwnerID() || event.service.AccountID() != c.service.AccountID() || event.service.connection != c.service.connection || event.service.calendar != c.service.calendar {
		return nil, storage.ErrCalendarIncomingChanged
	}
	next := &UserCalendarIncomingMessageSnapshot{repository: s, service: c.service}
	err = s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next.message, err = db.BeginUserCalendarIncomingDelivery(ctx, c.message, event.event, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}
