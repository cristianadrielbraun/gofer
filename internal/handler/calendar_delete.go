package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func calendarDeleteReason(reason string) string {
	return strings.NewReplacer("edited", "deleted", "editing", "deleting", "cannot edit yet", "cannot delete yet", "Edit it", "Delete it").Replace(reason)
}

func (h *Handler) calendarEventDeleteAccess(ctx context.Context, event storage.CalendarEvent) (storage.CalendarSource, string, error) {
	source, reason, err := h.calendarEventMutationAccess(ctx, event, h.calendarDeleteEvent != nil, calendarEventIsSeries(event))
	return source, calendarDeleteReason(reason), err
}

func (h *Handler) deleteCalendarProviderEvent(ctx context.Context, source storage.CalendarSource, event storage.CalendarEvent, series bool) error {
	if h.calendarDeleteEvent != nil {
		return h.calendarDeleteEvent(ctx, source, event)
	}
	credentials := calendarCredentials{}
	switch source.Provider {
	case providers.ProviderGmail:
		credentials.token, credentials.err = h.mailCredentials().GetGoogleCalendarWriteTokenForAccount(ctx, source.AccountID)
	case providers.ProviderOutlook:
		credentials.token, credentials.err = h.mailCredentials().GetMicrosoftGraphCalendarWriteTokenForAccount(ctx, source.AccountID)
	case storage.CalendarSourceProviderCalDAV:
		credentials = h.calendarCredentialsForSource(ctx, source.UserID, source)
	default:
		return errCalendarUpdateUnsupported
	}
	if credentials.err != nil {
		return calendarCreateAuthError{credentials.err}
	}
	switch source.Provider {
	case providers.ProviderGmail:
		return deleteGoogleCalendarEventScope(ctx, credentials.token, source.RemoteID, event, series)
	case providers.ProviderOutlook:
		return deleteOutlookCalendarEventScope(ctx, credentials.token, source.RemoteID, event, series)
	default:
		if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
			return calendarDeletePreflightError{err}
		}
		return deleteCalDAVCalendarEventScope(ctx, source, credentials.username, credentials.password, event, series)
	}
}

