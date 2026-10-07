package config

import (
	"context"
	"database/sql"
	"sort"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserContactDeleteSnapshot struct {
	repository *UserAccountStore
	deletion   *storage.ContactDeleteSnapshot
	services   []*AccountServiceSnapshot
}

func (s *UserContactDeleteSnapshot) Sources() []models.ContactCard { return s.deletion.Sources() }
func (s *UserContactDeleteSnapshot) Services() []*AccountServiceSnapshot {
	return append([]*AccountServiceSnapshot(nil), s.services...)
}

// Source resolves an ID against private captured metadata. Mutating a public
// card/service slice cannot choose another endpoint or credential scope.
func (s *UserContactDeleteSnapshot) Source(id string) (models.ContactCard, *AccountServiceSnapshot, error) {
	if s == nil || s.deletion == nil {
		return models.ContactCard{}, nil, storage.ErrContactEditChanged
	}
	for _, card := range s.deletion.Sources() {
		if card.ID != id {
			continue
		}
		if card.AccountID == "" && card.RemoteID == "" {
			return card, nil, nil
		}
		for _, service := range s.services {
			if service.AccountID() == card.AccountID {
				return card, service, nil
			}
		}
		return models.ContactCard{}, nil, storage.ErrAccountRoute
	}
	return models.ContactCard{}, nil, storage.ErrContactEditChanged
}

func (s *UserAccountStore) SnapshotContactDelete(ctx context.Context, owner, id string) (*UserContactDeleteSnapshot, error) {
	snapshot := &UserContactDeleteSnapshot{repository: s}
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.deletion, err = db.SnapshotUserContactDelete(ctx, owner, id, s.contactSyncOwnerGuard(ctx, owner), func(tx *sql.Tx, cards []models.ContactCard) error {
			byAccount := map[string]*AccountServiceSnapshot{}
			for _, card := range cards {
				if card.AccountID == "" {
					if card.RemoteID != "" {
						return storage.ErrAccountRoute
					}
					continue
				}
				service := byAccount[card.AccountID]
				if service == nil {
					state, err := s.routing.AccountStateForUser(ctx, owner, card.AccountID)
					if err != nil {
						return err
					}
					if state != storage.AccountActive {
						return storage.ErrAccountRoute
					}
					service, err = s.serviceSnapshot(ctx, tx, owner, card.AccountID)
					if err != nil {
						return err
					}
					byAccount[card.AccountID] = service
				}
				if service.ContactConfig().Provider != card.Provider {
					return ErrAccountServicesChanged
				}
			}
			var ids []string
			for id := range byAccount {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				snapshot.services = append(snapshot.services, byAccount[id])
			}
			return nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}
func (s *UserAccountStore) contactDeleteGuard(ctx context.Context, snapshot *UserContactDeleteSnapshot) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		if err := s.routing.ValidateUser(ctx, snapshot.deletion.OwnerID()); err != nil {
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
func (s *UserAccountStore) validContactDelete(snapshot *UserContactDeleteSnapshot) bool {
	return snapshot != nil && snapshot.repository == s && snapshot.deletion != nil
}
func (s *UserAccountStore) ValidateContactDelete(ctx context.Context, snapshot *UserContactDeleteSnapshot) error {
	if !s.validContactDelete(snapshot) {
		return storage.ErrContactEditChanged
	}
	return s.WithUser(ctx, snapshot.deletion.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserContactDelete(ctx, snapshot.deletion, s.contactDeleteGuard(ctx, snapshot))
	})
}
func (s *UserAccountStore) AcknowledgeContactDeleteSource(ctx context.Context, snapshot *UserContactDeleteSnapshot, cardID string) (*UserContactDeleteSnapshot, error) {
	if !s.validContactDelete(snapshot) {
		return nil, storage.ErrContactEditChanged
	}
	var next *storage.ContactDeleteSnapshot
	err := s.WithUser(ctx, snapshot.deletion.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		next, err = db.AcknowledgeUserContactDeleteSource(ctx, snapshot.deletion, cardID, s.contactDeleteGuard(ctx, snapshot))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &UserContactDeleteSnapshot{repository: s, deletion: next, services: append([]*AccountServiceSnapshot(nil), snapshot.services...)}, nil
}
func (s *UserAccountStore) FinishContactDelete(ctx context.Context, snapshot *UserContactDeleteSnapshot) (id string, err error) {
	if !s.validContactDelete(snapshot) {
		return "", storage.ErrContactEditChanged
	}
	err = s.WithUser(ctx, snapshot.deletion.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		id, err = db.FinishUserContactDelete(ctx, snapshot.deletion, s.contactDeleteGuard(ctx, snapshot))
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}
