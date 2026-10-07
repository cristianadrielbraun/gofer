package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (p *userCalendarRequest) createWriteGuards(ctx context.Context) ([]func() error, error) {
	if p == nil || p.source == nil || !p.write {
		return nil, storage.ErrCalendarCreateConflict
	}
	if err := p.validate(ctx); err != nil {
		return nil, err
	}
	if p.provider() == storage.CalendarSourceProviderCalDAV {
		return nil, nil
	}
	if p.authorization == nil || p.h.userCredentials == nil {
		return nil, mailauth.ErrMailboxAuthorizationChanged
	}
	authorization, service := p.authorization, p.service()
	return []func() error{func() error {
		return p.h.userCredentials.ValidateCalendarAuthorization(ctx, authorization, service.OwnerID(), service.AccountID(), true)
	}}, nil
}

func (p *userCalendarRequest) beginCreate(ctx context.Context, request, hash string) (*config.UserCalendarCreateClaim, error) {
	guards, err := p.createWriteGuards(ctx)
	if err != nil {
		return nil, err
	}
	claim, err := p.h.userAccounts.BeginCalendarCreate(ctx, p.source, request, hash, guards...)
	if err != nil {
		return nil, err
	}
	p.create = claim
	return claim, nil
}

// Cooldowns constrain another native HTTP request, never the local publication
// of an already confirmed provider result. The exact grant and original durable
// reservation are revalidated in the owner's final writer transaction.
func (p *userCalendarRequest) publishCreate(ctx context.Context, event storage.CalendarEvent, series bool) (storage.CalendarCreateRequest, error) {
	if p == nil || p.create == nil {
		return storage.CalendarCreateRequest{}, storage.ErrCalendarCreateConflict
	}
	guards, err := p.createWriteGuards(ctx)
	if err != nil {
		return storage.CalendarCreateRequest{}, err
	}
	return p.h.userAccounts.PublishCalendarCreate(ctx, p.create, event, series, guards...)
}
