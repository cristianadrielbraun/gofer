package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

// One application-owned property, shared by preparation, sync, save and cleanup.
const calendarTeamsDraftProperty = "String {534725f9-4d8e-4616-a2ea-3b020983e0c8} Name GoferTeamsDraft"

func calendarTeamsDraftExpand() string {
	return "singleValueExtendedProperties($filter=id eq '" + calendarTeamsDraftProperty + "')"
}
func calendarTeamsDraftMarker(remote outlookCalendarEvent) string {
	for _, property := range remote.SingleValueExtendedProperties {
		if property.ID == calendarTeamsDraftProperty {
			return property.Value
		}
	}
	return ""
}
func calendarTeamsDraftProperties(value string) []map[string]string {
	return []map[string]string{{"id": calendarTeamsDraftProperty, "value": value}}
}
func calendarTeamsDraftCollection(source storage.CalendarSource) string {
	return outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(source.RemoteID) + "/events"
}
func calendarTeamsDraftEndpoint(source storage.CalendarSource, id string) string {
	return calendarTeamsDraftCollection(source) + "/" + url.PathEscape(id)
}
func readCalendarTeamsDraft(ctx context.Context, source storage.CalendarSource, id, token string) (outlookCalendarUpdateEvent, error) {
	var remote outlookCalendarUpdateEvent
	query := url.Values{"$expand": {calendarTeamsDraftExpand()}}
	err := calendarCreateJSON(ctx, http.MethodGet, calendarTeamsDraftEndpoint(source, id)+"?"+query.Encode(), token, nil, &remote)
	return remote, err
}

