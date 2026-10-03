package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

func calendarRecurringForm() url.Values {
	form := calendarCreateForm()
	form.Set("repeat_frequency", "weekly")
	form.Set("repeat_interval", "1")
	form.Set("repeat_end", "count")
	form.Set("repeat_count", "4")
	return form
}

func TestCalendarRecurrenceValidation(t *testing.T) {
	for _, frequency := range []string{"daily", "weekly", "monthly", "yearly"} {
		for _, ending := range []string{"never", "until", "count"} {
			form := calendarRecurringForm()
			form.Set("repeat_frequency", frequency)
			form.Set("repeat_end", ending)
			if ending != "count" {
				form.Del("repeat_count")
			}
			if ending == "until" {
				form.Set("repeat_until", "2026-10-02") // Inclusive; one occurrence is valid.
			}
			draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one"))
			if err != nil || draft.Recurrence == nil || draft.Recurrence.Frequency != frequency {
				t.Fatalf("%s/%s: %#v, %v", frequency, ending, draft, err)
			}
		}
	}
	for _, test := range []struct{ key, value string }{
		{"repeat_frequency", "hourly"}, {"repeat_frequency", "WEEKLY"}, {"repeat_frequency", "none"},
		{"repeat_interval", ""}, {"repeat_interval", "0"}, {"repeat_interval", "-1"}, {"repeat_interval", "100"}, {"repeat_interval", "1.5"}, {"repeat_interval", "1e2"},
		{"repeat_end", ""}, {"repeat_end", "forever"}, {"repeat_end", "never"}, {"repeat_end", "until"},
		{"repeat_count", "0"}, {"repeat_count", "-1"}, {"repeat_count", "1000"}, {"repeat_count", "2.5"},
		{"repeat_until", "2026-10-10"}, {"recurrence", "RRULE:FREQ=DAILY"}, {"attendees", "a@example.com"},
	} {
		form := calendarRecurringForm()
		form.Set(test.key, test.value)
		if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one")); err == nil {
			t.Errorf("accepted invalid %s=%q", test.key, test.value)
		}
	}
	for _, until := range []string{"2026-10-01", "2026-02-30", "", "0000-01-01", "2026-10-20\r\nCOUNT=1"} {
		form := calendarRecurringForm()
		form.Set("repeat_end", "until")
		form.Del("repeat_count")
		form.Set("repeat_until", until)
		if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one")); err == nil {
			t.Errorf("accepted invalid until=%q", until)
		}
	}
	form := calendarRecurringForm()
	form.Add("repeat_frequency", "daily")
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one")); err == nil {
		t.Fatal("accepted duplicate recurrence field")
	}
	form = calendarCreateForm()
	form.Set("repeat_frequency", "none")
	draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one"))
	if err != nil || draft.Recurrence != nil {
		t.Fatal("does-not-repeat changed a single event")
	}
	// Adding the optional field must preserve durable pre-feature hashes.
	raw, _ := json.Marshal(draft)
	if strings.Contains(string(raw), "Recurrence") {
		t.Fatal("nil recurrence changed existing idempotency hashes")
	}
	form = calendarRecurringForm()
	form.Set("version", `"v1"`)
	if edited, _, err := parseCalendarUpdateDraft(calendarCreateHTTPRequest(form, "one")); err != nil || edited.Recurrence == nil || edited.Recurrence.Count != 4 {
		t.Fatalf("edit rejected valid repeat settings: %+v %v", edited.Recurrence, err)
	}
	r := calendarCreateHTTPRequest(calendarCreateForm(), "one")
	r.URL.RawQuery = "repeat_frequency=daily&repeat_interval=1&repeat_end=never"
	draft, err = parseCalendarEventDraft(r)
	if err != nil || draft.Recurrence != nil {
		t.Fatal("query string introduced recurrence")
	}
	form = calendarRecurringForm()
	form.Set("repeat_end", "until")
	form.Del("repeat_count")
	form.Set("repeat_until", "9999-12-31")
	form.Set("timezone", "America/New_York")
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one")); err == nil {
		t.Fatal("accepted an UNTIL outside the four-digit UTC year range")
	}
}

