package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarDeleteFixture(t *testing.T) *Handler {
	t.Helper()
	h := calendarUpdateFixture(t)
	h.calendarDeleteEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent) error { return nil }
	return h
}

func calendarDeleteRequest(user, query string) *http.Request {
	r := httptest.NewRequest(http.MethodDelete, "/api/calendar/events/edit-event?"+query, nil)
	r.SetPathValue("id", "edit-event")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
}

func TestCalendarDeleteConfirmedEventOnlyAndRefreshNotification(t *testing.T) {
	h := calendarDeleteFixture(t)
	before, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	bus := h.syncer.Events()
	events := bus.Subscribe()
	defer bus.Unsubscribe(events)
	calls := 0
	h.calendarDeleteEvent = func(_ context.Context, source storage.CalendarSource, event storage.CalendarEvent) error {
		calls++
		if source.UserID != "one" || source.ID != "one-source" || event.RemoteID != "remote-event" || event.ETag != `"v1"` {
			t.Fatal("incorrect deletion target")
		}
		return nil
	}
	query := url.Values{"version": {`"v1"`}}.Encode()
	w := httptest.NewRecorder()
	h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", query))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"deleted":true`) || !strings.Contains(w.Body.String(), `"event_id":"edit-event"`) {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("deleted event remains visible")
	}
	var tombstone bool
	if err := h.db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='edit-event'`).Scan(&tombstone); err != nil || !tombstone {
		t.Fatalf("missing deletion tombstone: %v", err)
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "untouched"); err != nil {
		t.Fatal("delete removed an unrelated event")
	}
	after, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	if !before[0].LastSuccessAt.Equal(*after[0].LastSuccessAt) || before[0].SyncAttempt != after[0].SyncAttempt {
		t.Fatal("delete fabricated a full-calendar synchronization")
	}
	select {
	case event := <-events:
		if event.Type != mail.EventCalendarChanged || event.UserID != "one" {
			t.Fatal("incorrect refresh notification")
		}
	default:
		t.Fatal("missing HTMX cache refresh notification")
	}
	w = httptest.NewRecorder()
	h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", query))
	if w.Code != 404 || calls != 1 {
		t.Fatal("repeated deletion reached the provider again")
	}
}

func TestCalendarDeleteAuthorizationVersionAndShapeGates(t *testing.T) {
	for _, tc := range []struct {
		name, user, query, sql string
		status                 int
	}{
		{name: "foreign owner", user: "two", status: 404},
		{name: "missing version", query: "none", status: 400},
		{name: "duplicate version", query: "version=one&version=two", status: 400},
		{name: "unexpected field", query: "version=one&source_id=two-source", status: 400},
		{name: "oversized version", query: "version=" + strings.Repeat("v", 2049), status: 400},
		{name: "stale version", query: "version=old", status: 409},
		{name: "read only", sql: `UPDATE calendar_sources SET access_role='reader' WHERE id='one-source'`, status: 403},
		{name: "deselected", sql: `UPDATE calendar_sources SET is_selected=0 WHERE id='one-source'`, status: 404},
		{name: "deleting account", sql: `UPDATE accounts SET is_deleting=1 WHERE id='one-account'`, status: 404},
		{name: "guests", sql: `UPDATE calendar_events SET attendees_json='[{"email":"guest@example.com"}]' WHERE id='edit-event'`, status: 403},
		{name: "recurring", sql: `UPDATE calendar_events SET recurrence_json='["RRULE:FREQ=DAILY"]' WHERE id='edit-event'`, status: 403},
		{name: "occurrence", sql: `UPDATE calendar_events SET series_remote_id='series' WHERE id='edit-event'`, status: 403},
		{name: "online meeting", sql: `UPDATE calendar_events SET online_meeting_json='{"url":"https://meeting.example"}' WHERE id='edit-event'`, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := calendarDeleteFixture(t)
			if tc.sql != "" {
				if _, err := h.db.Write().Exec(tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			h.calendarDeleteEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent) error {
				t.Fatal("ineligible delete reached provider")
				return nil
			}
			query, user := tc.query, tc.user
			if query == "" {
				query = url.Values{"version": {`"v1"`}}.Encode()
			}
			if query == "none" {
				query = ""
			}
			if user == "" {
				user = "one"
			}
			w := httptest.NewRecorder()
			h.handleDeleteCalendarEvent(w, calendarDeleteRequest(user, query))
			if w.Code != tc.status {
				t.Fatalf("status=%d expected=%d body=%s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	h := calendarDeleteFixture(t)
	h.calendarDeleteEvent = nil
	w := httptest.NewRecorder()
	h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", url.Values{"version": {`"v1"`}}.Encode()))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Reconnect") {
		t.Fatal("missing OAuth write grant allowed deletion")
	}
}

func TestCalendarDeleteFailuresPreserveCache(t *testing.T) {
	for _, tc := range []struct {
		name                string
		err                 error
		status              int
		uncertain, conflict bool
	}{
		{"changed", errCalendarUpdateConflict, 409, false, true},
		{"unsupported", errCalendarUpdateUnsupported, 403, false, true},
		{"missing remotely", calendarCreateProviderError{404}, 409, false, true},
		{"denied", calendarCreateProviderError{403}, 502, false, false},
		{"timeout before write", calendarDeletePreflightError{context.DeadlineExceeded}, 502, false, false},
		{"timeout after write", context.DeadlineExceeded, 502, true, false},
		{"provider unavailable", calendarCreateProviderError{503}, 502, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := calendarDeleteFixture(t)
			h.calendarDeleteEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent) error { return tc.err }
			w := httptest.NewRecorder()
			h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", url.Values{"version": {`"v1"`}}.Encode()))
			var result struct{ Uncertain, Conflict bool }
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || result.Uncertain != tc.uncertain || result.Conflict != tc.conflict {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event"); err != nil || event.IsDeleted || event.ETag != `"v1"` {
				t.Fatal("unconfirmed delete changed cache")
			}
		})
	}
}

func TestCalendarDeleteLocalConflictAfterRemoteSuccess(t *testing.T) {
	h := calendarDeleteFixture(t)
	h.calendarDeleteEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent) error {
		_, err := h.db.Write().Exec(`UPDATE calendar_events SET etag='newer' WHERE id='edit-event'`)
		return err
	}
	w := httptest.NewRecorder()
	h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", url.Values{"version": {`"v1"`}}.Encode()))
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"uncertain":true`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event"); err != nil {
		t.Fatal("delete hid a different cached version")
	}
}

func TestCalendarDetailsOfferDeleteOnlyWhenAllowed(t *testing.T) {
	h := calendarDeleteFixture(t)
	for _, allowed := range []bool{true, false} {
		if !allowed {
			_, err := h.db.Write().Exec(`UPDATE calendar_events SET attendees_json='[{"email":"guest@example.com"}]' WHERE id='edit-event'`)
			if err != nil {
				t.Fatal(err)
			}
		}
		r := calendarDeleteRequest("one", "")
		w := httptest.NewRecorder()
		h.handleCalendarEvent(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), "data-calendar-delete-form") != allowed {
			t.Fatalf("allowed=%v status=%d body=%s", allowed, w.Code, w.Body.String())
		}
	}
}
