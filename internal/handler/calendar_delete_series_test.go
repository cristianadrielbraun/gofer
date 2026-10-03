package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarDeleteSeriesQuery(master calendar.RemoteEvent) url.Values {
	return url.Values{"version": {master.ETag}, "scope": {"series"}, "series_id": {master.RemoteID}}
}

func TestCalendarDeleteSeriesConfirmationLoadsFreshMasterWithoutWriting(t *testing.T) {
	h, master := calendarSeriesFixture(t)
	master.Summary = "Series <script>title</script>"
	reads := 0
	h.calendarReadSeries = func(_ context.Context, source storage.CalendarSource, id string) (calendar.RemoteEvent, error) {
		reads++
		if source.UserID != "one" || source.ID != "one-source" || id != "master" {
			t.Fatal("confirmation read wrong source/master")
		}
		return master, nil
	}
	h.calendarDeleteEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent) error {
		t.Fatal("confirmation must not write")
		return nil
	}
	w := httptest.NewRecorder()
	r := calendarDeleteRequest("one", "")
	r.Method = http.MethodGet
	h.handleCalendarEvent(w, r)
	if w.Code != 200 || reads != 0 {
		t.Fatal("cached event details should not wait for a provider read")
	}
	for _, want := range []string{`Delete series`, `/delete-series-confirmation`, `data-calendar-delete-ready="false"`, `name="version" value=""`, `Checking the series`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("lazy confirmation missing %q", want)
		}
	}
	w = httptest.NewRecorder()
	h.handleCalendarSeriesDeleteConfirmation(w, r)
	if w.Code != 200 || reads != 1 {
		t.Fatalf("confirmation: %d %s", w.Code, w.Body.String())
	}
	for _, want := range []string{`Delete the entire series?`, `all occurrences, including past and future events`, `data-calendar-delete-ready="true"`, `name="scope" value="series"`, `name="series_id" value="master"`, `name="version" value="` + html.EscapeString(master.ETag) + `"`, html.EscapeString(master.Summary), `hx-params="version,scope,series_id"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("ready confirmation missing %q", want)
		}
	}
	if strings.Contains(w.Body.String(), "<dialog") || strings.Contains(w.Body.String(), "<script>") || strings.Contains(w.Body.String(), "occurrence-v1") {
		t.Fatal("confirmation replaced backdrop, escaped poorly, or used occurrence version")
	}
	if _, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event"); err != nil {
		t.Fatal("confirmation changed cache")
	}
	w = httptest.NewRecorder()
	r = calendarDeleteRequest("two", "")
	r.Method = http.MethodGet
	h.handleCalendarSeriesDeleteConfirmation(w, r)
	if w.Code != 404 || reads != 1 {
		t.Fatal("foreign confirmation reached provider")
	}
}

func TestCalendarDeleteSeriesRemovesAllCachedInstancesAndPublishes(t *testing.T) {
	h, master := calendarSeriesFixture(t)
	occurrence, _ := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	start, end := occurrence.StartAt.AddDate(5, 0, 0), occurrence.EndAt.AddDate(5, 0, 0)
	if err := h.db.ReplaceCalendarEvents(t.Context(), "one", "one-source", []storage.CalendarEvent{
		{ID: "distant", RemoteID: "distant-instance", SeriesRemoteID: "master", ETag: `"d1"`, StartAt: &start, EndAt: &end},
		{ID: "other-series", RemoteID: "other-instance", SeriesRemoteID: "other-master", ETag: `"o1"`, StartAt: &start, EndAt: &end},
	}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	bus := h.syncer.Events()
	events := bus.Subscribe()
	defer bus.Unsubscribe(events)
	writes := 0
	h.calendarDeleteEvent = func(_ context.Context, source storage.CalendarSource, event storage.CalendarEvent) error {
		writes++
		if source.ID != "one-source" || source.UserID != "one" || event.RemoteID != master.RemoteID || event.ETag != master.ETag || event.SeriesRemoteID != "" {
			t.Fatal("delete targeted an occurrence or a different series")
		}
		return nil
	}
	w := httptest.NewRecorder()
	query := calendarDeleteSeriesQuery(master).Encode()
	h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", query))
	var result struct {
		Deleted  bool
		EventID  string `json:"event_id"`
		SeriesID string `json:"series_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Deleted || result.EventID != "edit-event" || result.SeriesID != "master" || writes != 1 {
		t.Fatalf("delete: %d %s %v", w.Code, w.Body.String(), err)
	}
	for _, id := range []string{"edit-event", "distant"} {
		if _, err := h.db.GetCalendarEvent(t.Context(), "one", id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("instance remains visible: %s %v", id, err)
		}
		var status string
		if err := h.db.Read().QueryRow(`SELECT status FROM calendar_events WHERE id=?`, id).Scan(&status); err != nil || status != "cancelled" {
			t.Fatalf("missing tombstone: %s %v", id, err)
		}
	}
	for _, id := range []string{"untouched", "other-series"} {
		if _, err := h.db.GetCalendarEvent(t.Context(), "one", id); err != nil {
			t.Fatalf("unrelated event removed: %s %v", id, err)
		}
	}
	after, _ := h.db.ListSelectedCalendarSources(t.Context(), "one")
	if before[0].SyncAttempt != after[0].SyncAttempt || !before[0].LastSuccessAt.Equal(*after[0].LastSuccessAt) {
		t.Fatal("deletion fabricated a full-calendar refresh")
	}
	select {
	case event := <-events:
		if event.Type != mail.EventCalendarChanged || event.UserID != "one" {
			t.Fatal("wrong cache refresh notification")
		}
	default:
		t.Fatal("missing Calendar/Upcoming refresh notification")
	}
	w = httptest.NewRecorder()
	h.handleDeleteCalendarEvent(w, calendarDeleteRequest("one", query))
	if w.Code != 404 || writes != 1 {
		t.Fatal("replayed delete reached provider")
	}
}

