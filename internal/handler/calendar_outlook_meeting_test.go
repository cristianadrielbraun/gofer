package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const originalOnlineBody = `<!DOCTYPE html><html><head><style>.meeting{color:black}</style></head><body><p>Original organizer notes: reunión</p><!-- opaque meeting metadata --><div class="meeting" data-meeting="123"><a href="https://teams.live.com/meet/123?p=token&amp;lang=es">Unirse a la reunión</a><span>Meeting ID: 123</span></div></body></html>`

func TestOutlookConsumerMeetingBodyFallbackAndMetadataPriority(t *testing.T) {
	for _, tc := range []struct {
		name, provider, metadata, legacy string
		online                           bool
		want                             string
	}{
		{"consumer pending", "unknown", `null`, "", false, "https://teams.live.com/meet/123?p=token&lang=es"},
		{"business pending", "teamsForBusiness", `null`, "", true, "https://teams.live.com/meet/123?p=token&lang=es"},
		{"structured wins", "teamsForBusiness", `{"joinUrl":"https://teams.microsoft.com/l/meetup-join/actual"}`, "", true, "https://teams.microsoft.com/l/meetup-join/actual"},
		{"legacy wins", "unknown", `null`, "https://teams.live.com/meet/legacy", true, "https://teams.live.com/meet/legacy"},
		{"explicitly disabled business", "teamsForBusiness", `null`, "", false, ""},
		{"explicitly disabled structured", "unknown", `{"joinUrl":"https://teams.live.com/meet/old"}`, "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := calendarOutlookMeetingJSON(outlookCalendarEvent{IsOnlineMeeting: &tc.online, OnlineMeetingProvider: tc.provider, OnlineMeeting: json.RawMessage(tc.metadata), OnlineMeetingURL: tc.legacy, Body: outlookCalendarItemBody{Content: originalOnlineBody, ContentType: "html"}})
			if got := calendar.MeetingJoinURL(string(raw)); got != tc.want {
				t.Fatalf("got=%q want=%q metadata=%s", got, tc.want, raw)
			}
		})
	}
}

