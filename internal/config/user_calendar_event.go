package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserCalendarSourceSnapshot struct {
	repository *UserAccountStore
	source     *storage.UserCalendarSourceSnapshot
	service    *AccountServiceSnapshot
}

func (s *UserCalendarSourceSnapshot) Source() storage.CalendarSource   { return s.source.Source() }
func (s *UserCalendarSourceSnapshot) Service() *AccountServiceSnapshot { return s.service }

// The account is derived from the owner's selected source, not a form field.
func (s *UserAccountStore) SnapshotCalendarSource(ctx context.Context, owner, id string) (*UserCalendarSourceSnapshot, error) {
	snapshot := &UserCalendarSourceSnapshot{repository: s}
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.source, err = db.SnapshotUserCalendarSource(ctx, owner, id, s.calendarControlGuard(ctx, owner), func(tx *sql.Tx, account string) error {
			var err error
			snapshot.service, err = s.serviceSnapshot(ctx, tx, owner, account)
			return err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) calendarSourceGuard(ctx context.Context, snapshot *UserCalendarSourceSnapshot, extra []func() error) (func(*sql.Tx, string) error, error) {
	if snapshot == nil || snapshot.repository != s || snapshot.source == nil || snapshot.service == nil || len(extra) > 1 || (len(extra) == 1 && extra[0] == nil) {
		return nil, storage.ErrCalendarSourceChanged
	}
	if err := s.checkServiceSnapshot(snapshot.service); err != nil {
		return nil, err
	}
	extra = append([]func() error(nil), extra...)
	return func(tx *sql.Tx, id string) error {
		if id != snapshot.service.AccountID() {
			return storage.ErrAccountRoute
		}
		if err := s.serviceGuard(ctx, snapshot.service, false)(tx); err != nil {
			return err
		}
		if len(extra) == 1 {
			return extra[0]()
		}
		return nil
	}, nil
}

// Optional guards read central state only, while the local transaction is open.
func (s *UserAccountStore) ValidateCalendarSource(ctx context.Context, snapshot *UserCalendarSourceSnapshot, extra ...func() error) error {
	guard, err := s.calendarSourceGuard(ctx, snapshot, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, snapshot.service.OwnerID(), snapshot.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarSource(ctx, snapshot.source, guard)
	})
}

type UserCalendarEventSnapshot struct {
	repository *UserAccountStore
	event      *storage.UserCalendarEventSnapshot
	service    *AccountServiceSnapshot
}

func (s *UserCalendarEventSnapshot) Event() storage.CalendarEvent     { return s.event.Event() }
func (s *UserCalendarEventSnapshot) Source() storage.CalendarSource   { return s.event.Source() }
func (s *UserCalendarEventSnapshot) Service() *AccountServiceSnapshot { return s.service }

func (s *UserAccountStore) SnapshotCalendarEvent(ctx context.Context, owner, id string) (*UserCalendarEventSnapshot, error) {
	snapshot := &UserCalendarEventSnapshot{repository: s}
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.event, err = db.SnapshotUserCalendarEvent(ctx, owner, id, s.calendarControlGuard(ctx, owner), func(tx *sql.Tx, account string) error {
			var err error
			snapshot.service, err = s.serviceSnapshot(ctx, tx, owner, account)
			return err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) calendarEventGuard(ctx context.Context, snapshot *UserCalendarEventSnapshot, extra []func() error) (func(*sql.Tx, string) error, error) {
	if snapshot == nil || snapshot.repository != s || snapshot.event == nil || snapshot.service == nil || len(extra) > 1 || (len(extra) == 1 && extra[0] == nil) {
		return nil, storage.ErrCalendarEventChanged
	}
	if err := s.checkServiceSnapshot(snapshot.service); err != nil {
		return nil, err
	}
	extra = append([]func() error(nil), extra...)
	return func(tx *sql.Tx, id string) error {
		if id != snapshot.service.AccountID() {
			return storage.ErrAccountRoute
		}
		if err := s.serviceGuard(ctx, snapshot.service, false)(tx); err != nil {
			return err
		}
		if len(extra) == 1 {
			return extra[0]()
		}
		return nil
	}, nil
}

// Optional guards must only read central state: a local transaction is active
// while they run. Provider actions use this for exact write-grant revisions.
func (s *UserAccountStore) ValidateCalendarEvent(ctx context.Context, snapshot *UserCalendarEventSnapshot, extra ...func() error) error {
	guard, err := s.calendarEventGuard(ctx, snapshot, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, snapshot.service.OwnerID(), snapshot.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarEvent(ctx, snapshot.event, guard)
	})
}
