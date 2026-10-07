package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserUpdateCalendarEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 384<<10)
	if r.URL.RawQuery != "" {
		calendarUpdateFailure(w, 400, "Event details could not be read.", false, false)
		return
	}
	draft, version, err := parseCalendarUpdateDraft(r)
	if err != nil {
		calendarUpdateFailure(w, 400, err.Error(), false, false)
		return
	}
	if draft.TeamsDraftID != "" {
		calendarUpdateFailure(w, 400, "Prepared Teams links can only be used for new events.", false, false)
		return
	}
	series, instance := r.PostForm.Get("edit_scope") == "series", r.PostForm.Get("edit_scope") == "occurrence"
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	attempt := &userCalendarResponseAttempt{}
	var response map[string]any
	err = h.userIMAP.RunUserServiceWork(ctx, h.userID(r.Context()), func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(r.Context()), r.PathValue("id"))
		if err != nil {
			return err
		}
		p := &userCalendarRequest{h: h, event: snapshot, writeAttempt: attempt}
		original, source := snapshot.Event(), snapshot.Source()
		if draft.GoogleMeetMeeting && source.Provider != "gmail" {
			return userCalendarMutationFailure{status: 400, message: "Google Meet meetings require a supported Google calendar."}
		}
		if draft.TeamsMeeting && source.Provider != "outlook" {
			return userCalendarMutationFailure{status: 400, message: "Teams meetings require a supported Outlook calendar."}
		}
		if r.PostForm.Get("source_id") != source.ID {
			return userCalendarMutationFailure{status: 400, message: "Editing cannot move an event to another calendar."}
		}
		if (series || instance) != calendarEventIsSeries(original) || (series && draft.Recurrence == nil) || (instance && (original.SeriesRemoteID == "" || draft.Recurrence != nil)) {
			return userCalendarMutationFailure{status: 400, message: "Choose This event or Entire series. Only Entire series can change repeat settings."}
		}
		if !series && original.ETag != version {
			return userCalendarMutationFailure{status: 409, message: "This event changed. Refresh the calendar and reopen it before saving.", conflict: true}
		}
		if err := p.mutationAccess(ctx, false); err != nil {
			return err
		}
		var savedRemoteID string
		err = h.userIMAP.RunAccountService(ctx, source.UserID, source.AccountID, mail.AccountServiceCalendar, calendarSourceTimeout, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, source)
			if err != nil {
				return err
			}
			defer unlock()
			if err := p.mutationAccess(operation, false); err != nil {
				return err
			}
			operation, err = p.actionContext(operation, true)
			if err != nil {
				return err
			}
			event := original
			if draft.Recurrence != nil && calendarUpdateHasDetails(json.RawMessage(event.AttendeesJSON)) {
				return userCalendarMutationFailure{status: 400, message: "Recurring meetings with guests are not supported yet."}
			}
			draft.OrganizerEmail, draft.OrganizerName = event.OrganizerEmail, event.OrganizerName
			if calendarCachedOrganizer(event) {
				event.ResponseStatus = "organizer"
			}
			if series {
				credentials, err := p.actionCredentials()
				if err != nil {
					return err
				}
				event, err = readCalendarProviderSeriesWithCredentials(operation, source, original, credentials)
				if err != nil || event.ETag != version {
					message := "The series could not be loaded or has changed. Reopen it before saving."
					if errors.Is(err, errCalendarUpdateUnsupported) {
						message = err.Error()
					}
					return userCalendarMutationFailure{status: 409, message: message, conflict: true}
				}
			}
			if !event.AllDay && !draft.AllDay {
				if event.StartAt != nil && event.StartAt.Truncate(time.Minute).Equal(*draft.StartAt) {
					draft.StartAt = event.StartAt
				}
				if event.EndAt != nil && event.EndAt.Truncate(time.Minute).Equal(*draft.EndAt) {
					draft.EndAt = event.EndAt
				}
			}
			if err := h.attachCalendarMeetDraft(operation, source, &draft, "event:"+event.ID); err != nil {
				return userCalendarMutationFailure{status: 409, message: "The Google Meet link could not be loaded. Toggle Google Meet off and on to prepare it again."}
			}
			remote, err := h.updateCalendarProviderEvent(operation, source, event, draft, series, instance)
			if err != nil {
				if attempt.uncertain {
					return userCalendarMutationFailure{status: 502, message: "The save could not be confirmed. Refresh the calendar and reopen the event to check before making further changes.", uncertain: true}
				}
				var provider calendarCreateProviderError
				var configuration calendarReplyConfigurationError
				switch {
				case errors.As(err, &configuration), errors.Is(err, errCalendarOccurrenceBoundary):
					return userCalendarMutationFailure{status: 400, message: err.Error()}
				case errors.Is(err, errCalendarUpdateConflict):
					return userCalendarMutationFailure{status: 409, message: "This event changed at the provider. Refresh the calendar and reopen it; your changes were not applied.", conflict: true}
				case errors.Is(err, errCalendarUpdateUnsupported):
					return userCalendarMutationFailure{status: 403, message: err.Error() + " Refresh the calendar and reopen the event.", conflict: true}
				case errors.As(err, &provider) && (provider.Status == 404 || provider.Status == 410):
					return userCalendarMutationFailure{status: 409, message: "This event is no longer available at the provider. Refresh the calendar.", conflict: true}
				default:
					return userCalendarMutationFailure{status: 502, message: "Could not update the event. Check Calendar write access and try again."}
				}
			}
			confirmed := remote
			if (source.Provider == "outlook" || (source.Provider == "gmail" && calendarGoogleMeetJSON(remote.OnlineMeeting))) && calendarUpdateHasDetails(remote.OnlineMeeting) {
				confirmed.OnlineMeeting = nil
			}
			restriction := calendarUpdateSavedRestriction(confirmed, draft)
			if instance {
				restriction = calendarOccurrenceRestriction(remote, event.SeriesRemoteID)
			}
			if remote.RemoteID != event.RemoteID || strings.TrimSpace(remote.ETag) == "" || restriction != "" || ((draft.Recurrence != nil || instance) && !calendarUpdateMatchesDraft(remote, draft)) {
				return userCalendarMutationFailure{status: 502, message: "The provider's saved event could not be confirmed. Refresh the calendar and reopen the event.", uncertain: true}
			}
			kind := storage.CalendarPublishUpdate
			switch {
			case series:
				kind = storage.CalendarPublishSeriesUpdate
			case instance:
				kind = storage.CalendarPublishOccurrenceUpdate
			case draft.Recurrence != nil:
				kind = storage.CalendarPublishSeriesConversion
			case (source.Provider == "gmail" || source.Provider == "outlook") && (len(draft.Guests) > 0 || calendarUpdateHasDetails(json.RawMessage(original.AttendeesJSON)) || calendarUpdateHasDetails(remote.Attendees)):
				// Guest-only meetings need the same atomic identity/attendee
				// publication as conferences. An editable-fields-only update
				// would attach the new version to the previous guest list.
				kind = storage.CalendarPublishOnlineUpdate
			case source.Provider == "gmail" && (calendarStoredGoogleMeet(original) || calendarGoogleMeetJSON(remote.OnlineMeeting)):
				kind = storage.CalendarPublishOnlineUpdate
			case source.Provider == "outlook" && (calendarStoredOutlookOnline(original) || calendarUpdateHasDetails(remote.OnlineMeeting)):
				kind = storage.CalendarPublishOnlineUpdate
			}
			if err := p.publishEvent(operation, kind, calendarStorageEvent(source.UserID, source.ID, remote)); err != nil {
				return userCalendarMutationFailure{status: 503, message: "The provider saved the event, but the local update could not finish. Refresh the calendar and reopen the event.", uncertain: true}
			}
			savedRemoteID = remote.RemoteID
			response = map[string]any{"saved": true, "event_id": original.ID, "source_id": source.ID, "hidden": source.IsHidden, "notify_guests": len(draft.Guests) > 0 || calendarUpdateHasDetails(json.RawMessage(original.AttendeesJSON))}
			if draft.TeamsMeeting {
				response["teams_unconfirmed"] = !calendarTeamsLinkConfirmed(remote.OnlineMeeting)
			}
			if draft.GoogleMeetMeeting {
				response["google_meet_unconfirmed"] = !calendarGoogleMeetConfirmed(remote.OnlineMeeting)
			}
			if instance {
				response["scope"] = "occurrence"
			}
			return nil
		})
		if err != nil {
			return err
		}
		h.userIMAP.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: source.UserID, AccountID: source.AccountID, Payload: map[string]any{"source_id": source.ID, "event_id": original.ID}})
		if draft.Recurrence != nil {
			response["series_id"] = savedRemoteID
			if series {
				response["refresh_pending"] = !h.refreshUserCalendarEditedSeries(ctx, source, original)
			} else {
				response["refresh_pending"] = !h.refreshUserCalendarCreatedSeries(ctx, source, draft)
			}
		}
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
