package config

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// TrashContacts moves the owner's contacts to the Trash.
func (s *UserAccountStore) TrashContacts(ctx context.Context, owner string, ids []string) (count int, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		count, err = db.TrashUserContacts(ctx, owner, ids, s.contactSyncOwnerGuard(ctx, owner))
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// RestoreContacts brings the owner's contacts back from the Trash.
func (s *UserAccountStore) RestoreContacts(ctx context.Context, owner string, ids []string) (count int, err error) {
	return s.RestoreContactsTo(ctx, owner, ids, "")
}

// RestoreContactsTo brings contacts back from the Trash to live in place,
// or where they lived when place is empty.
func (s *UserAccountStore) RestoreContactsTo(ctx context.Context, owner string, ids []string, place string) (count int, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		count, err = db.RestoreUserContactsTo(ctx, owner, ids, place, s.contactSyncOwnerGuard(ctx, owner))
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
