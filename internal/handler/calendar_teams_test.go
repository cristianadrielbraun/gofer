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

func TestCalendarTeamsDraftValidation(t *testing.T) {
	for _, value := range []string{"true", "false", "", "invalid", "on"} {
		values := calendarCreateForm()
		values.Set("teams_meeting", value)
		draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
		valid := value == "true" || value == "false" || value == ""
		if (err == nil) != valid || (valid && (!draft.TeamsMeetingSet || draft.TeamsMeeting != (value == "true"))) {
			t.Fatalf("value=%q draft=%#v err=%v", value, draft, err)
		}
	}
	values := calendarCreateForm()
	values.Set("teams_meeting", "true")
	values.Set("repeat_frequency", "daily")
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
		t.Fatal("Teams recurrence accepted")
	}
	values.Del("repeat_frequency")
	values.Add("teams_meeting", "false")
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
		t.Fatal("duplicate Teams values accepted")
	}
	draft := calendarProviderDraft(t, false)
	raw, _ := json.Marshal(draft)
	if strings.Contains(string(raw), "TeamsMeeting") {
		t.Fatal("new option changed existing appointment hashes")
	}
}

func TestOutlookTeamsCreationAndConversion(t *testing.T) {
	for _, editing := range []bool{false, true} {
		for _, mode := range []string{"success", "default-teams", "personal-success", "personal-body-link", "personal-legacy-provider", "personal-unknown-provider", "personal-no-link", "guests", "unsupported", "read-only", "capability-error", "wrong-calendar", "missing-link", "delayed-link", "wrong-uid", "body-lost", "invitee", "recurring"} {
			if !editing && (mode == "wrong-uid" || mode == "invitee") {
				continue
			}
			name := "create/" + mode
			if editing {
				name = "convert/" + mode
			}
			t.Run(name, func(t *testing.T) {
				draft := calendarProviderDraft(t, false)
				rich := "<p><strong>My description</strong></p>"
				draft.DescriptionHTML, draft.Description = &rich, calendar.DescriptionPlainText(rich)
				draft.TeamsMeeting, draft.TeamsMeetingSet = true, true
				if mode == "guests" {
					draft.GuestsSet = true
					draft.Guests = []calendar.GuestDraft{{Email: "guest@example.com"}}
				}
				if mode == "recurring" {
					draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 3}
				}
				current := map[string]any{
					"id": "event", "iCalUId": "uid", "changeKey": "v1", "@odata.etag": `W/"v1"`, "type": "singleInstance", "subject": "Before",
					"body":        outlookCalendarItemBody{ContentType: "html", Content: "<p>Before</p>"},
					"start":       map[string]string{"dateTime": "2026-10-02T07:00:00", "timeZone": "UTC"},
					"end":         map[string]string{"dateTime": "2026-10-02T08:00:00", "timeZone": "UTC"},
					"isOrganizer": mode != "invitee", "organizer": map[string]any{"emailAddress": map[string]string{"address": "owner@example.com"}},
					"isOnlineMeeting": false,
				}
				capabilities, writes, gets := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer token" {
						t.Error("missing authorization")
					}
					if r.URL.Path == "/me/calendars/primary" {
						capabilities++
						if r.Method != "GET" || r.URL.Query().Get("$select") != "id,canEdit,allowedOnlineMeetingProviders,defaultOnlineMeetingProvider" {
							t.Error("invalid capabilities read")
						}
						if mode == "capability-error" {
							w.WriteHeader(503)
							return
						}
						id := "primary"
						if mode == "wrong-calendar" {
							id = "other"
						}
						providers := []string{"teamsForBusiness"}
						if mode == "unsupported" {
							providers = []string{"skypeForBusiness"}
						}
						defaultProvider := "unknown"
						if strings.HasPrefix(mode, "personal-") {
							providers = []string{"unknown"}
						}
						if mode == "personal-legacy-provider" {
							providers = []string{"skypeForConsumer"}
							defaultProvider = "skypeForConsumer"
						}
						if mode == "default-teams" {
							providers = []string{}
							defaultProvider = "teamsForBusiness"
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "canEdit": mode != "read-only", "allowedOnlineMeetingProviders": providers, "defaultOnlineMeetingProvider": defaultProvider})
						return
					}
					if r.URL.Path != "/me/calendars/primary/events/event" && r.URL.Path != "/me/calendars/primary/events" {
						t.Errorf("wrong event endpoint %s", r.URL.Path)
					}
					if r.Method == "GET" {
						gets++
						if writes > 0 && mode == "delayed-link" {
							current["onlineMeeting"] = map[string]string{"joinUrl": "https://teams.microsoft.com/l/meetup-join/test"}
						}
						if writes > 0 && mode == "wrong-uid" {
							current["iCalUId"] = "different"
						}
						_ = json.NewEncoder(w).Encode(current)
						return
					}
					writes++
					if editing && (r.Method != "PATCH" || r.Header.Get("If-Match") != `W/"v1"` || gets != 1) {
						t.Error("conversion missing fresh read and concurrency guard")
					}
					if !editing && r.Method != "POST" {
						t.Error("creation must POST")
					}
					if capabilities != 1 {
						t.Error("write without capability check")
					}
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if payload["isOnlineMeeting"] != true {
						t.Error("Teams flags missing")
					}
					if strings.HasPrefix(mode, "personal-") {
						if _, ok := payload["onlineMeetingProvider"]; ok {
							t.Error("consumer fallback must omit business provider")
						}
						current["onlineMeetingProvider"] = "teamsForBusiness"
					} else if payload["onlineMeetingProvider"] != "teamsForBusiness" {
						t.Error("advertised business Teams provider missing")
					}
					if !editing && payload["transactionId"] != draft.RequestID {
						t.Error("creation lost idempotency")
					}
					if _, ok := payload["onlineMeeting"]; ok {
						t.Error("client supplied generated meeting metadata")
					}
					for key, value := range payload {
						current[key] = value
					}
					current["changeKey"] = "v2"
					content := rich + `<hr><p><a href="https://teams.microsoft.com/l/meetup-join/test">Join Teams meeting</a></p>`
					if mode == "body-lost" {
						content = "<p>Different description</p>"
					}
					if mode == "personal-no-link" || mode == "missing-link" {
						content = rich
					}
					current["body"] = outlookCalendarItemBody{ContentType: "html", Content: content}
					if mode != "missing-link" && mode != "delayed-link" {
						current["onlineMeeting"] = map[string]string{"joinUrl": "https://teams.microsoft.com/l/meetup-join/test"}
					}
					if mode == "personal-unknown-provider" {
						current["onlineMeetingProvider"] = "unknown"
						current["onlineMeeting"] = map[string]string{"joinUrl": "https://teams.live.com/meet/123?p=token"}
					}
					if mode == "personal-legacy-provider" {
						current["onlineMeetingProvider"] = "skypeForConsumer"
						current["onlineMeeting"] = map[string]string{"joinUrl": "https://teams.live.com/meet/123?p=token"}
					}
					if mode == "personal-no-link" {
						current["isOnlineMeeting"] = false
						current["onlineMeetingProvider"] = "unknown"
						delete(current, "onlineMeeting")
					}
					if mode == "personal-body-link" {
						current["isOnlineMeeting"] = false
						current["onlineMeetingProvider"] = "unknown"
						delete(current, "onlineMeeting")
					}
					_ = json.NewEncoder(w).Encode(current)
				}))
				defer server.Close()
				prior := outlookGraphBaseURL
				outlookGraphBaseURL = server.URL
				defer func() { outlookGraphBaseURL = prior }()
				var remote calendar.RemoteEvent
				var err error
				if editing {
					existing := storage.CalendarEvent{RemoteID: "event", ICalUID: "uid", ETag: "v1", Description: "<p>Before</p>", SourceProvider: "outlook", ResponseStatus: "organizer"}
					remote, err = updateOutlookCalendarEvent(t.Context(), "token", "primary", existing, draft)
				} else {
					remote, err = createOutlookCalendarEvent(t.Context(), "token", "primary", draft)
				}
				ok := mode == "success" || mode == "default-teams" || mode == "delayed-link" || mode == "guests" || mode == "personal-success" || mode == "personal-body-link" || mode == "personal-unknown-provider" || mode == "personal-legacy-provider"
				if ok {
					if err != nil || writes != 1 || !calendarOutlookTeamsJSON(remote.OnlineMeeting) || calendar.MeetingJoinURL(string(remote.OnlineMeeting)) == "" || !strings.Contains(remote.Description, rich) {
						t.Fatalf("remote=%#v err=%v writes=%d", remote, err, writes)
					}
					if editing && gets != 2 {
						t.Error("conversion not confirmed with fresh GET")
					}
				} else if mode == "missing-link" || mode == "personal-no-link" {
					if err != nil || writes != 1 || calendarTeamsLinkConfirmed(remote.OnlineMeeting) || remote.RemoteID != "event" || remote.ETag != "v2" {
						t.Fatalf("confirmed event without Teams must be returned for caching and warning: remote=%#v err=%v", remote, err)
					}
				} else {
					if err == nil {
						t.Fatalf("unsafe %s accepted", mode)
					}
					afterWrite := mode == "wrong-uid" || mode == "body-lost"
					if (writes != 0) != afterWrite {
						t.Fatalf("writes=%d for %s", writes, mode)
					}
					if afterWrite && !calendarCreateUncertain(err) {
						t.Error("unconfirmed write reported as definite failure")
					}
					if mode == "capability-error" && calendarCreateUncertain(err) {
						t.Error("failed capability read misreported as an uncertain write")
					}
				}
			})
		}
	}
}