// Look up our marker before POST, including recovery after a lost create response.
// transactionId additionally makes a repeated POST idempotent at Microsoft.
func findCalendarTeamsDraft(ctx context.Context, source storage.CalendarSource, d storage.CalendarTeamsDraft, token string) (string, error) {
	query := url.Values{"$expand": {calendarTeamsDraftExpand()}, "$top": {"2"}, "$filter": {"singleValueExtendedProperties/Any(ep: ep/id eq '" + calendarTeamsDraftProperty + "' and ep/value eq 'draft:" + d.DraftID + "')"}}
	var page outlookCalendarEventsResponse
	if err := calendarCreateJSON(ctx, http.MethodGet, calendarTeamsDraftCollection(source)+"?"+query.Encode(), token, nil, &page); err != nil {
		return "", err
	}
	if len(page.Events) > 1 || page.NextLink != "" {
		return "", fmt.Errorf("Microsoft returned ambiguous draft identities")
	}
	if len(page.Events) == 0 {
		return "", nil
	}
	remote := page.Events[0]
	if remote.ID == "" || calendarTeamsDraftMarker(remote) != "draft:"+d.DraftID {
		return "", fmt.Errorf("Microsoft did not confirm the draft identity")
	}
	if p, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); owned && p.cleanup != nil {
		if err := p.allowMeetingCleanupRead(ctx, d.DraftID, remote.ID); err != nil {
			return "", err
		}
	}
	return remote.ID, nil
}
func calendarTeamsDraftPrivate(remote outlookCalendarUpdateEvent, d storage.CalendarTeamsDraft) bool {
	return remote.ID == d.RemoteID && calendarTeamsDraftMarker(remote.outlookCalendarEvent) == "draft:"+d.DraftID && !remote.IsCancelled && remote.IsOrganizer != nil && *remote.IsOrganizer && !calendarUpdateHasDetails(remote.Attendees) && remote.SeriesMasterID == "" && !calendarUpdateHasDetails(remote.Recurrence) && remote.Subject == "Preparing Teams meeting" && remote.Sensitivity == "private" && remote.ShowAs == "free" && remote.IsReminderOn != nil && !*remote.IsReminderOn
}
func (h *Handler) prepareCalendarTeamsDraft(ctx context.Context, source storage.CalendarSource, d *storage.CalendarTeamsDraft, token string) (bool, error) {
	if d.RemoteID == "" {
		id, err := findCalendarTeamsDraft(ctx, source, *d, token)
		if err != nil {
			return false, err
		}
		if id == "" {
			mode, err := outlookCalendarTeamsMode(ctx, token, source.RemoteID)
			if err != nil {
				return false, err
			}
			if mode != "available" && mode != "available-default" {
				return false, calendarUpdateUnsupported(calendarTeamsUnavailableMessage)
			}
			now := time.Now().UTC()
			payload := map[string]any{"subject": "Preparing Teams meeting", "sensitivity": "private", "showAs": "free", "isReminderOn": false, "isOnlineMeeting": true, "transactionId": d.DraftID,
				"start": map[string]string{"dateTime": now.Format("2006-01-02T15:04:05"), "timeZone": "UTC"}, "end": map[string]string{"dateTime": now.Add(5 * time.Minute).Format("2006-01-02T15:04:05"), "timeZone": "UTC"}, "singleValueExtendedProperties": calendarTeamsDraftProperties("draft:" + d.DraftID)}
			if mode == "available" {
				payload["onlineMeetingProvider"] = "teamsForBusiness"
			}
			var created outlookCalendarEvent
			if err := calendarCreateJSON(ctx, http.MethodPost, calendarTeamsDraftCollection(source), token, payload, &created); err != nil {
				return false, err
			}
			id = created.ID
		}
		if id == "" {
			return false, fmt.Errorf("Microsoft did not return a draft identity")
		}
		if err := h.setCalendarTeamsDraftRemote(ctx, *d, id); err != nil {
			return false, err
		}
		d.RemoteID = id
	}
	deadline := time.Now().Add(12 * time.Second)
	for {
		remote, err := readCalendarTeamsDraft(ctx, source, d.RemoteID, token)
		if err != nil {
			return false, err
		}
		if !calendarTeamsDraftPrivate(remote, *d) {
			return false, fmt.Errorf("Microsoft did not confirm this private draft")
		}
		if calendarOutlookTeamsConfirmed(remote.outlookCalendarEvent) && calendar.TeamsJoinURL(calendar.MeetingJoinURL(string(calendarOutlookMeetingJSON(remote.outlookCalendarEvent)))) != "" {
			meeting := string(calendarOutlookMeetingJSON(remote.outlookCalendarEvent))
			if d.MeetingJSON != "" && calendar.MeetingJoinURL(d.MeetingJSON) != calendar.MeetingJoinURL(meeting) {
				return false, fmt.Errorf("Microsoft changed the prepared Teams link")
			}
			// Retain stable conference metadata instead of rewriting it on each check.
			if d.MeetingJSON != "" {
				meeting = d.MeetingJSON
			}
			if err := h.completeCalendarTeamsDraft(ctx, *d, meeting); err != nil {
				return false, err
			}
			d.MeetingJSON = meeting
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		timer := time.NewTimer(400 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}
func (h *Handler) handleCalendarTeamsDraft(w http.ResponseWriter, r *http.Request) {
	h.calendarTeamsDraftRequest(w, r, false)
}
func (h *Handler) handleCalendarTeamsDraftDiscard(w http.ResponseWriter, r *http.Request) {
	h.calendarTeamsDraftRequest(w, r, true)
}
func (h *Handler) calendarTeamsDraftRequest(w http.ResponseWriter, r *http.Request, discard bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	fail := func(status int, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		fail(400, "Could not read the meeting request.")
		return
	}
	for name, values := range r.PostForm {
		if (name != "source_id" && name != "draft_id") || len(values) != 1 {
			fail(400, "Invalid meeting request.")
			return
		}
	}
	id, err := uuid.Parse(r.PostForm.Get("draft_id"))
	if err != nil || id == uuid.Nil {
		fail(400, "Invalid meeting request.")
		return
	}
	user := h.userID(ctx)
	sources, err := h.db.ListSelectedCalendarSources(ctx, user)
	if err != nil {
		fail(500, "Could not load this calendar.")
		return
	}
	var source storage.CalendarSource
	for _, s := range sources {
		if s.ID == r.PostForm.Get("source_id") {
			source = s
			break
		}
	}
	if source.ID == "" || source.Provider != "outlook" {
		fail(404, "This Outlook calendar is unavailable.")
		return
	}
	if !calendarSourceWritable(source) || !h.calendarWriteAuthorized(ctx, source) {
		fail(403, "Reconnect this account from Accounts to grant Calendar write access.")
		return
	}
	unlock, err := h.lockCalendarCreate(ctx, source)
	if err != nil {
		fail(503, "Calendar is busy. Retry.")
		return
	}
	defer unlock()
	if discard {
		// Create a tombstone even when closing races ahead of preparation, so a
		// late POST cannot allocate a new draft after the dialog has been discarded.
		d, err := h.db.BeginCalendarTeamsDraft(ctx, user, source.ID, id.String())
		if err == nil {
			err = h.db.AbandonCalendarTeamsDraft(ctx, d)
		}
		if err != nil {
			fail(503, "Could not discard the draft. Cleanup will retry.")
			return
		}
		w.WriteHeader(204)
		return
	}
	d, err := h.db.BeginCalendarTeamsDraft(ctx, user, source.ID, id.String())
	if err != nil {
		fail(409, "Calendar configuration changed. Reopen the event.")
		return
	}
	if d.State != "active" {
		fail(409, "This Teams draft is no longer available. Toggle Teams off and on to prepare another link.")
		return
	}
	credentials := h.calendarUpdateCredentials(ctx, source)
	if credentials.err != nil {
		fail(403, "Reconnect this Outlook account to grant Calendar write access.")
		return
	}
	ready, err := h.prepareCalendarTeamsDraft(ctx, source, &d, credentials.token)
	if err != nil {
		fail(502, "Could not prepare the Teams link. Retry, or toggle Teams off and on.")
		return
	}
	response := map[string]any{"draft_id": d.DraftID, "source_id": source.ID}
	if !ready {
		w.WriteHeader(202)
		response["pending"] = true
	} else {
		response["join_url"] = calendar.MeetingJoinURL(d.MeetingJSON)
	}
	_ = json.NewEncoder(w).Encode(response)
}
func (h *Handler) attachCalendarTeamsDraft(ctx context.Context, source storage.CalendarSource, draft *calendar.EventDraft) error {
	if !draft.TeamsMeeting || draft.TeamsDraftID == "" {
		return nil
	}
	if p, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); owned {
		if p == nil || p.h != h {
			return storage.ErrCalendarCreateConflict
		}
		return p.attachMeetingDraft(ctx, source, draft, "create:"+draft.RequestID, "outlook")
	}
	d, err := h.db.GetCalendarTeamsDraft(ctx, source.UserID, source.ID, draft.TeamsDraftID)
	if err != nil {
		return err
	}
	if !calendarTeamsLinkConfirmed(json.RawMessage(d.MeetingJSON)) {
		return fmt.Errorf("Teams link is not ready")
	}
	if err := h.db.BindCalendarTeamsDraft(ctx, d, "create:"+draft.RequestID); err != nil {
		return err
	}
	draft.TeamsRemoteID = d.RemoteID
	draft.TeamsConference = json.RawMessage(d.MeetingJSON)
	return nil
}
func finalizeCalendarTeamsDraft(ctx context.Context, source storage.CalendarSource, token string, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	endpoint := calendarTeamsDraftEndpoint(source, draft.TeamsRemoteID)
	current, err := readCalendarTeamsDraft(ctx, source, draft.TeamsRemoteID, token)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	marker := calendarTeamsDraftMarker(current.outlookCalendarEvent)
	savedMarker := "saved:" + draft.RequestID + ":" + calendarDraftHash(draft)
	if current.ID != draft.TeamsRemoteID || current.IsCancelled || !calendarOutlookTeamsConfirmed(current.outlookCalendarEvent) || calendar.MeetingJoinURL(string(calendarOutlookMeetingJSON(current.outlookCalendarEvent))) != calendar.MeetingJoinURL(string(draft.TeamsConference)) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the prepared Teams meeting")
	}
	if marker != savedMarker {
		if !calendarTeamsDraftPrivate(current, storage.CalendarTeamsDraft{RemoteID: draft.TeamsRemoteID, DraftID: draft.TeamsDraftID}) || !calendarUpdateValidETag(current.ODataETag, true) {
			return calendar.RemoteEvent{}, fmt.Errorf("The prepared Teams event changed. It could not be finalized safely")
		}
		location, err := calendarUpdateDraftLocation(draft)
		if err != nil {
			return calendar.RemoteEvent{}, calendarDeletePreflightError{err}
		}
		start, end := map[string]string{"timeZone": draft.TimeZone}, map[string]string{"timeZone": draft.TimeZone}
		if draft.AllDay {
			start["dateTime"], end["dateTime"] = draft.StartDate+"T00:00:00", draft.EndDate+"T00:00:00"
		} else {
			start["dateTime"], end["dateTime"] = draft.StartAt.In(location).Format("2006-01-02T15:04:05"), draft.EndAt.In(location).Format("2006-01-02T15:04:05")
		}
		body := calendarDraftOutlookBody(draft)
		// Preserve Microsoft's original meeting block when setting user content.
		block := current.Body.Content
		if !strings.EqualFold(current.Body.ContentType, "html") {
			block = calendar.DescriptionHTML(block)
		}
		if block != "" {
			body.ContentType = "html"
			body.Content = calendar.DescriptionHTML(calendar.DraftDescription(draft)) + "<br>" + block
		}
		payload := map[string]any{"subject": draft.Summary, "body": body, "start": start, "end": end, "isAllDay": draft.AllDay, "location": outlookCalendarLocation{DisplayName: draft.Location}, "sensitivity": "normal", "showAs": "busy", "isReminderOn": true, "reminderMinutesBeforeStart": 15, "attendees": calendarMeetingOutlookGuests(draft, nil), "responseRequested": len(draft.Guests) > 0, "singleValueExtendedProperties": calendarTeamsDraftProperties(savedMarker)}
		var updated outlookCalendarUpdateEvent
		if err := calendarUpdateJSON(ctx, endpoint, token, current.ODataETag, payload, &updated); err != nil {
			return calendar.RemoteEvent{}, err
		}
		priorUID := current.ICalUID
		current, err = readCalendarTeamsDraft(ctx, source, draft.TeamsRemoteID, token)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		if current.ICalUID != priorUID {
			return calendar.RemoteEvent{}, fmt.Errorf("Microsoft changed the prepared event identity")
		}
	}
	if current.ID != draft.TeamsRemoteID || calendarTeamsDraftMarker(current.outlookCalendarEvent) != savedMarker || current.IsCancelled || current.ChangeKey == "" || !calendarOutlookTeamsConfirmed(current.outlookCalendarEvent) || calendar.MeetingJoinURL(string(calendarOutlookMeetingJSON(current.outlookCalendarEvent))) != calendar.MeetingJoinURL(string(draft.TeamsConference)) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the finalized Teams meeting")
	}
	event, err := normalizeOutlookCalendarEvent(current.outlookCalendarEvent)
	if err != nil {
		return calendar.RemoteEvent{}, err
	}
	if !calendarMeetingGuestsMatch(event.Attendees, draft.Guests, event.OrganizerEmail) || !calendarTeamsDescriptionConfirmed(event.Description, draft) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the meeting details and guests")
	}
	confirmed := event
	confirmed.Description = calendar.DraftDescription(draft)
	if !calendarUpdateMatchesDraft(confirmed, draft) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft did not confirm the submitted event changes")
	}
	event.StartTimeZone, event.EndTimeZone = draft.TimeZone, draft.TimeZone
	return event, nil
}
func cleanupCalendarTeamsDraft(ctx context.Context, source storage.CalendarSource, d storage.CalendarTeamsDraft, token string) error {
	if d.RemoteID == "" {
		id, err := findCalendarTeamsDraft(ctx, source, d, token)
		if err != nil {
			return err
		}
		if id == "" {
			return nil
		}
		d.RemoteID = id
	}
	remote, err := readCalendarTeamsDraft(ctx, source, d.RemoteID, token)
	var missing calendarCreateProviderError
	if errors.As(err, &missing) && (missing.Status == 404 || missing.Status == 410) {
		return nil
	}
	if err != nil {
		return err
	}
	if !calendarTeamsDraftPrivate(remote, d) || !calendarUpdateValidETag(remote.ODataETag, true) {
		return fmt.Errorf("temporary Teams event identity or version changed")
	}
	if p, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); owned && p.cleanup != nil {
		if err := p.allowMeetingCleanupDelete(ctx, source, d.DraftID, remote.ID, remote.ODataETag); err != nil {
			return err
		}
	}
	return calendarDeleteJSON(ctx, calendarTeamsDraftEndpoint(source, d.RemoteID), token, remote.ODataETag)
}
func (h *Handler) runCalendarTeamsDraftCleanupTick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	drafts, err := h.db.ListCalendarTeamsDraftCleanup(ctx)
	if err != nil {
		return
	}
	for _, d := range drafts {
		if ctx.Err() != nil {
			break
		}
		source, err := h.db.CalendarTeamsDraftSource(ctx, d)
		if err != nil {
			continue
		}
		unlock, err := h.lockCalendarCreate(ctx, source)
		if err != nil {
			continue
		}
		credentials := h.calendarUpdateCredentials(ctx, source)
		if credentials.err != nil {
			unlock()
			continue
		}
		if d.State == "saving" {
			remote, err := readCalendarTeamsDraft(ctx, source, d.RemoteID, credentials.token)
			if err != nil {
				unlock()
				continue
			}
			// A disconnected save may already have invited guests. Preserve it,
			// including when its local cache commit needs a later safe retry.
			if remote.ID == d.RemoteID && strings.HasPrefix(calendarTeamsDraftMarker(remote.outlookCalendarEvent), "saved:"+strings.TrimPrefix(d.UsedBy, "create:")+":") {
				_ = h.db.FinishCalendarTeamsDraft(ctx, d, "saved")
				unlock()
				continue
			}
			if !calendarTeamsDraftPrivate(remote, d) || h.db.ExpireCalendarTeamsReservation(ctx, d) != nil {
				unlock()
				continue
			}
			d.State = "abandoned"
		}
		if cleanupCalendarTeamsDraft(ctx, source, d, credentials.token) == nil {
			_ = h.db.FinishCalendarTeamsDraft(ctx, d, "cleaned")
		}
		unlock()
	}
	_ = h.db.PruneCalendarTeamsDrafts(ctx)
}
