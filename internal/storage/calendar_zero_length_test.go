package storage

import (
	"testing"
	"time"
)

// A provider may confirm an event that ends when it starts; an edit or a
// response to one is saved like any other.
func TestCalendarMutationsKeepEventsThatEndWhenTheyStart(t *testing.T) {
	at := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	event := CalendarEvent{RemoteID: "moment", ETag: `"v2"`, StartAt: &at, EndAt: &at, ResponseStatus: "accepted", AttendeesJSON: "[]"}
	if _, err := calendarUpdateValues(`"v1"`, event, false, false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := calendarResponseValues(CalendarEvent{RemoteID: "moment"}, event, false); err != nil {
		t.Fatalf("response: %v", err)
	}
	before := at.Add(-time.Minute)
	event.EndAt = &before
	if _, err := calendarUpdateValues(`"v1"`, event, false, false); err == nil {
		t.Fatal("update accepted an end before the start")
	}
}
