package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarCreateForm() url.Values {
	return url.Values{"request_id": {"cc77e9f1-81da-4fe3-9566-cd3b7e01700c"}, "source_id": {"one-source"}, "summary": {"Planning"},
		"start_date": {"2026-10-02"}, "end_date": {"2026-10-02"}, "start_time": {"09:00"}, "end_time": {"10:00"}, "timezone": {"Europe/Prague"}, "description": {"Full notes"}, "location": {"Room 2"}}
}

func calendarCreateHTTPRequest(values url.Values, user string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/calendar/events", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
}

func calendarCreateFixture(t *testing.T) *Handler {
	t.Helper()
	h := calendarWorkerFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET access_role = 'writer'`); err != nil {
		t.Fatal(err)
	}
	return h
}

func calendarCreatedRemote(draft calendar.EventDraft) calendar.RemoteEvent {
	return calendar.RemoteEvent{RemoteID: draft.RequestID, Summary: draft.Summary, Description: draft.Description, Location: draft.Location,
		AllDay: draft.AllDay, StartDate: draft.StartDate, EndDate: draft.EndDate, StartAt: draft.StartAt, EndAt: draft.EndAt, StartTimeZone: draft.TimeZone, EndTimeZone: draft.TimeZone}
}

func TestCalendarCreateValidationAndAllDayDateSemantics(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"missing title", "summary", " "}, {"invalid request", "request_id", "invalid"}, {"unknown timezone", "timezone", "Not/AZone"},
		{"invalid date", "start_date", "2026-02-30"}, {"end before start", "end_time", "08:00"}, {"same start end", "end_time", "09:00"},
		{"missing time", "start_time", ""}, {"oversized location", "location", strings.Repeat("a", 1025)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := calendarCreateForm()
			values.Set(tc.key, tc.value)
			if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
				t.Fatal("invalid draft accepted")
			}
		})
	}
	for _, date := range []string{"2026-03-29", "2026-10-25"} {
		values := calendarCreateForm()
		values.Set("start_date", date)
		values.Set("end_date", date)
		values.Set("start_time", "02:30")
		values.Set("end_time", "04:00")
		if _, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one")); err == nil {
			t.Fatalf("nonexistent or ambiguous DST time %s accepted", date)
		}
	}
	values := calendarCreateForm()
	values.Set("all_day", "true")
	values.Del("start_time")
	values.Del("end_time")
	values.Set("start_date", "2026-10-25")
	values.Set("end_date", "2026-10-25")
	draft, err := parseCalendarEventDraft(calendarCreateHTTPRequest(values, "one"))
	if err != nil || !draft.AllDay || draft.StartDate != "2026-10-25" || draft.EndDate != "2026-10-26" || draft.StartAt != nil {
		t.Fatalf("all-day draft=%#v, error=%v", draft, err)
	}
}

func TestCalendarCreateStoresOnlyConfirmedEventAndReplaysWithoutAnotherProviderWrite(t *testing.T) {
	h := calendarCreateFixture(t)
	start, end := time.Now(), time.Now().Add(time.Hour)
	if err := h.db.ReplaceCalendarEvents(t.Context(), "one", "one-source", []storage.CalendarEvent{{ID: "existing", RemoteID: "existing", Summary: "Keep me", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	var calls atomic.Int32
	h.calendarCreateEvent = func(_ context.Context, source storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		calls.Add(1)
		if source.UserID != "one" || source.ID != "one-source" {
			t.Fatal("foreign source")
		}
		return calendarCreatedRemote(draft), nil
	}
	bus := h.syncer.Events()
	events := bus.Subscribe()
	defer bus.Unsubscribe(events)
	var id string
	for range 2 {
		w := httptest.NewRecorder()
		h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(calendarCreateForm(), "one"))
		if w.Code != 201 {
			t.Fatalf("create status=%d body=%s", w.Code, w.Body.String())
		}
		var result struct {
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if id != "" && id != result.EventID {
			t.Fatal("retry changed local event identity")
		}
		id = result.EventID
	}
	if calls.Load() != 1 {
		t.Fatalf("provider writes=%d", calls.Load())
	}
	created, err := h.db.GetCalendarEvent(t.Context(), "one", id)
	if err != nil || created.Summary != "Planning" || created.Description != "Full notes" {
		t.Fatalf("cache=%#v, error=%v", created, err)
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "existing"); err != nil {
		t.Fatal("creation reconciled away unrelated events")
	}
	after, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	if !after[0].LastSuccessAt.Equal(*before[0].LastSuccessAt) || after[0].SyncAttempt != before[0].SyncAttempt {
		t.Fatal("creation fabricated a full sync result")
	}
	select {
	case event := <-events:
		if event.Type != mail.EventCalendarChanged || event.UserID != "one" || sseEventVisible(event, "two", nil, false) {
			t.Fatalf("change event leaked: %#v", event)
		}
	default:
		t.Fatal("missing real-time cache change")
	}
	values := calendarCreateForm()
	values.Set("summary", "Different event")
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
	if w.Code != 409 || calls.Load() != 1 {
		t.Fatal("a reused request ID accepted different details")
	}
}

func TestCalendarCreateEnforcesOwnerSelectionAndWritePermissionsBeforeProvider(t *testing.T) {
	h := calendarCreateFixture(t)
	var calls atomic.Int32
	h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
		calls.Add(1)
		return calendar.RemoteEvent{}, nil
	}
	for _, source := range []string{"two-source", "one-unselected", "missing"} {
		values := calendarCreateForm()
		values.Set("source_id", source)
		w := httptest.NewRecorder()
		h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(values, "one"))
		if w.Code != 404 {
			t.Fatalf("source %s status=%d", source, w.Code)
		}
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET access_role='reader' WHERE id='one-source'`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(calendarCreateForm(), "one"))
	if w.Code != 403 || calls.Load() != 0 {
		t.Fatal("read-only source reached a provider write")
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET access_role='writer' WHERE id='one-source'`); err != nil {
		t.Fatal(err)
	}
	h.calendarCreateEvent = nil // No OAuth grant: independent of the calendar's ownership role.
	w = httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(calendarCreateForm(), "one"))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Reconnect") {
		t.Fatal("missing authorization allowed creation")
	}
}

func TestCalendarCreateFailureKeepsDraftAndRetriesAnUncertainResult(t *testing.T) {
	h := calendarCreateFixture(t)
	var calls atomic.Int32
	h.calendarCreateEvent = func(_ context.Context, _ storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		if calls.Add(1) == 1 {
			return calendar.RemoteEvent{}, context.DeadlineExceeded
		}
		return calendarCreatedRemote(draft), nil
	}
	w := httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(calendarCreateForm(), "one"))
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"uncertain":true`) {
		t.Fatalf("ambiguous creation falsely reported success: %s", w.Body.String())
	}
	var count int
	_ = h.db.Read().QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&count)
	if count != 0 {
		t.Fatal("unconfirmed event entered the cache")
	}
	w = httptest.NewRecorder()
	h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(calendarCreateForm(), "one"))
	if w.Code != 201 || calls.Load() != 2 {
		t.Fatal("same request could not recover")
	}
}

