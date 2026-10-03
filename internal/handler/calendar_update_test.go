package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarUpdateFixture(t *testing.T) *Handler {
	t.Helper()
	h := calendarCreateFixture(t)
	start := time.Date(2026, 10, 2, 7, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	if err := h.db.ReplaceCalendarEvents(t.Context(), "one", "one-source", []storage.CalendarEvent{
		{ID: "edit-event", RemoteID: "remote-event", ETag: `"v1"`, ICalUID: "keep-uid", Summary: "Original", Description: "Old notes", StartAt: &start, EndAt: &end, StartTimeZone: "Europe/Prague", EndTimeZone: "Europe/Prague"},
		{ID: "untouched", RemoteID: "untouched", Summary: "Keep me", StartAt: &start, EndAt: &end},
	}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		remote := calendarCreatedRemote(draft)
		remote.RemoteID, remote.ETag, remote.ICalUID = existing.RemoteID, `"v2"`, existing.ICalUID
		return remote, nil
	}
	return h
}

func calendarUpdateRequest(values url.Values, user string) *http.Request {
	values = cloneCalendarUpdateForm(values)
	if _, exists := values["version"]; !exists {
		values.Set("version", `"v1"`)
	}
	r := httptest.NewRequest(http.MethodPatch, "/api/calendar/events/edit-event", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", "edit-event")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
}

func cloneCalendarUpdateForm(values url.Values) url.Values {
	copy := make(url.Values, len(values))
	for key, items := range values {
		copy[key] = append([]string(nil), items...)
	}
	return copy
}

func TestCalendarUpdateSavesInPlaceAndPublishesWithoutReconciling(t *testing.T) {
	h := calendarUpdateFixture(t)
	before, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	bus := h.syncer.Events()
	events := bus.Subscribe()
	defer bus.Unsubscribe(events)
	calls := 0
	update := h.calendarUpdateEvent
	h.calendarUpdateEvent = func(ctx context.Context, source storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		calls++
		if source.UserID != "one" || existing.ID != "edit-event" || existing.ETag != `"v1"` {
			t.Fatal("incorrect update target or version")
		}
		return update(ctx, source, existing, draft)
	}
	w := httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(calendarCreateForm(), "one"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"saved":true`) || !strings.Contains(w.Body.String(), `"event_id":"edit-event"`) {
		t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
	}
	updated, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || updated.Summary != "Planning" || updated.Description != "Full notes" || updated.ETag != `"v2"` || updated.ICalUID != "keep-uid" {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "untouched"); err != nil {
		t.Fatal("update removed an unrelated event")
	}
	after, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	if !after[0].LastSuccessAt.Equal(*before[0].LastSuccessAt) || after[0].SyncAttempt != before[0].SyncAttempt {
		t.Fatal("edit fabricated a successful full sync")
	}
	select {
	case event := <-events:
		if event.Type != mail.EventCalendarChanged || event.UserID != "one" {
			t.Fatalf("incorrect notification %#v", event)
		}
	default:
		t.Fatal("missing change notification for HTMX cache refresh")
	}
	w = httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(calendarCreateForm(), "one"))
	if w.Code != http.StatusConflict || calls != 1 {
		t.Fatal("stale resubmission reached another provider write")
	}
}

func TestCalendarUpdateAuthorizationAndSupportedEventGates(t *testing.T) {
	for _, tc := range []struct {
		name, user, sql string
		form            func(url.Values)
		status          int
	}{
		{name: "foreign owner", user: "two", status: 404},
		{name: "calendar move", form: func(v url.Values) { v.Set("source_id", "two-source") }, status: 400},
		{name: "missing version", form: func(v url.Values) { v.Set("version", "") }, status: 400},
		{name: "duplicate version", form: func(v url.Values) { v["version"] = []string{`"v1"`, `"v2"`} }, status: 400},
		{name: "stale version", form: func(v url.Values) { v.Set("version", `"old"`) }, status: 409},
		{name: "read only", sql: `UPDATE calendar_sources SET access_role='reader' WHERE id='one-source'`, status: 403},
		{name: "deselected", sql: `UPDATE calendar_sources SET is_selected=0 WHERE id='one-source'`, status: 404},
		{name: "deleted account", sql: `UPDATE accounts SET is_deleting=1 WHERE id='one-account'`, status: 404},
		{name: "guests", sql: `UPDATE calendar_events SET attendees_json='[{"email":"guest@example.com"}]' WHERE id='edit-event'`, status: 403},
		{name: "recurring without series scope", sql: `UPDATE calendar_events SET recurrence_json='["RRULE:FREQ=DAILY"]' WHERE id='edit-event'`, status: 400},
		{name: "instance without series scope", sql: `UPDATE calendar_events SET series_remote_id='series' WHERE id='edit-event'`, status: 400},
		{name: "online meeting", sql: `UPDATE calendar_events SET online_meeting_json='{"url":"https://meeting.example"}' WHERE id='edit-event'`, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := calendarUpdateFixture(t)
			if tc.sql != "" {
				if _, err := h.db.Write().Exec(tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			h.calendarUpdateEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent, calendar.EventDraft) (calendar.RemoteEvent, error) {
				t.Fatal("ineligible edit reached the provider")
				return calendar.RemoteEvent{}, nil
			}
			values := calendarCreateForm()
			if tc.form != nil {
				tc.form(values)
			}
			user := tc.user
			if user == "" {
				user = "one"
			}
			w := httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, calendarUpdateRequest(values, user))
			if w.Code != tc.status {
				t.Fatalf("status=%d expected=%d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	h := calendarUpdateFixture(t)
	h.calendarUpdateEvent = nil
	w := httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(calendarCreateForm(), "one"))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Reconnect") {
		t.Fatal("missing OAuth write grant allowed an edit")
	}
}

func TestCalendarUpdateFailureDoesNotChangeCachedEvent(t *testing.T) {
	for _, tc := range []struct {
		name                string
		err                 error
		status              int
		uncertain, conflict bool
	}{
		{"remote conflict", errCalendarUpdateConflict, 409, false, true},
		{"remote guests added", errCalendarUpdateUnsupported, 403, false, true},
		{"deleted remotely", calendarCreateProviderError{404}, 409, false, true},
		{"write denied", calendarCreateProviderError{403}, 502, false, false},
		{"connection lost", context.DeadlineExceeded, 502, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := calendarUpdateFixture(t)
			h.calendarUpdateEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent, calendar.EventDraft) (calendar.RemoteEvent, error) {
				return calendar.RemoteEvent{}, tc.err
			}
			w := httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, calendarUpdateRequest(calendarCreateForm(), "one"))
			var response struct{ Uncertain, Conflict bool }
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || response.Uncertain != tc.uncertain || response.Conflict != tc.conflict {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
			if err != nil || event.Summary != "Original" || event.ETag != `"v1"` {
				t.Fatal("failed update changed the cache")
			}
		})
	}
}

func TestCalendarEditPrefillsExistingEventAndInclusiveAllDayEnd(t *testing.T) {
	h := calendarUpdateFixture(t)
	for _, allDay := range []bool{false, true} {
		if allDay {
			if _, err := h.db.Write().Exec(`UPDATE calendar_events SET all_day=1,start_date='2026-10-02',end_date='2026-10-05',start_at=NULL,end_at=NULL WHERE id='edit-event'`); err != nil {
				t.Fatal(err)
			}
		}
		r := httptest.NewRequest(http.MethodGet, "/api/calendar/events/edit-event/edit", nil)
		r.SetPathValue("id", "edit-event")
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
		w := httptest.NewRecorder()
		h.handleEditCalendarEvent(w, r)
		if w.Code != 200 {
			t.Fatalf("editor=%d body=%s", w.Code, w.Body.String())
		}
		for _, want := range []string{"Edit event", "Original", "Old notes", "Europe/Prague", `name="version"`, `value="2026-10-02"`} {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("editor missing %q", want)
			}
		}
		if allDay && !strings.Contains(w.Body.String(), `value="2026-10-04"`) {
			t.Fatal("exclusive all-day end was not converted to the last occupied day")
		}
		if !allDay && !strings.Contains(w.Body.String(), `value="09:00"`) {
			t.Fatal("timed edit did not retain the event's local wall clock")
		}
	}
}

func TestCalendarDetailsAdvertiseOnlySupportedEdits(t *testing.T) {
	h := calendarUpdateFixture(t)
	request := func() string {
		r := httptest.NewRequest(http.MethodGet, "/api/calendar/events/edit-event", nil)
		r.SetPathValue("id", "edit-event")
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
		w := httptest.NewRecorder()
		h.handleCalendarEvent(w, r)
		if w.Code != 200 {
			t.Fatalf("details=%d body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if !strings.Contains(request(), "data-calendar-edit-trigger") {
		t.Fatal("writable, supported event has no edit action")
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_events SET attendees_json='[{"email":"guest@example.com"}]' WHERE id='edit-event'`); err != nil {
		t.Fatal(err)
	}
	html := request()
	if strings.Contains(html, "data-calendar-edit-trigger") || !strings.Contains(html, "Events with guests") {
		t.Fatal("meeting details did not explain why editing is unavailable")
	}
}