func TestOutlookTeamsCapabilityDistinguishesIncompleteAndUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name, reply, mode string
		failed            bool
	}{
		{"default Teams with omitted list", `{"id":"primary","canEdit":true,"defaultOnlineMeetingProvider":"teamsForBusiness"}`, "available", false},
		{"listed Teams", `{"id":"primary","canEdit":true,"allowedOnlineMeetingProviders":["teamsForBusiness"]}`, "available", false},
		{"read-only Teams default", `{"id":"primary","canEdit":false,"defaultOnlineMeetingProvider":"teamsForBusiness"}`, "unsupported", false},
		{"missing write capability", `{"id":"primary","defaultOnlineMeetingProvider":"teamsForBusiness"}`, "", true},
		{"consumer unknown", `{"id":"primary","canEdit":true,"allowedOnlineMeetingProviders":["unknown"],"defaultOnlineMeetingProvider":"unknown"}`, "available-default", false},
		{"consumer legacy", `{"id":"primary","canEdit":true,"allowedOnlineMeetingProviders":["skypeForConsumer"],"defaultOnlineMeetingProvider":"skypeForConsumer"}`, "available-default", false},
		{"other business provider", `{"id":"primary","canEdit":true,"allowedOnlineMeetingProviders":["skypeForBusiness"]}`, "unsupported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/me/calendars/primary" {
					t.Error("capability check must not write or switch calendars")
				}
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer server.Close()
			prior := outlookGraphBaseURL
			outlookGraphBaseURL = server.URL
			defer func() { outlookGraphBaseURL = prior }()
			mode, err := outlookCalendarTeamsMode(t.Context(), "token", "primary")
			if mode != tc.mode || (err != nil) != tc.failed {
				t.Fatalf("mode=%v err=%v", mode, err)
			}
		})
	}
}

