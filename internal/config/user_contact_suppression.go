package config

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (s *UserAccountStore) ReadContactSuppression(ctx context.Context, owner string) (contacts []models.Contact, count int, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		contacts, count, err = db.ReadUserSuppressedContacts(ctx, owner, s.contactSyncOwnerGuard(ctx, owner))
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return contacts, count, nil
}

func (s *UserAccountStore) ClearContactSuppression(ctx context.Context, owner string, id *string) (count int64, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		count, err = db.ClearUserContactSuppression(ctx, owner, id, s.contactSyncOwnerGuard(ctx, owner))
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *UserAccountStore) DeleteObservedContacts(ctx context.Context, owner string) (count int64, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		count, err = db.DeleteUserObservedContacts(ctx, owner, s.contactSyncOwnerGuard(ctx, owner))
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
