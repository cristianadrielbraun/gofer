package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func invitationTestMail(from, method, events string) []byte {
	return []byte("From: Host <" + from + ">\r\nTo: guest@example.com\r\nMIME-Version: 1.0\r\nContent-Type: text/calendar; method=" + method + "; charset=utf-8\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nMETHOD:" + method + "\r\n" + events + "END:VCALENDAR\r\n")
}

func invitationTestEvent(sequence, extra string) string {
	return "BEGIN:VEVENT\r\nUID:invite-uid\r\nSEQUENCE:" + sequence + "\r\nDTSTAMP:20261001T000000Z\r\nDTSTART:20261002T070000Z\r\nDTEND:20261002T080000Z\r\nSUMMARY:Planning\r\n" +
		"ORGANIZER;CN=Host:mailto:host@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:guest@example.com\r\n" + extra + "END:VEVENT\r\n"
}

func TestMailInvitationIsFromItsOrganizerToTheAccount(t *testing.T) {
	if _, err := readMailInvitation(invitationTestMail("host@example.com", "REQUEST", invitationTestEvent("0", "")), "invite-uid", "guest@example.com", false); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"another sender":  invitationTestMail("someone@example.com", "REQUEST", invitationTestEvent("0", "")),
		"not invited":     invitationTestMail("host@example.com", "REQUEST", strings.Replace(invitationTestEvent("0", ""), "guest@example.com", "other@example.com", 1)),
		"own meeting":     invitationTestMail("guest@example.com", "REQUEST", strings.Replace(invitationTestEvent("0", ""), "ORGANIZER;CN=Host:mailto:host@example.com", "ORGANIZER:mailto:guest@example.com", 1)),
		"a reply":         invitationTestMail("host@example.com", "REPLY", invitationTestEvent("0", "")),
		"another event":   invitationTestMail("host@example.com", "REQUEST", strings.Replace(invitationTestEvent("0", ""), "invite-uid", "other-uid", 1)),
		"date with rule":  invitationTestMail("host@example.com", "REQUEST", invitationTestEvent("0", "RECURRENCE-ID:20261002T070000Z\r\nRRULE:FREQ=DAILY\r\n")),
		"extra dates":     invitationTestMail("host@example.com", "REQUEST", invitationTestEvent("0", "RDATE:20261005T070000Z\r\n")),
		"invalid version": invitationTestMail("host@example.com", "REQUEST", invitationTestEvent("x", "")),
	} {
		if _, err := readMailInvitation(raw, "invite-uid", "guest@example.com", false); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestCalDAVInvitationAddUpdateAndCancel(t *testing.T) {
	var mu sync.Mutex
	resources := map[string]string{}
	versions := map[string]int{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := r.URL.Path
		etag := func() string { return `"v` + strings.Repeat("1", versions[path]+1) + `"` }
		switch r.Method {
		case "PUT":
			_, exists := resources[path]
			if (r.Header.Get("If-None-Match") == "*" && exists) || (r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != etag()) {
				w.WriteHeader(412)
				return
			}
			data, _ := io.ReadAll(r.Body)
			resources[path] = string(data)
			if exists {
				versions[path]++
			}
			w.WriteHeader(201)
		case "GET":
			body, ok := resources[path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("ETag", etag())
			io.WriteString(w, body)
		case "DELETE":
			if r.Header.Get("If-Match") != etag() {
				w.WriteHeader(412)
				return
			}
			delete(resources, path)
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	prior := davHTTPTransport
	davHTTPTransport = server.Client().Transport
	defer func() { davHTTPTransport = prior }()
	source := storage.CalendarSource{ID: "source", AccountID: "account", Provider: "caldav", RemoteID: server.URL + "/cal/", AccessRole: "owner"}
	credentials := calendarCredentials{username: "user", password: "pass", baseURL: source.RemoteID}
	invitation := func(method, events string) mailInvitation {
		t.Helper()
		result, err := readMailInvitation(invitationTestMail("host@example.com", method, events), "invite-uid", "guest@example.com", false)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cached := func(endpoint string) storage.CalendarEvent {
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(endpoint, server.URL)
		return storage.CalendarEvent{ID: "cached", SourceID: "source", SourceProvider: "caldav", RemoteID: endpoint, ETag: `"v` + strings.Repeat("1", versions[path]+1) + `"`}
	}
	body := func(endpoint string) string {
		mu.Lock()
		defer mu.Unlock()
		return resources[strings.TrimPrefix(endpoint, server.URL)]
	}

	// Adding: the invitation becomes a calendar event, without its METHOD.
	weekly := invitationTestEvent("0", "RRULE:FREQ=WEEKLY;COUNT=4\r\n")
	endpoint, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation("REQUEST", weekly), storage.CalendarEvent{})
	if err != nil || !strings.Contains(body(endpoint), "RRULE:FREQ=WEEKLY") || strings.Contains(body(endpoint), "METHOD") {
		t.Fatalf("add: %v %s", err, body(endpoint))
	}
	// A newer version replaces it; an older one is refused.
	moved := strings.Replace(strings.Replace(weekly, "SEQUENCE:0", "SEQUENCE:1", 1), "SUMMARY:Planning", "SUMMARY:Moved planning", 1)
	if _, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation("REQUEST", moved), cached(endpoint)); err != nil || !strings.Contains(body(endpoint), "Moved planning") {
		t.Fatalf("update: %v %s", err, body(endpoint))
	}
	var refusal calendarInvitationRefusal
	if _, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation("REQUEST", weekly), cached(endpoint)); !errors.As(err, &refusal) {
		t.Fatalf("older version accepted: %v", err)
	}
	// Cancelling one date removes just that date.
	date := "BEGIN:VEVENT\r\nUID:invite-uid\r\nSEQUENCE:2\r\nDTSTAMP:20261001T000000Z\r\nRECURRENCE-ID:20261009T070000Z\r\nDTSTART:20261009T070000Z\r\nDTEND:20261009T080000Z\r\nSUMMARY:Moved planning\r\nORGANIZER:mailto:host@example.com\r\nATTENDEE:mailto:guest@example.com\r\nSTATUS:CANCELLED\r\nEND:VEVENT\r\n"
	if _, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation("CANCEL", date), cached(endpoint)); err != nil || !strings.Contains(body(endpoint), "EXDATE:20261009T070000Z") {
		t.Fatalf("date cancel: %v %s", err, body(endpoint))
	}
	// Someone else's event with the same UID is never changed.
	other := cached(endpoint)
	mu.Lock()
	resources[strings.TrimPrefix(endpoint, server.URL)] = strings.ReplaceAll(resources[strings.TrimPrefix(endpoint, server.URL)], "mailto:host@example.com", "mailto:intruder@example.com")
	mu.Unlock()
	if _, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation("CANCEL", invitationTestEvent("3", "STATUS:CANCELLED\r\n")), other); !errors.As(err, &refusal) {
		t.Fatalf("another organizer's event changed: %v", err)
	}
	mu.Lock()
	resources[strings.TrimPrefix(endpoint, server.URL)] = strings.ReplaceAll(resources[strings.TrimPrefix(endpoint, server.URL)], "mailto:intruder@example.com", "mailto:host@example.com")
	mu.Unlock()
	// Cancelling the whole event removes it.
	if _, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation("CANCEL", invitationTestEvent("3", "STATUS:CANCELLED\r\n")), cached(endpoint)); err != nil || body(endpoint) != "" {
		t.Fatalf("cancel: %v %s", err, body(endpoint))
	}
	// A cancellation can't add an event.
	if _, err := applyCalDAVInvitation(t.Context(), source, credentials, invitation("CANCEL", invitationTestEvent("4", "")), storage.CalendarEvent{}); !errors.As(err, &refusal) {
		t.Fatalf("cancellation added an event: %v", err)
	}
}

