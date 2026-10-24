package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/emersion/go-ical"
)

func decodeJMAPEvent(t *testing.T, raw string) jmap.CalendarEvent {
	t.Helper()
	var event jmap.CalendarEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	return event
}

// icsEvents encodes and decodes the converted calendar, as a CalDAV server's
// data would arrive, and gives its VEVENTs.
func icsEvents(t *testing.T, event jmap.CalendarEvent) []ical.Event {
	t.Helper()
	data, err := JMAPEventICS(event)
	if err != nil {
		t.Fatalf("JMAPEventICS: %v", err)
	}
	cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		t.Fatalf("decode:\n%s\n%v", data, err)
	}
	return cal.Events()
}

func propValue(props ical.Props, name string) string {
	if prop := props.Get(name); prop != nil {
		return prop.Value
	}
	return ""
}

// weeklySync is a meeting series in the JSCalendar form Stalwart uses: one
// occurrence cancelled and one moved.
const weeklySync = `{
  "@type": "Event", "uid": "weekly-sync@example.test", "title": "Weekly sync", "description": "Agenda\nline two",
  "start": "2026-10-26T09:00:00", "timeZone": "Europe/Prague", "duration": "PT1H", "sequence": 2,
  "updated": "2026-10-24T10:00:00Z", "status": "confirmed", "freeBusyStatus": "free", "privacy": "private",
  "locations": {"l1": {"@type": "Location", "name": "Room 4"}},
  "recurrenceRule": {"frequency": "weekly", "until": "2026-11-30T09:00:00", "byDay": [{"day": "mo"}]},
  "organizerCalendarAddress": "mailto:hana@example.test",
  "participants": {
    "p1": {"@type": "Participant", "name": "Hana Novak", "calendarAddress": "mailto:hana@example.test", "participationStatus": "accepted", "roles": {"chair": true, "owner": true}},
    "p2": {"@type": "Participant", "name": "Ana Lopes", "calendarAddress": "mailto:ana@example.test", "participationStatus": "needs-action", "roles": {"required": true}, "expectReply": true}
  },
  "alerts": {"k1": {"@type": "Alert", "action": "display", "trigger": {"@type": "OffsetTrigger", "offset": "-PT10M"}}},
  "recurrenceOverrides": {
    "2026-11-02T09:00:00": {"excluded": true},
    "2026-11-09T09:00:00": {"title": "Weekly sync (moved)", "start": "2026-11-09T11:00:00", "duration": "PT1H",
      "participants": {"p2": {"@type": "Participant", "name": "Ana Lopes", "calendarAddress": "mailto:ana@example.test", "participationStatus": "accepted"}}}
  },
  "iCalendar": {"name": "vevent", "properties": [["x-example-note", {}, "unknown", "keep me"]]}
}`

