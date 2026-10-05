package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const testGoogleMeet = `{"conferenceId":"abc-defg-hij","conferenceSolution":{"key":{"type":"hangoutsMeet"}},"entryPoints":[{"entryPointType":"phone","uri":"tel:+420123456789"},{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij"}],"signature":"keep-me"}`
const testGoogleMeetPending = `{"createRequest":{"requestId":"requested","conferenceSolutionKey":{"type":"hangoutsMeet"},"status":{"statusCode":"pending"}}}`

func TestGoogleMeetDraftValidation(t *testing.T) {
	for _, value := range []string{"true", "false", "", "on", "invalid"} {
		values := calendarCreateForm()
		values.Set("google_meet_meeting", value)
		draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
		valid := value == "true" || value == "false" || value == ""
		if (err == nil) != valid || (valid && (!draft.GoogleMeetMeetingSet || draft.GoogleMeetMeeting != (value == "true"))) {
			t.Fatalf("value=%q draft=%#v err=%v", value, draft, err)
		}
	}
	for _, mode := range []string{"duplicate", "both", "recurring"} {
		values := calendarCreateForm()
		values.Set("google_meet_meeting", "true")
		switch mode {
		case "duplicate":
			values.Add("google_meet_meeting", "false")
		case "both":
			values.Set("teams_meeting", "true")
		case "recurring":
			values.Set("repeat_frequency", "daily")
		}
		if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
	raw, _ := json.Marshal(calendarProviderDraft(t, false))
	if strings.Contains(string(raw), "GoogleMeetMeeting") {
		t.Fatal("new flag changes existing idempotency hashes")
	}
}

func TestGoogleMeetCreateConvertAndEdit(t *testing.T) {
	for _, operation := range []string{"create", "convert", "edit"} {
		for _, mode := range []string{"success", "pending", "guests", "all-day", "unsupported", "read-only", "wrong-calendar", "capability-error", "race", "stale", "invitee", "recurring", "lost-conference", "wrong-uid", "wrong-id", "lost-description", "confirmation-failure", "disable", "other-provider", "retry", "prepared", "prepared-wrong-link"} {
			if operation == "create" && (mode == "race" || mode == "stale" || mode == "invitee" || mode == "lost-conference" || mode == "wrong-uid" || mode == "disable" || mode == "other-provider") {
				continue
			}
			if operation == "edit" && (mode == "prepared" || mode == "prepared-wrong-link" || mode == "unsupported" || mode == "read-only" || mode == "wrong-calendar" || mode == "capability-error" || mode == "retry") {
				continue
			}
			if operation != "edit" && (mode == "lost-conference" || mode == "disable") {
				continue
			}
			if operation != "create" && mode == "retry" {
				continue
			}
			t.Run(operation+"/"+mode, func(t *testing.T) {
				draft := calendarProviderDraft(t, mode == "all-day")
				draft.GoogleMeetMeeting = operation != "edit"
				draft.GoogleMeetMeetingSet = draft.GoogleMeetMeeting
				if strings.HasPrefix(mode, "prepared") {
					draft.GoogleMeetConference = json.RawMessage(testGoogleMeet)
				}
				draft.Description = "My description"
				draft.Location = "Room 2"
				if mode == "recurring" {
					draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 3}
				}
				if mode == "guests" {
					draft.GuestsSet = true
					draft.Guests = []calendar.GuestDraft{{Email: "guest@example.com"}}
				}
				if mode == "disable" {
					draft.GoogleMeetMeetingSet = true
				}
				if mode == "other-provider" {
					draft.GoogleMeetMeeting = true
					draft.GoogleMeetMeetingSet = true
				}
				id := "event"
				if operation == "create" {
					id = strings.ReplaceAll(draft.RequestID, "-", "")
				}
				current := map[string]any{"id": id, "iCalUID": "uid", "etag": `"v1"`, "summary": "Before", "description": "Before", "location": "Room 2", "start": map[string]string{"dateTime": "2026-10-02T07:00:00Z", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-02T08:00:00Z", "timeZone": "UTC"}, "organizer": map[string]any{"email": "owner@example.com", "self": mode != "invitee"}, "attendees": []any{}}
				existing := storage.CalendarEvent{RemoteID: id, ICalUID: "uid", ETag: `"v1"`, SourceProvider: "gmail", Description: "Before", ResponseStatus: "organizer"}
				if operation == "edit" {
					current["conferenceData"] = json.RawMessage(testGoogleMeet)
					existing.OnlineMeetingJSON = testGoogleMeet
				}
				if mode == "other-provider" {
					current["conferenceData"] = map[string]any{"conferenceSolution": map[string]any{"key": map[string]string{"type": "addOn"}}}
				}
				if operation == "edit" && mode == "pending" {
					current["conferenceData"] = json.RawMessage(testGoogleMeetPending)
					existing.OnlineMeetingJSON = testGoogleMeetPending
				}
				if mode == "stale" {
					current["etag"] = `"newer"`
				}
				writes, gets, capabilities := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer token" {
						t.Error("missing account token")
					}
					if strings.Contains(r.URL.Path, "/users/me/calendarList/") {
						capabilities++
						if r.Method != "GET" || r.URL.Path != "/users/me/calendarList/primary" {
							t.Error("wrong capability endpoint")
						}
						if mode == "capability-error" {
							w.WriteHeader(503)
							return
						}
						calendarID := "primary"
						if mode == "wrong-calendar" {
							calendarID = "other"
						}
						role := "owner"
						if mode == "read-only" {
							role = "reader"
						}
						allowed := []string{"hangoutsMeet"}
						if mode == "unsupported" {
							allowed = []string{"eventHangout"}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"id": calendarID, "accessRole": role, "conferenceProperties": map[string]any{"allowedConferenceSolutionTypes": allowed}})
						return
					}
					endpoint := "/calendars/primary/events"
					if r.Method != "POST" {
						endpoint += "/" + id
					}
					if r.URL.Path != endpoint {
						t.Errorf("wrong event target %s", r.URL.Path)
					}
					if r.Method == "GET" {
						gets++
						if writes > 0 && mode == "confirmation-failure" {
							w.WriteHeader(503)
							return
						}
						_ = json.NewEncoder(w).Encode(current)
						return
					}
					writes++
					if operation == "create" && mode == "retry" {
						saved := calendarCreatedRemote(draft)
						current["summary"], current["description"], current["location"] = saved.Summary, saved.Description, saved.Location
						current["start"] = map[string]string{"dateTime": draft.StartAt.Format("2006-01-02T15:04:05Z07:00"), "timeZone": draft.TimeZone}
						current["end"] = map[string]string{"dateTime": draft.EndAt.Format("2006-01-02T15:04:05Z07:00"), "timeZone": draft.TimeZone}
						current["conferenceData"] = json.RawMessage(testGoogleMeet)
						current["extendedProperties"] = map[string]any{"private": map[string]string{"goferCreateHash": calendarDraftHash(draft)}}
						w.WriteHeader(409)
						return
					}
					if mode == "race" {
						w.WriteHeader(412)
						return
					}
					if operation == "create" {
						if r.Method != "POST" {
							t.Error("creation must POST")
						}
					} else if r.Method != "PATCH" || r.Header.Get("If-Match") != `"v1"` || gets != 1 {
						t.Error("edit must re-read and use conditional PATCH")
					}
					if r.URL.Query().Get("conferenceDataVersion") != "1" {
						t.Error("missing conferencing version")
					}
					if mode == "guests" && r.URL.Query().Get("sendUpdates") != "all" {
						t.Error("guests not notified")
					}
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if operation == "edit" {
						if _, ok := payload["conferenceData"]; ok {
							t.Error("ordinary edit replaced the existing conference")
						}
					} else {
						data, _ := payload["conferenceData"].(map[string]any)
						request, _ := data["createRequest"].(map[string]any)
						key, _ := request["conferenceSolutionKey"].(map[string]any)
						if strings.HasPrefix(mode, "prepared") {
							if data["signature"] != "keep-me" || data["conferenceId"] != "abc-defg-hij" || data["createRequest"] != nil {
								t.Errorf("prepared conference changed: %v", data)
							}
						} else if request["requestId"] != draft.RequestID || key["type"] != "hangoutsMeet" {
							t.Errorf("missing stable Meet request: %v", payload)
						}
					}
					for k, v := range payload {
						current[k] = v
					}
					if mode == "pending" || mode == "confirmation-failure" {
						current["conferenceData"] = json.RawMessage(testGoogleMeetPending)
					} else {
						current["conferenceData"] = json.RawMessage(testGoogleMeet)
					}
					current["etag"] = `"v2"`
					switch mode {
					case "prepared-wrong-link":
						current["conferenceData"] = json.RawMessage(strings.ReplaceAll(testGoogleMeet, "abc-defg-hij", "xyz-abcd-efg"))
					case "lost-conference":
						delete(current, "conferenceData")
					case "wrong-uid":
						current["iCalUID"] = "other"
					case "wrong-id":
						current["id"] = "other"
					case "lost-description":
						current["description"] = "lost"
					}
					_ = json.NewEncoder(w).Encode(current)
				}))
				defer server.Close()
				prior := googleCalendarAPIBaseURL
				googleCalendarAPIBaseURL = server.URL
				defer func() { googleCalendarAPIBaseURL = prior }()
				var remote calendar.RemoteEvent
				var err error
				if operation == "create" {
					remote, err = createGoogleCalendarEvent(t.Context(), "token", "primary", draft)
				} else {
					remote, err = updateGoogleCalendarEvent(t.Context(), "token", "primary", existing, draft)
				}
				success := mode == "prepared" || mode == "success" || mode == "pending" || mode == "guests" || mode == "all-day" || mode == "retry" || (mode == "confirmation-failure" && operation == "create")
				if success {
					if err != nil || remote.RemoteID != id || writes != 1 || !calendarGoogleMeetJSON(remote.OnlineMeeting) || !calendarUpdateMatchesDraft(remote, draft) {
						t.Fatalf("remote=%#v err=%v writes=%d", remote, err, writes)
					}
					if mode == "pending" && calendarGoogleMeetConfirmed(remote.OnlineMeeting) {
						t.Fatal("pending link claimed ready")
					}
					if operation == "edit" && capabilities != 0 {
						t.Fatal("editing checked creation capability")
					}
				} else {
					if err == nil {
						t.Fatalf("accepted unsafe %s", mode)
					}
					afterWrite := mode == "prepared-wrong-link" || mode == "wrong-uid" || mode == "wrong-id" || mode == "lost-description" || mode == "lost-conference" || mode == "confirmation-failure" || mode == "race"
					if (writes > 0) != afterWrite {
						t.Fatalf("unexpected writes=%d err=%v", writes, err)
					}
					if mode == "race" || mode == "stale" {
						if !errors.Is(err, errCalendarUpdateConflict) {
							t.Fatalf("missing conflict: %v", err)
						}
					}
					if mode == "capability-error" && calendarCreateUncertain(err) {
						t.Fatal("capability read treated as uncertain write")
					}
				}
			})
		}
	}
}

