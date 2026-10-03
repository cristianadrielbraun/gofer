package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type calendarCredentials struct {
	token    string
	baseURL  string
	username string
	password string
	err      error
}

var errCalendarAccountNotConfigured = errors.New("no calendars are configured for this account")

func (h *Handler) syncCalendarWindow(ctx context.Context, userID string, start, end time.Time) (int, error) {
	return h.syncCalendarProviderWindow(ctx, userID, start, end, "")
}

func (h *Handler) syncCalendarProviderWindow(ctx context.Context, userID string, start, end time.Time, provider string) (int, error) {
	return h.syncCalendarScopedWindow(ctx, userID, start, end, provider, "")
}

func (h *Handler) syncCalendarScopedWindow(ctx context.Context, userID string, start, end time.Time, provider, accountID string) (int, error) {
	sources, err := h.db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list selected calendars: %w", err)
	}
	credentials := make(map[string]calendarCredentials)
	total := 0
	matched := accountID == ""
	var failures []error
	for _, source := range sources {
		if (provider != "" && source.Provider != provider) || (accountID != "" && source.AccountID != accountID) {
			continue
		}
		matched = true
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, err := h.syncCalendarSource(ctx, source, start, end, false, credentials)
		total += count
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", source.Name, err))
		}
	}
	if !matched {
		return 0, errCalendarAccountNotConfigured
	}
	if len(failures) > 1 {
		return total, fmt.Errorf("%w (and %d more errors)", failures[0], len(failures)-1)
	}
	if len(failures) == 1 {
		return total, failures[0]
	}
	return total, nil
}

func (h *Handler) fetchCalendarSource(ctx context.Context, source storage.CalendarSource, query calendar.EventQuery, credentials map[string]calendarCredentials) (calendar.EventPage, error) {
	if h.calendarFetchEvents != nil {
		return h.calendarFetchEvents(ctx, source, query)
	}
	key := source.Provider + ":" + source.AccountID
	credential, ok := credentials[key]
	if !ok {
		credential = h.calendarCredentialsForSource(ctx, source.UserID, source)
		credentials[key] = credential
	}
	if credential.err != nil {
		return calendar.EventPage{}, credential.err
	}
	switch source.Provider {
	case providers.ProviderGmail:
		return listGoogleCalendarEvents(ctx, credential.token, source.RemoteID, query)
	case providers.ProviderOutlook:
		return listOutlookCalendarEvents(ctx, credential.token, source.RemoteID, query)
	case storage.CalendarSourceProviderCalDAV:
		if _, err := resolveCalDAVHref(credential.baseURL, source.RemoteID); err != nil {
			return calendar.EventPage{}, err
		}
		return listCalDAVCalendarEvents(ctx, source, credential.username, credential.password, query)
	default:
		return calendar.EventPage{}, fmt.Errorf("this calendar provider is not supported")
	}
}

func (h *Handler) calendarCredentialsForSource(ctx context.Context, userID string, source storage.CalendarSource) calendarCredentials {
	var result calendarCredentials
	switch source.Provider {
	case providers.ProviderGmail:
		if h.mailCredentials() == nil {
			result.err = fmt.Errorf("Google authorization is not configured")
		} else {
			result.token, result.err = h.mailCredentials().GetGoogleCalendarTokenForAccount(ctx, source.AccountID)
		}
	case providers.ProviderOutlook:
		if h.mailCredentials() == nil {
			result.err = fmt.Errorf("Microsoft authorization is not configured")
		} else {
			result.token, result.err = h.mailCredentials().GetMicrosoftGraphCalendarTokenForAccount(ctx, source.AccountID)
		}
	case storage.CalendarSourceProviderCalDAV:
		if h.accountStore == nil {
			result.err = fmt.Errorf("CalDAV account settings are not available")
			return result
		}
		cfg, err := h.accountStore.GetCalendarSyncConfig(ctx, userID, source.AccountID)
		if err != nil {
			result.err = err
			return result
		}
		if cfg.Provider != providers.ProviderIMAP {
			result.err = fmt.Errorf("CalDAV is not configured for this account")
			return result
		}
		result.baseURL, result.err = normalizeCalDAVBaseURL(cfg.CalDAVBaseURL)
		if result.err != nil {
			return result
		}
		result.username = strings.TrimSpace(cfg.CalDAVUsername)
		if cfg.CalDAVUseAccountCredentials {
			result.err = h.db.Read().QueryRowContext(ctx, `
				SELECT COALESCE(NULLIF(username, ''), email_address)
				FROM accounts WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, source.AccountID, userID).Scan(&result.username)
			if result.err == nil {
				result.password, result.err = h.accountStore.DecryptPassword(ctx, source.AccountID)
			}
		} else {
			result.password, result.err = h.accountStore.CalDAVPassword(ctx, userID, source.AccountID)
		}
		if result.err == nil && (strings.TrimSpace(result.username) == "" || result.password == "") {
			result.err = fmt.Errorf("complete Calendar authentication in account setup")
		}
	default:
		result.err = fmt.Errorf("this calendar provider is not supported")
	}
	return result
}

func calendarStorageEvent(userID, sourceID string, remote calendar.RemoteEvent) storage.CalendarEvent {
	return storage.CalendarEvent{
		UserID: userID, SourceID: sourceID, RemoteID: remote.RemoteID,
		ICalUID: remote.ICalUID, SeriesRemoteID: remote.SeriesRemoteID, ETag: remote.ETag,
		Status: remote.Status, Summary: remote.Summary, Description: remote.Description, Location: remote.Location,
		OrganizerName: remote.OrganizerName, OrganizerEmail: remote.OrganizerEmail,
		ResponseStatus: remote.ResponseStatus,
		AllDay:         remote.AllDay, StartDate: remote.StartDate, EndDate: remote.EndDate,
		StartAt: remote.StartAt, EndAt: remote.EndAt, StartTimeZone: remote.StartTimeZone, EndTimeZone: remote.EndTimeZone,
		RecurrenceJSON: string(remote.Recurrence), AttendeesJSON: string(remote.Attendees), OnlineMeetingJSON: string(remote.OnlineMeeting),
		HTMLLink: remote.HTMLLink, ProviderCreatedAt: remote.ProviderCreatedAt, ProviderUpdatedAt: remote.ProviderUpdatedAt,
		IsDeleted: remote.Deleted,
	}
}
