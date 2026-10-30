package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Calendar lists are read again in the background, so calendars removed on
// the provider go away and new ones show up, unticked, without Refresh
// calendars. Reading a list is one request per account, so it can follow the
// calendars' own refreshes closely.
const (
	calendarListInterval = 5 * time.Minute
	// calendarListGoneInterval spaces the reads a calendar the provider no
	// longer finds asks for, in case it still lists it: one list request, as
	// often as the quick checks that find it gone.
	calendarListGoneInterval = calendarQuickCheckInterval
)

// calendarListDue reports whether the account's calendar list is to be read
// again now: soon after one of its calendars was not found, or five minutes
// after the last read. An account first seen waits its interval, so a restart
// doesn't read every list at once.
func (h *Handler) calendarListDue(accountID string, now time.Time, gone bool) bool {
	last, seen := h.calendarListRead.LoadOrStore(accountID, now)
	if !seen {
		return gone
	}
	wait := calendarListInterval
	if gone {
		wait = calendarListGoneInterval
	}
	if now.Sub(last.(time.Time)) < wait {
		return false
	}
	h.calendarListRead.Store(accountID, now)
	return true
}

// calendarSourceGone reports whether a calendar's provider said the calendar
// itself isn't there: Graph and Google answer 404 or 410 for a deleted one,
// and a CalDAV server 404 for a calendar collection.
func calendarSourceGone(err error) bool {
	gone := func(status int) bool { return status == http.StatusNotFound || status == http.StatusGone }
	var outlook outlookAPIError
	var google googleAPIError
	var dav calDAVDiscoveryError
	var provider calendarCreateProviderError
	return (errors.As(err, &outlook) && gone(outlook.Status)) || (errors.As(err, &google) && gone(google.Status)) ||
		(errors.As(err, &dav) && gone(dav.status)) || (errors.As(err, &provider) && gone(provider.Status))
}

// refreshCalendarList reads the calendar list of a shared-store account
// again, with the login its calendar source uses. Calendars the provider no
// longer lists are removed, new ones are added unticked, and the others keep
// their choice. A list that comes back empty is ignored: a provider that
// suddenly lists nothing is more likely failing than emptied.
func (h *Handler) refreshCalendarList(ctx context.Context, source storage.CalendarSource, credentials map[string]calendarCredentials) error {
	credential := h.calendarCredentialsCached(ctx, source, credentials)
	if credential.err != nil {
		return credential.err
	}
	var sources []storage.CalendarSource
	var err error
	switch source.Provider {
	case providers.ProviderGmail, providers.ProviderOutlook:
		var calendars []calendar.RemoteCalendar
		if source.Provider == providers.ProviderGmail {
			calendars, err = discoverGoogleCalendars(ctx, credential.token)
		} else {
			calendars, err = discoverOutlookCalendars(ctx, credential.token)
		}
		sources = calendarDiscoveredSources(calendars)
	case storage.CalendarSourceProviderJMAP:
		var calendars []jmap.Calendar
		calendars, err = mail.JMAPCalendars(ctx, credential.jmap, credential.password)
		sources = jmapCalendarSources(source.UserID, source.AccountID, calendars)
	case storage.CalendarSourceProviderCalDAV:
		sources, err = discoverCalDAVCalendars(ctx, credential.baseURL, credential.username, credential.password, source.UserID, source.AccountID, nil)
	default:
		return nil
	}
	if err != nil || len(sources) == 0 {
		return err
	}
	for i := range sources {
		sources[i].UserID, sources[i].AccountID, sources[i].Provider = source.UserID, source.AccountID, source.Provider
		sources[i].IsSelected = false
	}
	if err := h.db.ReplaceCalendarSources(ctx, source.UserID, source.AccountID, source.Provider, sources); err != nil {
		return err
	}
	if h.syncer != nil {
		// An open Calendar redraws its list of calendars.
		h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: source.UserID, AccountID: source.AccountID, Payload: map[string]any{"sources": true}})
	}
	return nil
}

// calendarListSources is one calendar of each of the owner's accounts that
// has calendars, synced or not, or of accountID alone when it isn't empty:
// what a list refresh starts from.
func calendarListSources(ctx context.Context, db *storage.DB, owner, accountID string) ([]storage.CalendarSource, error) {
	selected, err := db.ListSelectedCalendarSources(ctx, owner)
	if err != nil {
		return nil, err
	}
	others, err := db.ListUnselectedCalendarSources(ctx, owner)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []storage.CalendarSource
	for _, source := range append(selected, others...) {
		if seen[source.AccountID] || (accountID != "" && source.AccountID != accountID) {
			continue
		}
		seen[source.AccountID] = true
		out = append(out, source)
	}
	return out, nil
}

// refreshUserCalendarLists reads the calendar lists of a per-user owner's
// accounts again, as the Calendar refresh button asks, so calendars added or
// removed on the provider show up without waiting.
func (h *Handler) refreshUserCalendarLists(ctx context.Context, owner, accountID string) {
	var sources []storage.CalendarSource
	if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		sources, err = calendarListSources(ctx, db, owner, accountID)
		return err
	}); err != nil {
		log.Printf("owned calendar: list calendars to refresh: %v", err)
		return
	}
	for _, source := range sources {
		h.calendarListRead.Store(source.AccountID, time.Now())
		if _, err := h.runUserCalendarDiscovery(ctx, owner, source.AccountID, nil, nil, true); err != nil && ctx.Err() == nil {
			log.Printf("owned calendar: refresh calendar list %s: %v", source.AccountID, err)
		}
	}
}

// refreshCalendarLists is refreshUserCalendarLists for the shared store.
func (h *Handler) refreshCalendarLists(ctx context.Context, userID, accountID string) {
	sources, err := calendarListSources(ctx, h.db, userID, accountID)
	if err != nil {
		log.Printf("calendar: list calendars to refresh: %v", err)
		return
	}
	credentials := make(map[string]calendarCredentials)
	for _, source := range sources {
		h.calendarListRead.Store(source.AccountID, time.Now())
		if err := h.refreshCalendarList(ctx, source, credentials); err != nil && ctx.Err() == nil {
			log.Printf("calendar list %s: %v", source.AccountID, err)
		}
	}
}
