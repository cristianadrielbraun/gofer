package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// UserContactEditSnapshot binds copied editor and destination service state to
// this repository. No user database or plaintext provider credential is retained.
type UserContactEditSnapshot struct {
	repository *UserAccountStore
	edit       *storage.ContactEditSnapshot
	services   []*AccountServiceSnapshot
}

func (s *UserContactEditSnapshot) Contact() models.Contact   { return s.edit.Contact() }
func (s *UserContactEditSnapshot) Previous() *models.Contact { return s.edit.Previous() }

func (s *UserAccountStore) SnapshotContactEdit(ctx context.Context, owner string, request models.Contact) (snapshot *UserContactEditSnapshot, err error) {
	candidate := &UserContactEditSnapshot{repository: s}
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var captureErr error
		candidate.edit, captureErr = db.SnapshotUserContactEdit(ctx, owner, request, s.contactSyncOwnerGuard(ctx, owner), func(tx *sql.Tx, ids []string) error {
			return s.captureContactEditServices(ctx, owner, candidate, tx, ids)
		})
		return captureErr
	})
	if err != nil {
		return nil, err
	}
	return candidate, nil
}

func (s *UserAccountStore) SaveContactEdit(ctx context.Context, snapshot *UserContactEditSnapshot, deferSetup bool) (result storage.ContactEditResult, err error) {
	if snapshot == nil || snapshot.repository != s || snapshot.edit == nil {
		return result, storage.ErrContactEditChanged
	}
	owner := snapshot.edit.OwnerID()
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var saveErr error
		result, saveErr = db.SaveUserContactEdit(ctx, snapshot.edit, deferSetup, s.contactEditGuard(ctx, snapshot))
		return saveErr
	})
	if err != nil {
		return storage.ContactEditResult{}, err
	}
	return result, nil
}

func (s *UserAccountStore) SnapshotContactUnify(ctx context.Context, owner, id string) (*UserContactEditSnapshot, error) {
	snapshot := &UserContactEditSnapshot{repository: s}
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.edit, err = db.SnapshotUserContactUnify(ctx, owner, id, s.contactSyncOwnerGuard(ctx, owner), func(tx *sql.Tx, ids []string) error {
			return s.captureContactEditServices(ctx, owner, snapshot, tx, ids)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) captureContactEditServices(ctx context.Context, owner string, snapshot *UserContactEditSnapshot, tx *sql.Tx, ids []string) error {
	for _, id := range ids {
		state, err := s.routing.AccountStateForUser(ctx, owner, id)
		if err != nil {
			return err
		}
		if state != storage.AccountActive {
			return storage.ErrAccountRoute
		}
		service, err := s.serviceSnapshot(ctx, tx, owner, id)
		if err != nil {
			return err
		}
		snapshot.services = append(snapshot.services, service)
	}
	return nil
}

func (s *UserAccountStore) contactEditGuard(ctx context.Context, snapshot *UserContactEditSnapshot) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		if err := s.routing.ValidateUser(ctx, snapshot.edit.OwnerID()); err != nil {
			return err
		}
		for _, service := range snapshot.services {
			if err := s.serviceGuard(ctx, service, true)(tx); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *UserAccountStore) SnapshotContactSetup(ctx context.Context, owner, id string) (*UserContactEditSnapshot, error) {
	snapshot := &UserContactEditSnapshot{repository: s}
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.edit, err = db.SnapshotUserContactSetup(ctx, owner, id, s.contactSyncOwnerGuard(ctx, owner), func(tx *sql.Tx, ids []string) error {
			return s.captureContactEditServices(ctx, owner, snapshot, tx, ids)
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) ConfirmContactSetup(ctx context.Context, snapshot *UserContactEditSnapshot, selected map[string]string) (result storage.ContactEditResult, err error) {
	if snapshot == nil || snapshot.repository != s || snapshot.edit == nil {
		return result, storage.ErrContactEditChanged
	}
	err = s.WithUser(ctx, snapshot.edit.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		result, err = db.ConfirmUserContactSetup(ctx, snapshot.edit, selected, s.contactEditGuard(ctx, snapshot))
		return err
	})
	if err != nil {
		return storage.ContactEditResult{}, err
	}
	return result, nil
}
