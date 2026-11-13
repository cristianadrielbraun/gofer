package handler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func TestOutlookWindowsZonesLoad(t *testing.T) {
	for windows, name := range outlookWindowsZones {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("%s: %s does not load: %v", windows, name, err)
		}
	}
	if outlookCalendarTimeLocation("GMT Standard Time").String() != "Europe/London" {
		t.Fatal("GMT Standard Time is London, with daylight saving")
	}
}

// Microsoft reads times in UTC; the zone the event was made in is kept.
func TestOutlookEventKeepsItsOriginalZone(t *testing.T) {
	for original, want := range map[string]string{"Central Europe Standard Time": "Europe/Budapest", "Europe/Prague": "Europe/Prague", "UTC": "UTC", "": "UTC", "Customized Time Zone": "UTC"} {
		var outlook outlookCalendarEvent
		_ = json.Unmarshal([]byte(`{"id":"o","start":{"dateTime":"2026-11-03T08:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-03T09:00:00","timeZone":"UTC"},"originalStartTimeZone":"`+original+`","originalEndTimeZone":"`+original+`"}`), &outlook)
		event, err := normalizeOutlookCalendarEvent(outlook)
		if err != nil || event.StartTimeZone != want || event.EndTimeZone != want || !event.StartAt.Equal(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)) {
			t.Errorf("%q: %s %s %v %v", original, event.StartTimeZone, event.EndTimeZone, event.StartAt, err)
		}
	}
}

// Gofer's own CalDAV events keep their times in UTC and the zone beside them.
func TestCalDAVEventKeepsGoferZone(t *testing.T) {
	for extra, want := range map[string]string{"X-GOFER-TIMEZONE:Europe/Prague\r\n": "Europe/Prague", "X-GOFER-TIMEZONE:Nowhere/Else\r\n": "UTC", "": "UTC"} {
		cal, err := calendarUpdateDecodeICS([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:c\r\nDTSTART:20261103T080000Z\r\nDTEND:20261103T090000Z\r\n" + extra + "END:VEVENT\r\nEND:VCALENDAR\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		event, err := normalizeCalDAVEvent(cal.Events()[0], "c.ics", `"v1"`, time.UTC)
		if err != nil || event.StartTimeZone != want || !event.StartAt.Equal(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)) {
			t.Errorf("%q: %s %v", extra, event.StartTimeZone, err)
		}
	}
	// Another zone in DTSTART wins over Gofer's note.
	cal, _ := calendarUpdateDecodeICS([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VEVENT\r\nUID:c\r\nDTSTART;TZID=America/New_York:20261103T080000\r\nDTEND;TZID=America/New_York:20261103T090000\r\nX-GOFER-TIMEZONE:Europe/Prague\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if event, err := normalizeCalDAVEvent(cal.Events()[0], "c.ics", `"v1"`, time.UTC); err != nil || event.StartTimeZone != "America/New_York" {
		t.Fatalf("DTSTART zone: %s %v", event.StartTimeZone, err)
	}
}

// A single event in UTC opens in the user's zone; a series keeps UTC.
func TestCalendarEditorShowsUTCEventsInTheUsersZone(t *testing.T) {
	prague, _ := time.LoadLocation("Europe/Prague")
	start, end := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC), time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	event := storage.CalendarEvent{ID: "e", ETag: `"v1"`, Summary: "Review", StartAt: &start, EndAt: &end, StartTimeZone: "UTC", EndTimeZone: "UTC"}
	source := storage.CalendarSource{ID: "s", Provider: "caldav"}
	data, err := calendarEditData(event, source, "", prague, false, false, nil)
	if err != nil || data.TimeZone != "Europe/Prague" || data.StartTime != "09:00" || data.EndTime != "10:00" {
		t.Fatalf("single event: %s %s-%s %v", data.TimeZone, data.StartTime, data.EndTime, err)
	}
	data, err = calendarEditData(event, source, "", prague, false, true, nil)
	if err != nil || data.TimeZone != "UTC" || data.StartTime != "08:00" {
		t.Fatalf("date of a series: %s %s %v", data.TimeZone, data.StartTime, err)
	}
	event.StartTimeZone = "America/New_York"
	if data, _ := calendarEditData(event, source, "", prague, false, false, nil); data.TimeZone != "America/New_York" || data.StartTime != "03:00" {
		t.Fatalf("event zone: %s %s", data.TimeZone, data.StartTime)
	}
	// Budapest keeps Prague's clocks, so the event opens in Prague.
	event.StartTimeZone = "Europe/Budapest"
	if data, _ := calendarEditData(event, source, "", prague, true, false, nil); data.TimeZone != "Europe/Prague" || data.StartTime != "09:00" {
		t.Fatalf("same clocks: %s %s", data.TimeZone, data.StartTime)
	}
}

// A range picked in the week view fills the new event's times.
func TestCalendarNewEventRange(t *testing.T) {
	prague, _ := time.LoadLocation("Europe/Prague")
	data := views.CalendarCreateData{Date: "2026-11-03", StartTime: "09:00", EndDate: "2026-11-03", EndTime: "10:00"}
	if err := calendarNewEventRange(&data, "", "", prague); err != nil || data.StartTime != "09:00" {
		t.Fatal("no range changed the times")
	}
	if err := calendarNewEventRange(&data, "2026-11-03T13:15", "2026-11-04T00:00", prague); err != nil || data.Date != "2026-11-03" || data.StartTime != "13:15" || data.EndDate != "2026-11-04" || data.EndTime != "00:00" {
		t.Fatalf("range: %+v %v", data, err)
	}
	for _, bad := range [][2]string{{"2026-11-03T13:00", "2026-11-03T12:00"}, {"2026-11-03T13:00", ""}, {"soon", "2026-11-03T14:00"}, {"2026-11-03T13:00", "2026-12-20T13:00"}} {
		if calendarNewEventRange(&data, bad[0], bad[1], prague) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// An event in a zone with the user's own clocks opens in the user's zone.
func TestCalendarZonesMatch(t *testing.T) {
	load := func(name string) *time.Location {
		location, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return location
	}
	prague := load("Europe/Prague")
	for name, want := range map[string]bool{"Europe/Budapest": true, "Europe/Berlin": true, "Europe/Prague": true, "Europe/London": false, "Africa/Lagos": false, "UTC": false, "America/New_York": false} {
		if got := calendarZonesMatch(load(name), prague); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// Regression: Go's bundled timezone data can return a non-progressing
// ZoneBounds boundary near the end of 2028.
func TestCalendarZonesMatchBundledTZData(t *testing.T) {
	zoneinfo := filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip")
	if _, err := os.Stat(zoneinfo); err != nil {
		t.Skipf("Go timezone database unavailable: %v", err)
	}
	t.Setenv("ZONEINFO", zoneinfo)

	budapest, err := time.LoadLocation("Europe/Budapest")
	if err != nil {
		t.Fatal(err)
	}
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}

	if !calendarZonesMatch(budapest, prague) {
		t.Fatal("Budapest and Prague should have matching timezone offsets")
	}
}