func TestMailCalendarInvitationOffer(t *testing.T) {
	sources := []storage.CalendarSource{{ID: "caldav", AccountID: "account", Provider: "caldav", Name: "Personal", AccessRole: "owner"}}
	start, end := time.Date(2026, 10, 2, 7, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	saved, newer := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	item := mailCalendarEvent{UID: "invite-uid", Method: "REQUEST", Summary: "Planning", Organizer: "host@example.com", Invited: true, Start: &start, End: &end, Modified: &newer}
	cached := storage.CalendarEvent{ID: "event", SourceProvider: "caldav", Summary: "Planning", StartAt: &start, EndAt: &end, OrganizerEmail: "host@example.com", AccountEmail: "guest@example.com", ProviderUpdatedAt: &saved}
	label := func(item mailCalendarEvent, candidates []storage.CalendarEvent, sources []storage.CalendarSource) string {
		if offer := mailCalendarInvitationOffer(context.Background(), item, candidates, sources, "account", "guest@example.com"); offer != nil {
			return offer.Label
		}
		return ""
	}
	if got := label(item, nil, sources); got != "Add to calendar" {
		t.Errorf("new invitation: %q", got)
	}
	if got := label(item, []storage.CalendarEvent{cached}, sources); got != "" {
		t.Errorf("invitation already in the calendar: %q", got)
	}
	changed := item
	changed.Summary = "Moved planning"
	if got := label(changed, []storage.CalendarEvent{cached}, sources); got != "Update in calendar" {
		t.Errorf("changed invitation: %q", got)
	}
	// An older email, or one whose age is unknown, never offers to replace the calendar.
	older := changed
	older.Modified = &saved
	if got := label(older, []storage.CalendarEvent{cached}, sources); got != "" {
		t.Errorf("older invitation: %q", got)
	}
	undated := changed
	undated.Modified = nil
	if got := label(undated, []storage.CalendarEvent{cached}, sources); got != "" {
		t.Errorf("undated invitation: %q", got)
	}
	cancel := item
	cancel.Method, cancel.Cancelled = "CANCEL", true
	if got := label(cancel, []storage.CalendarEvent{cached}, sources); got != "Remove from calendar" {
		t.Errorf("cancellation: %q", got)
	}
	if got := label(cancel, nil, sources); got != "" {
		t.Errorf("cancellation of an event not in the calendar: %q", got)
	}
	google := cached
	google.SourceProvider = "gmail"
	if got := label(changed, []storage.CalendarEvent{google}, sources); got != "" {
		t.Errorf("an event in another kind of calendar: %q", got)
	}
	if got := label(item, nil, []storage.CalendarSource{{ID: "gmail", AccountID: "account", Provider: "gmail", AccessRole: "owner"}}); got != "" {
		t.Errorf("an account without CalDAV calendars: %q", got)
	}
	own := item
	own.Organizer = "guest@example.com"
	if got := label(own, nil, sources); got != "" {
		t.Errorf("the account's own meeting: %q", got)
	}
}

// A card offering to update the calendar shows the invitation's new details.
func TestMailCalendarInvitationCardShowsTheNewDetails(t *testing.T) {
	sources := []storage.CalendarSource{{ID: "caldav", AccountID: "account", Provider: "caldav", Name: "Personal", AccessRole: "owner"}}
	old, oldEnd := time.Date(2026, 12, 1, 14, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)
	moved, movedEnd := old.Add(time.Hour), oldEnd.Add(time.Hour)
	saved, newer := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	cached := storage.CalendarEvent{ID: "event", SourceProvider: "caldav", Summary: "Planning", StartAt: &old, EndAt: &oldEnd, OrganizerEmail: "host@example.com", AccountEmail: "guest@example.com", ProviderUpdatedAt: &saved}
	item := mailCalendarEvent{UID: "invite-uid", Method: "REQUEST", Summary: "Planning", Organizer: "host@example.com", Invited: true, Start: &moved, End: &movedEnd, Modified: &newer}
	card := mailCalendarEventCard(context.Background(), item, cached, true, time.UTC, "1", func(storage.CalendarEvent) bool { return true })
	before := card.Time
	mailCalendarInvitationCard(context.Background(), &card, item, []storage.CalendarEvent{cached}, sources, "account", "guest@example.com", time.UTC)
	if card.Invitation == nil || card.Invitation.Label != "Update in calendar" || card.Time == before || card.Time != mailCalendarCard(context.Background(), item, time.UTC).Time {
		t.Fatalf("card shows %q (was %q), offer %+v", card.Time, before, card.Invitation)
	}
}
