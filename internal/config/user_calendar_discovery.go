package config

import (
	"context"
	"database/sql"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserCalendarDiscoverySnapshot struct {
	repository *UserAccountStore
	discovery  *storage.CalendarDiscoverySnapshot
	service    *AccountServiceSnapshot
}

func (s *UserCalendarDiscoverySnapshot) Service() *AccountServiceSnapshot { return s.service }
func (s *UserCalendarDiscoverySnapshot) Sources() []storage.CalendarSource {
	return s.discovery.Sources()
}
func (s *UserCalendarDiscoverySnapshot) Provider() string { return s.discovery.Provider() }

func (s *UserAccountStore) SnapshotCalendarDiscovery(ctx context.Context, owner, id string) (*UserCalendarDiscoverySnapshot, error) {
	snapshot := &UserCalendarDiscoverySnapshot{repository: s}
	err := s.WithAccountForUser(ctx, owner, id, func(_ *AccountStore, db *storage.DB) error {
		var err error
		snapshot.discovery, err = db.SnapshotUserCalendarDiscovery(ctx, owner, id, s.calendarControlGuard(ctx, owner), func(tx *sql.Tx) error {
			var err error
			snapshot.service, err = s.serviceSnapshot(ctx, tx, owner, id)
			return err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *UserAccountStore) checkCalendarDiscovery(snapshot *UserCalendarDiscoverySnapshot) error {
	if snapshot == nil || snapshot.repository != s || snapshot.discovery == nil || snapshot.service == nil {
		return storage.ErrCalendarDiscoveryChanged
	}
	return s.checkServiceSnapshot(snapshot.service)
}

func (s *UserAccountStore) calendarDiscoveryGuard(ctx context.Context, snapshot *UserCalendarDiscoverySnapshot) func(*sql.Tx, string) error {
	return func(tx *sql.Tx, id string) error {
		if id != snapshot.service.AccountID() {
			return storage.ErrAccountRoute
		}
		return s.serviceGuard(ctx, snapshot.service, false)(tx)
	}
}

func (s *UserAccountStore) ValidateCalendarDiscovery(ctx context.Context, snapshot *UserCalendarDiscoverySnapshot) error {
	if err := s.checkCalendarDiscovery(snapshot); err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, snapshot.service.OwnerID(), snapshot.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarDiscovery(ctx, snapshot.discovery, s.calendarDiscoveryGuard(ctx, snapshot))
	})
}

type DiscoveredCalDAVSettings struct {
	BaseURL, Username, Password string
	UseAccountCredentials       bool
}

// A nil DAV value keeps OAuth account configuration untouched. DAV discovery
// saves its tested endpoint/credentials with the entire source catalog.
// Publication guards must be short central checks, without a local lease or
// provider request; they run after the local writer wait and before any writes.
func (s *UserAccountStore) PublishCalendarDiscovery(ctx context.Context, snapshot *UserCalendarDiscoverySnapshot, sources []storage.CalendarSource, dav *DiscoveredCalDAVSettings, publicationGuards ...func() error) (*UserCalendarDiscoverySnapshot, error) {
	if err := s.checkCalendarDiscovery(snapshot); err != nil {
		return nil, err
	}
	if (snapshot.Provider() == storage.CalendarSourceProviderCalDAV) != (dav != nil) {
		return nil, storage.ErrCalendarDiscoveryInvalid
	}
	if len(publicationGuards) > 1 || (len(publicationGuards) == 1 && publicationGuards[0] == nil) {
		return nil, storage.ErrCalendarDiscoveryInvalid
	}
	sources = append([]storage.CalendarSource(nil), sources...)
	var savedDAV DiscoveredCalDAVSettings
	if dav != nil {
		savedDAV = *dav
	}
	next := &UserCalendarDiscoverySnapshot{repository: s}
	owner, id := snapshot.service.OwnerID(), snapshot.service.AccountID()
	err := s.WithAccountForUser(ctx, owner, id, func(local *AccountStore, db *storage.DB) error {
		var before func(*sql.Tx) error
		if dav != nil {
			before = func(tx *sql.Tx) error {
				return local.saveCalDAVConfigTx(ctx, tx, owner, id, savedDAV.BaseURL, savedDAV.Username, savedDAV.Password, savedDAV.UseAccountCredentials)
			}
		}
		var err error
		guard := func(tx *sql.Tx, account string) error {
			if err := s.calendarDiscoveryGuard(ctx, snapshot)(tx, account); err != nil {
				return err
			}
			if len(publicationGuards) == 1 {
				return publicationGuards[0]()
			}
			return nil
		}
		next.discovery, err = db.PublishUserCalendarDiscovery(ctx, snapshot.discovery, sources, guard, before, func(tx *sql.Tx) error {
			var err error
			next.service, err = s.serviceSnapshot(ctx, tx, owner, id)
			return err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}
