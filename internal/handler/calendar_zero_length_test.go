package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/emersion/go-ical"
)

// An event may end when it starts, as a reminder of a moment. Providers
// allow it, so sync keeps it; only an end before the start is refused.
func TestCalendarSyncKeepsEventsThatEndWhenTheyStart(t *testing.T) {
	google := googleCalendarEvent{ID: "moment", Start: googleCalendarEventDateTime{DateTime: "2026-10-02T14:00:00Z"}, End: googleCalendarEventDateTime{DateTime: "2026-10-02T14:00:00Z"}}
	event, err := normalizeGoogleCalendarEvent(google)
	if err != nil || event.StartAt == nil || event.EndAt == nil || !event.EndAt.Equal(*event.StartAt) {
		t.Fatalf("Google event that ends when it starts: %#v, %v", event, err)
	}
	google.End.DateTime = "2026-10-02T13:59:00Z"
	if _, err := normalizeGoogleCalendarEvent(google); err == nil {
		t.Fatal("Google event that ends before it starts accepted")
	}

	var outlook outlookCalendarEvent
	if err := json.Unmarshal([]byte(`{"id":"moment","start":{"dateTime":"2026-10-02T14:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-10-02T14:00:00","timeZone":"UTC"}}`), &outlook); err != nil {
		t.Fatal(err)
	}
	event, err = normalizeOutlookCalendarEvent(outlook)
	if err != nil || event.StartAt == nil || event.EndAt == nil || !event.EndAt.Equal(*event.StartAt) {
		t.Fatalf("Microsoft event that ends when it starts: %#v, %v", event, err)
	}
	outlook.End.DateTime = "2026-10-02T13:59:00"
	if _, err := normalizeOutlookCalendarEvent(outlook); err == nil {
		t.Fatal("Microsoft event that ends before it starts accepted")
	}
}

// The form takes an end equal to the start. iCalendar wants DTEND after
// DTSTART, so the event is written without one and still reads back with
// the same end.
func TestCalendarDraftThatEndsWhenItStarts(t *testing.T) {
	values := calendarCreateForm()
	values.Set("end_time", "09:00")
	draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
	if err != nil || draft.StartAt == nil || draft.EndAt == nil || !draft.EndAt.Equal(*draft.StartAt) {
		t.Fatalf("draft that ends when it starts: %#v, %v", draft, err)
	}
	if _, err := calendarUpdateDraftLocation(draft); err != nil {
		t.Fatalf("update refused a draft that ends when it starts: %v", err)
	}
	raw, err := calendarCreateICS(draft)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "DTEND") {
		t.Fatalf("event that ends when it starts written with DTEND:\n%s", raw)
	}
	decoded, err := calendarUpdateDecodeICS([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeCalDAVEvent(decoded.Events()[0], "https://example.com/event.ics", `"v1"`, nil)
	if err != nil || normalized.EndAt == nil || !normalized.EndAt.Equal(*draft.StartAt) {
		t.Fatalf("read back: %#v, %v", normalized, err)
	}
	if !calendarUpdateMatchesDraft(normalized, draft) {
		t.Fatal("confirmation rejected an event that ends when it starts")
	}

	values.Set("end_time", "08:59")
	if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
		t.Fatal("draft that ends before it starts accepted")
	}
}

// Editing an event to end when it starts removes its old DTEND.
func TestCalendarCopyICSPropRemovesWhatTheEditDropped(t *testing.T) {
	saved := ical.NewEvent()
	saved.Props.SetText("DTEND", "20261002T100000Z")
	saved.Props.SetText("SUMMARY", "Old")
	edited := ical.NewEvent()
	edited.Props.SetText("SUMMARY", "New")
	for _, name := range []string{"SUMMARY", "DTEND"} {
		calendarCopyICSProp(saved.Props, edited.Props, name)
	}
	if saved.Props.Get("DTEND") != nil {
		t.Fatal("old DTEND kept")
	}
	if summary, _ := saved.Props.Text("SUMMARY"); summary != "New" {
		t.Fatalf("SUMMARY = %q", summary)
	}
}