func TestJMAPEventCalendarWritesASeriesWithItsChangedOccurrences(t *testing.T) {
	events := icsEvents(t, decodeJMAPEvent(t, weeklySync))
	if len(events) != 2 {
		t.Fatalf("got %d VEVENTs, want the series and the moved occurrence", len(events))
	}
	master, moved := events[0].Props, events[1].Props
	checks := map[string][2]string{
		"UID":           {propValue(master, "UID"), "weekly-sync@example.test"},
		"SUMMARY":       {propValue(master, "SUMMARY"), "Weekly sync"},
		"DTSTART":       {propValue(master, "DTSTART"), "20261026T090000"},
		"DTSTART TZID":  {master.Get("DTSTART").Params.Get("TZID"), "Europe/Prague"},
		"DTEND":         {propValue(master, "DTEND"), "20261026T100000"},
		"RRULE":         {propValue(master, "RRULE"), "FREQ=WEEKLY;BYDAY=MO;UNTIL=20261130T080000Z"},
		"EXDATE":        {propValue(master, "EXDATE"), "20261102T090000"},
		"RDATE":         {propValue(master, "RDATE"), ""},
		"STATUS":        {propValue(master, "STATUS"), "CONFIRMED"},
		"TRANSP":        {propValue(master, "TRANSP"), "TRANSPARENT"},
		"CLASS":         {propValue(master, "CLASS"), "PRIVATE"},
		"LOCATION":      {propValue(master, "LOCATION"), "Room 4"},
		"SEQUENCE":      {propValue(master, "SEQUENCE"), "2"},
		"ORGANIZER":     {propValue(master, "ORGANIZER"), "mailto:hana@example.test"},
		"ORGANIZER CN":  {master.Get("ORGANIZER").Params.Get("CN"), "Hana Novak"},
		"X-EXAMPLE":     {propValue(master, "X-EXAMPLE-NOTE"), "keep me"},
		"RECURRENCE-ID": {propValue(moved, "RECURRENCE-ID"), "20261109T090000"},
		"moved DTSTART": {propValue(moved, "DTSTART"), "20261109T110000"},
		"moved DTEND":   {propValue(moved, "DTEND"), "20261109T120000"},
		"moved SUMMARY": {propValue(moved, "SUMMARY"), "Weekly sync (moved)"},
		"moved RRULE":   {propValue(moved, "RRULE"), ""},
	}
	for name, check := range checks {
		if check[0] != check[1] {
			t.Errorf("%s = %q, want %q", name, check[0], check[1])
		}
	}
	if description, _ := master.Text("DESCRIPTION"); description != "Agenda\nline two" {
		t.Errorf("DESCRIPTION = %q", description)
	}
	attendees := master.Values("ATTENDEE")
	if len(attendees) != 2 {
		t.Fatalf("attendees = %+v", attendees)
	}
	if a := attendees[0]; a.Value != "mailto:hana@example.test" || a.Params.Get("ROLE") != "CHAIR" || a.Params.Get("PARTSTAT") != "ACCEPTED" {
		t.Errorf("organizer attendee = %+v", a)
	}
	if a := attendees[1]; a.Params.Get("CN") != "Ana Lopes" || a.Params.Get("ROLE") != "REQ-PARTICIPANT" || a.Params.Get("PARTSTAT") != "NEEDS-ACTION" || a.Params.Get("RSVP") != "TRUE" {
		t.Errorf("guest = %+v", a)
	}
	// The moved occurrence lists its own participants: the organizer stays
	// as the organizer, Ana has accepted.
	if propValue(moved, "ORGANIZER") != "mailto:hana@example.test" {
		t.Errorf("moved ORGANIZER = %q", propValue(moved, "ORGANIZER"))
	}
	if a := moved.Values("ATTENDEE"); len(a) != 1 || a[0].Params.Get("PARTSTAT") != "ACCEPTED" {
		t.Errorf("moved attendees = %+v", a)
	}
	alarms := events[0].Children
	if len(alarms) != 1 || propValue(alarms[0].Props, "TRIGGER") != "-PT10M" || propValue(alarms[0].Props, "ACTION") != "DISPLAY" {
		t.Errorf("alarms = %+v", alarms)
	}
}

func TestJMAPEventCalendarReadsRFC8984Names(t *testing.T) {
	event := decodeJMAPEvent(t, `{
	  "@type": "Event", "uid": "book-club@example.test", "title": "Book club",
	  "start": "2026-03-05T19:00:00", "timeZone": "America/New_York", "duration": "PT2H",
	  "recurrenceRules": [{"@type": "RecurrenceRule", "frequency": "monthly", "interval": 2, "byDay": [{"day": "th", "nthOfPeriod": 1}], "count": 4}],
	  "replyTo": {"imip": "mailto:omar@example.test"},
	  "participants": {
	    "o": {"@type": "Participant", "name": "Omar Haddad", "sendTo": {"imip": "mailto:omar@example.test"}, "roles": {"owner": true}},
	    "g": {"@type": "Participant", "email": "lin@example.test", "roles": {"attendee": true, "optional": true}, "participationStatus": "tentative"}
	  },
	  "recurrenceOverrides": {
	    "2026-05-07T19:00:00": {"participants/g/participationStatus": "declined"},
	    "2026-06-20T19:00:00": {"title": "Book club picnic"}
	  }
	}`)
	events := icsEvents(t, event)
	if len(events) != 3 {
		t.Fatalf("got %d VEVENTs", len(events))
	}
	master := events[0].Props
	if got := propValue(master, "RRULE"); got != "FREQ=MONTHLY;INTERVAL=2;BYDAY=1TH;COUNT=4" {
		t.Errorf("RRULE = %q", got)
	}
	if propValue(master, "ORGANIZER") != "mailto:omar@example.test" || master.Get("ORGANIZER").Params.Get("CN") != "Omar Haddad" {
		t.Errorf("ORGANIZER = %+v", master.Get("ORGANIZER"))
	}
	// The owner without an attendee role is only the organizer.
	if a := master.Values("ATTENDEE"); len(a) != 1 || a[0].Value != "mailto:lin@example.test" || a[0].Params.Get("ROLE") != "OPT-PARTICIPANT" {
		t.Errorf("attendees = %+v", a)
	}
	// 20 June isn't a first Thursday: it is an extra date of the series.
	if got := master.Values("RDATE"); len(got) != 1 || got[0].Value != "20260620T190000" {
		t.Errorf("RDATE = %+v", got)
	}
	declined := events[1].Props
	if propValue(declined, "RECURRENCE-ID") != "20260507T190000" || declined.Values("ATTENDEE")[0].Params.Get("PARTSTAT") != "DECLINED" {
		t.Errorf("patched occurrence = %+v", declined)
	}
	if propValue(declined, "DTSTART") != "20260507T190000" || propValue(declined, "SUMMARY") != "Book club" {
		t.Errorf("patched occurrence start/title = %q %q", propValue(declined, "DTSTART"), propValue(declined, "SUMMARY"))
	}
	if propValue(events[2].Props, "SUMMARY") != "Book club picnic" {
		t.Errorf("extra occurrence = %+v", events[2].Props)
	}
}