func TestCalendarTeamsOptionsAreScopedAndRetryable(t *testing.T) {
	h := calendarCreateFixture(t)
	h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
		t.Fatal("options must not write events")
		return calendar.RemoteEvent{}, nil
	}
	calls := 0
	for _, tc := range []struct {
		source, user, mode, want string
		status                   int
	}{
		{"two-source", "two", "supported", `data-calendar-teams-available="true"`, 200},
		{"two-source", "two", "unsupported", `data-calendar-teams-available="false"`, 200},
		{"two-source", "two", "error", "Retry Teams availability check", 200},
		{"one-source", "one", "supported", "", 200},
		{"two-source", "one", "supported", "", 404},
		{"two-unselected", "two", "supported", "", 404},
		{"missing", "two", "supported", "", 404},
	} {
		h.calendarTeamsSupported = func(_ context.Context, source storage.CalendarSource) (bool, error) {
			calls++
			if source.ID != "two-source" {
				t.Error("wrong capability source")
			}
			if tc.mode == "error" {
				return false, errors.New("unavailable")
			}
			return tc.mode == "supported", nil
		}
		before := calls
		r := httptest.NewRequest("GET", "/api/calendar/teams-options?source_id="+tc.source, nil)
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: tc.user}))
		w := httptest.NewRecorder()
		h.handleCalendarTeamsOptions(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%+v status=%d body=%s", tc, w.Code, w.Body.String())
		}
		if (tc.status == 404 || tc.source == "one-source") && calls != before {
			t.Error("unauthorized or non-Outlook capability lookup")
		}
		if tc.status == 200 && w.Header().Get("X-Gofer-Calendar-Source") != tc.source {
			t.Error("missing source identity for stale response guard")
		}
	}
}

