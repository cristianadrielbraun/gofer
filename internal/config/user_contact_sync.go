package config

import (
	"context"
	"database/sql"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// These wrappers bind copied queue/profile/card state to the routed repository.
// No local DB or AccountStore survives a short lease or cache eviction.
type UserContactSyncClaim struct {
	repository *UserAccountStore
	claim      *storage.ContactSyncClaim
}

func (c *UserContactSyncClaim) OwnerID() string     { return c.claim.OwnerID() }
func (c *UserContactSyncClaim) OperationID() string { return c.claim.OperationID() }
func (c *UserContactSyncClaim) ContactID() string   { return c.claim.ContactID() }
func (c *UserContactSyncClaim) AttemptCount() int   { return c.claim.AttemptCount() }

type UserContactSyncProfile struct {
	claim   *UserContactSyncClaim
	profile *storage.ContactSyncProfile
}

func (p *UserContactSyncProfile) Contact() models.Contact              { return p.profile.Contact() }
func (p *UserContactSyncProfile) Targets() []storage.ContactSyncTarget { return p.profile.Targets() }

type UserContactSyncDestination struct {
	profile     *UserContactSyncProfile
	services    *AccountServiceSnapshot
	destination *storage.ContactSyncDestination
}

func (d *UserContactSyncDestination) Source() *storage.ContactSource { return d.destination.Source() }

func (s *UserAccountStore) contactSyncOwnerGuard(ctx context.Context, owner string) func(*sql.Tx) error {
	return func(*sql.Tx) error { return s.routing.ValidateUser(ctx, owner) }
}

func (s *UserAccountStore) checkContactSyncClaim(c *UserContactSyncClaim) error {
	if c == nil || c.repository != s || c.claim == nil || c.claim.OwnerID() == "" {
		return storage.ErrContactSyncSuperseded
	}
	return nil
}

func (s *UserAccountStore) EnqueueContactSyncForUser(ctx context.Context, owner, profile string) (id string, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		var enqueueErr error
		id, enqueueErr = db.EnqueueUserContactSync(ctx, owner, profile, s.contactSyncOwnerGuard(ctx, owner))
		return enqueueErr
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *UserAccountStore) ClaimContactSync(ctx context.Context, owner string, limit int, lockTimeout time.Duration) (claims []*UserContactSyncClaim, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		copied, err := db.ClaimUserContactSyncOperations(ctx, owner, limit, lockTimeout, s.contactSyncOwnerGuard(ctx, owner))
		if err != nil {
			return err
		}
		for _, claim := range copied {
			claims = append(claims, &UserContactSyncClaim{repository: s, claim: claim})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *UserAccountStore) SnapshotContactSync(ctx context.Context, claim *UserContactSyncClaim) (profile *UserContactSyncProfile, err error) {
	if err := s.checkContactSyncClaim(claim); err != nil {
		return nil, err
	}
	err = s.WithUser(ctx, claim.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		copied, err := db.SnapshotUserContactSyncProfile(ctx, claim.claim, s.contactSyncOwnerGuard(ctx, claim.OwnerID()))
		if err != nil {
			return err
		}
		if copied != nil {
			profile = &UserContactSyncProfile{claim: claim, profile: copied}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return profile, nil
}

func (s *UserAccountStore) StartContactSync(ctx context.Context, profile *UserContactSyncProfile) error {
	if profile == nil || profile.profile == nil {
		return storage.ErrContactSyncSuperseded
	}
	if err := s.checkContactSyncClaim(profile.claim); err != nil {
		return err
	}
	return s.WithUser(ctx, profile.claim.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		return db.StartUserContactSyncOperation(ctx, profile.profile, s.contactSyncOwnerGuard(ctx, profile.claim.OwnerID()))
	})
}

func (s *UserAccountStore) CancelContactSync(ctx context.Context, claim *UserContactSyncClaim) error {
	if err := s.checkContactSyncClaim(claim); err != nil {
		return err
	}
	return s.WithUser(ctx, claim.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		return db.CancelUserContactSyncOperation(ctx, claim.claim, s.contactSyncOwnerGuard(ctx, claim.OwnerID()))
	})
}

func (s *UserAccountStore) SnapshotContactSyncDestination(ctx context.Context, profile *UserContactSyncProfile, services *AccountServiceSnapshot, book string) (destination *UserContactSyncDestination, err error) {
	if profile == nil || profile.profile == nil {
		return nil, storage.ErrContactSyncSuperseded
	}
	if err := s.checkContactSyncClaim(profile.claim); err != nil {
		return nil, err
	}
	if err := s.checkServiceSnapshot(services); err != nil {
		return nil, err
	}
	if services.owner != profile.claim.OwnerID() {
		return nil, storage.ErrAccountRoute
	}
	err = s.WithAccountForUser(ctx, services.owner, services.id, func(_ *AccountStore, db *storage.DB) error {
		copied, err := db.SnapshotUserContactSyncDestination(ctx, profile.profile, services.id, services.ContactConfig().Provider, book, s.serviceGuard(ctx, services, true))
		if err != nil {
			return err
		}
		destination = &UserContactSyncDestination{profile: profile, services: services, destination: copied}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return destination, nil
}

func (s *UserAccountStore) checkContactSyncDestination(d *UserContactSyncDestination) error {
	if d == nil || d.profile == nil || d.profile.profile == nil || d.destination == nil {
		return storage.ErrContactSyncSuperseded
	}
	if err := s.checkContactSyncClaim(d.profile.claim); err != nil {
		return err
	}
	if err := s.checkServiceSnapshot(d.services); err != nil {
		return err
	}
	if d.services.owner != d.profile.claim.OwnerID() {
		return storage.ErrAccountRoute
	}
	return nil
}

func (s *UserAccountStore) ValidateContactSyncDestination(ctx context.Context, d *UserContactSyncDestination) error {
	if err := s.checkContactSyncDestination(d); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, d.services.owner, d.services.id, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserContactSyncDestination(ctx, d.destination, s.serviceGuard(ctx, d.services, true))
	})
}

func (s *UserAccountStore) PublishContactSyncResult(ctx context.Context, d *UserContactSyncDestination, remoteID, etag string) error {
	if err := s.checkContactSyncDestination(d); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, d.services.owner, d.services.id, func(_ *AccountStore, db *storage.DB) error {
		return db.PublishUserContactSyncResult(ctx, d.destination, remoteID, etag, s.serviceGuard(ctx, d.services, true))
	})
}

func (s *UserAccountStore) FinishContactSync(ctx context.Context, claim *UserContactSyncClaim, status, message string, retryAt time.Time) error {
	if err := s.checkContactSyncClaim(claim); err != nil {
		return err
	}
	return s.WithUser(ctx, claim.OwnerID(), func(_ *AccountStore, db *storage.DB) error {
		return db.FinishUserContactSyncOperation(ctx, claim.claim, status, message, retryAt, s.contactSyncOwnerGuard(ctx, claim.OwnerID()))
	})
}

func (s *UserAccountStore) NextContactSyncAttempt(ctx context.Context, owner string, lockTimeout time.Duration) (next time.Time, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		next, err = db.NextUserContactSyncAttempt(ctx, owner, lockTimeout)
		return err
	})
	return next, err
}

func (s *UserAccountStore) ValidateContactSyncRemote(ctx context.Context, d *UserContactSyncDestination, remoteID string) error {
	if err := s.checkContactSyncDestination(d); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, d.services.owner, d.services.id, func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserContactSyncRemote(ctx, d.destination, remoteID, s.serviceGuard(ctx, d.services, true))
	})
}

func (s *UserAccountStore) PublishContactSyncVersion(ctx context.Context, d *UserContactSyncDestination, remoteID, etag string) error {
	if err := s.checkContactSyncDestination(d); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, d.services.owner, d.services.id, func(_ *AccountStore, db *storage.DB) error {
		return db.PublishUserContactSyncVersion(ctx, d.destination, remoteID, etag, s.serviceGuard(ctx, d.services, true))
	})
}
