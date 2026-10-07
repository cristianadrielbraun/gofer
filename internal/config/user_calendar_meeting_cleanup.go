package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserCalendarMeetingCleanupSnapshot struct {
	repository *UserAccountStore
	service    *AccountServiceSnapshot
	cleanup    *storage.UserCalendarMeetingCleanupSnapshot
}

func (c *UserCalendarMeetingCleanupSnapshot) Source() storage.CalendarSource {
	return c.cleanup.Source()
}
func (c *UserCalendarMeetingCleanupSnapshot) Service() *AccountServiceSnapshot { return c.service }
func (c *UserCalendarMeetingCleanupSnapshot) Google() storage.CalendarMeetDraft {
	return c.cleanup.Google()
}
func (c *UserCalendarMeetingCleanupSnapshot) Teams() storage.CalendarTeamsDraft {
	return c.cleanup.Teams()
}
func (c *UserCalendarMeetingCleanupSnapshot) FinalResult() storage.CalendarCreateRequest {
	return c.cleanup.FinalResult()
}
func (c *UserCalendarMeetingCleanupSnapshot) Prune() bool { return c.cleanup.Prune() }

func (s *UserAccountStore) NextCalendarMeetingCleanup(ctx context.Context, owner, account string) (*storage.CalendarMeetingCleanupCandidate, error) {
	var candidate *storage.CalendarMeetingCleanupCandidate
	err := s.WithAccountForUser(ctx, owner, account, func(_ *AccountStore, db *storage.DB) error {
		var err error
		candidate, err = db.NextAccountCalendarMeetingCleanup(ctx, owner, account, s.calendarControlGuard(ctx, owner))
		return err
	})
	return candidate, err
}

func (s *UserAccountStore) SnapshotCalendarMeetingCleanup(ctx context.Context, owner, account, source, id string, prune bool) (*UserCalendarMeetingCleanupSnapshot, error) {
	c := &UserCalendarMeetingCleanupSnapshot{repository: s}
	err := s.WithAccountForUser(ctx, owner, account, func(_ *AccountStore, db *storage.DB) error {
		var err error
		c.cleanup, err = db.SnapshotUserCalendarMeetingCleanup(ctx, owner, account, source, id, prune, s.calendarControlGuard(ctx, owner), func(tx *sql.Tx, source storage.CalendarSource) ([32]byte, error) {
			var err error
			c.service, err = s.serviceSnapshot(ctx, tx, owner, account)
			if err != nil {
				return [32]byte{}, err
			}
			if !calendarCreateSourceWritable(source) {
				return [32]byte{}, storage.ErrCalendarSourceChanged
			}
			// Preparation only admitted selected sources. Reconstruct that
			// original principal for comparison; retain the real current
			// selection in the separate cleanup capability/fingerprint.
			source.IsSelected = true
			return calendarCreateBindingFor(source, c.service)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *UserAccountStore) calendarMeetingCleanupGuard(ctx context.Context, c *UserCalendarMeetingCleanupSnapshot, extra []func() error) (func(*sql.Tx, string) error, error) {
	if c == nil || c.repository != s || c.cleanup == nil || len(extra) > 1 || (len(extra) == 1 && extra[0] == nil) {
		return nil, storage.ErrCalendarCreateConflict
	}
	if err := s.checkServiceSnapshot(c.service); err != nil {
		return nil, err
	}
	extra = append([]func() error(nil), extra...)
	return func(tx *sql.Tx, account string) error {
		if account != c.service.AccountID() {
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

func (s *UserAccountStore) ValidateCalendarMeetingCleanup(ctx context.Context, c *UserCalendarMeetingCleanupSnapshot, extra ...func() error) error {
	guard, err := s.calendarMeetingCleanupGuard(ctx, c, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarMeetingCleanup(ctx, c.cleanup, guard)
	})
}

// Native cleanup transitions require the exact Calendar write-purpose grant.
func (s *UserAccountStore) PublishCalendarMeetingCleanup(ctx context.Context, c *UserCalendarMeetingCleanupSnapshot, transition storage.CalendarMeetingDraftTransition, extra ...func() error) (*UserCalendarMeetingCleanupSnapshot, error) {
	if len(extra) != 1 {
		return nil, storage.ErrCalendarCreateConflict
	}
	guard, err := s.calendarMeetingCleanupGuard(ctx, c, extra)
	if err != nil {
		return nil, err
	}
	next := &UserCalendarMeetingCleanupSnapshot{repository: s, service: c.service}
	err = s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next.cleanup, err = db.PublishUserCalendarMeetingCleanup(ctx, c.cleanup, transition, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

// Local retention needs no token refresh. Current lifecycle, service identity,
// original principal and exact draft/reservation/target remain guarded.
func (s *UserAccountStore) PruneCalendarMeetingCleanup(ctx context.Context, c *UserCalendarMeetingCleanupSnapshot) error {
	guard, err := s.calendarMeetingCleanupGuard(ctx, c, nil)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, c.service.OwnerID(), c.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.PruneUserCalendarMeetingCleanup(ctx, c.cleanup, guard)
	})
}
