package handler

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Provider adapters read back the confirmed result before this local boundary.
// A cooldown limits further HTTP, not publication of an already accepted write.
// The exact write-purpose authorization is checked again inside the local TX.
func (p *userCalendarRequest) publishEvent(ctx context.Context, kind storage.CalendarEventPublicationKind, event storage.CalendarEvent) error {
	if p == nil || p.event == nil || !p.write {
		return storage.ErrCalendarEventChanged
	}
	if err := p.validate(ctx); err != nil {
		return err
	}
	var guards []func() error
	if p.provider() != storage.CalendarSourceProviderCalDAV {
		if p.authorization == nil {
			return mailauth.ErrMailboxAuthorizationChanged
		}
		service := p.service()
		guards = append(guards, func() error {
			return p.h.userCredentials.ValidateCalendarAuthorization(ctx, p.authorization, service.OwnerID(), service.AccountID(), true)
		})
	}
	if kind == storage.CalendarPublishResponse && p.response != nil {
		return p.h.userAccounts.PublishCalendarResponse(ctx, p.response, event, guards...)
	}
	return p.h.userAccounts.PublishCalendarEvent(ctx, p.event, kind, event, guards...)
}
