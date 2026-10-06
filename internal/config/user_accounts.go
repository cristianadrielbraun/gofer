package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// UserAccountStore adapts the existing account repository to explicit leased
// user stores. It is not enabled by main yet. Instance transport policy remains
// central, while mailbox configuration and encrypted passwords have one local
// authoritative copy. OAuth credentials and workers require separate wiring.
type UserAccountStore struct {
	routing *storage.AccountRouting
	base    *AccountStore
}

func NewUserAccountStore(routing *storage.AccountRouting, secretKey []byte) (*UserAccountStore, error) {
	if routing == nil {
		return nil, errors.New("account routing is required")
	}
	base, err := NewAccountStore(routing.System(), secretKey)
	if err != nil {
		return nil, err
	}
	return &UserAccountStore{routing: routing, base: base}, nil
}

func (s *UserAccountStore) local(db *storage.DB) *AccountStore {
	return &AccountStore{db: db, policyDB: s.routing.System(), aes: s.base.aes}
}

// WithUser/WithAccountForUser follow a centrally authenticated request identity.
// WithAccount resolves trusted worker IDs from the central directory. Callbacks
// must not retain the AccountStore or invoke network operations while leased.
func (s *UserAccountStore) WithUser(ctx context.Context, userID string, fn func(*AccountStore, *storage.DB) error) error {
	return s.routing.WithUser(ctx, userID, func(db *storage.DB) error { return fn(s.local(db), db) })
}

func (s *UserAccountStore) WithAccountForUser(ctx context.Context, userID, accountID string, fn func(*AccountStore, *storage.DB) error) error {
	return s.routing.WithAccountForUser(ctx, userID, accountID, func(db *storage.DB) error { return fn(s.local(db), db) })
}

func (s *UserAccountStore) WithAccount(ctx context.Context, accountID string, fn func(*AccountStore, *storage.DB, string) error) error {
	return s.routing.WithAccount(ctx, accountID, func(db *storage.DB, userID string) error { return fn(s.local(db), db, userID) })
}

// PendingAccountCreationError carries the durable reservation ID for recovery.
// A failed response does not mean the account row was rolled back across files.
type PendingAccountCreationError struct {
	AccountID string
	Err       error
}

func (e *PendingAccountCreationError) Error() string {
	return fmt.Sprintf("account %s creation remains pending: %v", e.AccountID, e.Err)
}

func (e *PendingAccountCreationError) Unwrap() error { return e.Err }

func (s *UserAccountStore) CreateAccount(ctx context.Context, userID string, req *models.CreateAccountRequest) (*models.Account, error) {
	if req == nil {
		return nil, errors.New("account request is required")
	}
	// Validate transport policy before reserving an ID. Do not acquire another
	// user's cache slot while holding a lease: release validation before creation.
	copy := *req
	if err := s.WithUser(ctx, userID, func(local *AccountStore, _ *storage.DB) error {
		return local.prepareAccountCreation(ctx, &copy)
	}); err != nil {
		return nil, err
	}
	entry, err := s.routing.ReserveAccount(ctx, userID)
	if err != nil {
		return nil, err
	}
	var account *models.Account
	err = s.routing.CompleteAccountCreation(ctx, userID, entry.AccountID, func(db *storage.DB) error {
		var err error
		account, err = s.local(db).createAccountWithID(ctx, userID, entry.AccountID, &copy, false)
		return err
	})
	if err != nil {
		return nil, &PendingAccountCreationError{AccountID: entry.AccountID, Err: err}
	}
	if account == nil {
		// A recovery operation can finish a reservation before this caller gets
		// its transition. Load the committed result instead of returning nil.
		err := s.WithAccountForUser(ctx, userID, entry.AccountID, func(local *AccountStore, _ *storage.DB) error {
			var err error
			account, err = local.GetAccountByIDForUser(ctx, userID, entry.AccountID)
			if err == nil && account == nil {
				return storage.ErrAccountRoute
			}
			return err
		})
		if err != nil {
			return nil, &PendingAccountCreationError{AccountID: entry.AccountID, Err: err}
		}
	}
	return account, nil
}

// DeleteAccount drains routed callbacks and leaves durable retry state on any
// failure. cleanup must stop provider workers and remove blobs/central OAuth
// credentials without retaining a database lease. It is mandatory even when an
// earlier attempt already removed the local account row, and must be idempotent
// because retries/concurrent deletion requests can invoke it again.
func (s *UserAccountStore) DeleteAccount(ctx context.Context, userID, accountID string, cleanup func(context.Context, string) error) error {
	if cleanup == nil {
		return errors.New("external account cleanup is required")
	}
	if err := s.routing.BeginAccountDeletion(ctx, userID, accountID); err != nil {
		return err
	}
	if err := cleanup(ctx, accountID); err != nil {
		return err
	}
	return s.routing.CompleteAccountDeletion(ctx, userID, accountID, func(db *storage.DB) error {
		local := s.local(db)
		if err := local.MarkAccountDeleting(ctx, accountID); err != nil {
			return err
		}
		return local.DeleteAccount(ctx, accountID)
	})
}
