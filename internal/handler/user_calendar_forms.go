package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

type userCalendarFormFailure struct {
	status  int
	message string
}

func (e userCalendarFormFailure) Error() string { return e.message }

func userCalendarFormError(w http.ResponseWriter, r *http.Request, err error) {
	var failure userCalendarFormFailure
	if errors.As(err, &failure) {
		http.Error(w, failure.message, failure.status)
		return
	}
	if errors.Is(err, errCalendarUpdateUnsupported) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, mailauth.ErrMailboxAuthorizationChanged) {
		http.Error(w, "Calendar write access changed. Reopen the event.", http.StatusConflict)
		return
	}
	userCalendarEventError(w, r, err)
}

func (p *userCalendarRequest) mutationAccess(ctx context.Context, deleting bool) error {
	event, source := p.event.Event(), p.event.Source()
	reason := calendarEventEditRestriction(event)
	if deleting {
		reason = ""
		if calendarStoredEditableOnline(event) {
			reason = "Online meetings cannot be deleted in Gofer yet."
		}
	}
	if reason == "" {
		reason = calendarEventMutationRestriction(event, source, calendarEventIsSeries(event))
	}
	if deleting {
		reason = calendarDeleteReason(reason)
	}
	if reason == "" && !p.h.userCalendarWriteAuthorized(ctx, source) {
		reason = "Reconnect this account from Accounts to grant Calendar write access."
	}
	if reason != "" {
		return userCalendarFormFailure{http.StatusForbidden, reason}
	}
	return p.validate(ctx)
}

// Credentials come exclusively from the copied account service. The dispatch
// boundary independently validates them and the selected source at each HTTP.
func (p *userCalendarRequest) actionCredentials() (calendarCredentials, error) {
	credentials := calendarCredentials{token: p.token}
	if p.provider() != storage.CalendarSourceProviderCalDAV {
		return credentials, nil
	}
	base, username, useAccount := p.service().CalDAVSettings()
	base, err := normalizeCalDAVBaseURL(base)
	if err != nil {
		return credentials, err
	}
	if _, err := resolveCalDAVHref(base, p.actionSource().RemoteID); err != nil {
		return credentials, err
	}
	if useAccount {
		username = strings.TrimSpace(p.service().Identity().Username)
		if username == "" {
			username = p.service().Identity().EmailAddress
		}
	}
	password, err := p.service().CalDAVPassword("", useAccount)
	if err != nil {
		return credentials, err
	}
	if strings.TrimSpace(username) == "" || password == "" {
		return credentials, errors.New("complete Calendar authentication in account setup")
	}
	credentials.baseURL, credentials.username, credentials.password = base, username, password
	return credentials, nil
}

// A master read joins the bounded account service and the owner/source flight.
// It retains no repository lease across the provider request or UI rendering.
func (p *userCalendarRequest) readSeries(ctx context.Context, deleting bool) (master storage.CalendarEvent, err error) {
	source := p.event.Source()
	err = p.h.userIMAP.RunAccountService(ctx, source.UserID, source.AccountID, mail.AccountServiceCalendar, 30*time.Second, func(operation context.Context) error {
		unlock, err := p.h.lockCalendarCreate(operation, source)
		if err != nil {
			return err
		}
		defer unlock()
		if err := p.mutationAccess(operation, deleting); err != nil {
			return err
		}
		operation, err = p.actionContext(operation, true)
		if err != nil {
			return err
		}
		credentials, err := p.actionCredentials()
		if err != nil {
			return err
		}
		master, err = readCalendarProviderSeriesWithCredentials(operation, source, p.event.Event(), credentials, deleting)
		if err != nil {
			return err
		}
		return p.validate(operation)
	})
	return master, err
}

