package config

import (
	"context"
	"database/sql"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserContactPreviewSnapshot struct {
	repository *UserAccountStore
	preview    *storage.ContactPreviewSnapshot
	edit       *UserContactEditSnapshot
}

func (s *UserContactPreviewSnapshot) Contact() models.Contact { return s.preview.Contact() }
func (s *UserContactPreviewSnapshot) Selections() []storage.ContactSetupSelection {
	return s.preview.Selections()
}
func (s *UserContactPreviewSnapshot) StoredContact(account string) *models.Contact {
	return s.preview.StoredContact(account)
}
func (s *UserContactPreviewSnapshot) Services() []*AccountServiceSnapshot {
	return append([]*AccountServiceSnapshot(nil), s.edit.services...)
}
func (s *UserContactEditSnapshot) Services() []*AccountServiceSnapshot {
	return append([]*AccountServiceSnapshot(nil), s.services...)
}
func (s *UserContactEditSnapshot) SetupFields() []models.ContactField { return s.edit.SetupFields() }

func (s *UserAccountStore) SnapshotContactPreview(ctx context.Context, owner, id string, keys map[string]string) (*UserContactPreviewSnapshot, error) {
	snapshot := &UserContactPreviewSnapshot{repository: s, edit: &UserContactEditSnapshot{repository: s}}
	err := s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.preview, err = db.SnapshotUserContactPreview(ctx, owner, id, s.contactSyncOwnerGuard(ctx, owner), func(tx *sql.Tx, accounts []string) ([]storage.ContactSetupSelection, error) {
			if err := s.captureContactEditServices(ctx, owner, snapshot.edit, tx, accounts); err != nil {
				return nil, err
			}
			var selections []storage.ContactSetupSelection
			for _, service := range snapshot.edit.services {
				key := strings.TrimSpace(keys[service.AccountID()])
				if key == "" || key == "none" {
					continue
				}
				selection := storage.ContactSetupSelection{AccountID: service.AccountID(), Provider: service.ContactConfig().Provider}
				switch {
				case strings.HasPrefix(key, "stored:"):
					selection.StoredProfileID = strings.TrimSpace(strings.TrimPrefix(key, "stored:"))
				case selection.Provider == "gmail" && strings.HasPrefix(key, "gmail:"):
					selection.RemoteID = strings.TrimSpace(strings.TrimPrefix(key, "gmail:"))
				case selection.Provider == "outlook" && strings.HasPrefix(key, "outlook:"):
					selection.RemoteID = strings.TrimSpace(strings.TrimPrefix(key, "outlook:"))
				default:
					return nil, storage.ErrContactSetupInvalid
				}
				selections = append(selections, selection)
			}
			return selections, nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	snapshot.edit.edit = snapshot.preview.SetupSnapshot()
	return snapshot, nil
}

func (s *UserAccountStore) ValidateContactSetup(ctx context.Context, snapshot *UserContactEditSnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.edit == nil {
		return storage.ErrContactEditChanged
	}
	return s.WithUser(ctx, snapshot.edit.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserContactSetup(ctx, snapshot.edit, s.contactEditGuard(ctx, snapshot))
	})
}

func (s *UserAccountStore) ContactSetupCandidates(ctx context.Context, snapshot *UserContactEditSnapshot, account, after string) (out []storage.StoredContactSetupCandidate, err error) {
	if snapshot == nil || snapshot.repository != s || snapshot.edit == nil {
		return nil, storage.ErrContactEditChanged
	}
	var selected *AccountServiceSnapshot
	for _, service := range snapshot.services {
		if service.AccountID() == account {
			selected = service
			break
		}
	}
	if selected == nil {
		return nil, storage.ErrAccountRoute
	}
	err = s.WithUser(ctx, snapshot.edit.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		out, err = db.ReadUserContactSetupCandidates(ctx, snapshot.edit, account, selected.ContactConfig().Provider, after, s.contactEditGuard(ctx, snapshot))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *UserAccountStore) ValidateContactPreview(ctx context.Context, snapshot *UserContactPreviewSnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.preview == nil || snapshot.edit == nil {
		return storage.ErrContactEditChanged
	}
	return s.WithUser(ctx, snapshot.preview.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserContactPreview(ctx, snapshot.preview, s.contactEditGuard(ctx, snapshot.edit))
	})
}

func (s *UserAccountStore) PublishContactPreview(ctx context.Context, snapshot *UserContactPreviewSnapshot, values []storage.ContactSetupCandidateValue) (result storage.ContactPreviewResult, err error) {
	if snapshot == nil || snapshot.repository != s || snapshot.preview == nil || snapshot.edit == nil {
		return result, storage.ErrContactEditChanged
	}
	err = s.WithUser(ctx, snapshot.preview.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		result, err = db.PublishUserContactPreview(ctx, snapshot.preview, values, s.contactEditGuard(ctx, snapshot.edit))
		return err
	})
	if err != nil {
		return storage.ContactPreviewResult{}, err
	}
	return result, nil
}