// Load the master lazily when opening the confirmation. Event details remain
// cache-first, and the existing popover/dialog/backdrop stay mounted.
func (h *Handler) handleCalendarSeriesDeleteConfirmation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	event, err := h.db.GetCalendarEvent(ctx, h.userID(ctx), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "This series is no longer available. Refresh the calendar.", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Could not load this event.", http.StatusInternalServerError)
		return
	}
	if !calendarEventIsSeries(event) {
		http.Error(w, "This event is not a recurring series.", http.StatusConflict)
		return
	}
	source, reason, err := h.calendarEventDeleteAccess(ctx, event)
	if err != nil || reason != "" {
		if reason == "" {
			reason = "Calendar write access could not be verified."
		}
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	master, err := h.readCalendarProviderSeries(ctx, source, event)
	if err != nil {
		message := "Could not load the series. Close and reopen this confirmation to try again."
		if errors.Is(err, errCalendarUpdateUnsupported) {
			message = calendarDeleteReason(err.Error())
		}
		http.Error(w, message, http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	details := views.CalendarEventDetails{Event: calendarViewEvent(master), DeleteSeries: true, CanDelete: true, DeleteVersion: master.ETag, DeleteSeriesID: master.RemoteID}
	if err := views.CalendarEventDeleteConfirmation(details).Render(ctx, w); err != nil {
		http.Error(w, "Could not open the confirmation.", http.StatusInternalServerError)
	}
}

func (h *Handler) handleDeleteCalendarEvent(w http.ResponseWriter, r *http.Request) {
	// HTMX sends DELETE parameters in the URL. Accept exactly one version;
	// never turn a missing/wildcard version into an unconditional provider write.
	values, err := url.ParseQuery(r.URL.RawQuery)
	versions := values["version"]
	scopes, scoped := values["scope"]
	series := scoped && len(scopes) == 1 && scopes[0] == "series"
	seriesIDs := values["series_id"]
	validFields := (len(values) == 1 && !scoped) || (len(values) == 3 && series && len(seriesIDs) == 1 && seriesIDs[0] != "" && len(seriesIDs[0]) <= 4096)
	if err != nil || !validFields || len(versions) != 1 || strings.TrimSpace(versions[0]) == "" || len(versions[0]) > 2048 {
		calendarUpdateFailure(w, http.StatusBadRequest, "Event version is missing or invalid. Reopen the event.", false, false)
		return
	}
	version := versions[0]
	userID := h.userID(r.Context())
	event, err := h.db.GetCalendarEvent(r.Context(), userID, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		calendarUpdateFailure(w, http.StatusNotFound, "This event is no longer available. Refresh the calendar.", false, true)
		return
	}
	if err != nil {
		calendarUpdateFailure(w, http.StatusInternalServerError, "Could not load this event.", false, false)
		return
	}
	if series != calendarEventIsSeries(event) {
		calendarUpdateFailure(w, http.StatusForbidden, "Use Delete series and confirm all occurrences to delete a recurring event.", false, false)
		return
	}
	if series && seriesIDs[0] != calendarSeriesID(event) {
		calendarUpdateFailure(w, http.StatusConflict, "The series changed. Reopen its confirmation before deleting.", false, true)
		return
	}
	// Serialize with edits, creation and background reconciliation. Finish an
	// accepted request even if the user closes the dialog or loses connectivity.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	unlock, err := h.lockCalendarCreate(ctx, storage.CalendarSource{ID: event.SourceID, UserID: userID})
	if err != nil {
		calendarUpdateFailure(w, http.StatusServiceUnavailable, "Calendar is still refreshing. Try again.", false, false)
		return
	}
	defer unlock()
	event, err = h.db.GetCalendarEvent(ctx, userID, event.ID)
	if err != nil || (!series && event.ETag != version) || series != calendarEventIsSeries(event) || (series && seriesIDs[0] != calendarSeriesID(event)) {
		calendarUpdateFailure(w, http.StatusConflict, "This event changed. Refresh the calendar and reopen it before deleting.", false, true)
		return
	}
	source, reason, err := h.calendarEventDeleteAccess(ctx, event)
	if err != nil || reason != "" {
		if reason == "" {
			reason = "The calendar configuration changed. Reopen the event."
		}
		calendarUpdateFailure(w, http.StatusForbidden, reason, false, false)
		return
	}
	occurrence := event
	if series {
		event, err = h.readCalendarProviderSeries(ctx, source, occurrence)
		if err != nil || event.ETag != version {
			calendarUpdateFailure(w, http.StatusConflict, "The series could not be verified or has changed. Reopen it before deleting; nothing was deleted.", false, true)
			return
		}
	}
	if err := h.deleteCalendarProviderEvent(ctx, source, event, series); err != nil {
		var provider calendarCreateProviderError
		var preflight calendarDeletePreflightError
		switch {
		case errors.Is(err, errCalendarUpdateConflict):
			calendarUpdateFailure(w, http.StatusConflict, "This event changed at the provider. Refresh the calendar and reopen it; nothing was deleted.", false, true)
		case errors.Is(err, errCalendarUpdateUnsupported):
			calendarUpdateFailure(w, http.StatusForbidden, calendarDeleteReason(err.Error())+" Refresh the calendar and reopen the event.", false, true)
		case errors.As(err, &provider) && (provider.Status == 404 || provider.Status == 410):
			calendarUpdateFailure(w, http.StatusConflict, "This event is no longer available at the provider. Refresh the calendar.", false, true)
		default:
			uncertain := !errors.As(err, &preflight) && calendarCreateUncertain(err)
			message := "Could not delete the event. Check Calendar write access and try again."
			if uncertain {
				message = "The deletion could not be confirmed. Refresh the calendar and reopen the event before trying again."
			}
			calendarUpdateFailure(w, http.StatusBadGateway, message, uncertain, false)
		}
		return
	}
	if series {
		err = h.db.CompleteCalendarSeriesDelete(ctx, userID, occurrence.ID, source.ID, occurrence.ETag, event)
	} else {
		err = h.db.CompleteCalendarDelete(ctx, userID, event.ID, source.ID, version)
	}
	if err != nil {
		calendarUpdateFailure(w, http.StatusServiceUnavailable, "The provider deleted the event, but the local update could not finish. Refresh the calendar.", true, false)
		return
	}
	if h.syncer != nil {
		h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: userID, Payload: map[string]any{"source_id": source.ID, "event_id": event.ID}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	response := map[string]any{"deleted": true, "event_id": event.ID, "source_id": source.ID}
	if series {
		response["series_id"] = event.RemoteID
	}
	_ = json.NewEncoder(w).Encode(response)
}