func (h *Handler) handleUserEditCalendarEvent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	var data views.CalendarCreateData
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(ctx), r.PathValue("id"))
		if err != nil {
			return err
		}
		p := &userCalendarRequest{h: h, event: snapshot}
		if err := p.mutationAccess(ctx, false); err != nil {
			return err
		}
		event, source := snapshot.Event(), snapshot.Source()
		scopes := r.URL.Query()["scope"]
		occurrence := len(scopes) == 1 && scopes[0] == "occurrence"
		series := calendarEventIsSeries(event) && len(scopes) == 1 && scopes[0] == "series"
		if (len(scopes) != 0 && !series && !occurrence) || (occurrence && event.SeriesRemoteID == "") || (calendarEventIsSeries(event) && !series && !occurrence) {
			return userCalendarFormFailure{http.StatusBadRequest, "Choose This event or Entire series to edit a recurring event."}
		}
		if occurrence {
			if err := calendarOccurrenceExistingRestriction(event); err != nil {
				return userCalendarFormFailure{http.StatusConflict, err.Error()}
			}
		}
		var repeat *calendar.RecurrenceDraft
		if series {
			event, err = p.readSeries(ctx, false)
			if err != nil {
				return err
			}
			draft, _ := calendarSeriesDraft(calendar.RemoteEvent{AllDay: event.AllDay, StartDate: event.StartDate, EndDate: event.EndDate, StartAt: event.StartAt, EndAt: event.EndAt, StartTimeZone: event.StartTimeZone, Recurrence: json.RawMessage(event.RecurrenceJSON)})
			repeat = draft.Recurrence
		}
		var location *time.Location
		if err := h.userAccounts.WithAccountForUser(ctx, source.UserID, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			location = viewsCalendarLocation(db.GetUISettings(ctx, source.UserID))
			return nil
		}); err != nil {
			return err
		}
		identity := snapshot.Service().Identity()
		name := identity.DisplayName
		if name == "" {
			name = identity.EmailAddress
		}
		data, err = calendarEditData(event, source, name, location, series, occurrence, repeat)
		if err != nil {
			return userCalendarFormFailure{http.StatusConflict, err.Error()}
		}
		return p.mutationAccess(ctx, false)
	})
	if err != nil {
		userCalendarFormError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.CalendarCreateDialog(data).Render(r.Context(), w); err != nil {
		http.Error(w, "Could not open the event editor.", http.StatusInternalServerError)
	}
}

func (h *Handler) handleUserCalendarSeriesDeleteConfirmation(w http.ResponseWriter, r *http.Request) {
	h.renderUserCalendarDeleteConfirmation(w, r, true)
}

func (h *Handler) handleUserCalendarOccurrenceDeleteConfirmation(w http.ResponseWriter, r *http.Request) {
	h.renderUserCalendarDeleteConfirmation(w, r, false)
}

func (h *Handler) renderUserCalendarDeleteConfirmation(w http.ResponseWriter, r *http.Request, series bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	var details views.CalendarEventDetails
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(ctx), r.PathValue("id"))
		if err != nil {
			return err
		}
		p := &userCalendarRequest{h: h, event: snapshot}
		event := snapshot.Event()
		if series && !calendarEventIsSeries(event) {
			return userCalendarFormFailure{http.StatusConflict, "This event is not a recurring series."}
		}
		if err := p.mutationAccess(ctx, true); err != nil {
			return err
		}
		if !series {
			if err := calendarOccurrenceExistingRestriction(event); err != nil {
				return userCalendarFormFailure{http.StatusForbidden, "This occurrence cannot be deleted. Refresh the calendar and check its write access."}
			}
			details = views.CalendarEventDetails{Event: calendarViewEvent(event), HasOccurrence: true, CanDelete: true, DeleteOccurrence: true, DeleteVersion: event.ETag}
		} else {
			master, err := p.readSeries(ctx, true)
			if err != nil {
				return err
			}
			details = views.CalendarEventDetails{Event: calendarViewEvent(master), HasOccurrence: event.SeriesRemoteID != "", DeleteSeries: true, CanDelete: true, DeleteVersion: master.ETag, DeleteSeriesID: master.RemoteID}
		}
		return p.mutationAccess(ctx, true)
	})
	if err != nil {
		userCalendarFormError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.CalendarEventDeleteConfirmation(details).Render(r.Context(), w); err != nil {
		http.Error(w, "Could not open the confirmation.", http.StatusInternalServerError)
	}
}
