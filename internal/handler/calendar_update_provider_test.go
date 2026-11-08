package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

func TestCalendarJSONProviderUpdateConditionalAndPreserving(t *testing.T) {
	for _, graph := range []bool{false, true} {
		for _, mode := range []string{"timed", "all-day", "stale", "race", "guests", "unconfirmed", "recurring", "recurring-all-day", "recurring-mismatch", "recurring-missing", "already-recurring", "series", "series-all-day", "series-stale", "series-instance", "series-complex", "series-guests", "series-organizer-guests", "series-guest-change", "recurring-organizer-guests", "series-meeting", "series-remove-meet", "series-race"} {
			t.Run(fmt.Sprintf("graph=%v/%s", graph, mode), func(t *testing.T) {
				draft := calendarProviderDraft(t, strings.HasSuffix(mode, "all-day"))
				series := strings.HasPrefix(mode, "series")
				converting := strings.HasPrefix(mode, "recurring") || mode == "already-recurring" || series
				if converting {
					draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "weekly", Interval: 2, Count: 4}
				}
				draft.Description, draft.Location = "Rich notes", "Room 2"
				existing := storage.CalendarEvent{RemoteID: "event", ETag: `"v1"`, Description: "<b>Rich notes</b>", Location: "Room 2"}
				current := map[string]any{
					"id": "event", "etag": `"v1"`, "summary": "Before", "description": existing.Description, "location": existing.Location,
					"start":     map[string]any{"dateTime": "2026-10-02T07:00:00Z", "timeZone": "UTC"},
					"end":       map[string]any{"dateTime": "2026-10-02T08:00:00Z", "timeZone": "UTC"},
					"attendees": []any{}, "recurrence": []any{}, "reminders": map[string]any{"useDefault": false},
				}
				if graph {
					existing.ETag = "v1"
					current = map[string]any{
						"id": "event", "changeKey": "v1", "@odata.etag": `W/"v1"`, "type": "singleInstance", "subject": "Before",
						"body":      map[string]any{"contentType": "html", "content": existing.Description},
						"location":  map[string]any{"displayName": existing.Location, "address": map[string]string{"city": "Prague"}},
						"start":     map[string]any{"dateTime": "2026-10-02T07:00:00", "timeZone": "UTC"},
						"end":       map[string]any{"dateTime": "2026-10-02T08:00:00", "timeZone": "UTC"},
						"attendees": []any{}, "recurrence": nil, "isReminderOn": true, "reminderMinutesBeforeStart": 15,
					}
				}
				if mode == "stale" {
					current["etag"], current["changeKey"] = `"newer"`, "newer"
				}
				if mode == "guests" {
					current["attendees"] = []any{map[string]string{"email": "guest@example.com"}}
				}
				if mode == "already-recurring" {
					current["recurrence"] = []string{"RRULE:FREQ=DAILY"}
					if graph {
						current["recurrence"], current["type"] = map[string]any{"pattern": map[string]any{"type": "daily", "interval": 1}}, "seriesMaster"
					}
				}
				if series {
					before := calendarProviderDraft(t, false)
					before.TimeZone = "UTC"
					before.Recurrence = &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 6}
					current["recurrence"] = []string{"RRULE:" + calendarRecurrenceRule(before)}
					if graph {
						current["recurrence"], current["type"] = calendarOutlookRecurrence(before), "seriesMaster"
					}
					original := calendarCreatedRemote(before)
					original.RemoteID, original.ETag = existing.RemoteID, existing.ETag
					original.Recurrence, _ = json.Marshal(current["recurrence"])
					existing = calendarStorageEvent("one", "source", original)
					existing.Description, existing.Location = "<b>Rich notes</b>", "Room 2"
					if mode == "series-stale" {
						current["etag"], current["changeKey"] = `"v-new"`, "v-new"
					}
					if mode == "series-instance" {
						current["recurringEventId"], current["seriesMasterId"], current["type"] = "parent", "parent", "occurrence"
					}
					if mode == "series-guests" {
						current["attendees"] = []any{map[string]string{"email": "guest@example.com"}}
					}
					if mode == "series-complex" {
						if graph {
							current["recurrence"].(map[string]any)["pattern"] = map[string]any{"type": "relativeMonthly", "interval": 1, "index": "first", "daysOfWeek": []string{"monday"}}
						} else {
							current["recurrence"] = []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,WE"}
						}
					}
				}
				// The organizer of a recurring meeting can change the series or turn
				// a meeting into one; Google and Outlook send guests the update.
				organizerGuests := mode == "series-organizer-guests" || mode == "series-guest-change" || mode == "recurring-organizer-guests"
				if organizerGuests {
					current["organizer"], current["isOrganizer"] = map[string]any{"self": true, "email": "owner@example.com", "emailAddress": map[string]string{"address": "owner@example.com"}}, true
					current["attendees"] = []any{map[string]any{"email": "guest@example.com", "emailAddress": map[string]string{"address": "guest@example.com"}, "type": "required"}}
					existing.ResponseStatus, existing.AttendeesJSON = "organizer", `[{"email":"guest@example.com"}]`
				}
				// A Meet or Teams series keeps its meeting through edits; Meet can be
				// removed from the whole series.
				meeting := mode == "series-meeting" || mode == "series-remove-meet"
				if meeting {
					current["organizer"], current["isOrganizer"] = map[string]any{"self": true, "email": "owner@example.com"}, true
					existing.ResponseStatus = "organizer"
					if graph {
						current["isOnlineMeeting"], current["onlineMeetingProvider"] = true, "teamsForBusiness"
						current["onlineMeeting"] = map[string]string{"joinUrl": "https://teams.microsoft.com/l/meetup-join/series"}
						existing.SourceProvider, existing.OnlineMeetingJSON = "outlook", `{"joinUrl":"https://teams.microsoft.com/l/meetup-join/series"}`
					} else {
						current["iCalUID"], current["conferenceData"] = "uid", json.RawMessage(testGoogleMeet)
						existing.SourceProvider, existing.OnlineMeetingJSON, existing.ICalUID = "gmail", testGoogleMeet, "uid"
					}
				}
				removingMeet := mode == "series-remove-meet" && !graph
				if removingMeet {
					draft.GoogleMeetMeetingSet = true
				}
				if mode == "series-guest-change" {
					draft.GuestsSet = true
					draft.Guests = []calendar.GuestDraft{{Email: "second@example.com"}}
				}
				gets, patches := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer test-token" {
						t.Error("missing account authorization")
					}
					if !strings.HasSuffix(r.URL.Path, "/calendars/work/events/event") {
						t.Errorf("wrong calendar/event target: %s", r.URL.Path)
					}
					if r.Method == http.MethodGet {
						gets++
						_ = json.NewEncoder(w).Encode(current)
						return
					}
					if r.Method != http.MethodPatch {
						t.Errorf("unexpected write method %s", r.Method)
						w.WriteHeader(405)
						return
					}
					patches++
					tag := `"v1"`
					if graph {
						tag = `W/"v1"`
					}
					if gets != 1 || r.Header.Get("If-Match") != tag {
						t.Error("update missing re-read and atomic provider version")
					}
					if mode == "race" || mode == "series-race" {
						w.WriteHeader(412)
						return
					}
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if !graph && organizerGuests != (r.URL.Query().Get("sendUpdates") == "all") {
						t.Errorf("guest notification mismatch: %s", r.URL.RawQuery)
					}
					for _, key := range []string{"attendees", "recurrence", "reminders", "isReminderOn", "reminderMinutesBeforeStart", "description", "body"} {
						if key == "recurrence" && converting {
							raw, _ := json.Marshal(payload[key])
							if !calendarRecurrenceMatchesDraft(raw, draft) {
								t.Errorf("repeat settings missing from update: %s", raw)
							}
							continue
						}
						if key == "attendees" && mode == "series-guest-change" {
							if _, exists := payload[key]; !exists {
								t.Error("changed guests missing from update")
							}
							continue
						}
						if _, exists := payload[key]; exists {
							t.Errorf("title/time edit overwrites unrelated %s", key)
						}
					}
					if graph {
						if _, exists := payload["location"]; exists {
							t.Error("unchanged structured location overwritten")
						}
					}
					if value, exists := payload["conferenceData"]; exists != removingMeet || (exists && value != nil) {
						t.Errorf("series conference mismatch: %v", payload["conferenceData"])
					}
					if !graph && meeting && r.URL.Query().Get("conferenceDataVersion") != "1" {
						t.Error("Meet series edit without conferencing version")
					}
					for key, value := range payload {
						current[key] = value
					}
					if removingMeet {
						delete(current, "conferenceData")
					}
					if converting && graph {
						current["type"] = "seriesMaster"
					}
					if mode == "recurring-missing" {
						delete(current, "recurrence")
					}
					if mode == "recurring-mismatch" {
						if graph {
							current["recurrence"].(map[string]any)["range"].(map[string]any)["numberOfOccurrences"] = 99
						} else {
							current["recurrence"] = []string{"RRULE:FREQ=DAILY;COUNT=99"}
						}
					}
					current["etag"], current["changeKey"], current["@odata.etag"] = `"v2"`, "v2", `W/"v2"`
					if mode == "unconfirmed" {
						_, _ = io.WriteString(w, "malformed")
						return
					}
					_ = json.NewEncoder(w).Encode(current)
				}))
				defer server.Close()
				var remote calendar.RemoteEvent
				var err error
				if graph {
					old := outlookGraphBaseURL
					outlookGraphBaseURL = server.URL
					defer func() { outlookGraphBaseURL = old }()
					remote, err = updateOutlookCalendarEventScope(davTestContext(t), "test-token", "work", existing, draft, series)
				} else {
					old := googleCalendarAPIBaseURL
					googleCalendarAPIBaseURL = server.URL
					defer func() { googleCalendarAPIBaseURL = old }()
					remote, err = updateGoogleCalendarEventScope(davTestContext(t), "test-token", "work", existing, draft, series)
				}
				switch mode {
				case "stale", "race", "series-stale", "series-race":
					if !errors.Is(err, errCalendarUpdateConflict) {
						t.Fatalf("expected conflict, got %v", err)
					}
					if strings.HasSuffix(mode, "stale") && patches != 0 {
						t.Error("stale version was written")
					}
				case "guests", "already-recurring", "series-instance", "series-complex", "series-guests":
					if !errors.Is(err, errCalendarUpdateUnsupported) || patches != 0 {
						t.Fatalf("meeting update: patches=%d err=%v", patches, err)
					}
				case "unconfirmed", "recurring-missing", "recurring-mismatch":
					if err == nil || !calendarCreateUncertain(err) {
						t.Fatalf("ambiguous success reported as confirmed: %v", err)
					}
				default:
					if err != nil || remote.RemoteID != "event" || patches != 1 || remote.ETag == existing.ETag || !calendarUpdateMatchesDraft(remote, draft) {
						t.Fatalf("remote=%#v patches=%d err=%v", remote, patches, err)
					}
					if converting && !calendarRecurrenceMatchesDraft(remote.Recurrence, draft) {
						t.Fatal("conversion response lost recurrence")
					}
					if organizerGuests && !draft.GuestsSet && !calendarMeetingGuestsMatch(remote.Attendees, []calendar.GuestDraft{{Email: "guest@example.com"}}, "owner@example.com") {
						t.Fatalf("series guests lost: %s", remote.Attendees)
					}
					if meeting && (calendar.MeetingJoinURL(string(remote.OnlineMeeting)) == "") != removingMeet {
						t.Fatalf("series meeting after save: %s", remote.OnlineMeeting)
					}
					if mode == "series-guest-change" && !calendarMeetingGuestsMatch(remote.Attendees, draft.Guests, "owner@example.com") {
						t.Fatalf("series guests not saved: %s", remote.Attendees)
					}
				}
			})
		}
	}
}

