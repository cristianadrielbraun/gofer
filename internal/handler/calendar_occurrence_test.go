package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarOccurrenceEditorAndUpdateOnlySelectedEvent(t *testing.T) {
	h, _ := calendarSeriesFixture(t)
	h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
		t.Fatal("occurrence edit must not fetch or edit its master")
		return calendar.RemoteEvent{}, nil
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_events SET series_remote_id='master', etag='"sibling"' WHERE id='untouched'`); err != nil {
		t.Fatal(err)
	}
	r := calendarUpdateRequest(calendarCreateForm(), "one")
	r.Method, r.URL.RawQuery = http.MethodGet, "scope=occurrence"
	w := httptest.NewRecorder()
	h.handleEditCalendarEvent(w, r)
	for _, text := range []string{`name="edit_scope" value="occurrence"`, `value="&#34;occurrence-v1&#34;"`, `name="start_date" value="2026-10-02"`, `this event only`, `data-calendar-edit-occurrence="true"`} {
		if w.Code != 200 || !strings.Contains(w.Body.String(), text) {
			t.Errorf("editor missing %s: %d", text, w.Code)
		}
	}
	if strings.Contains(w.Body.String(), `name="repeat_frequency"`) {
		t.Fatal("occurrence editor exposes series repeat controls")
	}
	writes := 0
	h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, event storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
		writes++
		if event.RemoteID != "remote-event" || event.SeriesRemoteID != "master" || event.ETag != `"occurrence-v1"` || draft.Recurrence != nil {
			t.Fatal("incorrect write scope")
		}
		result := calendarCreatedRemote(draft)
		result.RemoteID, result.SeriesRemoteID, result.ETag, result.ICalUID = event.RemoteID, event.SeriesRemoteID, `"occurrence-v2"`, event.ICalUID
		return result, nil
	}
	form := calendarCreateForm()
	form.Set("version", `"occurrence-v1"`)
	form.Set("edit_scope", "occurrence")
	form.Set("start_date", "2026-10-03")
	form.Set("end_date", "2026-10-03")
	w = httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(form, "one"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"scope":"occurrence"`) || writes != 1 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	got, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || got.Summary != "Planning" || got.SeriesRemoteID != "master" || got.ETag != `"occurrence-v2"` || got.StartAt.Day() != 3 {
		t.Fatalf("selected cache: %+v %v", got, err)
	}
	sibling, err := h.db.GetCalendarEvent(t.Context(), "one", "untouched")
	if err != nil || sibling.Summary != "Keep me" || sibling.ETag != `"sibling"` {
		t.Fatal("one-off save changed a sibling")
	}
	w = httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(form, "one"))
	if w.Code != 409 || writes != 1 {
		t.Fatal("stale retry wrote again")
	}
}

func TestCalendarOccurrenceMutationGates(t *testing.T) {
	for _, mode := range []string{"repeat", "foreign", "wrong-parent", "missing-parent", "duplicate-scope", "single", "guests", "stale"} {
		t.Run(mode, func(t *testing.T) {
			h, _ := calendarSeriesFixture(t)
			form, user := calendarCreateForm(), "one"
			form.Set("version", `"occurrence-v1"`)
			form.Set("edit_scope", "occurrence")
			writes := 0
			h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, event storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
				writes++
				result := calendarCreatedRemote(draft)
				result.RemoteID, result.ETag, result.SeriesRemoteID = event.RemoteID, `"new"`, "other-series"
				if mode == "missing-parent" {
					result.SeriesRemoteID = ""
				}
				return result, nil
			}
			switch mode {
			case "repeat":
				form.Set("repeat_frequency", "daily")
				form.Set("repeat_interval", "1")
				form.Set("repeat_end", "never")
			case "foreign":
				user = "two"
			case "duplicate-scope":
				form.Add("edit_scope", "series")
			case "single":
				_, _ = h.db.Write().Exec(`UPDATE calendar_events SET series_remote_id='' WHERE id='edit-event'`)
			case "guests":
				_, _ = h.db.Write().Exec(`UPDATE calendar_events SET attendees_json='[{"email":"guest@example.com"}]' WHERE id='edit-event'`)
			case "stale":
				form.Set("version", `"old"`)
			}
			w := httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, calendarUpdateRequest(form, user))
			if w.Code < 400 {
				t.Fatalf("accepted %s", mode)
			}
			if mode != "wrong-parent" && mode != "missing-parent" && writes != 0 {
				t.Fatal("invalid request reached provider")
			}
			got, _ := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
			if got.Summary != "Original" || got.ETag != `"occurrence-v1"` {
				t.Fatal("unconfirmed mutation changed cache")
			}
		})
	}
}

func TestCalendarOccurrenceDeleteKeepsOtherOccurrences(t *testing.T) {
	h, _ := calendarSeriesFixture(t)
	h.calendarDeleteEvent = func(_ context.Context, _ storage.CalendarSource, event storage.CalendarEvent) error {
		if event.RemoteID != "remote-event" || event.SeriesRemoteID != "master" || event.ETag != `"occurrence-v1"` {
			t.Fatal("deleted master instead of occurrence")
		}
		return nil
	}
	_, _ = h.db.Write().Exec(`UPDATE calendar_events SET series_remote_id='master' WHERE id='untouched'`)
	w := httptest.NewRecorder()
	r := calendarDeleteRequest("one", "")
	r.Method = http.MethodGet
	h.handleCalendarOccurrenceDeleteConfirmation(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="scope" value="occurrence"`) || !strings.Contains(w.Body.String(), `Only this occurrence`) {
		t.Fatalf("confirmation: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", url.Values{"version": {`"occurrence-v1"`}, "scope": {"occurrence"}}.Encode()))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"scope":"occurrence"`) || strings.Contains(w.Body.String(), `"series_id"`) {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("selected event still visible")
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "untouched"); err != nil {
		t.Fatal("sibling deleted")
	}
}
