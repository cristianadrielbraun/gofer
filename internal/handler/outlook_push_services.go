package handler

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// OutlookServiceChanged refreshes the calendars or contacts of the Outlook
// accounts Microsoft announced changes for, now rather than at their next
// scheduled check. Graph says only that something changed, so the usual
// sync finds what. A calendar change also reads the account's calendar list
// again, at most once a minute, since the event may be in a calendar Gofer
// doesn't know yet.
func (h *Handler) OutlookServiceChanged(ctx context.Context, service mail.OutlookPushService, accountIDs []string) {
	for _, accountID := range accountIDs {
		var err error
		switch {
		case h.userStorage != nil && service == mail.OutlookPushCalendar:
			err = h.wakeUserCalendarAccountID(ctx, accountID)
		case h.userStorage != nil && service == mail.OutlookPushContacts:
			err = h.wakeUserContactAccountID(ctx, accountID)
		case service == mail.OutlookPushCalendar:
			err = h.scheduleOutlookCalendarsNow(ctx, accountID)
		case service == mail.OutlookPushContacts:
			h.syncOutlookContactsNow(accountID)
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("outlook webhook account=%s %s: refresh: %v", accountID, service, err)
		}
		if service == mail.OutlookPushCalendar && ctx.Err() == nil && h.calendarListDue(accountID, time.Now(), true) {
			h.refreshOutlookCalendarList(ctx, accountID)
		}
	}
}

// refreshOutlookCalendarList reads the calendar list of the account a
// calendar notification was for.
func (h *Handler) refreshOutlookCalendarList(ctx context.Context, accountID string) {
	if h.userStorage == nil {
		userID, err := h.db.GetAccountUserID(ctx, accountID)
		if err != nil {
			log.Printf("outlook webhook account=%s calendar: find owner: %v", accountID, err)
			return
		}
		h.refreshCalendarLists(ctx, userID, accountID)
		return
	}
	var owner string
	if err := h.userStorage.WithAccount(ctx, accountID, func(_ *storage.DB, user string) error { owner = user; return nil }); err != nil {
		log.Printf("outlook webhook account=%s calendar: find owner: %v", accountID, err)
		return
	}
	h.refreshUserCalendarLists(ctx, owner, accountID)
}

// scheduleOutlookCalendarsNow makes the shared store's calendar worker
// refresh the account's selected Outlook calendars on its next tick.
func (h *Handler) scheduleOutlookCalendarsNow(ctx context.Context, accountID string) error {
	userID, err := h.db.GetAccountUserID(ctx, accountID)
	if err != nil {
		return err
	}
	sources, err := h.db.ListCalendarSourcesForAccount(ctx, userID, accountID)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, source := range sources {
		if source.Provider != providers.ProviderOutlook || !source.IsSelected || source.IsDeleted {
			continue
		}
		if err := h.db.ScheduleCalendarSync(ctx, userID, source.ID, now); err != nil {
			return err
		}
	}
	return nil
}

// syncOutlookContactsNow pulls the shared store account's contacts, unless a
// pull is already running.
func (h *Handler) syncOutlookContactsNow(accountID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := h.SyncContactAccount(ctx, accountID); err != nil && !errors.Is(err, errContactSyncAlreadyRunning) {
			log.Printf("outlook webhook account=%s contacts: refresh: %v", accountID, err)
		}
	}()
}
