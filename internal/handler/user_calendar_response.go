package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (p *userCalendarRequest) responseWriteGuards(ctx context.Context) ([]func() error, error) {
	if p == nil || p.event == nil || !p.write {
		return nil, storage.ErrCalendarEventChanged
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

func (p *userCalendarRequest) reserveResponse(ctx context.Context, target calendarResponseTarget, response string) (*config.UserCalendarResponseClaim, error) {
	guards, err := p.responseWriteGuards(ctx)
	if err != nil {
		return nil, err
	}
	existing := p.event.Event()
	if !calendarResponseScopeValid(existing, target.Scope) || target.Event.Deleted || calendarResponseStatus(target.Event.ResponseStatus) == "" || target.Event.ETag == "" {
		return nil, storage.ErrCalendarEventChanged
	}
	remote := existing.RemoteID
	if target.Scope == "series" {
		remote = calendarSeriesID(existing)
	}
	if target.Event.RemoteID != remote || (target.OccurrenceVersion != "" && target.OccurrenceVersion != existing.ETag) {
		return nil, storage.ErrCalendarEventChanged
	}
	if target.Scope != "series" && target.Event.SeriesRemoteID != existing.SeriesRemoteID {
		return nil, storage.ErrCalendarEventChanged
	}
	return p.h.userAccounts.ReserveCalendarResponse(ctx, p.event, target.Scope, target.Event.ETag, response, guards...)
}

// The current operation authorization may have advanced through its own bounded
// 401 refresh. A reconnect does not update this private authorization and fails
// validation instead of letting a stale operation release a replacement claim.
func (p *userCalendarRequest) releaseResponse(ctx context.Context, claim *config.UserCalendarResponseClaim) error {
	guards, err := p.responseWriteGuards(ctx)
	if err != nil {
		return err
	}
	if err := p.h.userAccounts.ReleaseCalendarResponse(ctx, claim, guards...); err != nil {
		return err
	}
	if p.response == claim {
		p.response = nil
	}
	return nil
}
