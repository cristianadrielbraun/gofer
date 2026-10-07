package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (s *UserAccountStore) calendarControlGuard(ctx context.Context, owner string) func(*sql.Tx, string) error {
	return func(_ *sql.Tx, accountID string) error {
		if err := s.routing.ValidateUser(ctx, owner); err != nil {
			return err
		}
		state, err := s.routing.AccountStateForUser(ctx, owner, accountID)
		if err != nil {
			return err
		}
		if state != storage.AccountActive {
			return storage.ErrAccountRoute
		}
		return nil
	}
}

func (s *UserAccountStore) SetCalendarSourcesSelected(ctx context.Context, owner, accountID string, ids []string) error {
	ids = append([]string(nil), ids...)
	return s.WithAccountForUser(ctx, owner, accountID, func(_ *AccountStore, db *storage.DB) error {
		return db.SetUserCalendarSourceSelection(ctx, owner, accountID, ids, s.calendarControlGuard(ctx, owner))
	})
}

func (s *UserAccountStore) SetCalendarSourceVisible(ctx context.Context, owner, sourceID string, visible bool) error {
	return s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		return db.SetUserCalendarSourceVisibility(ctx, owner, sourceID, visible, s.calendarControlGuard(ctx, owner))
	})
}

func (s *UserAccountStore) SetCalendarServiceEnabled(ctx context.Context, owner, accountID string, enabled bool) error {
	return s.WithAccountForUser(ctx, owner, accountID, func(_ *AccountStore, db *storage.DB) error {
		return db.SetUserCalendarServiceEnabled(ctx, owner, accountID, enabled, s.calendarControlGuard(ctx, owner))
	})
}
