package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarResponseProviders(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, scope := range []string{"event", "occurrence", "series"} {
			for _, response := range []string{"accepted", "tentative", "declined"} {
				t.Run(provider+"/"+scope+"/"+response, func(t *testing.T) { exerciseCalendarResponseProvider(t, provider, scope, response, "success") })
			}
		}
		for _, mode := range []string{"organizer", "unknown-self", "cancelled", "wrong-id", "wrong-parent", "missing-version", "wrong-type", "preflight-failure", "race", "denied", "server-error", "redirect", "unconfirmed", "confirmation-failure", "missing-after-decline", "unexpected-success", "all-day", "meeting", "hidden-guests", "duplicate-self", "bad-original"} {
			t.Run(provider+"/"+mode, func(t *testing.T) { exerciseCalendarResponseProvider(t, provider, "occurrence", "declined", mode) })
		}
	}
}

func exerciseCalendarResponseProvider(t *testing.T, provider, scope, response, mode string) {
	t.Helper()
	oldGoogle, oldOutlook := googleCalendarAPIBaseURL, outlookGraphBaseURL
	t.Cleanup(func() { googleCalendarAPIBaseURL, outlookGraphBaseURL = oldGoogle, oldOutlook })
	event := storage.CalendarEvent{RemoteID: "instance", ICalUID: "uid", ETag: `"v1"`}
	if scope != "event" {
		event.SeriesRemoteID = "master"
	}
	source := storage.CalendarSource{Provider: provider, RemoteID: "calendar"}
	targetID := "instance"
	if scope == "series" {
		targetID = "master"
	}
	posted, writes := false, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
			t.Error("missing auth/immutable-ID headers")
		}
		if r.Method != http.MethodGet {
			writes++
			if !strings.Contains(r.URL.Path, "/events/"+targetID) {
				t.Errorf("wrong write target %q", r.URL.Path)
			}
			etag := `"v1"`
			if provider == "outlook" {
				etag = `W/"v1"`
			}
			if r.Header.Get("If-Match") != etag {
				t.Error("write did not retain the read version")
			}
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if provider == "gmail" {
				if r.Method != http.MethodPatch || r.URL.Query().Get("sendUpdates") != "all" || len(payload) != 2 || string(payload["attendeesOmitted"]) != "true" {
					t.Errorf("unsafe Google response: %s %v", r.Method, payload)
				}
				var guests []map[string]string
				_ = json.Unmarshal(payload["attendees"], &guests)
				if len(guests) != 1 || len(guests[0]) != 2 || guests[0]["email"] != "me@example.com" || guests[0]["responseStatus"] != response {
					t.Errorf("response changed the attendee roster: %v", guests)
				}
			} else {
				action := map[string]string{"accepted": "accept", "tentative": "tentativelyAccept", "declined": "decline"}[response]
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/"+action) || len(payload) != 1 || string(payload["sendResponse"]) != "true" {
					t.Errorf("unsafe Graph response: %s %v", r.Method, payload)
				}
			}
			switch mode {
			case "race":
				w.WriteHeader(412)
				return
			case "denied":
				w.WriteHeader(403)
				return
			case "server-error":
				w.WriteHeader(503)
				return
			case "redirect":
				w.Header().Set("Location", "/unexpected")
				w.WriteHeader(307)
				return
			case "unexpected-success":
				w.WriteHeader(201)
				return
			}
			posted = true
			if provider == "outlook" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			_, _ = w.Write([]byte(`{}`)) // Only the subsequent full GET confirms the reply.
			return
		}
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if id != "instance" && id != "master" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if mode == "preflight-failure" || (posted && mode == "confirmation-failure") {
			w.WriteHeader(503)
			return
		}
		if posted && mode == "missing-after-decline" {
			w.WriteHeader(404)
			return
		}
		version, answer := "v1", "needsAction"
		if posted && mode != "unconfirmed" && id == targetID {
			version, answer = "v2", response
		}
		if provider == "gmail" {
			remote := map[string]any{"id": id, "iCalUID": "uid", "etag": `"` + version + `"`, "status": "confirmed", "summary": "Meeting", "organizer": map[string]any{"email": "host@example.com", "self": false}, "attendees": []any{map[string]any{"email": "me@example.com", "self": true, "responseStatus": answer}, map[string]any{"email": "other@example.com", "responseStatus": "accepted"}}, "start": map[string]string{"dateTime": "2026-10-03T09:00:00Z"}, "end": map[string]string{"dateTime": "2026-10-03T10:00:00Z"}}
			if id == "master" {
				remote["recurrence"] = []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,FR"}
			} else if scope != "event" {
				remote["recurringEventId"], remote["originalStartTime"] = "master", map[string]string{"dateTime": "2026-10-03T09:00:00Z"}
			}
			switch mode {
			case "organizer":
				remote["organizer"] = map[string]any{"email": "me@example.com", "self": true}
			case "unknown-self":
				remote["attendees"] = []map[string]string{{"email": "other@example.com", "responseStatus": "accepted"}}
			case "duplicate-self":
				remote["attendees"] = []map[string]any{{"email": "me@example.com", "self": true, "responseStatus": answer}, {"email": "other@example.com", "self": true, "responseStatus": "accepted"}}
			case "cancelled":
				remote["status"] = "cancelled"
			case "wrong-id":
				remote["id"] = "other"
			case "wrong-parent":
				remote["recurringEventId"] = "other"
			case "missing-version":
				remote["etag"] = "*"
			case "wrong-type":
				remote["eventType"] = "outOfOffice"
			case "bad-original":
				remote["originalStartTime"] = map[string]string{"dateTime": "invalid"}
			case "all-day":
				remote["start"], remote["end"], remote["originalStartTime"] = map[string]string{"date": "2026-10-03"}, map[string]string{"date": "2026-10-04"}, map[string]string{"date": "2026-10-03"}
			case "meeting":
				remote["conferenceData"] = json.RawMessage(testGoogleMeet)
			case "hidden-guests":
				remote["attendeesOmitted"] = true
			}
			_ = json.NewEncoder(w).Encode(remote)
		} else {
			if answer == "needsAction" {
				answer = "notResponded"
			} else if answer == "tentative" {
				answer = "tentativelyAccepted"
			}
			remote := map[string]any{"id": id, "iCalUId": "uid", "changeKey": `"` + version + `"`, "@odata.etag": `W/"` + version + `"`, "subject": "Meeting", "type": "singleInstance", "isOrganizer": false, "responseStatus": map[string]string{"response": answer}, "organizer": map[string]any{"emailAddress": map[string]string{"address": "host@example.com"}}, "attendees": []any{map[string]any{"emailAddress": map[string]string{"address": "me@example.com"}, "status": map[string]string{"response": answer}}}, "start": map[string]string{"dateTime": "2026-10-03T09:00:00", "timeZone": "UTC"}, "end": map[string]string{"dateTime": "2026-10-03T10:00:00", "timeZone": "UTC"}}
			if id == "master" {
				remote["type"], remote["recurrence"] = "seriesMaster", map[string]any{"pattern": map[string]any{"type": "weekly", "interval": 1, "daysOfWeek": []string{"monday", "friday"}}}
			} else if scope != "event" {
				remote["type"], remote["seriesMasterId"] = "occurrence", "master"
			}
			switch mode {
			case "organizer":
				remote["isOrganizer"] = true
			case "unknown-self":
				delete(remote, "isOrganizer")
			case "cancelled":
				remote["isCancelled"] = true
			case "wrong-id":
				remote["id"] = "other"
			case "wrong-parent":
				remote["seriesMasterId"] = "other"
			case "missing-version":
				delete(remote, "@odata.etag")
			case "wrong-type":
				remote["type"] = "seriesMaster"
			case "all-day":
				remote["isAllDay"], remote["start"], remote["end"] = true, map[string]string{"dateTime": "2026-10-03T00:00:00", "timeZone": "UTC"}, map[string]string{"dateTime": "2026-10-04T00:00:00", "timeZone": "UTC"}
			case "meeting":
				remote["onlineMeeting"] = map[string]string{"joinUrl": "https://example.com/join"}
			}
			_ = json.NewEncoder(w).Encode(remote)
		}
	}))
	defer server.Close()
	googleCalendarAPIBaseURL, outlookGraphBaseURL = server.URL, server.URL
	target, err := readCalendarResponseTarget(context.Background(), source, event, scope, "token")
	preflightFailure := map[string]bool{"organizer": true, "unknown-self": true, "cancelled": true, "wrong-id": true, "wrong-parent": true, "missing-version": true, "wrong-type": true, "preflight-failure": true}[mode] || (provider == "gmail" && (mode == "duplicate-self" || mode == "bad-original"))
	if preflightFailure {
		if err == nil || writes != 0 {
			t.Fatalf("unsafe preflight accepted: err=%v writes=%d", err, writes)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if target.Event.RemoteID != targetID || target.Event.ResponseStatus != "needsAction" {
		t.Fatalf("wrong response target: %#v", target.Event)
	}
	result, err := sendCalendarResponse(context.Background(), source, event, target, response)
	if writes != 1 {
		t.Fatalf("write count=%d; never retry a response", writes)
	}
	if mode == "race" {
		if !errors.Is(err, errCalendarUpdateConflict) {
			t.Fatal(err)
		}
		return
	}
	if mode == "denied" || mode == "server-error" || mode == "redirect" || mode == "unexpected-success" {
		if err == nil {
			t.Fatal("unconfirmed write was accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	pending := mode == "unconfirmed" || mode == "confirmation-failure" || mode == "missing-after-decline"
	if result.Pending != pending || (!pending && (result.Event.ResponseStatus != response || result.Event.RemoteID != targetID)) {
		t.Fatalf("result=%#v", result)
	}
}
