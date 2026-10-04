package handler

import (
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarViewEventPreservesResponse(t *testing.T) {
	for _, status := range []string{"", "organizer", "needsAction", "tentative", "declined", "accepted"} {
		event := storage.CalendarEvent{ResponseStatus: status, SourceColor: "#4285f4", SourceHidden: true}
		view := calendarViewEvent(event)
		if view.ResponseStatus != status || view.SourceColor != event.SourceColor || !view.SourceHidden {
			t.Fatalf("response %q lost response or source presentation", status)
		}
	}
}

func TestCalendarViewCalDAVResponseMatchesOnlyLinkedMailbox(t *testing.T) {
	for _, test := range []struct{ name, attendees, organizer, cached, want string }{
		{"pending", `[{"email":"me@example.com","status":"NEEDS-ACTION"}]`, "host@example.com", "", "needsAction"},
		{"default pending", `[{"email":"me@example.com"}]`, "host@example.com", "", "needsAction"},
		{"maybe", `[{"email":"ME@example.com","status":"TENTATIVE"}]`, "host@example.com", "", "tentative"},
		{"declined", `[{"email":"mailto:me@example.com","status":"DECLINED"}]`, "host@example.com", "", "declined"},
		{"accepted", `[{"email":"me@example.com","status":"ACCEPTED"}]`, "host@example.com", "", "accepted"},
		{"own event", `[{"email":"guest@example.com","status":"NEEDS-ACTION"}]`, "me@example.com", "", "organizer"},
		{"other guest", `[{"email":"guest@example.com","status":"DECLINED"}]`, "host@example.com", "", ""},
		{"ambiguous self", `[{"email":"me@example.com","status":"ACCEPTED"},{"email":"me@example.com","status":"DECLINED"}]`, "host@example.com", "", ""},
		{"malformed", `{`, "host@example.com", "", ""},
		{"not an invitation", `[{"email":"me@example.com","status":"NEEDS-ACTION"}]`, "", "", ""},
		{"unknown response", `[{"email":"me@example.com","status":"DELEGATED"}]`, "host@example.com", "", ""},
		{"confirmed cached response", `[{"email":"me@example.com","status":"NEEDS-ACTION"}]`, "host@example.com", "accepted", "accepted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := storage.CalendarEvent{SourceProvider: "caldav", AccountEmail: "me@example.com", OrganizerEmail: test.organizer, AttendeesJSON: test.attendees, ResponseStatus: test.cached}
			if got := calendarViewEvent(event).ResponseStatus; got != test.want {
				t.Fatalf("response = %q, want %q", got, test.want)
			}
			if event.ResponseStatus != test.cached {
				t.Fatal("presentation must not alter response authorization data")
			}
		})
	}
}

func TestCalendarCachedResponseIncludesAccountContext(t *testing.T) {
	h := calendarResponseFixture(t, false)
	if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET provider='caldav' WHERE id='one-source'; UPDATE calendar_events SET response_status='', attendees_json='[{"email":"one@example.com","status":"TENTATIVE"}]' WHERE id='edit-event'`); err != nil {
		t.Fatal(err)
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil {
		t.Fatal(err)
	}
	if event.AccountEmail != "one@example.com" || event.SourceProvider != "caldav" || calendarViewEvent(event).ResponseStatus != "tentative" {
		t.Fatal("cached CalDAV event lost the mailbox context needed for response presentation")
	}
	events, err := h.db.ListCalendarEvents(t.Context(), "one", event.StartAt.Add(-time.Hour), event.EndAt.Add(time.Hour))
	if err != nil || len(events) == 0 {
		t.Fatalf("list cached events: count=%d, error=%v", len(events), err)
	}
	for _, cached := range events {
		if cached.ID == event.ID && calendarViewEvent(cached).ResponseStatus != "tentative" {
			t.Fatal("calendar list lost the cached response")
		}
	}
}