func TestCalendarRecurringCreateRequiresConfirmedSeries(t *testing.T) {
	h := calendarCreateFixture(t)
	h.calendarCreateEvent = func(_ context.Context, _ storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		return calendarCreatedRemote(draft), nil // Provider ignored recurrence.
	}
	h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
		t.Fatal("unconfirmed series must not refresh or claim success")
		return calendar.EventPage{}, nil
	}
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(calendarRecurringForm(), "one"))
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"uncertain":true`) {
		t.Fatalf("claimed successful series creation: %d %s", w.Code, w.Body.String())
	}
	var remoteID string
	if err := h.db.Read().QueryRow(`SELECT remote_id FROM calendar_create_requests WHERE user_id='one'`).Scan(&remoteID); err != nil || remoteID != "" {
		t.Fatal("unconfirmed series marked as completed")
	}
}

func TestCalendarRecurrenceRulesAndLocalTimes(t *testing.T) {
	for _, test := range []struct {
		name, frequency, start, until string
		allDay                        bool
		count                         int
		want                          []string
	}{
		{"weekly across DST", "weekly", "2026-10-02", "", false, 5, []string{"2026-10-02 09:00 +0200", "2026-10-09 09:00 +0200", "2026-10-16 09:00 +0200", "2026-10-23 09:00 +0200", "2026-10-30 09:00 +0100"}},
		{"month end", "monthly", "2026-01-31", "", true, 3, []string{"2026-01-31", "2026-02-28", "2026-03-31"}},
		{"leap day", "yearly", "2024-02-29", "", true, 3, []string{"2024-02-29", "2025-02-28", "2026-02-28"}},
		{"inclusive local until", "daily", "2026-10-24", "2026-10-26", false, 0, []string{"2026-10-24 09:00 +0200", "2026-10-25 09:00 +0100", "2026-10-26 09:00 +0100"}},
		{"inclusive all-day until", "daily", "2026-10-24", "2026-10-26", true, 0, []string{"2026-10-24", "2026-10-25", "2026-10-26"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			form := calendarRecurringForm()
			form.Set("repeat_frequency", test.frequency)
			form.Set("start_date", test.start)
			form.Set("end_date", test.start)
			if test.allDay {
				form.Set("all_day", "true")
			}
			if test.until != "" {
				form.Set("repeat_end", "until")
				form.Set("repeat_until", test.until)
				form.Del("repeat_count")
			}
			draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one"))
			if err != nil {
				t.Fatal(err)
			}
			draft.Recurrence.Count = test.count
			rule := calendarRecurrenceRule(draft)
			options, err := rrule.StrToROption(rule)
			if err != nil {
				t.Fatal(err)
			}
			options.Dtstart = calendarDraftStart(draft)
			recurrence, err := rrule.NewRRule(*options)
			if err != nil {
				t.Fatal(err)
			}
			got := recurrence.All()
			if len(got) != len(test.want) {
				t.Fatalf("%s: got %v", rule, got)
			}
			layout := "2006-01-02 15:04 -0700"
			if test.allDay {
				layout = "2006-01-02"
			}
			for i, at := range got {
				if at.Format(layout) != test.want[i] {
					t.Errorf("occurrence %d: %s != %s", i, at.Format(layout), test.want[i])
				}
			}
			graph := calendarOutlookRecurrence(draft)
			rangeValue := graph["range"].(map[string]any)
			if rangeValue["startDate"] != test.start || rangeValue["recurrenceTimeZone"] != "Europe/Prague" {
				t.Fatalf("Graph range lost local dates: %v", graph)
			}
			if test.until != "" && rangeValue["endDate"] != test.until {
				t.Fatal("Graph end date is not inclusive")
			}
		})
	}
}

func TestCalendarRecurringCreationCachesOnlyRealInstancesAndReplays(t *testing.T) {
	h := calendarCreateFixture(t)
	form := calendarRecurringForm()
	draft, _ := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one"))
	writes, reads := 0, 0
	h.calendarCreateEvent = func(_ context.Context, _ storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		writes++
		event := calendarCreatedRemote(draft)
		event.Recurrence = json.RawMessage(`["RRULE:FREQ=WEEKLY;COUNT=4"]`)
		return event, nil
	}
	h.calendarFetchEvents = func(_ context.Context, source storage.CalendarSource, query calendar.EventQuery) (calendar.EventPage, error) {
		reads++
		if source.ID != "one-source" || source.UserID != "one" || !query.WindowStart.Before(*draft.StartAt) || !query.WindowEnd.After(*draft.EndAt) {
			t.Fatal("refresh escaped the owned source/window")
		}
		if reads == 1 {
			return calendar.EventPage{}, errors.New("temporary read failure after confirmed creation")
		}
		event := calendarCreatedRemote(draft)
		event.RemoteID, event.SeriesRemoteID = "real-provider-occurrence", draft.RequestID
		event.ETag = `"instance-version"`
		return calendar.EventPage{Events: []calendar.RemoteEvent{event}}, nil
	}
	for attempt := range 3 {
		w := httptest.NewRecorder()
		h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(form, "one"))
		var result struct {
			EventID  string `json:"event_id"`
			SeriesID string `json:"series_id"`
			Pending  bool   `json:"refresh_pending"`
			Replayed bool   `json:"replayed"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 201 || result.SeriesID == "" || result.EventID != "" || result.Pending != (attempt == 0) || result.Replayed != (attempt > 0) {
			t.Fatalf("attempt %d: %d %s, %v", attempt, w.Code, w.Body.String(), err)
		}
		var count int
		_ = h.db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_events WHERE source_id='one-source'`).Scan(&count)
		if count != min(attempt, 1) {
			t.Fatalf("cached the master or duplicated an occurrence: %d", count)
		}
	}
	if writes != 1 || reads != 3 {
		t.Fatalf("provider writes=%d reads=%d", writes, reads)
	}
	events, err := h.db.ListCalendarEvents(t.Context(), "one", draft.StartAt.Add(-time.Hour), draft.EndAt.Add(time.Hour))
	if err != nil || len(events) != 1 || events[0].SeriesRemoteID == "" || calendarStoredEditRestriction(events[0]) == "" {
		t.Fatalf("series instance is editable or missing: %v %v", events, err)
	}
	form.Set("repeat_count", "5")
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(form, "one"))
	if w.Code != 409 || writes != 1 {
		t.Fatal("changing recurrence reused a completed request")
	}
}

func TestCalendarRecurrenceTimezoneCoversDSTAndOpenEndedSeries(t *testing.T) {
	for _, zone := range []string{"Europe/Prague", "Australia/Lord_Howe", "America/New_York", "Africa/Casablanca", "Asia/Kathmandu", "UTC"} {
		t.Run(zone, func(t *testing.T) {
			draft := calendarProviderDraft(t, false)
			draft.TimeZone = zone
			draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "weekly", Interval: 1}
			body, err := calendarCreateICS(draft)
			if err != nil {
				t.Fatal(err)
			}
			if len(body) > 1<<20 {
				t.Fatalf("unbounded timezone payload: %d bytes", len(body))
			}
			decoded, err := ical.NewDecoder(strings.NewReader(body)).Decode()
			if err != nil || len(decoded.Events()) != 1 {
				t.Fatalf("invalid recurrence ICS: %v", err)
			}
			event := decoded.Events()[0]
			start, err := event.DateTimeStart(nil)
			if err != nil || !start.Equal(*draft.StartAt) || event.Props.Get("RRULE") == nil {
				t.Fatalf("lost event time/rule: %v %v", start, err)
			}
			if zone == "UTC" {
				if strings.Contains(body, "VTIMEZONE") {
					t.Fatal("UTC should not embed an unnecessary timezone")
				}
				return
			}
			if !strings.Contains(body, "TZID="+zone) || !strings.Contains(body, "BEGIN:VTIMEZONE") {
				t.Fatal("timed recurrence must retain wall time and timezone rules")
			}
			if zone == "Europe/Prague" {
				unfolded := strings.ReplaceAll(body, "\r\n ", "")
				for _, want := range []string{"20261025T030000", "20270328T020000", "9999", "TZOFFSETFROM:+0200", "TZOFFSETTO:+0100"} {
					if !strings.Contains(unfolded, want) {
						t.Errorf("missing timezone transition %s", want)
					}
				}
			}
		})
	}
}
