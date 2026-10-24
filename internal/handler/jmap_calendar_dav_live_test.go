package handler

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

// TestJMAPCalendarLiveWrites creates, edits and deletes events and single
// occurrences on a real server through the CalDAV code, as the calendar
// does, and skips unless GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func TestJMAPCalendarLiveWrites(t *testing.T) {
	sessionURL, user, password := os.Getenv("GOFER_JMAP_TEST_URL"), os.Getenv("GOFER_JMAP_TEST_USER"), os.Getenv("GOFER_JMAP_TEST_PASSWORD")
	if sessionURL == "" || user == "" || password == "" {
		t.Skip("GOFER_JMAP_TEST_URL, GOFER_JMAP_TEST_USER and GOFER_JMAP_TEST_PASSWORD not set")
	}
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	ctx := context.Background()
	h, db := newAccountOwnershipTestHandler(t)
	address, _ := url.Parse(sessionURL)
	port, _ := strconv.Atoi(address.Port())
	if address.Scheme == "http" {
		if err := db.AddPlaintextTransportException(ctx, "http", address.Hostname(), port, "administrator"); err != nil {
			t.Fatal(err)
		}
		if err := db.AddPrivateTargetException(ctx, "http", address.Hostname(), port, "administrator"); err != nil {
			t.Fatal(err)
		}
	}
	account, err := h.accountStore.CreateAccount(ctx, "owner", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: user, DisplayName: "Live", JMAPSessionURL: sessionURL, Username: user, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.discoverJMAPCalendarSources(ctx, "owner", account.ID); err != nil {
		t.Fatal(err)
	}
	sources, err := db.ListCalendarSourcesForAccount(ctx, "owner", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var source storage.CalendarSource
	for _, candidate := range sources {
		if candidate.IsPrimary {
			source = candidate
		}
	}
	if !calendarSourceWritable(source) {
		t.Fatalf("default calendar is not writable: %+v", source)
	}
	cfg, err := h.accountStore.GetConfig(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := mail.OpenJMAPCalendars(ctx, cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	serverEvent := func(remoteID string) jmap.CalendarEvent {
		t.Helper()
		base, _, _ := strings.Cut(remoteID, "#")
		_, id, ok := jmapCalendarDAVParse(base)
		if !ok {
			t.Fatalf("event id %q", remoteID)
		}
		event, err := connection.Event(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	cleanup := func(remoteID string) {
		t.Cleanup(func() {
			if event := serverEvent(remoteID); event != nil {
				_, _ = connection.Delete(ctx, event.ID())
			}
		})
	}
	zone, _ := time.LoadLocation("Europe/Prague")
	day := time.Date(2026, 12, 7, 10, 0, 0, 0, zone)
	windowStart, windowEnd := day.AddDate(0, 0, -1), day.AddDate(0, 1, 0)
	stored := func(summary string) []storage.CalendarEvent {
		t.Helper()
		if _, err := h.syncCalendarScopedWindow(ctx, "owner", windowStart, windowEnd, "", account.ID); err != nil {
			t.Fatal(err)
		}
		events, err := db.ListCalendarEvents(ctx, "owner", windowStart, windowEnd)
		if err != nil {
			t.Fatal(err)
		}
		var out []storage.CalendarEvent
		for _, event := range events {
			if event.Summary == summary {
				out = append(out, event)
			}
		}
		return out
	}
	tag := time.Now().UTC().Format("150405")

	// A single event.
	start, end := day, day.Add(time.Hour)
	draft := calendar.EventDraft{RequestID: uuid.NewString(), Summary: "Gofer live " + tag, Description: "First notes", TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end}
	created, err := h.createCalendarProviderEvent(ctx, source, draft)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(created.RemoteID)
	if event := serverEvent(created.RemoteID); event == nil || event["title"] != draft.Summary {
		t.Fatalf("created %q = %+v", created.RemoteID, event)
	}
	// A retried create finds the event instead of making another, once the
	// server's search has indexed it.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if found, _ := connection.ByUID(ctx, jmapLiveCalendarID(t, source), draft.RequestID+"@gofer"); found != nil {
			break
		}
	}
	again, err := h.createCalendarProviderEvent(ctx, source, draft)
	if err != nil || again.RemoteID != created.RemoteID {
		t.Fatalf("retried create = %q, %v; want %q", again.RemoteID, err, created.RemoteID)
	}
	// Something Gofer doesn't keep, set on the server, survives an edit.
	_, id, _ := jmapCalendarDAVParse(created.RemoteID)
	if _, err := connection.Update(ctx, id, jmap.PatchObject{"keywords": map[string]any{"gofer-live": true}}); err != nil {
		t.Fatal(err)
	}
	events := stored(draft.Summary)
	if len(events) != 1 {
		t.Fatalf("stored = %+v", events)
	}
	edit := draft
	edit.Summary = draft.Summary + " edited"
	editStart, editEnd := start.Add(30*time.Minute), end.Add(30*time.Minute)
	edit.StartAt, edit.EndAt = &editStart, &editEnd
	updated, err := h.updateCalendarProviderEvent(ctx, source, events[0], edit, false)
	if err != nil {
		t.Fatal(err)
	}
	event := serverEvent(updated.RemoteID)
	// Gofer writes its own events' times in UTC: 10:30 in Prague.
	if event["title"] != edit.Summary || event["start"] != "2026-12-07T09:30:00" || event["timeZone"] != "Etc/UTC" || event["keywords"] == nil {
		t.Fatalf("after editing = %+v", event)
	}
	// An edit made from the old version is refused.
	stale := events[0]
	if _, err := h.updateCalendarProviderEvent(ctx, source, stale, draft, false); err == nil {
		t.Fatal("an edit of an outdated version was accepted")
	}
	events = stored(edit.Summary)
	if len(events) != 1 {
		t.Fatalf("stored after editing = %+v", events)
	}
	if _, err := h.deleteCalendarProviderEvent(ctx, source, events[0], false); err != nil {
		t.Fatal(err)
	}
	if serverEvent(updated.RemoteID) != nil {
		t.Fatal("the deleted event is still on the server")
	}

	// A weekly series: one occurrence moved, one deleted, then the series.
	series := calendar.EventDraft{RequestID: uuid.NewString(), Summary: "Gofer live series " + tag, TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end,
		Recurrence: &calendar.RecurrenceDraft{Frequency: "weekly", Interval: 1, Count: 3}}
	createdSeries, err := h.createCalendarProviderEvent(ctx, source, series)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(createdSeries.RemoteID)
	occurrences := stored(series.Summary)
	if len(occurrences) != 3 {
		t.Fatalf("series occurrences = %d", len(occurrences))
	}
	second := occurrences[1]
	moveStart, moveEnd := second.StartAt.Add(2*time.Hour), second.EndAt.Add(2*time.Hour)
	move := calendar.EventDraft{Summary: series.Summary + " (moved)", TimeZone: "Europe/Prague", StartAt: &moveStart, EndAt: &moveEnd}
	if _, err := h.updateCalendarProviderEvent(ctx, source, second, move, false, true); err != nil {
		t.Fatal(err)
	}
	remaining := stored(series.Summary)
	if len(remaining) != 2 || len(stored(move.Summary)) != 1 {
		t.Fatalf("after moving one occurrence: %d left, %d moved", len(remaining), len(stored(move.Summary)))
	}
	if _, err := h.deleteCalendarProviderEvent(ctx, source, remaining[1], false, true); err != nil {
		t.Fatal(err)
	}
	if left := stored(series.Summary); len(left) != 1 {
		t.Fatalf("after deleting one occurrence: %d left", len(left))
	}
	overrides, _ := serverEvent(createdSeries.RemoteID)["recurrenceOverrides"].(map[string]any)
	if len(overrides) != 2 {
		t.Fatalf("server overrides = %+v", overrides)
	}
	first := stored(series.Summary)[0]
	master, err := h.readCalendarProviderSeries(ctx, source, first, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.deleteCalendarProviderEvent(ctx, source, master, true); err != nil {
		t.Fatal(err)
	}
	if serverEvent(createdSeries.RemoteID) != nil {
		t.Fatal("the deleted series is still on the server")
	}

	// A whole series edited: its title and time.
	plain := calendar.EventDraft{RequestID: uuid.NewString(), Summary: "Gofer live plain series " + tag, TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end,
		Recurrence: &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 2}}
	createdPlain, err := h.createCalendarProviderEvent(ctx, source, plain)
	if err != nil {
		t.Fatal(err)
	}
	cleanup(createdPlain.RemoteID)
	master, err = h.readCalendarProviderSeries(ctx, source, stored(plain.Summary)[0])
	if err != nil {
		t.Fatal(err)
	}
	whole := plain
	whole.Summary = plain.Summary + " renamed"
	laterStart, laterEnd := start.Add(time.Hour), end.Add(time.Hour)
	whole.StartAt, whole.EndAt = &laterStart, &laterEnd
	if _, err := h.updateCalendarProviderEvent(ctx, source, master, whole, true); err != nil {
		t.Fatal(err)
	}
	renamed := stored(whole.Summary)
	if len(renamed) != 2 || !renamed[0].StartAt.Equal(laterStart) {
		t.Fatalf("renamed series = %+v", renamed)
	}
}

func jmapLiveCalendarID(t *testing.T, source storage.CalendarSource) string {
	t.Helper()
	id, _, ok := jmapCalendarDAVParse(source.RemoteID)
	if !ok {
		t.Fatalf("calendar id %q", source.RemoteID)
	}
	return id
}
