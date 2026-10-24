package handler

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

// memCalendarStore keeps JMAP events in memory. Its iCalendar reading knows
// only what Gofer writes: title, notes, times, a COUNT rule, cancelled and
// moved occurrences, and X- properties, which it keeps as a server would.
type memCalendarStore struct {
	mu      sync.Mutex
	events  map[string]jmap.CalendarEvent
	patches []jmap.PatchObject
	next    int
	// hideUIDs leaves events out of uid searches, as a server that hasn't
	// indexed them yet.
	hideUIDs bool
}

func (s *memCalendarStore) Event(_ context.Context, id string) (jmap.CalendarEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if event, ok := s.events[id]; ok {
		return maps.Clone(event), nil
	}
	return nil, nil
}

func (s *memCalendarStore) ByUID(_ context.Context, calendarID, uid string) (jmap.CalendarEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if !s.hideUIDs && event["uid"] == uid && jmapCalendarEventIn(event, calendarID) {
			return maps.Clone(event), nil
		}
	}
	return nil, nil
}

func (s *memCalendarStore) Create(_ context.Context, calendarID string, event jmap.CalendarEvent) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.events {
		if existing["uid"] == event["uid"] {
			return "", &jmap.SetError{Type: "invalidProperties", Description: "an event with this UID exists"}
		}
	}
	s.next++
	id := fmt.Sprintf("ev%d", s.next)
	event = maps.Clone(event)
	event["id"], event["calendarIds"] = id, map[string]any{calendarID: true}
	s.events[id] = event
	return id, nil
}

func (s *memCalendarStore) Update(_ context.Context, id string, patch jmap.PatchObject) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	event, ok := s.events[id]
	if !ok {
		return false, nil
	}
	s.patches = append(s.patches, patch)
	for key, value := range patch {
		if value == nil {
			delete(event, key)
		} else {
			event[key] = value
		}
	}
	return true, nil
}

func (s *memCalendarStore) Delete(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.events[id]
	delete(s.events, id)
	return ok, nil
}

func (s *memCalendarStore) FromICS(_ context.Context, data []byte) ([]jmap.CalendarEvent, error) {
	cal, err := calendarUpdateDecodeICS(data)
	if err != nil {
		return nil, err
	}
	local := func(prop *ical.Prop) (string, string, error) {
		if zone := prop.Params.Get("TZID"); zone != "" {
			at, err := time.Parse("20060102T150405", prop.Value)
			return at.Format(jscalendarLocalLayout), zone, err
		}
		at, err := time.Parse("20060102T150405Z", prop.Value)
		return at.Format(jscalendarLocalLayout), "Etc/UTC", err
	}
	var master jmap.CalendarEvent
	// Overrides are named in the series' zone, known once it is read.
	overrides := map[time.Time]any{}
	for _, event := range cal.Events() {
		props := event.Props
		start, zone, err := local(props.Get("DTSTART"))
		if err != nil {
			return nil, err
		}
		title, _ := props.Text("SUMMARY")
		object := map[string]any{"title": title, "start": start}
		begin, _ := event.DateTimeStart(time.UTC)
		end, _ := event.DateTimeEnd(time.UTC)
		object["duration"] = fmt.Sprintf("PT%dM", int(end.Sub(begin).Minutes()))
		if rid := props.Get("RECURRENCE-ID"); rid != nil {
			at, err := rid.DateTime(time.UTC)
			if err != nil {
				return nil, err
			}
			object["timeZone"] = zone
			overrides[at.UTC()] = object
			continue
		}
		uid, _ := props.Text("UID")
		master = jmap.CalendarEvent{"@type": "Event", "uid": uid, "timeZone": zone}
		maps.Copy(master, object)
		if description, _ := props.Text("DESCRIPTION"); description != "" {
			master["description"] = description
		}
		var kept []any
		for _, name := range slices.Sorted(maps.Keys(props)) {
			if strings.HasPrefix(name, "X-") {
				kept = append(kept, []any{strings.ToLower(name), map[string]any{}, "unknown", props.Get(name).Value})
			}
		}
		if len(kept) > 0 {
			master["iCalendar"] = map[string]any{"name": "vevent", "properties": kept}
		}
		if rule := props.Get("RRULE"); rule != nil {
			parts := map[string]string{}
			for _, part := range strings.Split(rule.Value, ";") {
				key, value, _ := strings.Cut(part, "=")
				parts[key] = value
			}
			recurrence := map[string]any{"frequency": strings.ToLower(parts["FREQ"])}
			if count := parts["COUNT"]; count != "" {
				var n float64
				_, _ = fmt.Sscan(count, &n)
				recurrence["count"] = n
			}
			master["recurrenceRule"] = recurrence
		}
		for _, exdate := range props.Values("EXDATE") {
			at, err := exdate.DateTime(time.UTC)
			if err != nil {
				return nil, err
			}
			overrides[at.UTC()] = map[string]any{"excluded": true}
		}
	}
	if master == nil {
		return nil, errors.New("no event")
	}
	if len(overrides) > 0 {
		zone := time.UTC
		if name, _ := master["timeZone"].(string); name != "Etc/UTC" {
			zone, _ = time.LoadLocation(name)
		}
		named := map[string]any{}
		for at, override := range overrides {
			named[at.In(zone).Format(jscalendarLocalLayout)] = override
		}
		master["recurrenceOverrides"] = named
	}
	return []jmap.CalendarEvent{master}, nil
}