func TestOutlookCachedConsumerMeetingShowsJoinWithoutSync(t *testing.T) {
	h := calendarUpdateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='outlook' WHERE id='one-account'; UPDATE calendar_sources SET provider='outlook' WHERE id='one-source'; UPDATE calendar_events SET description=?, online_meeting_json='{}', response_status='organizer', organizer_email='one@example.com' WHERE id='edit-event'`, originalOnlineBody); err != nil {
		t.Fatal(err)
	}
	r := calendarUpdateRequest(calendarCreateForm(), "one")
	r.Method = "GET"
	w := httptest.NewRecorder()
	h.handleCalendarEvent(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "data-calendar-join-meeting") || !strings.Contains(w.Body.String(), `href="https://teams.live.com/meet/123?p=token&amp;lang=es"`) {
		t.Fatalf("cached event lost Join button: status=%d body=%s", w.Code, w.Body.String())
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || !calendarStoredOutlookOnline(event) || !calendarOutlookTeamsJSON(calendarOutlookCachedMeetingJSON(event)) {
		t.Fatal("cached body-only Teams meeting not recognized", err)
	}
	h.calendarTeamsSupported = func(context.Context, storage.CalendarSource) (bool, error) {
		t.Fatal("existing cached meeting must not recheck creation capability")
		return false, nil
	}
	r.URL.RawQuery = "source_id=one-source&event_id=edit-event"
	w = httptest.NewRecorder()
	h.handleCalendarTeamsOptions(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "checked") || strings.Contains(w.Body.String(), `name="teams_meeting"`) {
		t.Fatalf("cached Teams toggle not recognized: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestOutlookOnlineMeetingFullDescriptionUpdate(t *testing.T) {
	for _, mode := range []string{"title-time", "full-description", "clear-description", "keep-link", "disable-meeting", "changed-link", "guests", "stale", "race", "invitee", "recurrence", "wrong-uid", "body-not-applied", "confirmation-failed"} {
		t.Run(mode, func(t *testing.T) {
			draft := calendarProviderDraft(t, false)
			rich := calendar.DescriptionHTML(originalOnlineBody)
			changesBody := mode == "full-description" || mode == "clear-description" || mode == "keep-link" || mode == "disable-meeting" || mode == "changed-link" || mode == "body-not-applied"
			if changesBody {
				rich = "<p><strong>Completely rewritten</strong></p>"
			}
			if mode == "clear-description" {
				rich = ""
			}
			draft.DescriptionHTML, draft.Description = &rich, calendar.DescriptionPlainText(rich)
			if mode == "recurrence" {
				draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 3}
			}
			if mode == "guests" {
				draft.GuestsSet = true
				draft.Guests = []calendar.GuestDraft{{Email: "new@example.com"}}
			}
			existing := storage.CalendarEvent{RemoteID: "event", ICalUID: "uid", ETag: "v1", SourceProvider: "outlook", Description: originalOnlineBody, OnlineMeetingJSON: `{"joinUrl":"https://teams.live.com/meet/123"}`, ResponseStatus: "organizer", OrganizerEmail: "owner@example.com"}
			current := map[string]any{"id": "event", "iCalUId": "uid", "changeKey": "v1", "@odata.etag": `W/"v1"`, "type": "singleInstance", "subject": "Before", "body": outlookCalendarItemBody{ContentType: "html", Content: originalOnlineBody}, "location": map[string]string{"displayName": ""},
				"start": map[string]string{"dateTime": "2026-10-02T07:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-02T08:00:00", "timeZone": "UTC"},
				"isOrganizer": true, "organizer": map[string]any{"emailAddress": map[string]string{"address": "owner@example.com"}},
				"attendees": []any{}, "isOnlineMeeting": true, "onlineMeetingProvider": "teamsForBusiness", "onlineMeeting": map[string]string{"joinUrl": "https://teams.live.com/meet/123", "conferenceId": "123"}}
			if mode == "stale" {
				current["changeKey"] = "newer"
			}
			if mode == "invitee" {
				current["isOrganizer"] = false
			}
			gets, patches := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer token" || r.URL.Path != "/me/calendars/primary/events/event" {
					t.Error("wrong authenticated target")
				}
				if r.Method == "GET" {
					gets++
					if gets > 1 {
						switch mode {
						case "confirmation-failed":
							w.WriteHeader(503)
							return
						case "disable-meeting":
							current["isOnlineMeeting"] = false
							current["onlineMeeting"] = nil
							current["onlineMeetingProvider"] = "unknown"
						case "changed-link":
							current["onlineMeeting"] = map[string]string{"joinUrl": "https://teams.live.com/meet/new"}
						case "wrong-uid":
							current["iCalUId"] = "different"
						case "body-not-applied":
							current["body"] = outlookCalendarItemBody{ContentType: "html", Content: originalOnlineBody}
						}
					}
					_ = json.NewEncoder(w).Encode(current)
					return
				}
				if r.Method != "PATCH" {
					t.Error("unexpected method")
					w.WriteHeader(405)
					return
				}
				patches++
				if gets != 1 || r.Header.Get("If-Match") != `W/"v1"` {
					t.Error("missing version guard")
				}
				if mode == "race" {
					w.WriteHeader(412)
					return
				}
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				for _, key := range []string{"onlineMeeting", "isOnlineMeeting", "onlineMeetingProvider", "onlineMeetingUrl", "organizer", "isReminderOn"} {
					if _, ok := payload[key]; ok {
						t.Errorf("description edit overwrote property %s", key)
					}
				}
				raw, bodyWritten := payload["body"]
				if bodyWritten != changesBody {
					t.Errorf("body write=%v for %s", bodyWritten, mode)
				}
				if bodyWritten {
					var body outlookCalendarItemBody
					_ = json.Unmarshal(raw, &body)
					if body.ContentType != "html" || body.Content != rich || strings.Contains(body.Content, "gofer-calendar-notes") {
						t.Error("full description was not submitted directly")
					}
				}
				for key, value := range payload {
					var decoded any
					_ = json.Unmarshal(value, &decoded)
					current[key] = decoded
				}
				current["changeKey"], current["@odata.etag"] = "v2", `W/"v2"`
				_ = json.NewEncoder(w).Encode(current)
			}))
			defer server.Close()
			old := outlookGraphBaseURL
			outlookGraphBaseURL = server.URL
			defer func() { outlookGraphBaseURL = old }()
			remote, err := updateOutlookCalendarEvent(context.Background(), "token", "primary", existing, draft)
			preflight := mode == "stale" || mode == "invitee" || mode == "recurrence"
			failed := preflight || mode == "race" || mode == "wrong-uid" || mode == "body-not-applied" || mode == "confirmation-failed"
			if failed {
				if err == nil {
					t.Fatal("unconfirmed/conflicting edit accepted")
				}
				if preflight && patches != 0 {
					t.Fatal("preflight wrote event")
				}
				if mode == "race" && !errors.Is(err, errCalendarUpdateConflict) {
					t.Fatal("version conflict lost")
				}
				return
			}
			if err != nil || gets != 2 || patches != 1 || remote.ETag != "v2" || calendar.DescriptionHTML(remote.Description) != rich {
				t.Fatalf("full description update %s: %+v %v", mode, remote, err)
			}
			join := calendar.MeetingJoinURL(string(remote.OnlineMeeting))
			if mode == "disable-meeting" {
				if join != "" {
					t.Fatal("disabled meeting retained stale join link")
				}
			} else if mode == "changed-link" {
				if join != "https://teams.live.com/meet/new" {
					t.Fatal("did not use confirmed join link", join)
				}
			} else if join != "https://teams.live.com/meet/123" {
				t.Fatal("independent join URL lost", join)
			}
		})
	}
}

func TestOutlookOnlineMeetingEditorUsesFullDescription(t *testing.T) {
	h := calendarUpdateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='outlook' WHERE id='one-account'; UPDATE calendar_sources SET provider='outlook' WHERE id='one-source'; UPDATE calendar_events SET online_meeting_json='{"joinUrl":"https://teams.live.com/meet/123"}',response_status='organizer',organizer_email='one@example.com',description=? WHERE id='edit-event'`, originalOnlineBody); err != nil {
		t.Fatal(err)
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil {
		t.Fatal(err)
	}
	if _, reason, err := h.calendarEventEditAccess(t.Context(), event); err != nil || reason != "" {
		t.Fatal("organizer cannot edit online meeting", reason, err)
	}
	if _, reason, _ := h.calendarEventDeleteAccess(t.Context(), event); reason == "" {
		t.Fatal("deletion was accidentally enabled")
	}
	r := calendarUpdateRequest(calendarCreateForm(), "one")
	r.Method = "GET"
	w := httptest.NewRecorder()
	h.handleEditCalendarEvent(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "Additional notes") || strings.Contains(w.Body.String(), "data-calendar-original-description") || strings.Contains(w.Body.String(), `contenteditable="false"`) || !strings.Contains(w.Body.String(), "Original organizer notes: reunión") || !strings.Contains(w.Body.String(), "https://teams.live.com/meet/123") || !strings.Contains(w.Body.String(), "data-calendar-rich-editor") || strings.Contains(w.Body.String(), "opaque meeting metadata") {
		t.Fatalf("full description editor %d %s", w.Code, w.Body.String())
	}
	for _, mode := range []string{"guest", "recurring"} {
		copy := event
		if mode == "guest" {
			copy.ResponseStatus = "needsAction"
		} else {
			copy.SeriesRemoteID = "master"
		}
		if _, reason, _ := h.calendarEventEditAccess(t.Context(), copy); reason == "" {
			t.Fatalf("unsupported %s online edit allowed", mode)
		}
	}
	// Description and authoritative conferencing metadata share one cache version.
	disabled := false
	h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, e storage.CalendarEvent, d calendar.EventDraft) (calendar.RemoteEvent, error) {
		remote := calendarCreatedRemote(d)
		remote.RemoteID = e.RemoteID
		remote.ICalUID = e.ICalUID
		remote.ETag = `"v2"`
		remote.OnlineMeeting = json.RawMessage(e.OnlineMeetingJSON)
		if disabled {
			remote.OnlineMeeting = json.RawMessage(`{}`)
			remote.ETag = `"v3"`
		}
		remote.Attendees = json.RawMessage(e.AttendeesJSON)
		if d.GuestsSet {
			remote.Attendees, _ = json.Marshal(calendarMeetingOutlookGuests(d, remote.Attendees))
		}
		remote.Description = calendar.DraftDescription(d)
		remote.ResponseStatus = "organizer"
		remote.OrganizerEmail = e.OrganizerEmail
		return remote, nil
	}
	w = httptest.NewRecorder()
	values := calendarCreateForm()
	values.Set("description_html", "<p>Entire description rewritten</p>")
	values.Set("guests", "new@example.com")
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, "one"))
	if w.Code != 200 {
		t.Fatalf("preserved online cache rejected %d %s", w.Code, w.Body.String())
	}
	stored, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || stored.Description != "<p>Entire description rewritten</p>" || !strings.Contains(stored.OnlineMeetingJSON, "teams.live.com") || !strings.Contains(stored.AttendeesJSON, "new@example.com") {
		t.Fatal("cache lost meeting details", err)
	}
	detailsRequest := calendarUpdateRequest(values, "one")
	detailsRequest.Method = "GET"
	detailsResponse := httptest.NewRecorder()
	h.handleCalendarEvent(detailsResponse, detailsRequest)
	if detailsResponse.Code != 200 || !strings.Contains(detailsResponse.Body.String(), `data-calendar-join-meeting`) || !strings.Contains(detailsResponse.Body.String(), `href="https://teams.live.com/meet/123"`) {
		t.Fatal("event details lost independent join link", detailsResponse.Body.String())
	}
	bad := stored
	bad.Summary, bad.ETag = "Should not change", `"v3"`
	if err := h.db.CompleteCalendarOutlookOnlineUpdate(t.Context(), event, bad); !errors.Is(err, storage.ErrCalendarUpdateConflict) {
		t.Fatal("stale online cache update allowed", err)
	}
	unchanged, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || unchanged.Summary != stored.Summary || unchanged.AttendeesJSON != stored.AttendeesJSON || unchanged.ETag != stored.ETag {
		t.Fatal("failed save partially altered the cache", err)
	}
	// A deliberate body rewrite may disable conferencing remotely. Do not keep
	// the old join URL even when the confirmed meeting JSON becomes empty.
	disabled = true
	values.Set("description_html", "")
	values.Set("version", stored.ETag)
	w = httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, "one"))
	if w.Code != 200 {
		t.Fatalf("disabled meeting save %d %s", w.Code, w.Body.String())
	}
	cleared, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || cleared.Description != "" || cleared.OnlineMeetingJSON != "{}" || calendar.MeetingJoinURL(cleared.OnlineMeetingJSON) != "" || cleared.ETag != `"v3"` {
		t.Fatal("disabled meeting kept stale cache/link", cleared, err)
	}
	detailsResponse = httptest.NewRecorder()
	h.handleCalendarEvent(detailsResponse, detailsRequest)
	if detailsResponse.Code != 200 || strings.Contains(detailsResponse.Body.String(), "data-calendar-join-meeting") {
		t.Fatal("disabled meeting still shows Join", detailsResponse.Body.String())
	}
}

func TestOutlookOnlineFlagIsRetainedInCache(t *testing.T) {
	for _, flags := range []string{`"isOnlineMeeting":true`, `"onlineMeetingProvider":"teamsForBusiness"`, `"onlineMeetingUrl":"https://teams.live.com/meet/123"`} {
		var remote outlookCalendarEvent
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"id":"event","start":{"dateTime":"2026-10-03T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-03T10:00:00","timeZone":"UTC"},"onlineMeeting":null,%s}`, flags)), &remote); err != nil {
			t.Fatal(err)
		}
		event, err := normalizeOutlookCalendarEvent(remote)
		if err != nil || !calendarUpdateHasDetails(event.OnlineMeeting) {
			t.Fatal("online meeting metadata disappeared", flags, err)
		}
	}
}
