package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserCalendarResponseClaim struct {
	repository *UserAccountStore
	event      *UserCalendarEventSnapshot
	claim      *storage.UserCalendarResponseClaim
}

func (s *UserAccountStore) calendarResponseGuard(ctx context.Context, snapshot *UserCalendarEventSnapshot, extra []func() error) (func(*sql.Tx, string) error, error) {
	guard, err := s.calendarEventGuard(ctx, snapshot, extra)
	if err != nil {
		return nil, err
	}
	if snapshot.Source().Provider != storage.CalendarSourceProviderCalDAV && len(extra) != 1 {
		return nil, storage.ErrCalendarEventChanged
	}
	return guard, nil
}

func (s *UserAccountStore) ReserveCalendarResponse(ctx context.Context, snapshot *UserCalendarEventSnapshot, scope, version, response string, extra ...func() error) (*UserCalendarResponseClaim, error) {
	guard, err := s.calendarResponseGuard(ctx, snapshot, extra)
	if err != nil {
		return nil, err
	}
	claim := &UserCalendarResponseClaim{repository: s, event: snapshot}
	err = s.WithAccountForUser(ctx, snapshot.service.OwnerID(), snapshot.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		claim.claim, err = db.ReserveUserCalendarResponse(ctx, snapshot.event, scope, version, response, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

func (s *UserAccountStore) ReleaseCalendarResponse(ctx context.Context, claim *UserCalendarResponseClaim, extra ...func() error) error {
	if claim == nil || claim.repository != s || claim.event == nil || claim.claim == nil {
		return storage.ErrCalendarEventChanged
	}
	guard, err := s.calendarResponseGuard(ctx, claim.event, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, claim.event.service.OwnerID(), claim.event.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ReleaseUserCalendarResponse(ctx, claim.claim, guard)
	})
}
