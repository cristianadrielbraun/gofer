package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"testing"
)

func calendarForwardRequest(values url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/calendar/events/e/forward", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestCalendarForwardForm(t *testing.T) {
	recipients, comment, err := parseCalendarForwardForm(calendarForwardRequest(url.Values{"recipients": {"Ada Field <ada@example.com>, bo@example.com\nada@example.com"}, "comment": {"  See you there  "}}))
	if err != nil || len(recipients) != 2 || recipients[0].Name != "Ada Field" || recipients[1].Address != "bo@example.com" || comment != "See you there" {
		t.Fatalf("%v %q %v", recipients, comment, err)
	}
	for _, values := range []url.Values{
		{"recipients": {""}, "comment": {""}},
		{"recipients": {"not an address"}, "comment": {""}},
		{"recipients": {"ada@example.com"}},
		{"recipients": {"ada@example.com"}, "comment": {""}, "extra": {"x"}},
	} {
		if _, _, err := parseCalendarForwardForm(calendarForwardRequest(values)); err == nil {
			t.Errorf("accepted %v", values)
		}
	}
}

// The forwarded invitation keeps the organizer and guests, invites the new
// people, and leaves out personal alarms.
func TestCalendarForwardICS(t *testing.T) {
	cal, err := calendarUpdateDecodeICS([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:planning\r\nDTSTAMP:20261001T080000Z\r\nDTSTART:20261103T080000Z\r\nDTEND:20261103T090000Z\r\nSUMMARY:Planning\r\nORGANIZER:mailto:host@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.com\r\nX-PRIVATE:secret\r\nBEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nEND:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := calendarForwardICS(cal, []*mail.Address{{Name: "Ada Field", Address: "ada@example.com"}, {Address: "guest@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"METHOD:REQUEST", "ORGANIZER:mailto:host@example.com", "mailto:guest@example.com", "CN=Ada Field", "RSVP=TRUE", "mailto:ada@example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Count(body, "mailto:guest@example.com") != 1 || strings.Contains(body, "VALARM") || strings.Contains(body, "X-PRIVATE") {
		t.Fatalf("wrong invitation:\n%s", body)
	}
}

func TestCalendarForwardOutlook(t *testing.T) {
	var got struct {
		ToRecipients []struct {
			EmailAddress struct{ Address, Name string }
		}
		Comment string
	}
	status := http.StatusAccepted
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		if r.Method != http.MethodPost || r.URL.Path != "/events/one/forward" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("wrong request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	target := calendarResponseTarget{Endpoint: server.URL + "/events/one", Token: "token"}
	if err := forwardOutlookInvitation(t.Context(), target, []*mail.Address{{Name: "Ada Field", Address: "ada@example.com"}}, "Join us"); err != nil {
		t.Fatal(err)
	}
	if len(got.ToRecipients) != 1 || got.ToRecipients[0].EmailAddress.Address != "ada@example.com" || got.ToRecipients[0].EmailAddress.Name != "Ada Field" || got.Comment != "Join us" {
		t.Fatalf("wrong payload %+v", got)
	}
	status = http.StatusForbidden
	if err := forwardOutlookInvitation(t.Context(), target, []*mail.Address{{Address: "ada@example.com"}}, ""); err == nil {
		t.Fatal("a refused forward succeeded")
	}
}

// A Google invitation becomes one other calendars read, in its own zone,
// with its repeat rules, organizer and guests.
func TestGoogleCalendarForwardCalendar(t *testing.T) {
	var remote googleCalendarEvent
	if err := json.Unmarshal([]byte(`{"iCalUID":"weekly@google.com","summary":"Weekly review","description":"Agenda","location":"Room 2","hangoutLink":"https://meet.google.com/abc-defg-hij",
		"organizer":{"email":"host@example.com","displayName":"Host Person"},
		"attendees":[{"email":"host@example.com","organizer":true,"responseStatus":"accepted"},{"email":"guest@example.com","displayName":"Guest","responseStatus":"needsAction","optional":true}],
		"start":{"dateTime":"2026-11-03T09:00:00+01:00","timeZone":"Europe/Prague"},"end":{"dateTime":"2026-11-03T10:00:00+01:00","timeZone":"Europe/Prague"},
		"recurrence":["RRULE:FREQ=WEEKLY;BYDAY=TU","EXDATE;TZID=Europe/Prague:20261110T090000"]}`), &remote); err != nil {
		t.Fatal(err)
	}
	cal, err := googleCalendarForwardCalendar(remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := calendarForwardICS(cal, []*mail.Address{{Address: "new@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	unfolded := strings.ReplaceAll(body, "\r\n ", "")
	for _, want := range []string{"METHOD:REQUEST", "UID:weekly@google.com", "BEGIN:VTIMEZONE", "TZID:Europe/Prague", "DTSTART;TZID=Europe/Prague:20261103T090000", "DTEND;TZID=Europe/Prague:20261103T100000", "RRULE:FREQ=WEEKLY;BYDAY=TU", "EXDATE;TZID=Europe/Prague:20261110T090000", "ORGANIZER;CN=Host Person:mailto:host@example.com", "mailto:guest@example.com", "ROLE=OPT-PARTICIPANT", "mailto:new@example.com", "SUMMARY:Weekly review", "LOCATION:Room 2", "https://meet.google.com/abc-defg-hij"} {
		if !strings.Contains(unfolded, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	// A rule that isn't one stays out.
	remote.Recurrence = []string{"SUMMARY:Injected"}
	if _, err := googleCalendarForwardCalendar(remote, nil); err == nil {
		t.Fatal("accepted a line that is not a repeat rule")
	}
	// An all-day event keeps its dates.
	remote.Recurrence = nil
	remote.Start, remote.End = googleCalendarEventDateTime{Date: "2026-11-03"}, googleCalendarEventDateTime{Date: "2026-11-04"}
	cal, _ = googleCalendarForwardCalendar(remote, nil)
	body, _ = calendarForwardICS(cal, nil)
	if !strings.Contains(body, "DTSTART;VALUE=DATE:20261103") || !strings.Contains(body, "DTEND;VALUE=DATE:20261104") {
		t.Fatalf("all-day dates:\n%s", body)
	}
}

// A series' dates changed on their own go with it, and its deleted dates
// are left out of it.
func TestGoogleCalendarForwardSeriesExceptions(t *testing.T) {
	var remote googleCalendarEvent
	var exceptions []googleCalendarUpdateEvent
	if err := json.Unmarshal([]byte(`{"id":"weekly","iCalUID":"weekly@google.com","summary":"Weekly review","organizer":{"email":"host@example.com"},
		"start":{"dateTime":"2026-11-03T09:00:00+01:00","timeZone":"Europe/Prague"},"end":{"dateTime":"2026-11-03T10:00:00+01:00","timeZone":"Europe/Prague"},
		"recurrence":["RRULE:FREQ=WEEKLY;BYDAY=TU"]}`), &remote); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`[
		{"id":"weekly_moved","iCalUID":"weekly@google.com","recurringEventId":"weekly","status":"confirmed","summary":"Weekly review, later","organizer":{"email":"host@example.com"},
		 "originalStartTime":{"dateTime":"2026-11-10T09:00:00+01:00","timeZone":"Europe/Prague"},
		 "start":{"dateTime":"2026-11-10T11:00:00+01:00","timeZone":"Europe/Prague"},"end":{"dateTime":"2026-11-10T12:00:00+01:00","timeZone":"Europe/Prague"}},
		{"id":"weekly_gone","iCalUID":"weekly@google.com","recurringEventId":"weekly","status":"cancelled","originalStartTime":{"dateTime":"2026-11-17T08:00:00Z"}},
		{"id":"other","iCalUID":"other@google.com","recurringEventId":"weekly","status":"cancelled","originalStartTime":{"dateTime":"2026-11-24T08:00:00Z"}}]`), &exceptions); err != nil {
		t.Fatal(err)
	}
	cal, err := googleCalendarForwardCalendar(remote, exceptions)
	if err != nil {
		t.Fatal(err)
	}
	body, err := calendarForwardICS(cal, []*mail.Address{{Address: "new@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	unfolded := strings.ReplaceAll(body, "\r\n ", "")
	for _, want := range []string{"EXDATE;TZID=Europe/Prague:20261117T090000", "RECURRENCE-ID;TZID=Europe/Prague:20261110T090000", "DTSTART;TZID=Europe/Prague:20261110T110000", `SUMMARY:Weekly review\, later`} {
		if !strings.Contains(unfolded, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	if strings.Count(body, "BEGIN:VEVENT") != 2 || strings.Count(body, "BEGIN:VTIMEZONE") != 1 || strings.Count(body, "mailto:new@example.com") != 2 {
		t.Fatalf("series:\n%s", body)
	}
	// The other series' deleted date must not become one of this series'
	// dates. Only the date lines are checked, because DTSTAMP holds the day
	// the invitation is written, which can be that same day.
	for _, line := range strings.Split(unfolded, "\r\n") {
		if (strings.HasPrefix(line, "EXDATE") || strings.HasPrefix(line, "RECURRENCE-ID")) && strings.Contains(line, "20261124") {
			t.Fatalf("another series' date in %q:\n%s", line, body)
		}
	}
}