func TestGoogleMeetHandlerCacheReplayAndEdit(t *testing.T) {
	h := calendarCreateFixture(t)
	writes := 0
	h.calendarCreateEvent = func(_ context.Context, _ storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		writes++
		remote := calendarCreatedRemote(draft)
		remote.ICalUID = "uid"
		remote.ETag = `"v1"`
		remote.ResponseStatus = "organizer"
		remote.OnlineMeeting = json.RawMessage(testGoogleMeetPending)
		return remote, nil
	}
	values := calendarCreateForm()
	values.Set("google_meet_meeting", "true")
	id := ""
	for attempt := 0; attempt < 2; attempt++ {
		w := httptest.NewRecorder()
		h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
		var result struct {
			EventID string `json:"event_id"`
			Pending bool   `json:"google_meet_unconfirmed"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		if w.Code != 201 || result.EventID == "" || !result.Pending {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if id != "" && id != result.EventID {
			t.Fatal("replay changed identity")
		}
		id = result.EventID
	}
	if writes != 1 {
		t.Fatal("replayed Meet create was sent twice")
	}
	h = calendarUpdateFixture(t)
	h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		remote := calendarCreatedRemote(draft)
		remote.RemoteID = existing.RemoteID
		remote.ICalUID = existing.ICalUID
		remote.ETag = `"v2"`
		remote.ResponseStatus = "organizer"
		remote.OrganizerEmail = "one@example.com"
		remote.Attendees = json.RawMessage(`[]`)
		remote.OnlineMeeting = json.RawMessage(testGoogleMeet)
		return remote, nil
	}
	w := httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, "one"))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	saved, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || saved.ETag != `"v2"` || !calendarGoogleMeetConfirmed(json.RawMessage(saved.OnlineMeetingJSON)) {
		t.Fatalf("saved=%#v err=%v", saved, err)
	}
	h.calendarMeetSupported = func(context.Context, storage.CalendarSource) (bool, error) {
		t.Fatal("existing Meet needs no capability check")
		return false, nil
	}
	r := httptest.NewRequest("GET", "/api/calendar/teams-options?source_id=one-source&event_id=edit-event", nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	w = httptest.NewRecorder()
	h.handleCalendarTeamsOptions(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Google Meet meeting") || !strings.Contains(w.Body.String(), "checked") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/api/calendar/teams-options?source_id=one-source&event_id=other-user-event", nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	w = httptest.NewRecorder()
	h.handleCalendarTeamsOptions(w, r)
	if w.Code != 404 {
		t.Fatal("foreign event allowed")
	}
}

func TestGoogleMeetRejectsNonGoogleBeforeWrite(t *testing.T) {
	h := calendarCreateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='outlook';UPDATE calendar_sources SET provider='outlook'`); err != nil {
		t.Fatal(err)
	}
	h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
		t.Fatal("Meet attempted on Outlook")
		return calendar.RemoteEvent{}, nil
	}
	values := calendarCreateForm()
	values.Set("google_meet_meeting", "true")
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
	if w.Code != 400 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestGoogleMeetInvitationsCanJoinAndRespondWithoutEditAccess(t *testing.T) {
	for _, response := range []string{"accepted", "tentative", "declined"} {
		t.Run(response, func(t *testing.T) { exerciseCalendarResponseProvider(t, "gmail", "event", response, "meeting") })
	}
	h := calendarResponseFixture(t, false)
	if _, err := h.db.Write().Exec(`UPDATE calendar_events SET online_meeting_json=? WHERE id='edit-event'`, testGoogleMeet); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/calendar/events/edit-event", nil)
	r.SetPathValue("id", "edit-event")
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	w := httptest.NewRecorder()
	h.handleCalendarEvent(w, r)
	for _, want := range []string{"This is a Google Meet meeting", "https://meet.google.com/abc-defg-hij", "Accept", "Maybe", "Decline"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(w.Body.String(), `href="/api/calendar/events/edit-event/edit"`) {
		t.Fatal("invitee can edit organizer event")
	}
}

func TestGoogleMeetNormalizationPreservesLegacyFallbackAndNativeConference(t *testing.T) {
	for _, tc := range []struct{ metadata, legacy, want string }{
		{testGoogleMeet, "https://meet.google.com/old-code-xyz", "https://meet.google.com/abc-defg-hij"},
		{testGoogleMeetPending, "https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij"},
		{"{}", "https://meet.google.com/abc-defg-hij", "https://meet.google.com/abc-defg-hij"},
	} {
		remote := googleCalendarEvent{ID: "event", ConferenceData: json.RawMessage(tc.metadata), HangoutLink: tc.legacy, Start: googleCalendarEventDateTime{Date: "2026-10-02"}, End: googleCalendarEventDateTime{Date: "2026-10-03"}}
		normalized, err := normalizeGoogleCalendarEvent(remote)
		if err != nil || calendar.MeetingJoinURL(string(normalized.OnlineMeeting)) != tc.want {
			t.Fatalf("event=%#v err=%v", normalized, err)
		}
		if tc.metadata == testGoogleMeet && !strings.Contains(string(normalized.OnlineMeeting), "keep-me") {
			t.Fatal("signature lost")
		}
	}
}

func TestGoogleMeetEditAccessRestrictsInvitationsAndRecurringEvents(t *testing.T) {
	h := calendarUpdateFixture(t)
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil {
		t.Fatal(err)
	}
	event.OnlineMeetingJSON = testGoogleMeet
	event.ResponseStatus = "organizer"
	if _, reason, err := h.calendarEventEditAccess(t.Context(), event); err != nil || reason != "" {
		t.Fatalf("organizer edit denied: reason=%s err=%v", reason, err)
	}
	event.ResponseStatus = "needsAction"
	if _, reason, _ := h.calendarEventEditAccess(t.Context(), event); !strings.Contains(reason, "organizer") {
		t.Fatal("invitation editable")
	}
	event.ResponseStatus = "organizer"
	event.SeriesRemoteID = "master"
	if _, reason, _ := h.calendarEventEditAccess(t.Context(), event); !strings.Contains(reason, "Recurring") {
		t.Fatal("recurring online edit available")
	}
}

func TestGoogleMeetOptionsAreScopedAndRetryable(t *testing.T) {
	h := calendarUpdateFixture(t)
	for _, mode := range []string{"available", "unsupported", "error", "foreign", "read-only"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			h.calendarMeetSupported = func(_ context.Context, source storage.CalendarSource) (bool, error) {
				calls++
				if source.ID != "one-source" {
					t.Fatal("wrong source")
				}
				if mode == "error" {
					return false, errors.New("offline")
				}
				return mode == "available", nil
			}
			if mode == "read-only" {
				if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET access_role='reader' WHERE id='one-source'`); err != nil {
					t.Fatal(err)
				}
			}
			user := "one"
			if mode == "foreign" {
				user = "two"
			}
			r := httptest.NewRequest("GET", "/api/calendar/meeting-options?source_id=one-source", nil)
			r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
			w := httptest.NewRecorder()
			h.handleCalendarTeamsOptions(w, r)
			if mode == "foreign" {
				if w.Code != 404 || calls != 0 {
					t.Fatal("foreign source read")
				}
				return
			}
			if w.Code != 200 || w.Header().Get("X-Gofer-Calendar-Source") != "one-source" || !strings.Contains(w.Body.String(), "Google Meet meeting") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if mode == "available" && !strings.Contains(w.Body.String(), `data-calendar-teams-available="true"`) {
				t.Fatal("supported Meet unavailable")
			}
			if mode == "error" && !strings.Contains(w.Body.String(), "Retry Google Meet availability check") {
				t.Fatal("availability error cannot retry")
			}
			if mode == "read-only" && calls != 0 {
				t.Fatal("read-only source checked creation")
			}
		})
	}
}
