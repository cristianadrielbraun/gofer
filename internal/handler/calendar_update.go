package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
	"github.com/google/uuid"
)

func calendarStoredEditRestriction(event storage.CalendarEvent) string {
	if calendarStoredEditableOnline(event) {
		event.OnlineMeetingJSON = "{}"
	}
	if reason := calendarUpdateRestriction(calendar.RemoteEvent{
		Status: event.Status, Deleted: event.IsDeleted, SeriesRemoteID: event.SeriesRemoteID,
		Recurrence: json.RawMessage(event.RecurrenceJSON), Attendees: json.RawMessage(event.AttendeesJSON),
		OnlineMeeting:  json.RawMessage(event.OnlineMeetingJSON),
		ResponseStatus: calendarStoredOrganizerStatus(event),
	}); reason != "" {
		return reason
	}
	if strings.TrimSpace(event.ETag) == "" {
		return "Refresh this calendar before editing: the event's version is not available."
	}
	return ""
}

func (h *Handler) calendarEventEditAccess(ctx context.Context, event storage.CalendarEvent) (storage.CalendarSource, string, error) {
	if calendarStoredEditableOnline(event) && !calendarCachedOrganizer(event) {
		return storage.CalendarSource{}, "Only the organizer can edit an online meeting.", nil
	}
	if calendarStoredEditableOnline(event) && calendarEventIsSeries(event) {
		return storage.CalendarSource{}, "Recurring online meetings cannot be edited in Gofer yet.", nil
	}
	return h.calendarEventMutationAccess(ctx, event, h.calendarUpdateEvent != nil, calendarEventIsSeries(event))
}

func (h *Handler) calendarEventMutationAccess(ctx context.Context, event storage.CalendarEvent, injectedWriter bool, seriesScope ...bool) (storage.CalendarSource, string, error) {
	series := len(seriesScope) == 1 && seriesScope[0]
	if series {
		// Occurrences do not carry the master's version or repeat rule. Both are
		// fetched and verified before opening the editor and before writing.
		event.SeriesRemoteID, event.RecurrenceJSON, event.ETag = "", "[]", `"master-read-required"`
	}
	sources, err := h.db.ListSelectedCalendarSources(ctx, event.UserID)
	if err != nil {
		return storage.CalendarSource{}, "", err
	}
	for _, source := range sources {
		if source.ID != event.SourceID {
			continue
		}
		if !calendarSourceWritable(source) {
			return source, "This calendar is read-only.", nil
		}
		if series && calendarUpdateHasDetails(json.RawMessage(event.AttendeesJSON)) {
			return source, "Recurring meetings with guests cannot be edited yet.", nil
		}
		if reason := calendarStoredEditRestriction(event); reason != "" {
			return source, reason, nil
		}
		if source.Provider != providers.ProviderOutlook && !calendarUpdateValidETag(event.ETag, false) {
			return source, "This provider must supply a strong event version before safe editing is available.", nil
		}
		if source.Provider == storage.CalendarSourceProviderCalDAV && (event.OrganizerName != "" || event.OrganizerEmail != "") && !calendarCachedOrganizer(event) {
			return source, "Invitations cannot be edited in Gofer yet.", nil
		}
		if !injectedWriter && !h.calendarWriteAuthorized(ctx, source) {
			return source, "Reconnect this account from Accounts to grant Calendar write access.", nil
		}
		return source, "", nil
	}
	return storage.CalendarSource{}, "This calendar is no longer configured.", sql.ErrNoRows
}

