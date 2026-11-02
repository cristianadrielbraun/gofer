package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// fakeGoogleCalendar is just enough of Google Calendar for import and
// export: imported events, found by their iCalendar UID.
type fakeGoogleCalendar struct {
	mu       sync.Mutex
	token    string
	imported []map[string]any
	requests int
}

func (f *fakeGoogleCalendar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("Authorization") != "Bearer "+f.token || !strings.HasPrefix(r.URL.Path, "/calendars/cal/events") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/calendars/cal/events/import":
		var event map[string]any
		_ = json.NewDecoder(r.Body).Decode(&event)
		event["id"] = fmt.Sprintf("e%d", len(f.imported))
		if _, ok := event["organizer"]; !ok {
			event["organizer"] = map[string]string{"email": "owner@example.com"}
		}
		if _, ok := event["originalStartTime"]; ok {
			for _, series := range f.imported {
				if series["iCalUID"] == event["iCalUID"] && series["originalStartTime"] == nil {
					event["recurringEventId"] = series["id"]
				}
			}
		}
		f.imported = append(f.imported, event)
		json.NewEncoder(w).Encode(map[string]any{"id": event["id"]})
	case r.Method == http.MethodGet && r.URL.Query().Get("iCalUID") != "":
		var items []map[string]any
		for _, event := range f.imported {
			if event["iCalUID"] == r.URL.Query().Get("iCalUID") {
				items = append(items, event)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == http.MethodGet && r.URL.Query().Get("singleEvents") == "false":
		json.NewEncoder(w).Encode(map[string]any{"items": f.imported})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestGoogleCalendarImportThenExport(t *testing.T) {
	fake := &fakeGoogleCalendar{token: "token"}
	server := httptest.NewServer(fake)
	defer server.Close()
	prior := googleCalendarAPIBaseURL
	googleCalendarAPIBaseURL = server.URL
	defer func() { googleCalendarAPIBaseURL = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "gmail", Name: "Garden club", RemoteID: "cal", AccessRole: "owner"}
	credentials := calendarCredentials{token: "token"}

	resources, err := calendarImportResources([]byte(calendarImportTestFile))
	if err != nil {
		t.Fatal(err)
	}
	result, err := importCalendarEvents(t.Context(), source, credentials, resources, nil)
	if err != nil || result != (calendarImportResult{Added: 3}) {
		t.Fatalf("first import = %+v, %v", result, err)
	}
	imported := fake.imported
	pruning := imported[0]
	if pruning["iCalUID"] != "weekly-pruning" || fmt.Sprint(pruning["recurrence"]) != "[RRULE:FREQ=WEEKLY;COUNT=4]" {
		t.Errorf("series imported as %v", pruning)
	}
	if start := pruning["start"].(map[string]any); start["dateTime"] != "2026-01-05T09:00:00+01:00" || start["timeZone"] != "Europe/Paris" {
		t.Errorf("series start = %v", start)
	}
	if reminders := fmt.Sprint(pruning["reminders"]); reminders != "map[overrides:[map[method:popup minutes:60]] useDefault:false]" {
		t.Errorf("reminders imported as %s", reminders)
	}
	if _, ok := imported[2]["reminders"]; ok {
		t.Error("an event without alarms didn't get the calendar's reminders")
	}
	if attendees := fmt.Sprint(pruning["attendees"]); !strings.Contains(attendees, "sam@example.com") || !strings.Contains(attendees, "accepted") {
		t.Errorf("guests imported as %s", attendees)
	}
	late := imported[1]
	if late["iCalUID"] != "weekly-pruning" || late["summary"] != "Pruning (late)" || late["originalStartTime"] == nil {
		t.Errorf("changed date imported as %v", late)
	}
	if swap := imported[2]; swap["start"].(map[string]any)["date"] != "2026-03-01" || swap["end"].(map[string]any)["date"] != "2026-03-02" {
		t.Errorf("all-day event imported as %v", swap)
	}

	fake.requests = 0
	result, err = importCalendarEvents(t.Context(), source, credentials, resources, nil)
	if err != nil || result != (calendarImportResult{Existing: 3}) || fake.requests != 1 {
		t.Fatalf("second import = %+v, %v", result, err)
	}
	if len(fake.imported) != 4 {
		t.Fatalf("second import added events: %d", len(imported))
	}

	data, err := exportCalendarEvents(t.Context(), source, credentials)
	if err != nil {
		t.Fatal(err)
	}
	exported := string(data)
	if strings.Count(exported, "BEGIN:VEVENT") != 4 || !strings.Contains(exported, "RECURRENCE-ID;TZID=Europe/Paris:20260112T090000") || !strings.Contains(exported, "X-WR-CALNAME:Garden club") || strings.Count(exported, "TRIGGER:-PT60M") != 1 {
		t.Fatalf("export has the wrong shape:\n%s", exported)
	}
	again, err := calendarImportResources(data)
	if err != nil || len(again) != 3 {
		t.Fatalf("export reimports as %d resources, %v", len(again), err)
	}
	for _, resource := range again {
		for _, event := range resource.Events() {
			if summary, _ := event.Props.Text("SUMMARY"); summary == "Seed swap" && event.Props.Get("ORGANIZER") != nil {
				t.Error("an event without guests was exported with an organizer")
			}
		}
	}
}

const outlookImportTestFile = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Example//Test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:choir\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;TZID=Europe/Paris:20260105T190000\r\n" +
	"DTEND;TZID=Europe/Paris:20260105T203000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
	"EXDATE;TZID=Europe/Paris:20260119T190000\r\n" +
	"RDATE;TZID=Europe/Paris:20260108T190000,20260105T190000\r\n" +
	"SUMMARY:Choir\r\n" +
	"TRANSP:TRANSPARENT\r\n" +
	"ATTENDEE:mailto:sam@example.com\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT30M\r\nEND:VALARM\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-P1D\r\nEND:VALARM\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:choir\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"RECURRENCE-ID;TZID=Europe/Paris:20260112T190000\r\n" +
	"DTSTART;TZID=Europe/Paris:20260112T200000\r\n" +
	"DTEND;TZID=Europe/Paris:20260112T213000\r\n" +
	"SUMMARY:Choir (late)\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:market\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART;VALUE=DATE:20260307\r\n" +
	"SUMMARY:Market\r\n" +
	"CLASS:PRIVATE\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:hourly\r\n" +
	"DTSTAMP:20260101T000000Z\r\n" +
	"DTSTART:20260101T080000Z\r\n" +
	"RRULE:FREQ=HOURLY;COUNT=3\r\n" +
	"SUMMARY:Hourly\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// fakeOutlookCalendar is just enough of Graph for import and export: events
// with a repeat rule of weekly dates, whose dates can be changed or deleted.
type fakeOutlookCalendar struct {
	mu       sync.Mutex
	token    string
	requests int
	events   []map[string]any
	deleted  map[string]bool
	changed  map[string]map[string]any
}

func (f *fakeOutlookCalendar) occurrences(series map[string]any) []map[string]any {
	start := series["start"].(map[string]any)
	zone, _ := time.LoadLocation(start["timeZone"].(string))
	at, _ := time.ParseInLocation("2006-01-02T15:04:05", start["dateTime"].(string), zone)
	count := int(series["recurrence"].(map[string]any)["range"].(map[string]any)["numberOfOccurrences"].(float64))
	var out []map[string]any
	for i := range count {
		id := fmt.Sprintf("%s-%d", series["id"], i)
		if f.deleted[id] {
			continue
		}
		original := at.AddDate(0, 0, 7*i).UTC()
		instance := map[string]any{"id": id, "type": "occurrence", "originalStart": original.Format(time.RFC3339), "subject": series["subject"],
			"start": map[string]string{"dateTime": original.Format("2006-01-02T15:04:05"), "timeZone": "UTC"},
			"end":   map[string]string{"dateTime": original.Add(90 * time.Minute).Format("2006-01-02T15:04:05"), "timeZone": "UTC"}}
		if change := f.changed[id]; change != nil {
			for key, value := range change {
				instance[key] = value
			}
			instance["type"] = "exception"
		}
		out = append(out, instance)
	}
	return out
}

func (f *fakeOutlookCalendar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	w.Header().Set("Content-Type", "application/json")
	const events = "/me/calendars/cal/events"
	if r.Header.Get("Authorization") != "Bearer "+f.token || !strings.HasPrefix(r.URL.Path, events) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, events), "/")
	switch {
	case r.Method == http.MethodGet && rest == "" && r.URL.Query().Get("$filter") != "":
		uid := regexp.MustCompile(`ep/value eq '([^']*)'`).FindStringSubmatch(r.URL.Query().Get("$filter"))
		var found []map[string]any
		for _, event := range f.events {
			if len(uid) == 2 && event["singleValueExtendedProperties"].([]any)[0].(map[string]any)["value"] == uid[1] {
				found = append(found, map[string]any{"id": event["id"]})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"value": found})
	case r.Method == http.MethodGet && rest == "":
		var listed []map[string]any
		for _, event := range f.events {
			copy := map[string]any{}
			for key, value := range event {
				copy[key] = value
			}
			copy["type"] = "singleInstance"
			if event["recurrence"] != nil {
				copy["type"] = "seriesMaster"
			}
			copy["originalStartTimeZone"] = event["start"].(map[string]any)["timeZone"]
			listed = append(listed, copy)
		}
		json.NewEncoder(w).Encode(map[string]any{"value": listed})
	case r.Method == http.MethodPost && rest == "":
		var event map[string]any
		_ = json.NewDecoder(r.Body).Decode(&event)
		event["id"] = fmt.Sprintf("e%d", len(f.events))
		f.events = append(f.events, event)
		json.NewEncoder(w).Encode(map[string]any{"id": event["id"]})
	case r.Method == http.MethodGet && strings.HasSuffix(rest, "/instances"):
		from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("startDateTime"))
		to, _ := time.Parse(time.RFC3339, r.URL.Query().Get("endDateTime"))
		var in []map[string]any
		for _, event := range f.events {
			if event["id"] == strings.TrimSuffix(rest, "/instances") {
				for _, instance := range f.occurrences(event) {
					original, _ := time.Parse(time.RFC3339, instance["originalStart"].(string))
					if !original.Before(from) && original.Before(to) {
						in = append(in, instance)
					}
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"value": in})
	case r.Method == http.MethodDelete:
		f.deleted[rest] = true
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch:
		var change map[string]any
		_ = json.NewDecoder(r.Body).Decode(&change)
		f.changed[rest] = change
		json.NewEncoder(w).Encode(map[string]any{"id": rest})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestOutlookCalendarImportThenExport(t *testing.T) {
	fake := &fakeOutlookCalendar{token: "token", deleted: map[string]bool{}, changed: map[string]map[string]any{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	prior := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	defer func() { outlookGraphBaseURL = prior }()
	source := storage.CalendarSource{ID: "source", Provider: "outlook", Name: "Evenings", RemoteID: "cal", AccessRole: "owner"}
	credentials := calendarCredentials{token: "token"}

	resources, err := calendarImportResources([]byte(outlookImportTestFile))
	if err != nil {
		t.Fatal(err)
	}
	result, err := importCalendarEvents(t.Context(), source, credentials, resources, nil)
	if err != nil || result.Added != 2 || result.Existing != 0 || result.Failed != 1 || !strings.HasPrefix(result.Reason, "“Hourly”: Microsoft calendars have no repeat pattern like its FREQ=HOURLY") {
		t.Fatalf("first import = %+v, %v (an hourly rule has no Microsoft pattern)", result, err)
	}
	choir := fake.events[0]
	if choir["isReminderOn"] != true || choir["reminderMinutesBeforeStart"] != float64(1440) {
		t.Errorf("reminder imported as %v %v, want the earliest, a day before", choir["isReminderOn"], choir["reminderMinutesBeforeStart"])
	}
	if extra := fake.events[1]; extra["subject"] != "Choir" || extra["recurrence"] != nil || extra["start"].(map[string]any)["dateTime"] != "2026-01-08T19:00:00" || extra["end"].(map[string]any)["dateTime"] != "2026-01-08T20:30:00" {
		t.Errorf("the series' extra date imported as %v", extra)
	}
	if len(fake.events) != 3 {
		t.Errorf("imported %d events, want the series, its one extra date and the market", len(fake.events))
	}
	if _, ok := fake.events[2]["isReminderOn"]; ok {
		t.Error("an event without alarms didn't get the calendar's reminder")
	}
	if body := choir["body"].(map[string]any)["content"]; body != "Guests: sam@example.com" {
		t.Errorf("guests written as %q", body)
	}
	if choir["attendees"] != nil || choir["showAs"] != "free" {
		t.Errorf("series imported as %v: guests would get invitations", choir)
	}
	if start := choir["start"].(map[string]any); start["dateTime"] != "2026-01-05T19:00:00" || start["timeZone"] != "Europe/Paris" {
		t.Errorf("series start = %v", start)
	}
	if pattern := fmt.Sprint(choir["recurrence"]); !strings.Contains(pattern, "daysOfWeek:[monday]") || !strings.Contains(pattern, "numberOfOccurrences:4") {
		t.Errorf("repeat imported as %s", pattern)
	}
	if !fake.deleted["e0-2"] || fake.changed["e0-1"]["subject"] != "Choir (late)" || len(fake.deleted) != 1 || len(fake.changed) != 1 {
		t.Errorf("series dates: deleted %v, changed %v", fake.deleted, fake.changed)
	}
	market := fake.events[2]
	if market["isAllDay"] != true || market["sensitivity"] != "private" || market["start"].(map[string]any)["dateTime"] != "2026-03-07T00:00:00" || market["end"].(map[string]any)["dateTime"] != "2026-03-08T00:00:00" {
		t.Errorf("all-day event imported as %v", market)
	}

	fake.requests = 0
	result, err = importCalendarEvents(t.Context(), source, credentials, resources, nil)
	if err != nil || result.Existing != 2 || result.Failed != 1 || len(fake.events) != 3 || fake.requests != 1 {
		t.Fatalf("second import = %+v, %v, %d events", result, err, len(fake.events))
	}

	data, err := exportCalendarEvents(t.Context(), source, credentials)
	if err != nil {
		t.Fatal(err)
	}
	exported := string(data)
	for _, want := range []string{"UID:choir", "UID:market", "RRULE:FREQ=WEEKLY;BYDAY=MO;WKST=MO;COUNT=4", "EXDATE;TZID=Europe/Paris:20260119T190000", "RECURRENCE-ID;TZID=Europe/Paris:20260112T190000", "SUMMARY:Choir (late)", "DTSTART;VALUE=DATE:20260307", "CLASS:PRIVATE", "TRANSP:TRANSPARENT", "TRIGGER:-PT1440M"} {
		if !strings.Contains(exported, want) {
			t.Errorf("export lacks %q:\n%s", want, exported)
		}
	}
	if strings.Count(exported, "BEGIN:VEVENT") != 4 || !strings.Contains(exported, "UID:choir#rdate-20260108T180000Z") {
		t.Errorf("export has %d events, want the series, its changed date, its extra date and the market:\n%s", strings.Count(exported, "BEGIN:VEVENT"), exported)
	}
}

func TestOutlookRecurrenceBothWays(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	start := calendarImportTime{at: time.Date(2026, 1, 13, 9, 0, 0, 0, paris), zone: paris}
	for _, test := range []struct{ rule, pattern, back string }{
		{"FREQ=DAILY;INTERVAL=2;COUNT=5", "daily", "FREQ=DAILY;INTERVAL=2;COUNT=5"},
		{"FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR", "weekly [monday tuesday wednesday thursday friday]", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"},
		{"FREQ=WEEKLY;BYDAY=TU,TH;WKST=MO;UNTIL=20260331T225959Z", "weekly [tuesday thursday]", "FREQ=WEEKLY;BYDAY=TU,TH;WKST=MO;UNTIL=20260331T215959Z"},
		{"FREQ=MONTHLY", "absoluteMonthly 13", "FREQ=MONTHLY;BYMONTHDAY=13"},
		{"FREQ=MONTHLY;BYDAY=2TU", "relativeMonthly [tuesday] second", "FREQ=MONTHLY;BYDAY=TU;BYSETPOS=2"},
		{"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1", "relativeMonthly [monday tuesday wednesday thursday friday] last", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1"},
		{"FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU", "relativeYearly [sunday] last 3", "FREQ=YEARLY;BYMONTH=3;BYDAY=SU;BYSETPOS=-1"},
		{"FREQ=YEARLY", "absoluteYearly 13 1", "FREQ=YEARLY;BYMONTH=1;BYMONTHDAY=13"},
	} {
		recurrence, err := outlookImportRecurrence(test.rule, start)
		if err != nil {
			t.Errorf("%s: %v", test.rule, err)
			continue
		}
		pattern := recurrence["pattern"].(map[string]any)
		var got []string
		for _, key := range []string{"type", "daysOfWeek", "dayOfMonth", "index", "month"} {
			if value, ok := pattern[key]; ok {
				got = append(got, fmt.Sprint(value))
			}
		}
		if strings.Join(got, " ") != test.pattern {
			t.Errorf("%s: pattern %q, want %q", test.rule, strings.Join(got, " "), test.pattern)
		}
		raw, _ := json.Marshal(recurrence)
		back, err := outlookExportRule(raw, false, paris)
		if err != nil || back != test.back {
			t.Errorf("%s: back as %q, %v, want %q", test.rule, back, err, test.back)
		}
	}
	for _, rule := range []string{"FREQ=HOURLY", "FREQ=MONTHLY;BYMONTHDAY=1,15", "FREQ=MONTHLY;BYDAY=5FR", "FREQ=WEEKLY;BYHOUR=9,17", "FREQ=MONTHLY;BYMONTHDAY=-1"} {
		if _, err := outlookImportRecurrence(rule, start); err == nil {
			t.Errorf("%s: imported though Microsoft has no pattern like it", rule)
		}
	}
}

func TestCalendarImportAborts(t *testing.T) {
	for status, aborts := range map[int]bool{400: false, 404: false, 409: false, 401: true, 403: true, 429: true, 500: true, 503: true} {
		if got := calendarImportAborts(calendarCreateProviderError{status}); got != aborts {
			t.Errorf("HTTP %d aborts = %v, want %v", status, got, aborts)
		}
	}
	if calendarImportAborts(calendarTransferProblem("unreadable event")) {
		t.Error("an unreadable event ends the import")
	}
}

func TestCalendarImportReminders(t *testing.T) {
	file := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:r\r\nDTSTAMP:20260101T000000Z\r\n" +
		"DTSTART:20260301T100000Z\r\nDTEND:20260301T120000Z\r\nSUMMARY:Talk\r\n" +
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nEND:VALARM\r\n" +
		"BEGIN:VALARM\r\nACTION:EMAIL\r\nTRIGGER;RELATED=END:-PT3H\r\nEND:VALARM\r\n" +
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER;VALUE=DATE-TIME:20260301T080000Z\r\nEND:VALARM\r\n" +
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:PT5M\r\nEND:VALARM\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	resources, err := calendarImportResources([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	series, _, err := readCalendarImportResource(resources[0])
	if err != nil {
		t.Fatal(err)
	}
	reminders, alarms := calendarImportReminders(*series)
	// Three hours before the end is an hour before the start; five minutes
	// after the start isn't a reminder Google or Microsoft keep.
	if got := fmt.Sprint(reminders); !alarms || got != "[{120 false} {60 true} {15 false}]" {
		t.Fatalf("reminders = %s, %v", got, alarms)
	}
}

func TestOutlookImportDescriptionNamesTheMeetingsPeople(t *testing.T) {
	file := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:m\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260301T100000Z\r\nSUMMARY:Planning\r\n" +
		"DESCRIPTION:Bring the budget.\r\n" +
		"ORGANIZER;CN=Robin Diaz:mailto:robin@example.com\r\n" +
		"ATTENDEE;CN=Robin Diaz;PARTSTAT=ACCEPTED:mailto:robin@example.com\r\n" +
		"ATTENDEE;CN=Sam Lee;PARTSTAT=DECLINED:mailto:sam@example.com\r\n" +
		"ATTENDEE;PARTSTAT=TENTATIVE:mailto:kim@example.com\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:p\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260302T100000Z\r\nSUMMARY:Dentist\r\nORGANIZER:mailto:robin@example.com\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	resources, err := calendarImportResources([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	want := "Bring the budget.\n\nOrganizer: Robin Diaz (robin@example.com)\nGuests: Sam Lee (sam@example.com), kim@example.com"
	if got := outlookImportDescription(resources[0].Events()[0].Component); got != want {
		t.Errorf("meeting description = %q, want %q", got, want)
	}
	if got := outlookImportDescription(resources[1].Events()[0].Component); got != "" {
		t.Errorf("an event with only an organizer got %q", got)
	}
}

// An extra date the file changed comes in as its changed copy, and one the
// file deleted doesn't come in; neither is looked for in the series.
func TestOutlookImportChangedAndDeletedExtraDates(t *testing.T) {
	fake := &fakeOutlookCalendar{token: "token", deleted: map[string]bool{}, changed: map[string]map[string]any{}}
	server := httptest.NewServer(fake)
	defer server.Close()
	prior := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	defer func() { outlookGraphBaseURL = prior }()
	file := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\n" +
		"BEGIN:VEVENT\r\nUID:yoga\r\nDTSTAMP:20260101T000000Z\r\nDTSTART:20260105T080000Z\r\nDTEND:20260105T090000Z\r\nRRULE:FREQ=WEEKLY;COUNT=2\r\n" +
		"RDATE:20260108T080000Z,20260110T080000Z\r\nEXDATE:20260110T080000Z\r\nSUMMARY:Yoga\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:yoga\r\nDTSTAMP:20260101T000000Z\r\nRECURRENCE-ID:20260108T080000Z\r\nDTSTART:20260109T100000Z\r\nDTEND:20260109T110000Z\r\nSUMMARY:Yoga outside\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	resources, err := calendarImportResources([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	result, err := importCalendarEvents(t.Context(), storage.CalendarSource{Provider: "outlook", RemoteID: "cal"}, calendarCredentials{token: "token"}, resources, nil)
	if err != nil || result != (calendarImportResult{Added: 1}) {
		t.Fatalf("import = %+v, %v", result, err)
	}
	if len(fake.events) != 2 || len(fake.changed) != 0 || len(fake.deleted) != 0 || fake.requests != 3 {
		t.Fatalf("events %v, changed %v, deleted %v", fake.events, fake.changed, fake.deleted)
	}
	if extra := fake.events[1]; extra["subject"] != "Yoga outside" || extra["start"].(map[string]any)["dateTime"] != "2026-01-09T10:00:00" {
		t.Errorf("the changed extra date imported as %v", extra)
	}
}
