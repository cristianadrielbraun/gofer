package config

import (
	"context"
	"database/sql"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserCalendarIncomingResponseClaim struct {
	repository *UserAccountStore
	message    *UserCalendarIncomingMessageSnapshot
	event      *UserCalendarEventSnapshot
	claim      *storage.UserCalendarIncomingResponseClaim
}

func (c *UserCalendarIncomingResponseClaim) Message() *UserCalendarIncomingMessageSnapshot {
	return c.message
}
func (c *UserCalendarIncomingResponseClaim) Event() *UserCalendarEventSnapshot { return c.event }

func (s *UserAccountStore) ValidateCalendarIncomingAssociation(ctx context.Context, c *UserCalendarIncomingMessageSnapshot, event *UserCalendarEventSnapshot) error {
	guard, err := s.incomingResponseGuard(ctx, c, event)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarIncomingAssociation(ctx, c.message, event.event, guard)
	})
}

func (s *UserAccountStore) FindCalendarIncomingEvent(ctx context.Context, c *UserCalendarIncomingMessageSnapshot, uid string) (*UserCalendarEventSnapshot, error) {
	if err := s.ValidateCalendarIncomingMessage(ctx, c); err != nil {
		return nil, err
	}
	var event storage.CalendarEvent
	err := s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		event, err = db.FindCalendarIncomingEvent(ctx, c.service.OwnerID(), c.service.AccountID(), uid)
		return err
	})
	if err != nil {
		return nil, err
	}
	snapshot, err := s.SnapshotCalendarEvent(ctx, c.service.OwnerID(), event.ID)
	if err != nil {
		return nil, err
	}
	if snapshot.Source().AccountID != c.service.AccountID() || snapshot.Event().ICalUID != uid {
		return nil, storage.ErrCalendarIncomingChanged
	}
	return snapshot, nil
}

func (s *UserAccountStore) incomingResponseGuard(ctx context.Context, c *UserCalendarIncomingMessageSnapshot, event *UserCalendarEventSnapshot) (func(*sql.Tx, string) error, error) {
	guard, err := s.calendarIncomingMessageGuard(ctx, c)
	if err != nil {
		return nil, err
	}
	if event == nil || event.repository != s || event.event == nil || event.service == nil || event.service.OwnerID() != c.service.OwnerID() || event.service.AccountID() != c.service.AccountID() || event.service.connection != c.service.connection || event.service.calendar != c.service.calendar {
		return nil, storage.ErrCalendarIncomingChanged
	}
	return guard, nil
}

func (s *UserAccountStore) ReserveCalendarIncomingResponse(ctx context.Context, c *UserCalendarIncomingMessageSnapshot, event *UserCalendarEventSnapshot, sequence int, stamp time.Time, response string) (*UserCalendarIncomingResponseClaim, error) {
	guard, err := s.incomingResponseGuard(ctx, c, event)
	if err != nil {
		return nil, err
	}
	claim := &UserCalendarIncomingResponseClaim{repository: s, message: c, event: event}
	err = s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		claim.claim, err = db.ReserveUserCalendarIncomingResponse(ctx, c.message, event.event, sequence, stamp, response, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

func (s *UserAccountStore) ValidateCalendarIncomingResponse(ctx context.Context, claim *UserCalendarIncomingResponseClaim) error {
	if claim == nil || claim.repository != s || claim.claim == nil {
		return storage.ErrCalendarIncomingChanged
	}
	guard, err := s.incomingResponseGuard(ctx, claim.message, claim.event)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, claim.message.service.OwnerID(), claim.message.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarIncomingResponse(ctx, claim.claim, guard)
	})
}

func (s *UserAccountStore) PublishCalendarIncomingResponse(ctx context.Context, claim *UserCalendarIncomingResponseClaim, event storage.CalendarEvent) error {
	if claim == nil || claim.repository != s || claim.claim == nil {
		return storage.ErrCalendarIncomingChanged
	}
	guard, err := s.incomingResponseGuard(ctx, claim.message, claim.event)
	if err != nil {
		return err
	}
	event = copyCalendarProviderEvent(event)
	return s.WithAccountForUser(ctx, claim.message.service.OwnerID(), claim.message.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.PublishUserCalendarIncomingResponse(ctx, claim.claim, event, guard)
	})
}