func TestJMAPEventCalendarWritesAllDayAndFloatingEvents(t *testing.T) {
	allDay := icsEvents(t, decodeJMAPEvent(t, `{"@type": "Event", "uid": "trip@example.test", "title": "Trip", "start": "2026-07-01T00:00:00", "duration": "P3D", "showWithoutTime": true}`))[0].Props
	if start := allDay.Get("DTSTART"); start.Value != "20260701" || start.ValueType() != ical.ValueDate {
		t.Errorf("all-day DTSTART = %+v", start)
	}
	if end := allDay.Get("DTEND"); end.Value != "20260704" || end.ValueType() != ical.ValueDate {
		t.Errorf("all-day DTEND = %+v", end)
	}
	oneDay := icsEvents(t, decodeJMAPEvent(t, `{"@type": "Event", "uid": "holiday@example.test", "start": "2026-12-25T00:00:00", "showWithoutTime": true}`))[0].Props
	if propValue(oneDay, "DTEND") != "20261226" {
		t.Errorf("one-day DTEND = %q", propValue(oneDay, "DTEND"))
	}
	floating := icsEvents(t, decodeJMAPEvent(t, `{"@type": "Event", "uid": "run@example.test", "start": "2026-04-02T06:30:00", "duration": "PT45M"}`))[0].Props
	if start := floating.Get("DTSTART"); start.Value != "20260402T063000" || start.Params.Get("TZID") != "" {
		t.Errorf("floating DTSTART = %+v", start)
	}
	utc := icsEvents(t, decodeJMAPEvent(t, `{"@type": "Event", "uid": "call@example.test", "start": "2026-04-02T06:30:00", "timeZone": "Etc/UTC", "duration": "PT1H"}`))[0].Props
	if propValue(utc, "DTSTART") != "20260402T063000Z" || propValue(utc, "DTEND") != "20260402T073000Z" {
		t.Errorf("UTC event = %q - %q", propValue(utc, "DTSTART"), propValue(utc, "DTEND"))
	}
}

