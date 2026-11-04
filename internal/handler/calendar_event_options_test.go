package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Each provider says whether an event leaves the time free.
func TestCalendarShowAsReadFromEachProvider(t *testing.T) {
	for raw, want := range map[string]string{`"transparency":"transparent"`: "free", `"transparency":"opaque"`: "busy", `"status":"confirmed"`: "busy"} {
		var google googleCalendarEvent
		_ = json.Unmarshal([]byte(`{"id":"g","start":{"dateTime":"2026-11-03T09:00:00Z"},"end":{"dateTime":"2026-11-03T10:00:00Z"},`+raw+`}`), &google)
		if event, err := normalizeGoogleCalendarEvent(google); err != nil || event.ShowAs != want {
			t.Errorf("Google %s: %q %v", raw, event.ShowAs, err)
		}
	}
	for value, want := range map[string]string{"free": "free", "oof": "oof", "workingElsewhere": "working_elsewhere", "unknown": ""} {
		var outlook outlookCalendarEvent
		_ = json.Unmarshal([]byte(`{"id":"o","start":{"dateTime":"2026-11-03T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-03T10:00:00","timeZone":"UTC"},"showAs":"`+value+`"}`), &outlook)
		if event, err := normalizeOutlookCalendarEvent(outlook); err != nil || event.ShowAs != want {
			t.Errorf("Outlook %s: %q %v", value, event.ShowAs, err)
		}
	}
	for line, want := range map[string]string{"TRANSP:TRANSPARENT\r\n": "free", "TRANSP:OPAQUE\r\n": "busy", "": "busy"} {
		cal, err := calendarUpdateDecodeICS([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:c\r\nDTSTART:20261103T090000Z\r\nDTEND:20261103T100000Z\r\n" + line + "END:VEVENT\r\nEND:VCALENDAR\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		if event, err := normalizeCalDAVEvent(cal.Events()[0], "c.ics", `"v1"`, time.UTC); err != nil || event.ShowAs != want {
			t.Errorf("CalDAV %q: %q %v", line, event.ShowAs, err)
		}
	}
}

func TestCalendarShowAsForm(t *testing.T) {
	for _, value := range []string{"busy", "free", "tentative", "oof", "working_elsewhere"} {
		values := calendarCreateForm()
		values.Set("show_as", value)
		if draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err != nil || draft.ShowAs != value {
			t.Errorf("%s: %q %v", value, draft.ShowAs, err)
		}
	}
	for _, value := range []string{"", "away", "BUSY"} {
		values := calendarCreateForm()
		values.Set("show_as", value)
		if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
			t.Errorf("invalid show-as %q accepted", value)
		}
	}
}

// Google and CalDAV have only busy and free; Outlook keeps every state.
func TestCalendarShowAsPayloads(t *testing.T) {
	if googleCalendarTransparency("free") != "transparent" || googleCalendarTransparency("oof") != "opaque" {
		t.Fatal("Google transparency")
	}
	if outlookCalendarShowAsValue("working_elsewhere") != "workingElsewhere" || outlookCalendarShowAsValue("free") != "free" {
		t.Fatal("Outlook showAs")
	}
	draft := calendarProviderDraft(t, false)
	draft.ShowAs = "free"
	body, err := calendarCreateICS(draft)
	if err != nil || !strings.Contains(body, "TRANSP:TRANSPARENT") {
		t.Fatalf("CalDAV create: %v %s", err, body)
	}
	if !calendarSameShowAs("busy", "") || calendarSameShowAs("free", "") || !calendarSameShowAs("oof", "oof") {
		t.Fatal("show-as comparison")
	}
}

// Each provider says whether an event is private; confidential counts too.
func TestCalendarPrivateReadFromEachProvider(t *testing.T) {
	for value, want := range map[string]bool{"private": true, "confidential": true, "public": false, "default": false} {
		var google googleCalendarEvent
		_ = json.Unmarshal([]byte(`{"id":"g","start":{"dateTime":"2026-11-03T09:00:00Z"},"end":{"dateTime":"2026-11-03T10:00:00Z"},"visibility":"`+value+`"}`), &google)
		if event, err := normalizeGoogleCalendarEvent(google); err != nil || event.Private != want {
			t.Errorf("Google %s: %v %v", value, event.Private, err)
		}
	}
	for value, want := range map[string]bool{"private": true, "confidential": true, "personal": false, "normal": false} {
		var outlook outlookCalendarEvent
		_ = json.Unmarshal([]byte(`{"id":"o","start":{"dateTime":"2026-11-03T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-03T10:00:00","timeZone":"UTC"},"sensitivity":"`+value+`"}`), &outlook)
		if event, err := normalizeOutlookCalendarEvent(outlook); err != nil || event.Private != want {
			t.Errorf("Outlook %s: %v %v", value, event.Private, err)
		}
	}
	for line, want := range map[string]bool{"CLASS:PRIVATE\r\n": true, "CLASS:CONFIDENTIAL\r\n": true, "CLASS:PUBLIC\r\n": false, "": false} {
		cal, err := calendarUpdateDecodeICS([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:c\r\nDTSTART:20261103T090000Z\r\nDTEND:20261103T100000Z\r\n" + line + "END:VEVENT\r\nEND:VCALENDAR\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		if event, err := normalizeCalDAVEvent(cal.Events()[0], "c.ics", `"v1"`, time.UTC); err != nil || event.Private != want {
			t.Errorf("CalDAV %q: %v %v", line, event.Private, err)
		}
	}
}

func TestCalendarPrivateFormAndPayloads(t *testing.T) {
	for value, want := range map[string]bool{"true": true, "false": false} {
		values := calendarCreateForm()
		values.Set("private", value)
		if draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err != nil || !draft.PrivateSet || draft.Private != want {
			t.Errorf("%s: %+v %v", value, draft.Private, err)
		}
	}
	values := calendarCreateForm()
	values.Set("private", "yes")
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
		t.Error("invalid private value accepted")
	}
	if googleCalendarVisibility(true) != "private" || googleCalendarVisibility(false) != "default" || outlookCalendarSensitivity(true) != "private" || outlookCalendarSensitivity(false) != "normal" {
		t.Fatal("provider values")
	}
	draft := calendarProviderDraft(t, false)
	draft.PrivateSet, draft.Private = true, true
	body, err := calendarCreateICS(draft)
	if err != nil || !strings.Contains(body, "CLASS:PRIVATE") {
		t.Fatalf("CalDAV create: %v %s", err, body)
	}
	cal, _ := calendarUpdateDecodeICS([]byte(body))
	calDAVSetPrivate(cal.Events()[0].Component, false)
	if cal.Events()[0].Props.Get("CLASS") != nil {
		t.Fatal("the private mark was not removed")
	}
}

// A new Outlook event whose version Microsoft changed without changing it can
// still be edited; any real change is still a conflict.
func TestCalendarOutlookUnchangedSince(t *testing.T) {
	read := func(raw string) outlookCalendarUpdateEvent {
		var event outlookCalendarUpdateEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	base := `{"id":"o","changeKey":"v2","@odata.etag":"W/\"v2\"","type":"singleInstance","subject":"Review","body":{"contentType":"html","content":"Notes"},"location":{"displayName":"Room 2"},"start":{"dateTime":"2026-11-03T09:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-03T10:00:00","timeZone":"UTC"},"showAs":"free","sensitivity":"private","isReminderOn":true,"reminderMinutesBeforeStart":15,"attendees":[]}`
	created, err := normalizeOutlookCalendarEvent(read(base).outlookCalendarEvent)
	if err != nil {
		t.Fatal(err)
	}
	cached := calendarStorageEvent("one", "source", created)
	cached.ETag = "v1"
	if !calendarOutlookUnchangedSince(read(base), cached) {
		t.Fatal("an unchanged event was a conflict")
	}
	for _, change := range []string{`"subject":"Moved"`, `"showAs":"busy"`, `"sensitivity":"normal"`, `"reminderMinutesBeforeStart":30`, `"location":{"displayName":"Room 3"}`} {
		key := change[:strings.Index(change, ":")]
		start := strings.Index(base, key)
		end := start + len(key) + 1
		for depth := 0; end < len(base); end++ {
			if c := base[end]; c == '{' {
				depth++
			} else if c == '}' && depth > 0 {
				depth--
			} else if (c == ',' || c == '}') && depth == 0 {
				break
			}
		}
		if calendarOutlookUnchangedSince(read(base[:start]+change+base[end:]), cached) {
			t.Errorf("%s was not seen as a change", change)
		}
	}
	cached.SeriesRemoteID = "series"
	if calendarOutlookUnchangedSince(read(base), cached) {
		t.Fatal("a series occurrence was let through")
	}
}

// Only events the editor would open can be dragged; a date of a series moves alone.
func TestCalendarEventMovable(t *testing.T) {
	start, end := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	owned := storage.CalendarSource{ID: "source", Provider: "gmail", AccessRole: "owner"}
	plain := storage.CalendarEvent{ID: "plain", SourceID: "source", RemoteID: "plain", ETag: `"v1"`, Summary: "Review", StartAt: &start, EndAt: &end}
	occurrence := plain
	occurrence.ID, occurrence.RemoteID, occurrence.SeriesRemoteID = "date", "series_20261103", "series"
	views := calendarViewEvents(context.Background(), []storage.CalendarEvent{plain, occurrence, {ID: "elsewhere", SourceID: "other", ETag: `"v1"`, StartAt: &start, EndAt: &end}}, []storage.CalendarSource{owned})
	if !views[0].Movable || views[0].Occurrence || !views[1].Movable || !views[1].Occurrence || views[2].Movable {
		t.Fatalf("movable: %+v", views)
	}
	readOnly := owned
	readOnly.AccessRole = "reader"
	if calendarEventMovable(plain, readOnly) {
		t.Fatal("an event on a read-only calendar can be dragged")
	}
	cancelled := plain
	cancelled.Status = "cancelled"
	if calendarEventMovable(cancelled, owned) {
		t.Fatal("a cancelled event can be dragged")
	}
}

// Google's birthdays and other special events are kept as such and can't be
// changed or dragged in Gofer.
func TestCalendarSpecialGoogleEventsAreNotEditable(t *testing.T) {
	for kind, want := range map[string]string{"birthday": "birthday", "fromGmail": "fromGmail", "default": "", "": ""} {
		var google googleCalendarEvent
		_ = json.Unmarshal([]byte(`{"id":"g","start":{"date":"2026-11-27"},"end":{"date":"2026-11-28"},"eventType":"`+kind+`"}`), &google)
		if event, err := normalizeGoogleCalendarEvent(google); err != nil || event.EventType != want {
			t.Errorf("%q: %q %v", kind, event.EventType, err)
		}
	}
	owned := storage.CalendarSource{ID: "source", Provider: "gmail", AccessRole: "owner"}
	birthday := storage.CalendarEvent{ID: "b", SourceID: "source", RemoteID: "b_20261127", SeriesRemoteID: "b", ETag: `"v1"`, Summary: "Birthday", AllDay: true, StartDate: "2026-11-27", EndDate: "2026-11-28", EventType: "birthday"}
	if calendarEventMovable(birthday, owned) {
		t.Fatal("a birthday can be dragged")
	}
	if reason := calendarEventMutationRestriction(birthday, owned, true); !strings.Contains(reason, "contact") {
		t.Fatalf("birthday reason: %q", reason)
	}
	birthday.EventType = ""
	if !calendarEventMovable(birthday, owned) {
		t.Fatal("an ordinary date of a series can't be dragged")
	}
	if calendarEventTypeRestriction("somethingNew") == "" {
		t.Fatal("an unknown kind of event can be changed")
	}
}
