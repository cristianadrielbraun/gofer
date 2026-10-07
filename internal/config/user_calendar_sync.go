package config

import (
	"context"
	"database/sql"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserCalendarSyncClaim struct {
	repository *UserAccountStore
	claim      *storage.UserCalendarSyncClaim
	service    *AccountServiceSnapshot
}

func (c *UserCalendarSyncClaim) Source() storage.CalendarSource   { return c.claim.Source() }
func (c *UserCalendarSyncClaim) Service() *AccountServiceSnapshot { return c.service }
func (c *UserCalendarSyncClaim) Window() (time.Time, time.Time)   { return c.claim.Window() }
func (c *UserCalendarSyncClaim) Attempt() int                     { return c.claim.Attempt() }

func (s *UserAccountStore) StartCalendarSync(ctx context.Context, owner, account, source string, start, end time.Time) (*UserCalendarSyncClaim, error) {
	c := &UserCalendarSyncClaim{repository: s}
	err := s.WithAccountForUser(ctx, owner, account, func(_ *AccountStore, db *storage.DB) error {
		var err error
		c.claim, err = db.StartUserCalendarSync(ctx, owner, account, source, start, end, s.calendarControlGuard(ctx, owner), func(tx *sql.Tx) error {
			var err error
			c.service, err = s.serviceSnapshot(ctx, tx, owner, account)
			return err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *UserAccountStore) calendarSyncGuard(ctx context.Context, c *UserCalendarSyncClaim, extra []func() error) (func(*sql.Tx, string) error, error) {
	if c == nil || c.repository != s || c.claim == nil || c.service == nil || len(extra) > 1 || (len(extra) == 1 && extra[0] == nil) {
		return nil, storage.ErrCalendarSyncChanged
	}
	if err := s.checkServiceSnapshot(c.service); err != nil {
		return nil, err
	}
	extra = append([]func() error(nil), extra...)
	return func(tx *sql.Tx, id string) error {
		if id != c.service.AccountID() {
			return storage.ErrAccountRoute
		}
		if err := s.serviceGuard(ctx, c.service, false)(tx); err != nil {
			return err
		}
		if len(extra) == 1 {
			return extra[0]()
		}
		return nil
	}, nil
}

func (s *UserAccountStore) ValidateCalendarSync(ctx context.Context, c *UserCalendarSyncClaim) error {
	guard, err := s.calendarSyncGuard(ctx, c, nil)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarSync(ctx, c.claim, guard)
	})
}

// Optional publication guards must read central state only. They run inside
// the local writer transaction, so must never reacquire a user-store lease.
func (s *UserAccountStore) PublishCalendarSync(ctx context.Context, c *UserCalendarSyncClaim, events []storage.CalendarEvent, next time.Time, extra ...func() error) error {
	guard, err := s.calendarSyncGuard(ctx, c, extra)
	if err != nil {
		return err
	}
	events = append([]storage.CalendarEvent(nil), events...)
	for i := range events {
		for _, at := range []**time.Time{&events[i].StartAt, &events[i].EndAt, &events[i].ProviderCreatedAt, &events[i].ProviderUpdatedAt} {
			if *at != nil {
				value := **at
				*at = &value
			}
		}
	}
	return s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.PublishUserCalendarSync(ctx, c.claim, events, next, guard)
	})
}

func (s *UserAccountStore) FailCalendarSync(ctx context.Context, c *UserCalendarSyncClaim, message string, next time.Time, extra ...func() error) error {
	guard, err := s.calendarSyncGuard(ctx, c, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.FailUserCalendarSync(ctx, c.claim, message, next, guard)
	})
}
