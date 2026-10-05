package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

func calendarMeetDraftRemoteID(user, source, id string) string {
	sum := sha256.Sum256([]byte(user + "\x00" + source + "\x00" + id))
	return "gofermeetdraft" + hex.EncodeToString(sum[:])
}

func calendarMeetDraftEndpoint(source storage.CalendarSource, d storage.CalendarMeetDraft) string {
	return googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(source.RemoteID) + "/events/" + url.PathEscape(d.RemoteID)
}

func calendarMeetDraftIdentity(remote googleCalendarEvent, d storage.CalendarMeetDraft) bool {
	return remote.ID == d.RemoteID && remote.ExtendedProperties.Private["goferMeetDraft"] == "true" && remote.ExtendedProperties.Private["goferMeetDraftID"] == d.DraftID
}

// Keep Google's conference data, including its signature when supplied. Meet
// conferences can be ready without a signature (including consumer accounts).
// A copied conference must not contain
// a new createRequest, otherwise saving could allocate a different meeting.
func calendarMeetDraftConference(remote googleCalendarEvent) json.RawMessage {
	if !calendarGoogleMeetConfirmed(remote.ConferenceData) {
		return nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(remote.ConferenceData, &data) != nil {
		return nil
	}
	if request, exists := data["createRequest"]; exists {
		var state struct {
			Status struct{ StatusCode string }
		}
		if json.Unmarshal(request, &state) != nil || state.Status.StatusCode != "success" {
			return nil
		}
	}
	delete(data, "createRequest")
	raw, _ := json.Marshal(data)
	return raw
}

func prepareGoogleMeetDraft(ctx context.Context, source storage.CalendarSource, d storage.CalendarMeetDraft, token string) (json.RawMessage, error) {
	endpoint := calendarMeetDraftEndpoint(source, d)
	var remote googleCalendarEvent
	err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &remote)
	var missing calendarCreateProviderError
	if errors.As(err, &missing) && missing.Status == 404 {
		start := time.Now().UTC()
		payload := map[string]any{"id": d.RemoteID, "summary": "Preparing Google Meet", "visibility": "private", "transparency": "transparent",
			"start": map[string]string{"dateTime": start.Format(time.RFC3339)}, "end": map[string]string{"dateTime": start.Add(5 * time.Minute).Format(time.RFC3339)},
			"reminders":          map[string]any{"useDefault": false, "overrides": []any{}},
			"extendedProperties": map[string]any{"private": map[string]string{"goferMeetDraft": "true", "goferMeetDraftID": d.DraftID}},
			"conferenceData":     calendarGoogleMeetRequest(d.RemoteID)}
		collection := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(source.RemoteID) + "/events?conferenceDataVersion=1&sendUpdates=none"
		err = calendarCreateJSON(ctx, http.MethodPost, collection, token, payload, &remote)
		var conflict calendarCreateProviderError
		if errors.As(err, &conflict) && conflict.Status == 409 {
			err = calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &remote)
		}
	}
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(12 * time.Second)
	for {
		if !calendarMeetDraftIdentity(remote, d) || remote.Status == "cancelled" {
			return nil, fmt.Errorf("Google did not confirm this temporary meeting")
		}
		if data := calendarMeetDraftConference(remote); len(data) > 0 {
			return data, nil
		}
		var state struct {
			CreateRequest struct{ Status struct{ StatusCode string } }
		}
		_ = json.Unmarshal(remote.ConferenceData, &state)
		if state.CreateRequest.Status.StatusCode == "failure" {
			return nil, calendarUpdateUnsupported("Google could not generate this Meet link. Toggle Google Meet off and on to try again.")
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		timer := time.NewTimer(400 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		remote = googleCalendarEvent{}
		if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &remote); err != nil {
			return nil, err
		}
	}
}

func cleanupGoogleMeetDraft(ctx context.Context, source storage.CalendarSource, d storage.CalendarMeetDraft, token string) error {
	endpoint := calendarMeetDraftEndpoint(source, d)
	var remote googleCalendarEvent
	err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &remote)
	var missing calendarCreateProviderError
	if errors.As(err, &missing) && (missing.Status == 404 || missing.Status == 410) {
		return nil
	}
	if err != nil {
		return err
	}
	if remote.Status == "cancelled" {
		return nil
	}
	if !calendarMeetDraftIdentity(remote, d) {
		return fmt.Errorf("temporary meeting identity changed")
	}
	if !calendarUpdateValidETag(remote.ETag, false) {
		return fmt.Errorf("temporary meeting has no strong version")
	}
	return calendarDeleteJSON(ctx, endpoint+"?sendUpdates=none", token, remote.ETag)
}

