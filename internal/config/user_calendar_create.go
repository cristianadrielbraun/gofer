package config

import (
	"context"
	"database/sql"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type UserCalendarCreateClaim struct {
	repository *UserAccountStore
	source     *UserCalendarSourceSnapshot
	claim      *storage.UserCalendarCreateClaim
}

func (c *UserCalendarCreateClaim) Source() *UserCalendarSourceSnapshot   { return c.source }
func (c *UserCalendarCreateClaim) Result() storage.CalendarCreateRequest { return c.claim.Result() }
func (c *UserCalendarCreateClaim) RequestID() string                     { return c.claim.RequestID() }

// Readable capabilities do not authorize a write. OAuth creates also require
// the exact write-purpose grant in every reservation/publication transaction.
func (s *UserAccountStore) calendarCreateGuard(ctx context.Context, source *UserCalendarSourceSnapshot, extra []func() error) (func(*sql.Tx, string) error, error) {
	guard, err := s.calendarSourceGuard(ctx, source, extra)
	if err != nil {
		return nil, err
	}
	if !calendarCreateSourceWritable(source.Source()) || (source.Source().Provider != storage.CalendarSourceProviderCalDAV && len(extra) != 1) {
		return nil, storage.ErrCalendarSourceChanged
	}
	return guard, nil
}

func (s *UserAccountStore) BeginCalendarCreate(ctx context.Context, source *UserCalendarSourceSnapshot, request, hash string, extra ...func() error) (*UserCalendarCreateClaim, error) {
	guard, err := s.calendarCreateGuard(ctx, source, extra)
	if err != nil {
		return nil, err
	}
	binding, err := calendarCreateBinding(source)
	if err != nil {
		return nil, err
	}
	claim := &UserCalendarCreateClaim{repository: s, source: source}
	err = s.WithAccountForUser(ctx, source.service.OwnerID(), source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		claim.claim, err = db.BeginUserCalendarCreate(ctx, source.source, request, hash, binding, guard)
		return err
	})
	if err != nil {
		return nil, err
	}
	return claim, nil
}

func (s *UserAccountStore) ValidateCalendarCreate(ctx context.Context, claim *UserCalendarCreateClaim, extra ...func() error) error {
	if claim == nil || claim.repository != s || claim.source == nil || claim.claim == nil {
		return storage.ErrCalendarCreateConflict
	}
	guard, err := s.calendarCreateGuard(ctx, claim.source, extra)
	if err != nil {
		return err
	}
	return s.WithAccountForUser(ctx, claim.source.service.OwnerID(), claim.source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.ValidateUserCalendarCreate(ctx, claim.claim, guard)
	})
}

func (s *UserAccountStore) PublishCalendarCreate(ctx context.Context, claim *UserCalendarCreateClaim, event storage.CalendarEvent, series bool, extra ...func() error) (storage.CalendarCreateRequest, error) {
	if claim == nil || claim.repository != s || claim.source == nil || claim.claim == nil {
		return storage.CalendarCreateRequest{}, storage.ErrCalendarCreateConflict
	}
	guard, err := s.calendarCreateGuard(ctx, claim.source, extra)
	if err != nil {
		return storage.CalendarCreateRequest{}, err
	}
	event = copyCalendarProviderEvent(event)
	var result storage.CalendarCreateRequest
	err = s.WithAccountForUser(ctx, claim.source.service.OwnerID(), claim.source.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		var err error
		result, err = db.PublishUserCalendarCreate(ctx, claim.claim, event, series, guard)
		return err
	})
	return result, err
}

func calendarCreateSourceWritable(source storage.CalendarSource) bool {
	role := strings.ToLower(strings.TrimSpace(source.AccessRole))
	switch source.Provider {
	case storage.CalendarSourceProviderCalDAV:
		return role == "" || role == "unknown" || role == "owner" || role == "writer"
	case "gmail", "outlook":
		return role == "owner" || role == "writer"
	default:
		return false
	}
}

func calendarCreateBinding(source *UserCalendarSourceSnapshot) ([32]byte, error) {
	return calendarCreateBindingFor(source.Source(), source.service)
}

func calendarCreateBindingFor(source storage.CalendarSource, service *AccountServiceSnapshot) ([32]byte, error) {
	identity, err := calendarReplySourceIdentity(source)
	if err != nil {
		return [32]byte{}, err
	}
	principal, err := calendarReplyPrincipal(service)
	if err != nil {
		return [32]byte{}, err
	}
	return serviceStateHash([]any{"user-calendar-create-v1", identity, principal})
}