func TestCalendarDeleteSeriesFailuresPreserveCache(t *testing.T) {
	for _, mode := range []string{"foreign", "missing-scope", "duplicate-scope", "missing-master", "wrong-master", "remote-wrong-master", "stale-master", "occurrence-version", "read-failed", "complex", "read-only", "deselected", "guests", "missing-write-grant", "provider-conflict", "provider-uncertain", "cache-conflict"} {
		t.Run(mode, func(t *testing.T) {
			h, master := calendarSeriesFixture(t)
			query := calendarDeleteSeriesQuery(master)
			user, writes := "one", 0
			h.calendarDeleteEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent) error {
				writes++
				switch mode {
				case "provider-conflict":
					return errCalendarUpdateConflict
				case "provider-uncertain":
					return context.DeadlineExceeded
				case "cache-conflict":
					_, err := h.db.Write().Exec(`UPDATE calendar_events SET etag='newer' WHERE id='edit-event'`)
					return err
				default:
					t.Fatal("unsafe deletion reached provider")
					return nil
				}
			}
			switch mode {
			case "foreign":
				user = "two"
			case "missing-scope":
				query.Del("scope")
				query.Del("series_id")
			case "duplicate-scope":
				query.Add("scope", "series")
			case "missing-master":
				query.Del("series_id")
			case "wrong-master":
				query.Set("series_id", "different")
			case "stale-master":
				query.Set("version", `"older"`)
			case "occurrence-version":
				query.Set("version", `"occurrence-v1"`)
			case "read-failed":
				h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
					return calendar.RemoteEvent{}, errors.New("offline")
				}
			case "remote-wrong-master":
				master.RemoteID = "different"
				h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
					return master, nil
				}
			case "complex":
				master.Recurrence = json.RawMessage(`["RRULE:FREQ=WEEKLY;BYDAY=MO,WE"]`)
				h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
					return master, nil
				}
			case "read-only":
				if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET access_role='reader' WHERE id='one-source'`); err != nil {
					t.Fatal(err)
				}
			case "deselected":
				if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='one-source'`); err != nil {
					t.Fatal(err)
				}
			case "guests":
				master.Attendees = json.RawMessage(`[{"email":"guest@example.com"}]`)
				h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
					return master, nil
				}
			case "missing-write-grant":
				h.calendarDeleteEvent = nil
			}
			w := httptest.NewRecorder()
			h.handleDeleteCalendarEvent(w, calendarDeleteRequest(user, query.Encode()))
			var result struct{ Uncertain bool }
			_ = json.Unmarshal(w.Body.Bytes(), &result)
			uncertain := mode == "provider-uncertain" || mode == "cache-conflict"
			if w.Code < 400 || result.Uncertain != uncertain {
				t.Fatalf("failure: %d %s", w.Code, w.Body.String())
			}
			var deleted bool
			if err := h.db.Read().QueryRow(`SELECT is_deleted FROM calendar_events WHERE id='edit-event'`).Scan(&deleted); err != nil || deleted {
				t.Fatalf("unconfirmed deletion hid cached event: %v", err)
			}
			wantWrites := 0
			if uncertain || mode == "provider-conflict" {
				wantWrites = 1
			}
			if writes != wantWrites {
				t.Fatalf("writes=%d, want %d", writes, wantWrites)
			}
		})
	}
}
