package config

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (c *UserCalendarResponseClaim) Event() *UserCalendarEventSnapshot { return c.event }

func (s *UserAccountStore) ValidateCalendarResponse(ctx context.Context, claim *UserCalendarResponseClaim, extra ...func() error) error {
	if claim == nil || claim.repository != s || claim.event == nil || claim.claim == nil {
		return storage.ErrCalendarEventChanged
	}
	guard, err := s.calendarResponseGuard(ctx, claim.event, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, claim.event.service.OwnerID(), claim.event.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarResponseClaim(ctx, claim.claim, guard)
	})
}

func (s *UserAccountStore) PublishCalendarResponse(ctx context.Context, claim *UserCalendarResponseClaim, event storage.CalendarEvent, extra ...func() error) error {
	if claim == nil || claim.repository != s || claim.event == nil || claim.claim == nil {
		return storage.ErrCalendarEventChanged
	}
	guard, err := s.calendarResponseGuard(ctx, claim.event, extra)
	if err != nil {
		return err
	}
	event = copyCalendarProviderEvent(event)
	return s.WithAccountForUser(ctx, claim.event.service.OwnerID(), claim.event.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.PublishUserCalendarResponse(ctx, claim.claim, event, guard)
	})
}