func TestCalendarCalDAVUpdatePreservesResourceAndConfirmsSave(t *testing.T) {
	for _, mode := range []string{"timed", "all-day", "race", "confirmation-failure", "confirmation-mismatch", "guests", "recurring", "recurring-all-day", "recurring-missing", "recurring-mismatch", "already-recurring", "series", "series-all-day", "series-exceptions", "series-dates-title", "series-dates-reset", "series-complex", "series-instance", "series-race"} {
		t.Run(mode, func(t *testing.T) {
			draft := calendarProviderDraft(t, strings.HasSuffix(mode, "all-day"))
			series := strings.HasPrefix(mode, "series")
			converting := strings.HasPrefix(mode, "recurring") || mode == "already-recurring" || series
			if converting {
				draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "weekly", Interval: 2, Count: 4}
			}
			before := calendarProviderDraft(t, false)
			if series {
				before.Recurrence = &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 6}
			}
			raw, err := calendarCreateICS(before)
			if err != nil {
				t.Fatal(err)
			}
			raw = strings.Replace(raw, "END:VEVENT", "X-PRESERVE:keep-me\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nDESCRIPTION:Reminder\r\nEND:VALARM\r\nEND:VEVENT", 1)
			if mode == "guests" {
				raw = strings.Replace(raw, "END:VEVENT", "ATTENDEE:mailto:guest@example.com\r\nEND:VEVENT", 1)
			}
			if mode == "already-recurring" {
				raw = strings.Replace(raw, "END:VEVENT", "RRULE:FREQ=DAILY\r\nEND:VEVENT", 1)
			}
			if mode == "series-complex" {
				raw = strings.Replace(raw, "FREQ=DAILY;INTERVAL=1;COUNT=6", "FREQ=WEEKLY;BYDAY=MO,WE", 1)
			}
			// Dates removed or changed one by one: a new title reaches a changed
			// date that kept the series' title; a new time or repeat rule needs
			// the organizer's confirmation to reset them.
			dated := mode == "series-exceptions" || strings.HasPrefix(mode, "series-dates")
			if dated {
				raw = strings.Replace(raw, "END:VEVENT", "EXDATE:20261003T070000Z\r\nEND:VEVENT", 1)
				raw = strings.Replace(raw, "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:"+draft.RequestID+"@gofer\r\nRECURRENCE-ID:20261004T070000Z\r\nDTSTAMP:20261001T000000Z\r\nDTSTART:20261004T080000Z\r\nDTEND:20261004T090000Z\r\nSUMMARY:Planning\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
			}
			if mode == "series-dates-title" {
				draft.Recurrence = before.Recurrence
				draft.Summary = "Renamed"
			}
			if mode == "series-dates-reset" {
				draft.ResetSeriesDates = true
			}
			if mode == "series-instance" {
				raw = strings.Replace(raw, "END:VEVENT", "RECURRENCE-ID:20261002T070000Z\r\nEND:VEVENT", 1)
			}
			version, gets, puts := `"v1"`, 0, 0
			var saved *ical.Calendar
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "calendar-user" || password != "calendar-password" {
					t.Error("wrong calendar credentials")
				}
				if r.URL.Path != "/calendar/original.ics" {
					t.Error("changed event resource URL")
				}
				if r.Method == http.MethodGet {
					gets++
					if puts > 0 && mode == "confirmation-failure" {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("ETag", version)
					body := raw
					if puts > 0 && mode == "confirmation-mismatch" {
						body = strings.Replace(body, "SUMMARY:Planning", "SUMMARY:Changed elsewhere", 1)
					}
					if puts > 0 && mode == "recurring-missing" {
						body = strings.Replace(body, "RRULE:"+calendarRecurrenceRule(draft)+"\r\n", "", 1)
					}
					if puts > 0 && mode == "recurring-mismatch" {
						body = strings.Replace(body, "COUNT=4", "COUNT=99", 1)
					}
					_, _ = io.WriteString(w, body)
					return
				}
				if r.Method != http.MethodPut {
					t.Error("unexpected DAV write method")
					w.WriteHeader(405)
					return
				}
				puts++
				if gets != 1 || r.Header.Get("If-Match") != `"v1"` {
					t.Error("missing DAV read-before-write/If-Match")
				}
				if mode == "race" || mode == "series-race" {
					w.WriteHeader(412)
					return
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				raw = string(data)
				saved, err = calendarUpdateDecodeICS(data)
				if err != nil {
					t.Error(err)
				}
				version = `"v2"`
				// Deliberately omit the PUT ETag: a confirmation GET is required.
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			old := davHTTPTransport
			davHTTPTransport = server.Client().Transport
			defer func() { davHTTPTransport = old }()
			source := storage.CalendarSource{RemoteID: server.URL + "/calendar/", TimeZone: "Europe/Prague"}
			existing := storage.CalendarEvent{RemoteID: server.URL + "/calendar/original.ics", ETag: `"v1"`, ICalUID: draft.RequestID + "@gofer"}
			if series {
				original := calendarCreatedRemote(before)
				original.RemoteID, original.ETag, original.ICalUID = existing.RemoteID, existing.ETag, existing.ICalUID
				original.Recurrence, _ = json.Marshal([]string{"RRULE:" + calendarRecurrenceRule(before)})
				existing = calendarStorageEvent("one", "source", original)
			}
			remote, err := updateCalDAVCalendarEventScope(davTestContext(t), source, "calendar-user", "calendar-password", existing, draft, series)
			switch mode {
			case "race", "series-race":
				if !errors.Is(err, errCalendarUpdateConflict) {
					t.Fatalf("expected conflict, got %v", err)
				}
			case "series-exceptions":
				if !errors.Is(err, errCalendarSeriesDatesReset) || puts != 0 {
					t.Fatalf("dates reset without confirmation: %v", err)
				}
			case "guests", "already-recurring", "series-instance", "series-complex":
				if !errors.Is(err, errCalendarUpdateUnsupported) || puts != 0 {
					t.Fatalf("guest event written: %v", err)
				}
			case "confirmation-failure", "confirmation-mismatch", "recurring-missing", "recurring-mismatch":
				if err == nil || !calendarCreateUncertain(err) {
					t.Fatalf("unconfirmed result was accepted: %v", err)
				}
			default:
				if err != nil || remote.ETag != `"v2"` || remote.RemoteID != existing.RemoteID || !calendarUpdateMatchesDraft(remote, draft) || gets != 2 || puts != 1 {
					t.Fatalf("remote=%#v gets=%d puts=%d err=%v", remote, gets, puts, err)
				}
				uid, _ := saved.Events()[0].Props.Text("UID")
				extra, _ := saved.Events()[0].Props.Text("X-PRESERVE")
				if uid != existing.ICalUID || extra != "keep-me" || len(saved.Events()[0].Children) != 1 || saved.Events()[0].Children[0].Name != "VALARM" {
					t.Fatal("edit destroyed event identity, metadata, or reminder")
				}
				if converting {
					if !calendarRecurrenceMatchesDraft(remote.Recurrence, draft) || saved.Events()[0].Props.Get("RRULE") == nil {
						t.Fatal("conversion response lost recurrence")
					}
					if !draft.AllDay && (!strings.Contains(raw, "BEGIN:VTIMEZONE") || saved.Events()[0].Props.Get("DTSTART").Params.Get("TZID") != draft.TimeZone) {
						t.Fatal("timed series conversion discarded DST timezone definitions")
					}
				}
				switch mode {
				case "series-dates-title":
					date := calendarMeetingComponent(saved, "2026-10-04T07:00:00Z")
					summary, _ := date.Props.Text("SUMMARY")
					if date == nil || summary != "Renamed" || date.Props.Get("DTSTART").Value != "20261004T080000Z" || saved.Events()[0].Props.Get("EXDATE") == nil {
						t.Fatalf("changed and removed dates not kept: %s", raw)
					}
				case "series-dates-reset":
					if len(saved.Events()) != 1 || saved.Events()[0].Props.Get("EXDATE") != nil {
						t.Fatalf("dates not reset: %s", raw)
					}
				}
			}
		})
	}
}
