package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type userCalendarMutationFailure struct {
	status              int
	message             string
	uncertain, conflict bool
}

func (e userCalendarMutationFailure) Error() string { return e.message }

func userCalendarMutationError(w http.ResponseWriter, err error, uncertain bool) {
	var failure userCalendarMutationFailure
	if errors.As(err, &failure) {
		calendarUpdateFailure(w, failure.status, failure.message, failure.uncertain, failure.conflict)
		return
	}
	var form userCalendarFormFailure
	if errors.As(err, &form) {
		calendarUpdateFailure(w, form.status, form.message, false, false)
		return
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		calendarUpdateFailure(w, 404, "This event is no longer available. Refresh the calendar.", false, true)
	case errors.Is(err, storage.ErrCalendarEventChanged), errors.Is(err, storage.ErrCalendarSourceChanged), errors.Is(err, config.ErrAccountServicesChanged), errors.Is(err, mailauth.ErrMailboxAuthorizationChanged):
		calendarUpdateFailure(w, 409, "Calendar access or event details changed. Refresh the calendar and reopen the event.", false, true)
	case errors.Is(err, storage.ErrUserStoreOwner), errors.Is(err, storage.ErrAccountRoute):
		calendarUpdateFailure(w, 403, "This account is no longer available.", false, false)
	default:
		calendarUpdateFailure(w, 503, "Calendar is unavailable. Refresh it and check the event before trying again.", uncertain, false)
	}
}

func (h *Handler) handleUserDeleteCalendarEvent(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	versions := values["version"]
	scopes, scoped := values["scope"]
	series := scoped && len(scopes) == 1 && scopes[0] == "series"
	instance := scoped && len(scopes) == 1 && scopes[0] == "occurrence"
	seriesIDs := values["series_id"]
	validFields := (len(values) == 1 && !scoped) || (len(values) == 2 && instance) || (len(values) == 3 && series && len(seriesIDs) == 1 && seriesIDs[0] != "" && len(seriesIDs[0]) <= 4096)
	if err != nil || !validFields || len(versions) != 1 || strings.TrimSpace(versions[0]) == "" || len(versions[0]) > 2048 {
		calendarUpdateFailure(w, 400, "Event version is missing or invalid. Reopen the event.", false, false)
		return
	}
	owner, version := h.userID(r.Context()), versions[0]
	// Detach a browser cancellation, then join the managed worker's root below.
	// Shutdown/account deletion still stops native work and final publication.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	var response map[string]any
	attempt := &userCalendarResponseAttempt{}
	err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, owner, r.PathValue("id"))
		if err != nil {
			return err
		}
		p := &userCalendarRequest{h: h, event: snapshot, writeAttempt: attempt}
		event, source := snapshot.Event(), snapshot.Source()
		if (series || instance) != calendarEventIsSeries(event) || (instance && event.SeriesRemoteID == "") {
			return userCalendarMutationFailure{status: 403, message: "Choose This event or Entire series and confirm before deleting a recurring event."}
		}
		if series && seriesIDs[0] != calendarSeriesID(event) {
			return userCalendarMutationFailure{status: 409, message: "The series changed. Reopen its confirmation before deleting.", conflict: true}
		}
		if !series && event.ETag != version {
			return userCalendarMutationFailure{status: 409, message: "This event changed. Refresh the calendar and reopen it before deleting.", conflict: true}
		}
		if err := p.mutationAccess(ctx, true); err != nil {
			return err
		}
		err = h.userIMAP.RunAccountService(ctx, owner, source.AccountID, mail.AccountServiceCalendar, calendarSourceTimeout, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, source)
			if err != nil {
				return err
			}
			defer unlock()
			if err := p.mutationAccess(operation, true); err != nil {
				return err
			}
			operation, err = p.actionContext(operation, true)
			if err != nil {
				return err
			}
			if series {
				credentials, err := p.actionCredentials()
				if err != nil {
					return err
				}
				event, err = readCalendarProviderSeriesWithCredentials(operation, source, snapshot.Event(), credentials, true)
				if err != nil || event.ETag != version {
					return userCalendarMutationFailure{status: 409, message: "The series could not be verified or has changed. Reopen it before deleting; nothing was deleted.", conflict: true}
				}
			}
			resourceVersion, err := h.deleteCalendarProviderEvent(operation, source, event, series, instance)
			if err != nil {
				if attempt.uncertain {
					return userCalendarMutationFailure{status: 502, message: "The deletion could not be confirmed. Refresh the calendar and reopen the event before trying again.", uncertain: true}
				}
				var provider calendarCreateProviderError
				switch {
				case errors.Is(err, errCalendarUpdateConflict):
					return userCalendarMutationFailure{status: 409, message: "This event changed at the provider. Refresh the calendar and reopen it; nothing was deleted.", conflict: true}
				case errors.Is(err, errCalendarUpdateUnsupported):
					return userCalendarMutationFailure{status: 403, message: calendarDeleteReason(err.Error()) + " Refresh the calendar and reopen the event.", conflict: true}
				case errors.As(err, &provider) && (provider.Status == 404 || provider.Status == 410):
					return userCalendarMutationFailure{status: 409, message: "This event is no longer available at the provider. Refresh the calendar.", conflict: true}
				default:
					uncertain := attempt.uncertain
					message := "Could not delete the event. Check Calendar write access and try again."
					if uncertain {
						message = "The deletion could not be confirmed. Refresh the calendar and reopen the event before trying again."
					}
					return userCalendarMutationFailure{status: 502, message: message, uncertain: uncertain}
				}
			}
			kind := storage.CalendarPublishDelete
			if series {
				kind = storage.CalendarPublishSeriesDelete
			} else if instance {
				kind = storage.CalendarPublishOccurrenceDelete
				if resourceVersion != "" {
					event.ETag = resourceVersion
				}
			}
			if err := p.publishEvent(operation, kind, event); err != nil {
				return userCalendarMutationFailure{status: 503, message: "The provider deleted the event, but the local update could not finish. Refresh the calendar.", uncertain: true}
			}
			response = map[string]any{"deleted": true, "event_id": snapshot.Event().ID, "source_id": source.ID}
			if series {
				response["series_id"] = event.RemoteID
			}
			if instance {
				response["scope"] = "occurrence"
			}
			return nil
		})
		if err != nil {
			return err
		}
		h.userIMAP.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: owner, AccountID: source.AccountID, Payload: map[string]any{"source_id": source.ID, "event_id": snapshot.Event().ID}})
		return nil
	})
	if err != nil {
		userCalendarMutationError(w, err, attempt.uncertain)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(response)
}
