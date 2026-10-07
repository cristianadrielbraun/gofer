package config

import (
	"context"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Publications copy all provider timestamp pointers before a cache/writer wait.
// OAuth callers must supply their exact central write-grant guard; DAV authority
// is already in the private service/source snapshot. No provider call occurs
// inside the transaction.
func (s *UserAccountStore) PublishCalendarEvent(ctx context.Context, snapshot *UserCalendarEventSnapshot, kind storage.CalendarEventPublicationKind, event storage.CalendarEvent, extra ...func() error) error {
	guard, err := s.calendarEventGuard(ctx, snapshot, extra)
	if err != nil {
		return err
	}
	if snapshot.Source().Provider != storage.CalendarSourceProviderCalDAV && len(extra) != 1 {
		return storage.ErrCalendarEventChanged
	}
	event = copyCalendarProviderEvent(event)
	return s.WithAccountForUser(ctx, snapshot.service.OwnerID(), snapshot.service.AccountID(), func(_ *AccountStore, db *storage.DB) error {
		return db.PublishUserCalendarEvent(ctx, snapshot.event, kind, event, guard)
	})
}

func copyCalendarProviderEvent(event storage.CalendarEvent) storage.CalendarEvent {
	for _, at := range []**time.Time{&event.StartAt, &event.EndAt, &event.ProviderCreatedAt, &event.ProviderUpdatedAt} {
		if *at != nil {
			value := **at
			*at = &value
		}
	}
	return event
}