func TestCalendarTeamsConversionCachesAuthoritativeJoinLink(t *testing.T) {
	h := calendarUpdateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='outlook' WHERE id='one-account'; UPDATE calendar_sources SET provider='outlook' WHERE id='one-source'`); err != nil {
		t.Fatal(err)
	}
	h.calendarUpdateEvent = func(_ context.Context, source storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		if source.Provider != "outlook" || !draft.TeamsMeeting {
			t.Fatal("lost Teams conversion intent")
		}
		remote := calendarCreatedRemote(draft)
		remote.RemoteID, remote.ICalUID, remote.ETag = existing.RemoteID, existing.ICalUID, "v2"
		remote.ResponseStatus = "organizer"
		remote.OrganizerEmail = "one@example.com"
		remote.Attendees = json.RawMessage(`[]`)
		remote.OnlineMeeting = json.RawMessage(`{"provider":"teamsForBusiness","isOnlineMeeting":true,"joinUrl":"https://teams.microsoft.com/l/meetup-join/new"}`)
		return remote, nil
	}
	values := calendarCreateForm()
	values.Set("teams_meeting", "true")
	w := httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, "one"))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || event.ETag != "v2" || calendar.MeetingJoinURL(event.OnlineMeetingJSON) != "https://teams.microsoft.com/l/meetup-join/new" {
		t.Fatalf("cache=%#v err=%v", event, err)
	}
	h.calendarTeamsSupported = func(context.Context, storage.CalendarSource) (bool, error) {
		t.Fatal("existing meeting must not check creation capabilities")
		return false, nil
	}
	r := httptest.NewRequest("GET", "/api/calendar/teams-options?source_id=one-source&event_id=edit-event", nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	w = httptest.NewRecorder()
	h.handleCalendarTeamsOptions(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "checked") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/api/calendar/teams-options?source_id=one-source&event_id=another-users-event", nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	w = httptest.NewRecorder()
	h.handleCalendarTeamsOptions(w, r)
	if w.Code != 404 {
		t.Fatal("unscoped event capability read")
	}
}

func TestCalendarTeamsRejectsNonOutlookBeforeWrite(t *testing.T) {
	h := calendarCreateFixture(t)
	h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
		t.Fatal("Teams attempted on Google")
		return calendar.RemoteEvent{}, nil
	}
	values := calendarCreateForm()
	values.Set("teams_meeting", "true")
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Teams meetings require") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCalendarTeamsMissingLinkCachesCreateAndReplayWithoutDuplicate(t *testing.T) {
	h := calendarCreateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='outlook' WHERE id='one-account'; UPDATE calendar_sources SET provider='outlook' WHERE id='one-source'`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h.calendarCreateEvent = func(_ context.Context, _ storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		calls++
		remote := calendarCreatedRemote(draft)
		remote.ETag = "v1"
		remote.ICalUID = "uid"
		remote.OnlineMeeting = json.RawMessage(`{}`)
		return remote, nil
	}
	values := calendarCreateForm()
	values.Set("teams_meeting", "true")
	id := ""
	for attempt := 0; attempt < 2; attempt++ {
		w := httptest.NewRecorder()
		h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
		var result struct {
			EventID     string `json:"event_id"`
			Unconfirmed bool   `json:"teams_unconfirmed"`
			Replayed    bool   `json:"replayed"`
			Uncertain   bool   `json:"uncertain"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusCreated || result.EventID == "" || !result.Unconfirmed || result.Uncertain || result.Replayed != (attempt == 1) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if attempt == 0 {
			id = result.EventID
		} else if result.EventID != id {
			t.Fatal("replay returned a different event")
		}
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", id)
	if err != nil || event.Summary != "Planning" || calls != 1 || calendar.MeetingJoinURL(event.OnlineMeetingJSON) != "" {
		t.Fatalf("event=%#v err=%v provider writes=%d", event, err, calls)
	}
}

func TestCalendarTeamsMissingLinkCachesConfirmedEdit(t *testing.T) {
	h := calendarUpdateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE accounts SET provider='outlook' WHERE id='one-account'; UPDATE calendar_sources SET provider='outlook' WHERE id='one-source'`); err != nil {
		t.Fatal(err)
	}
	h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		remote := calendarCreatedRemote(draft)
		remote.RemoteID, remote.ICalUID, remote.ETag = existing.RemoteID, existing.ICalUID, "v2"
		remote.ResponseStatus = "organizer"
		remote.OnlineMeeting = json.RawMessage(`{}`)
		return remote, nil
	}
	values := calendarCreateForm()
	values.Set("teams_meeting", "true")
	w := httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, "one"))
	var result struct {
		Saved       bool `json:"saved"`
		Unconfirmed bool `json:"teams_unconfirmed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !result.Saved || !result.Unconfirmed {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || event.ETag != "v2" || event.Summary != "Planning" || calendar.MeetingJoinURL(event.OnlineMeetingJSON) != "" {
		t.Fatalf("event=%#v err=%v", event, err)
	}
}

func TestCalendarTeamsConsumerLinkIdentity(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://teams.live.com/meet/123?p=token", true},
		{"https://teams.microsoft.com/l/meetup-join/123", true},
		{"https://teams.live.com.evil.example/meet/123", false},
		{"https://meet.google.com/123", false},
		{"http://teams.live.com/meet/123", false},
		{"https://user@teams.live.com/meet/123", false},
	} {
		raw, _ := json.Marshal(map[string]any{"provider": "unknown", "isOnlineMeeting": true, "joinUrl": tc.url})
		if calendarTeamsLinkConfirmed(raw) != tc.want {
			t.Errorf("incorrect consumer Teams recognition for %s", tc.url)
		}
		raw, _ = json.Marshal(map[string]any{"provider": "unknown", "isOnlineMeeting": false, "joinUrl": tc.url})
		if calendarTeamsLinkConfirmed(raw) {
			t.Error("explicitly disabled meeting recognized as Teams")
		}
	}
}

func TestOutlookTeamsRejectsChangingExistingOnlineProvider(t *testing.T) {
	for _, mode := range []string{"disable", "change-provider"} {
		t.Run(mode, func(t *testing.T) {
			draft := calendarProviderDraft(t, false)
			draft.TeamsMeetingSet = true
			draft.TeamsMeeting = mode == "change-provider"
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
					t.Error("irreversible meeting change attempted")
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "event", "isOnlineMeeting": true, "onlineMeetingProvider": "skypeForBusiness"})
			}))
			defer server.Close()
			prior := outlookGraphBaseURL
			outlookGraphBaseURL = server.URL
			defer func() { outlookGraphBaseURL = prior }()
			_, err := updateOutlookCalendarEvent(t.Context(), "token", "primary", storage.CalendarEvent{RemoteID: "event", SourceProvider: "outlook"}, draft)
			if !errors.Is(err, errCalendarUpdateUnsupported) || writes != 0 {
				t.Fatalf("err=%v writes=%d", err, writes)
			}
		})
	}
}