func TestJMAPEventCalendarRefusesWhatItCannotWrite(t *testing.T) {
	for name, raw := range map[string]string{
		"task":        `{"@type": "Task", "uid": "t@example.test", "start": "2026-01-01T00:00:00"}`,
		"no uid":      `{"@type": "Event", "start": "2026-01-01T00:00:00"}`,
		"no start":    `{"@type": "Event", "uid": "x@example.test"}`,
		"bad overrid": `{"@type": "Event", "uid": "x@example.test", "start": "2026-01-01T09:00:00", "recurrenceOverrides": {"2026-01-02T09:00:00": {"title": "x"}}}`,
	} {
		if _, err := JMAPEventICS(decodeJMAPEvent(t, raw)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestJSCalendarDuration(t *testing.T) {
	for value, want := range map[string]struct {
		days    int
		elapsed time.Duration
	}{
		"":          {},
		"PT1H":      {0, time.Hour},
		"P1D":       {1, 0},
		"P2W":       {14, 0},
		"P1DT2H30M": {1, 2*time.Hour + 30*time.Minute},
		"PT0.5S":    {0, 500 * time.Millisecond},
	} {
		days, elapsed, err := jscalendarDuration(value)
		if err != nil || days != want.days || elapsed != want.elapsed {
			t.Errorf("%q = %d, %v, %v", value, days, elapsed, err)
		}
	}
	for _, bad := range []string{"1H", "P", "PT1X", "P-1D"} {
		if _, _, err := jscalendarDuration(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// TestJMAPCalendarLiveRoundTrip has a real server read an iCalendar series,
// saves it, and checks the event read back converts to the same iCalendar
// details. It skips unless GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func TestJMAPCalendarLiveRoundTrip(t *testing.T) {
	sessionURL, user, password := os.Getenv("GOFER_JMAP_TEST_URL"), os.Getenv("GOFER_JMAP_TEST_USER"), os.Getenv("GOFER_JMAP_TEST_PASSWORD")
	if sessionURL == "" || user == "" || password == "" {
		t.Skip("GOFER_JMAP_TEST_URL, GOFER_JMAP_TEST_USER and GOFER_JMAP_TEST_PASSWORD not set")
	}
	ctx := context.Background()
	cfg := &models.AccountConfig{Provider: "jmap", JMAPSessionURL: sessionURL, Username: user, JMAPAllowPlaintext: true, JMAPAllowPrivate: true}
	calendars, err := JMAPCalendars(ctx, cfg, password)
	if err != nil || len(calendars) == 0 {
		t.Fatalf("calendars = %+v, %v", calendars, err)
	}
	calendarID := calendars[0].ID
	for _, c := range calendars {
		if c.IsDefault {
			calendarID = c.ID
		}
	}
	uid := "gofer-live-roundtrip-" + time.Now().UTC().Format("20060102T150405") + "@example.test"
	source := strings.Join([]string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//Gofer//test//EN",
		"BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20261024T100000Z",
		"DTSTART;TZID=Europe/Prague:20261026T090000", "DTEND;TZID=Europe/Prague:20261026T100000",
		"RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=4", "EXDATE;TZID=Europe/Prague:20261102T090000",
		"SUMMARY:Live round trip", "LOCATION:Room 9", "TRANSP:TRANSPARENT",
		"BEGIN:VALARM", "ACTION:DISPLAY", "DESCRIPTION:Reminder", "TRIGGER:-PT15M", "END:VALARM",
		"END:VEVENT",
		"BEGIN:VEVENT", "UID:" + uid, "DTSTAMP:20261024T100000Z",
		"RECURRENCE-ID;TZID=Europe/Prague:20261109T090000",
		"DTSTART;TZID=Europe/Prague:20261109T113000", "DTEND;TZID=Europe/Prague:20261109T123000",
		"SUMMARY:Live round trip (moved)", "END:VEVENT",
		"END:VCALENDAR", "",
	}, "\r\n")
	parsed, err := JMAPEventsFromICS(ctx, cfg, password, []byte(source))
	if err != nil || len(parsed) != 1 {
		t.Fatalf("parse = %+v, %v", parsed, err)
	}
	event := parsed[0]
	event["calendarIds"] = map[string]any{calendarID: true}

	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.SetCalendarEvents(ctx, session, jmap.CalendarEventSet{AccountID: account, Create: map[string]jmap.CalendarEvent{"new": event}})
	if err != nil || created.Created["new"].ID() == "" {
		t.Fatalf("create = %+v, %v", created, err)
	}
	id := created.Created["new"].ID()
	t.Cleanup(func() {
		_, _ = client.SetCalendarEvents(ctx, session, jmap.CalendarEventSet{AccountID: account, Destroy: []string{id}})
	})
	stored, err := client.GetCalendarEvents(ctx, session, account, []string{id})
	if err != nil || len(stored.List) != 1 {
		t.Fatalf("get = %+v, %v", stored, err)
	}
	events := icsEvents(t, stored.List[0])
	if len(events) != 2 {
		t.Fatalf("got %d VEVENTs", len(events))
	}
	master, moved := events[0].Props, events[1].Props
	for name, check := range map[string][2]string{
		"UID":           {propValue(master, "UID"), uid},
		"DTSTART":       {propValue(master, "DTSTART"), "20261026T090000"},
		"TZID":          {master.Get("DTSTART").Params.Get("TZID"), "Europe/Prague"},
		"DTEND":         {propValue(master, "DTEND"), "20261026T100000"},
		"RRULE":         {propValue(master, "RRULE"), "FREQ=WEEKLY;BYDAY=MO;COUNT=4"},
		"EXDATE":        {propValue(master, "EXDATE"), "20261102T090000"},
		"SUMMARY":       {propValue(master, "SUMMARY"), "Live round trip"},
		"LOCATION":      {propValue(master, "LOCATION"), "Room 9"},
		"TRANSP":        {propValue(master, "TRANSP"), "TRANSPARENT"},
		"RECURRENCE-ID": {propValue(moved, "RECURRENCE-ID"), "20261109T090000"},
		"moved DTSTART": {propValue(moved, "DTSTART"), "20261109T113000"},
		"moved SUMMARY": {propValue(moved, "SUMMARY"), "Live round trip (moved)"},
	} {
		if check[0] != check[1] {
			t.Errorf("%s = %q, want %q", name, check[0], check[1])
		}
	}
	if alarms := events[0].Children; len(alarms) != 1 || propValue(alarms[0].Props, "TRIGGER") != "-PT15M" {
		t.Errorf("alarms = %+v", alarms)
	}
	if found := slices.ContainsFunc(calendars, func(c jmap.Calendar) bool { return c.ID == calendarID }); !found {
		t.Error("calendar disappeared")
	}
}