const jscalendarLocalLayout = "2006-01-02T15:04:05"

func newMemCalendarDAV(store *memCalendarStore) (context.Context, storage.CalendarSource) {
	dav := &jmapCalendarDAV{calendarID: "c1", connection: store, created: map[string]string{}, written: map[string]jmapCalendarDAVWrite{}}
	return context.WithValue(context.Background(), jmapCalendarDAVKey{}, dav),
		storage.CalendarSource{ID: "source", Provider: "jmap", RemoteID: jmapCalendarDAVCollection("c1"), TimeZone: "Europe/Prague", AccessRole: "writer"}
}

// storedJMAPEvents reads the store's events in December 2026 as sync does.
func storedJMAPEvents(t *testing.T, store *memCalendarStore, source storage.CalendarSource) []storage.CalendarEvent {
	t.Helper()
	store.mu.Lock()
	var events []jmap.CalendarEvent
	for _, id := range slices.Sorted(maps.Keys(store.events)) {
		events = append(events, maps.Clone(store.events[id]))
	}
	store.mu.Unlock()
	page, err := jmapCalendarEventPage(events, source.RemoteID, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var out []storage.CalendarEvent
	for _, remote := range page.Events {
		out = append(out, calendarStorageEvent("owner", source.ID, remote))
	}
	slices.SortFunc(out, func(a, b storage.CalendarEvent) int { return a.StartAt.Compare(*b.StartAt) })
	return out
}

func TestJMAPCalendarDAVCreatesEditsAndDeletes(t *testing.T) {
	store := &memCalendarStore{events: map[string]jmap.CalendarEvent{}}
	ctx, source := newMemCalendarDAV(store)
	start := time.Date(2026, 12, 7, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	draft := calendar.EventDraft{RequestID: "req-1", Summary: "Planning", Description: "Notes", TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end}
	created, err := createCalDAVCalendarEvent(ctx, source, "jmap", "", draft)
	if err != nil {
		t.Fatal(err)
	}
	created = jmapCalendarDAVCanonical(ctx, created)
	if created.RemoteID != jmapCalendarDAVResource(source.RemoteID, "ev1") {
		t.Fatalf("created id = %q", created.RemoteID)
	}
	if kept, _ := store.events["ev1"]["iCalendar"].(map[string]any); !strings.Contains(fmt.Sprint(kept), "x-gofer-create-hash") {
		t.Fatalf("create hash not kept: %+v", store.events["ev1"])
	}

	// Tried again with a fresh connection, the create finds its event.
	again, err := createCalDAVCalendarEvent(func() context.Context { c, _ := newMemCalendarDAV(store); return c }(), source, "jmap", "", draft)
	if err != nil || len(store.events) != 1 {
		t.Fatalf("retried create = %+v, %v; %d events", again, err, len(store.events))
	}
	// Even when the server's search doesn't find it yet.
	store.hideUIDs = true
	retryCtx, _ := newMemCalendarDAV(store)
	again, err = createCalDAVCalendarEvent(retryCtx, source, "jmap", "", draft)
	if err == nil || len(store.events) != 1 {
		t.Fatalf("retried create before indexing = %+v, %v; %d events", again, err, len(store.events))
	}
	store.hideUIDs = false

	store.events["ev1"]["keywords"] = map[string]any{"server-only": true}
	events := storedJMAPEvents(t, store, source)
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	edit := draft
	edit.Summary = "Planning (moved)"
	later, laterEnd := start.Add(time.Hour), end.Add(time.Hour)
	edit.StartAt, edit.EndAt = &later, &laterEnd
	editCtx, _ := newMemCalendarDAV(store)
	updated, err := updateCalDAVCalendarEventScope(editCtx, source, "jmap", "", events[0], edit, false)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Summary != edit.Summary || !updated.StartAt.Equal(later) {
		t.Fatalf("updated = %+v", updated)
	}
	patch := store.patches[len(store.patches)-1]
	if slices.Sorted(maps.Keys(patch)) == nil || patch["title"] != edit.Summary || patch["start"] != "2026-12-07T10:00:00" || patch["description"] != nil || patch["keywords"] != nil {
		t.Fatalf("patch = %+v", patch)
	}
	if store.events["ev1"]["keywords"] == nil {
		t.Fatal("editing dropped what Gofer doesn't keep")
	}

	// The cached version is now old: editing or deleting from it is refused.
	staleCtx, _ := newMemCalendarDAV(store)
	if _, err := updateCalDAVCalendarEventScope(staleCtx, source, "jmap", "", events[0], draft, false); !errors.Is(err, errCalendarUpdateConflict) {
		t.Fatalf("stale edit = %v", err)
	}
	deleteCtx, _ := newMemCalendarDAV(store)
	if err := deleteCalDAVCalendarEventScope(deleteCtx, source, "jmap", "", events[0], false); err == nil {
		t.Fatal("stale delete succeeded")
	}
	current := storedJMAPEvents(t, store, source)
	deleteCtx, _ = newMemCalendarDAV(store)
	if err := deleteCalDAVCalendarEventScope(deleteCtx, source, "jmap", "", current[0], false); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 0 {
		t.Fatal("event not deleted")
	}
}

func TestJMAPCalendarDAVChangesOneOccurrence(t *testing.T) {
	store := &memCalendarStore{events: map[string]jmap.CalendarEvent{}}
	ctx, source := newMemCalendarDAV(store)
	start := time.Date(2026, 12, 7, 9, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	draft := calendar.EventDraft{RequestID: "req-2", Summary: "Standup", TimeZone: "Europe/Prague", StartAt: &start, EndAt: &end,
		Recurrence: &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1, Count: 3}}
	if _, err := createCalDAVCalendarEvent(ctx, source, "jmap", "", draft); err != nil {
		t.Fatal(err)
	}
	events := storedJMAPEvents(t, store, source)
	if len(events) != 3 {
		t.Fatalf("occurrences = %d", len(events))
	}
	moveStart, moveEnd := events[1].StartAt.Add(2*time.Hour), events[1].EndAt.Add(2*time.Hour)
	move := calendar.EventDraft{Summary: "Standup (moved)", TimeZone: "Europe/Prague", StartAt: &moveStart, EndAt: &moveEnd}
	moveCtx, _ := newMemCalendarDAV(store)
	if _, err := updateCalDAVCalendarOccurrence(moveCtx, source, "jmap", "", events[1], &move); err != nil {
		t.Fatal(err)
	}
	events = storedJMAPEvents(t, store, source)
	if len(events) != 3 || events[1].Summary != "Standup (moved)" || !events[1].StartAt.Equal(moveStart) {
		t.Fatalf("after moving = %+v", events)
	}
	deleteCtx, _ := newMemCalendarDAV(store)
	if _, err := updateCalDAVCalendarOccurrence(deleteCtx, source, "jmap", "", events[2], nil); err != nil {
		t.Fatal(err)
	}
	if events = storedJMAPEvents(t, store, source); len(events) != 2 {
		t.Fatalf("after deleting one = %+v", events)
	}
}

func TestJMAPCalendarDAVNeverLeavesGofer(t *testing.T) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, jmapCalendarDAVResource(jmapCalendarDAVCollection("c1"), "ev1"), nil)
	if _, err := calendarProviderDo(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("a JMAP calendar request went to the network")
		return nil, nil
	})}, req); err == nil {
		t.Fatal("a JMAP calendar request without its connection succeeded")
	}
	if id, name, ok := jmapCalendarDAVParse("https://jmap.invalid/c%2F1/ev%201.ics"); !ok || id != "c/1" || name != "ev 1" {
		t.Errorf("escaped ids = %q %q %v", id, name, ok)
	}
	for _, bad := range []string{"https://other.example/c1/ev1.ics", "https://jmap.invalid/c1/ev1", "https://jmap.invalid/c1/x/ev1.ics", "http://jmap.invalid/c1/"} {
		if _, _, ok := jmapCalendarDAVParse(bad); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}