func (h *Handler) handleEditCalendarEvent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	userID := h.userID(r.Context())
	event, err := h.db.GetCalendarEvent(r.Context(), userID, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Could not load this event.", http.StatusInternalServerError)
		return
	}
	source, reason, err := h.calendarEventEditAccess(r.Context(), event)
	if err != nil {
		http.Error(w, "Could not verify the calendar.", http.StatusConflict)
		return
	}
	if reason != "" {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	scopes := r.URL.Query()["scope"]
	occurrence := len(scopes) == 1 && scopes[0] == "occurrence"
	series := calendarEventIsSeries(event) && len(scopes) == 1 && scopes[0] == "series"
	if (len(scopes) != 0 && !series && !occurrence) || (occurrence && event.SeriesRemoteID == "") || (calendarEventIsSeries(event) && !series && !occurrence) {
		http.Error(w, "Choose This event or Entire series to edit a recurring event.", http.StatusBadRequest)
		return
	}
	if occurrence {
		if err := calendarOccurrenceExistingRestriction(event); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	var repeat *calendar.RecurrenceDraft
	if series {
		if r.URL.Query().Get("scope") != "series" {
			http.Error(w, "Choose Edit series to change all occurrences.", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		event, err = h.readCalendarProviderSeries(ctx, source, event)
		if err != nil {
			message := "Could not load the series. Refresh the calendar and try again."
			if errors.Is(err, errCalendarUpdateUnsupported) {
				message = err.Error()
			}
			http.Error(w, message, http.StatusConflict)
			return
		}
		draft, _ := calendarSeriesDraft(calendar.RemoteEvent{AllDay: event.AllDay, StartDate: event.StartDate, EndDate: event.EndDate, StartAt: event.StartAt, EndAt: event.EndAt, StartTimeZone: event.StartTimeZone, Recurrence: json.RawMessage(event.RecurrenceJSON)})
		repeat = draft.Recurrence
	}
	accounts, err := h.db.GetAccounts(r.Context(), userID)
	if err != nil {
		http.Error(w, "Could not load the calendar account.", http.StatusInternalServerError)
		return
	}
	accountName := ""
	for _, account := range accounts {
		if account.ID == source.AccountID {
			accountName = account.Name
			if accountName == "" {
				accountName = account.Email
			}
		}
	}
	location := viewsCalendarLocation(h.db.GetUISettings(r.Context(), userID))
	if zone, err := time.LoadLocation(event.StartTimeZone); err == nil && event.StartTimeZone != "" && event.StartTimeZone != "Local" {
		location = zone
	}
	if location.String() == "Local" {
		location = time.UTC
	}
	data := views.CalendarCreateData{
		EditSeries: series, EditOccurrence: occurrence, Recurrence: repeat,
		EventID: event.ID, Version: event.ETag, RequestID: uuid.NewString(), SourceID: source.ID,
		Sources: []views.CalendarCreateSource{{ID: source.ID, Name: source.Name, AccountName: accountName, Writable: true, Authorized: true, Provider: source.Provider}},
		Summary: event.Summary, Description: calendarDescriptionText(event.Description), DescriptionHTML: calendar.DescriptionHTML(event.Description), Location: event.Location,
		AllDay: event.AllDay, TimeZone: location.String(), StartTime: "09:00", EndTime: "10:00",
	}
	data.Guests = calendarMeetingGuestText(event)
	if calendarStoredOutlookOnline(event) {
		data.TeamsState = "other-online"
		if calendarOutlookTeamsJSON(calendarOutlookCachedMeetingJSON(event)) {
			data.TeamsState = "existing"
		}
	} else if calendarStoredGoogleMeet(event) {
		data.TeamsState = "existing"
		data.MeetingJoinURL = calendar.MeetingJoinURL(event.OnlineMeetingJSON)
	} else if series || occurrence {
		data.TeamsState = "recurring"
	}
	if event.AllDay {
		end, err := time.Parse("2006-01-02", event.EndDate)
		if err != nil {
			http.Error(w, "This event's dates are unavailable. Refresh the calendar.", http.StatusConflict)
			return
		}
		data.Date, data.EndDate = event.StartDate, end.AddDate(0, 0, -1).Format("2006-01-02")
	} else if event.StartAt != nil && event.EndAt != nil {
		start, end := event.StartAt.In(location), event.EndAt.In(location)
		data.Date, data.EndDate = start.Format("2006-01-02"), end.Format("2006-01-02")
		data.StartTime, data.EndTime = start.Format("15:04"), end.Format("15:04")
	} else {
		http.Error(w, "This event's times are unavailable. Refresh the calendar.", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.CalendarCreateDialog(data).Render(r.Context(), w); err != nil {
		http.Error(w, "Could not open the event editor.", http.StatusInternalServerError)
	}
}

func parseCalendarUpdateDraft(r *http.Request) (calendar.EventDraft, string, error) {
	if err := r.ParseForm(); err != nil {
		return calendar.EventDraft{}, "", fmt.Errorf("event details could not be read")
	}
	versions := r.PostForm["version"]
	if len(versions) != 1 || strings.TrimSpace(versions[0]) == "" || len(versions[0]) > 2048 {
		return calendar.EventDraft{}, "", fmt.Errorf("event version is missing; reopen the event")
	}
	if scopes, exists := r.PostForm["edit_scope"]; exists && (len(scopes) != 1 || (scopes[0] != "series" && scopes[0] != "occurrence")) {
		return calendar.EventDraft{}, "", fmt.Errorf("choose a valid edit scope")
	}
	// Reuse creation validation without allowing its callers extra fields.
	copy := r.Clone(r.Context())
	copy.PostForm = make(url.Values, len(r.PostForm))
	for name, values := range r.PostForm {
		if name != "version" && name != "edit_scope" {
			copy.PostForm[name] = values
		}
	}
	copy.Form = copy.PostForm // Only body fields, never query-string overrides.
	draft, err := parseCalendarEventDraft(copy)
	return draft, versions[0], err
}

func calendarUpdateFailure(w http.ResponseWriter, status int, message string, uncertain, conflict bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message, "uncertain": uncertain, "conflict": conflict})
}

func (h *Handler) updateCalendarProviderEvent(ctx context.Context, source storage.CalendarSource, event storage.CalendarEvent, draft calendar.EventDraft, series bool, occurrenceScope ...bool) (calendar.RemoteEvent, error) {
	occurrence := calendarOccurrenceScope(occurrenceScope)
	if h.calendarUpdateEvent != nil {
		return h.calendarUpdateEvent(ctx, source, event, draft)
	}
	credentials := h.calendarUpdateCredentials(ctx, source)
	if credentials.err != nil {
		return calendar.RemoteEvent{}, calendarCreateAuthError{credentials.err}
	}
	switch source.Provider {
	case providers.ProviderGmail:
		return updateGoogleCalendarEventScope(ctx, credentials.token, source.RemoteID, event, draft, series, occurrence)
	case providers.ProviderOutlook:
		return updateOutlookCalendarEventScope(ctx, credentials.token, source.RemoteID, event, draft, series, occurrence)
	default:
		if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
			return calendar.RemoteEvent{}, err
		}
		if occurrence {
			return updateCalDAVCalendarOccurrence(ctx, source, credentials.username, credentials.password, event, &draft)
		}
		if len(draft.Guests) > 0 || event.OrganizerEmail != "" || calendarUpdateHasDetails(json.RawMessage(event.AttendeesJSON)) {
			return h.updateCalDAVMeeting(ctx, source, credentials, event, draft)
		}
		return updateCalDAVCalendarEventScope(ctx, source, credentials.username, credentials.password, event, draft, series)
	}
}

func (h *Handler) calendarUpdateCredentials(ctx context.Context, source storage.CalendarSource) calendarCredentials {
	credentials := calendarCredentials{}
	if (source.Provider == providers.ProviderGmail || source.Provider == providers.ProviderOutlook) && h.mailCredentials() == nil {
		credentials.err = fmt.Errorf("Calendar OAuth is not configured")
		return credentials
	}
	switch source.Provider {
	case providers.ProviderGmail:
		credentials.token, credentials.err = h.mailCredentials().GetGoogleCalendarWriteTokenForAccount(ctx, source.AccountID)
	case providers.ProviderOutlook:
		credentials.token, credentials.err = h.mailCredentials().GetMicrosoftGraphCalendarWriteTokenForAccount(ctx, source.AccountID)
	case storage.CalendarSourceProviderCalDAV:
		credentials = h.calendarCredentialsForSource(ctx, source.UserID, source)
	default:
		credentials.err = errCalendarUpdateUnsupported
	}
	return credentials
}

func (h *Handler) handleUpdateCalendarEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 384<<10) // Bounded HTML + plain fallback, including URL-encoding overhead.
	draft, version, err := parseCalendarUpdateDraft(r)
	if err != nil {
		calendarUpdateFailure(w, http.StatusBadRequest, err.Error(), false, false)
		return
	}
	if draft.TeamsDraftID != "" {
		calendarUpdateFailure(w, 400, "Prepared Teams links can only be used for new events.", false, false)
		return
	}
	userID := h.userID(r.Context())
	event, err := h.db.GetCalendarEvent(r.Context(), userID, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		calendarUpdateFailure(w, http.StatusNotFound, "This event is no longer available.", false, true)
		return
	}
	if err != nil {
		calendarUpdateFailure(w, http.StatusInternalServerError, "Could not load this event.", false, false)
		return
	}
	if draft.GoogleMeetMeeting && event.SourceProvider != providers.ProviderGmail {
		calendarUpdateFailure(w, 400, "Google Meet meetings require a supported Google calendar.", false, false)
		return
	}
	if draft.TeamsMeeting && event.SourceProvider != providers.ProviderOutlook {
		calendarUpdateFailure(w, 400, "Teams meetings require a supported Outlook calendar.", false, false)
		return
	}
	if r.PostForm.Get("source_id") != event.SourceID {
		calendarUpdateFailure(w, http.StatusBadRequest, "Editing cannot move an event to another calendar.", false, false)
		return
	}
	series := r.PostForm.Get("edit_scope") == "series"
	instance := r.PostForm.Get("edit_scope") == "occurrence"
	if (series || instance) != calendarEventIsSeries(event) || (series && draft.Recurrence == nil) || (instance && (event.SeriesRemoteID == "" || draft.Recurrence != nil)) {
		calendarUpdateFailure(w, http.StatusBadRequest, "Choose This event or Entire series. Only Entire series can change repeat settings.", false, false)
		return
	}
	// The shared source gate serializes writes with cache reconciliation. Finish
	// an accepted write even if the browser disconnects; a stale retry conflicts.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	unlock, err := h.lockCalendarCreate(ctx, storage.CalendarSource{ID: event.SourceID, UserID: userID})
	if err != nil {
		calendarUpdateFailure(w, http.StatusServiceUnavailable, "Calendar is still refreshing. Try again.", false, false)
		return
	}
	defer unlock()
	event, err = h.db.GetCalendarEvent(ctx, userID, event.ID)
	if err != nil || (!series && event.ETag != version) || (series || instance) != calendarEventIsSeries(event) || (instance && event.SeriesRemoteID == "") {
		calendarUpdateFailure(w, http.StatusConflict, "This event changed. Refresh the calendar and reopen it before saving.", false, true)
		return
	}
	source, reason, err := h.calendarEventEditAccess(ctx, event)
	if err != nil || reason != "" {
		if reason == "" {
			reason = "The calendar configuration changed. Reopen the event."
		}
		calendarUpdateFailure(w, http.StatusForbidden, reason, false, false)
		return
	}
	occurrence := event
	if draft.Recurrence != nil && calendarUpdateHasDetails(json.RawMessage(event.AttendeesJSON)) {
		calendarUpdateFailure(w, 400, "Recurring meetings with guests are not supported yet.", false, false)
		return
	}
	draft.OrganizerEmail, draft.OrganizerName = event.OrganizerEmail, event.OrganizerName
	if calendarCachedOrganizer(event) {
		event.ResponseStatus = "organizer"
	}
	if series {
		event, err = h.readCalendarProviderSeries(ctx, source, occurrence)
		if err != nil || event.ETag != version {
			message := "The series could not be loaded or has changed. Reopen it before saving."
			if errors.Is(err, errCalendarUpdateUnsupported) {
				message = err.Error()
			}
			calendarUpdateFailure(w, http.StatusConflict, message, false, true)
			return
		}
	}
	// Pickers show minutes; retain hidden seconds when the user did not change
	// that endpoint, instead of moving an imported appointment on a title edit.
	if !event.AllDay && !draft.AllDay {
		if event.StartAt != nil && event.StartAt.Truncate(time.Minute).Equal(*draft.StartAt) {
			draft.StartAt = event.StartAt
		}
		if event.EndAt != nil && event.EndAt.Truncate(time.Minute).Equal(*draft.EndAt) {
			draft.EndAt = event.EndAt
		}
	}
	if err := h.attachCalendarMeetDraft(ctx, source, &draft, "event:"+event.ID); err != nil {
		calendarUpdateFailure(w, 409, "The Google Meet link could not be loaded. Toggle Google Meet off and on to prepare it again.", false, false)
		return
	}
	remote, err := h.updateCalendarProviderEvent(ctx, source, event, draft, series, instance)
	if err != nil {
		var provider calendarCreateProviderError
		var preflight calendarDeletePreflightError
		var configuration calendarReplyConfigurationError
		switch {
		case errors.As(err, &configuration):
			calendarUpdateFailure(w, http.StatusBadRequest, configuration.Error(), false, false)
		case errors.Is(err, errCalendarOccurrenceBoundary):
			calendarUpdateFailure(w, http.StatusBadRequest, err.Error(), false, false)
		case errors.Is(err, errCalendarUpdateConflict):
			calendarUpdateFailure(w, http.StatusConflict, "This event changed at the provider. Refresh the calendar and reopen it; your changes were not applied.", false, true)
		case errors.Is(err, errCalendarUpdateUnsupported):
			calendarUpdateFailure(w, http.StatusForbidden, err.Error()+" Refresh the calendar and reopen the event.", false, true)
		case errors.As(err, &provider) && (provider.Status == 404 || provider.Status == 410):
			calendarUpdateFailure(w, http.StatusConflict, "This event is no longer available at the provider. Refresh the calendar.", false, true)
		default:
			uncertain := !errors.As(err, &preflight) && calendarCreateUncertain(err)
			message := "Could not update the event. Check Calendar write access and try again."
			if uncertain {
				message = "The save could not be confirmed. Refresh the calendar and reopen the event to check before making further changes."
			}
			calendarUpdateFailure(w, http.StatusBadGateway, message, uncertain, false)
		}
		return
	}
	confirmed := remote
	if (source.Provider == providers.ProviderOutlook || (source.Provider == providers.ProviderGmail && calendarGoogleMeetJSON(remote.OnlineMeeting))) && calendarUpdateHasDetails(remote.OnlineMeeting) {
		confirmed.OnlineMeeting = nil
	}
	restriction := calendarUpdateSavedRestriction(confirmed, draft)
	if instance {
		restriction = calendarOccurrenceRestriction(remote, event.SeriesRemoteID)
	}
	if remote.RemoteID != event.RemoteID || strings.TrimSpace(remote.ETag) == "" || restriction != "" || ((draft.Recurrence != nil || instance) && !calendarUpdateMatchesDraft(remote, draft)) {
		calendarUpdateFailure(w, http.StatusBadGateway, "The provider's saved event could not be confirmed. Refresh the calendar and reopen the event.", true, false)
		return
	}
	stored := calendarStorageEvent(userID, source.ID, remote)
	if series {
		err = h.db.CompleteCalendarSeriesUpdate(ctx, userID, occurrence.ID, source.ID, occurrence.ETag, stored)
	} else if instance {
		err = h.db.CompleteCalendarOccurrenceUpdate(ctx, userID, event.ID, source.ID, version, stored)
	} else if draft.Recurrence != nil {
		err = h.db.CompleteCalendarSeriesConversion(ctx, userID, event.ID, source.ID, version, stored)
	} else if source.Provider == providers.ProviderGmail && (calendarStoredGoogleMeet(event) || calendarGoogleMeetJSON(remote.OnlineMeeting)) {
		err = h.db.CompleteCalendarOnlineUpdate(ctx, event, stored)
	} else if source.Provider == providers.ProviderOutlook && (calendarStoredOutlookOnline(event) || calendarUpdateHasDetails(remote.OnlineMeeting)) {
		err = h.db.CompleteCalendarOutlookOnlineUpdate(ctx, event, stored)
	} else {
		err = h.db.CompleteCalendarUpdate(ctx, userID, event.ID, source.ID, version, stored)
	}
	if err != nil {
		calendarUpdateFailure(w, http.StatusServiceUnavailable, "The provider saved the event, but the local update could not finish. Refresh the calendar and reopen the event.", true, false)
		return
	}
	if h.syncer != nil {
		h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: userID, Payload: map[string]any{"source_id": source.ID, "event_id": event.ID}})
	}
	response := map[string]any{"saved": true, "event_id": event.ID, "source_id": source.ID, "hidden": source.IsHidden, "notify_guests": len(draft.Guests) > 0 || calendarUpdateHasDetails(json.RawMessage(event.AttendeesJSON))}
	if draft.TeamsMeeting && source.Provider == providers.ProviderOutlook {
		response["teams_unconfirmed"] = !calendarTeamsLinkConfirmed(remote.OnlineMeeting)
	}
	if draft.GoogleMeetMeeting && source.Provider == providers.ProviderGmail {
		response["google_meet_unconfirmed"] = !calendarGoogleMeetConfirmed(remote.OnlineMeeting)
	}
	if instance {
		response["scope"] = "occurrence"
	}
	if draft.Recurrence != nil {
		response["series_id"] = remote.RemoteID
		if series {
			response["refresh_pending"] = !h.refreshCalendarEditedSeries(ctx, source, occurrence)
		} else {
			response["refresh_pending"] = !h.refreshCalendarSeries(ctx, source, draft)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(response)
}