func (h *Handler) handleCalendarGoogleMeetDraft(w http.ResponseWriter, r *http.Request) {
	// Bound the whole preparation, including credential/database access. Finish
	// the durable operation even when the browser disconnects.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
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
		if (name != "source_id" && name != "draft_id" && name != "event_id") || len(values) != 1 {
			fail(400, "Invalid meeting request.")
			return
		}
	}
	id, err := uuid.Parse(r.PostForm.Get("draft_id"))
	if err != nil || id == uuid.Nil {
		fail(400, "Invalid meeting request.")
		return
	}
	userID := h.userID(r.Context())
	sources, err := h.db.ListSelectedCalendarSources(r.Context(), userID)
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
	if source.ID == "" || source.Provider != "gmail" {
		fail(404, "This Google calendar is unavailable.")
		return
	}
	if !calendarSourceWritable(source) || !h.calendarWriteAuthorized(r.Context(), source) {
		fail(403, "Reconnect this account from Accounts to grant Calendar write access.")
		return
	}
	if eventID := r.PostForm.Get("event_id"); eventID != "" {
		event, err := h.db.GetCalendarEvent(r.Context(), userID, eventID)
		if err != nil || event.SourceID != source.ID {
			fail(404, "This event is unavailable.")
			return
		}
		if _, reason, err := h.calendarEventEditAccess(r.Context(), event); err != nil || reason != "" || calendarStoredGoogleMeet(event) || calendarEventIsSeries(event) {
			fail(403, "A Meet conference cannot be added to this event.")
			return
		}
	}
	// Persist the resource identity before contacting Google. Complete preparation
	// and cleanup even if the dialog closes or the browser loses its connection.
	unlock, err := h.lockCalendarCreate(ctx, source)
	if err != nil {
		fail(503, "Calendar is busy. Try again.")
		return
	}
	defer unlock()
	d, err := h.db.BeginCalendarMeetDraft(ctx, userID, source.ID, id.String(), calendarMeetDraftRemoteID(userID, source.ID, id.String()))
	if err != nil {
		fail(409, "Calendar configuration changed. Reopen the event.")
		return
	}
	if d.UsedBy != "" {
		fail(409, "This Meet link has already been used. Toggle Google Meet off and on to generate a new one.")
		return
	}
	credentials := h.calendarUpdateCredentials(ctx, source)
	if credentials.err != nil {
		fail(403, "Reconnect this Google account to grant Calendar write access.")
		return
	}
	if d.ConferenceJSON == "" {
		supported, err := googleCalendarMeetSupported(ctx, credentials.token, source.RemoteID)
		if err != nil || !supported {
			fail(502, calendarGoogleMeetUnavailableMessage)
			return
		}
		conference, err := prepareGoogleMeetDraft(ctx, source, d, credentials.token)
		if err != nil {
			fail(502, "Could not prepare the Google Meet link. Retry, or toggle Google Meet off and on.")
			return
		}
		if len(conference) == 0 {
			w.WriteHeader(202)
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": true, "draft_id": d.DraftID, "source_id": source.ID})
			return
		}
		if err := h.db.CompleteCalendarMeetDraft(ctx, d, string(conference)); err != nil {
			fail(503, "Could not retain the Meet link. Retry this request.")
			return
		}
		d.ConferenceJSON = string(conference)
	}
	// Cleanup failure never loses a ready link; the durable worker retries it.
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 3*time.Second)
	if cleanupGoogleMeetDraft(cleanupCtx, source, d, credentials.token) == nil {
		_ = h.db.FinishCalendarMeetDraftCleanup(ctx, d)
	}
	cleanupCancel()
	_ = json.NewEncoder(w).Encode(map[string]any{"draft_id": d.DraftID, "source_id": source.ID, "join_url": calendar.MeetingJoinURL(d.ConferenceJSON)})
}

func (h *Handler) attachCalendarMeetDraft(ctx context.Context, source storage.CalendarSource, draft *calendar.EventDraft, target string) error {
	if !draft.GoogleMeetMeeting || draft.GoogleMeetDraftID == "" {
		return nil
	} // Older clients retain the provider createRequest path.
	d, err := h.db.GetCalendarMeetDraft(ctx, source.UserID, source.ID, draft.GoogleMeetDraftID)
	if err != nil {
		return err
	}
	if !calendarGoogleMeetConfirmed(json.RawMessage(d.ConferenceJSON)) {
		return fmt.Errorf("Meet link is not ready")
	}
	if err := h.db.BindCalendarMeetDraft(ctx, d, target); err != nil {
		return err
	}
	draft.GoogleMeetConference = json.RawMessage(d.ConferenceJSON)
	return nil
}

func (h *Handler) runCalendarMeetDraftCleanupTick(ctx context.Context) {
	// Keep cleanup bounded so a slow provider does not stall calendar syncing.
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	drafts, err := h.db.ListCalendarMeetDraftCleanup(ctx)
	if err != nil {
		return
	}
	for _, d := range drafts {
		if ctx.Err() != nil {
			break
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		source, err := h.db.CalendarMeetDraftSource(cleanupCtx, d)
		if err == nil {
			unlock, lockErr := h.lockCalendarCreate(cleanupCtx, source)
			if lockErr == nil {
				credentials := h.calendarUpdateCredentials(cleanupCtx, source)
				if credentials.err == nil && cleanupGoogleMeetDraft(cleanupCtx, source, d, credentials.token) == nil {
					_ = h.db.FinishCalendarMeetDraftCleanup(cleanupCtx, d)
				}
				unlock()
			}
		}
		cancel()
	}
	_ = h.db.PruneCalendarMeetDrafts(ctx)
}