func TestCalendarCreateWaitsForActiveRefreshAndCoalescesDoubleSubmit(t *testing.T) {
	h := calendarCreateFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
		close(entered)
		<-release
		return calendar.EventPage{}, nil
	}
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_, _ = h.syncCalendarWindow(t.Context(), "one", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	}()
	<-entered
	var calls atomic.Int32
	h.calendarCreateEvent = func(_ context.Context, _ storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		calls.Add(1)
		return calendarCreatedRemote(draft), nil
	}
	results := make(chan int, 2)
	for range 2 {
		go func() {
			w := httptest.NewRecorder()
			h.handleCreateCalendarEvent(w, calendarCreateHTTPRequest(calendarCreateForm(), "one"))
			results <- w.Code
		}()
	}
	if calls.Load() != 0 {
		t.Fatal("creation overlapped the active refresh")
	}
	close(release)
	<-refreshDone
	for range 2 {
		select {
		case status := <-results:
			if status != 201 {
				t.Fatalf("create status=%d", status)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("creation did not resume")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("double submit made %d provider writes", calls.Load())
	}
}

func TestCalendarNewEventPrefillsSelectedDateWithoutProviderReads(t *testing.T) {
	h := calendarCreateFixture(t)
	if err := h.db.SetUISettings(t.Context(), "one", map[string]string{"timezone": "Europe/Prague"}); err != nil {
		t.Fatal(err)
	}
	h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
		return calendar.RemoteEvent{}, errors.New("opening must not write")
	}
	r := httptest.NewRequest(http.MethodGet, "/api/calendar/events/new?date=2026-12-03", nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	w := httptest.NewRecorder()
	h.handleNewCalendarEvent(w, r)
	if w.Code != 200 {
		t.Fatalf("dialog status=%d", w.Code)
	}
	for _, wanted := range []string{"New event", `value="2026-12-03"`, `value="Europe/Prague"`, "one-source", "Create event", "data-calendar-create-form"} {
		if !strings.Contains(w.Body.String(), wanted) {
			t.Errorf("missing %s", wanted)
		}
	}
	if strings.Contains(w.Body.String(), "two-source") || strings.Contains(w.Body.String(), "one-unselected") {
		t.Fatal("dialog showed an unowned or unselected source")
	}
}
