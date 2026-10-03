package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarSeriesFixture(t *testing.T) (*Handler, calendar.RemoteEvent) {
	t.Helper()
	h := calendarUpdateFixture(t)
	if _, err := h.db.Write().Exec(`UPDATE calendar_events SET series_remote_id='master', etag='"occurrence-v1"' WHERE id='edit-event'`); err != nil {
		t.Fatal(err)
	}
	draft := calendarProviderDraft(t, false)
	start, end := draft.StartAt.AddDate(-2, 0, 0), draft.EndAt.AddDate(-2, 0, 0)
	draft.StartAt, draft.EndAt = &start, &end
	draft.Recurrence = &calendar.RecurrenceDraft{Frequency: "daily", Interval: 1}
	master := calendarCreatedRemote(draft)
	master.RemoteID, master.ETag, master.Summary = "master", `"master-v1"`, "Original series"
	master.Recurrence, _ = json.Marshal([]string{"RRULE:" + calendarRecurrenceRule(draft)})
	h.calendarReadSeries = func(_ context.Context, source storage.CalendarSource, id string) (calendar.RemoteEvent, error) {
		if id != "master" || source.ID != "one-source" || source.UserID != "one" {
			t.Fatal("read escaped owned source/master")
		}
		return master, nil
	}
	return h, master
}

func TestCalendarSeriesEditorLoadsMasterAndRepeatSettings(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		h, master := calendarSeriesFixture(t)
		if allDay {
			master.AllDay, master.StartDate, master.EndDate = true, "2024-10-02", "2024-10-04"
			master.StartAt, master.EndAt, master.StartTimeZone = nil, nil, ""
			master.Recurrence = json.RawMessage(`["RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=WE;COUNT=9"]`)
			h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
				return master, nil
			}
		}
		r := calendarUpdateRequest(calendarCreateForm(), "one")
		r.Method, r.URL.RawQuery = http.MethodGet, "scope=series"
		w := httptest.NewRecorder()
		h.handleEditCalendarEvent(w, r)
		html := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("editor: %d %s", w.Code, html)
		}
		for _, want := range []string{`Edit series`, `Save series`, `name="edit_scope" value="series"`, `value="&#34;master-v1&#34;"`, `name="start_date" value="2024-10-02"`, `including past occurrences`, `data-calendar-edit-series="true"`} {
			if !strings.Contains(html, want) {
				t.Errorf("master editor missing %q", want)
			}
		}
		frequency := "daily"
		if allDay {
			frequency = "weekly"
			for _, want := range []string{`name="end_date" value="2024-10-03"`, `name="repeat_end" data-tui-selectbox-hidden-input value="count"`, `name="repeat_count" value="9"`} {
				if !strings.Contains(html, want) {
					t.Errorf("all-day prefill missing %q", want)
				}
			}
		}
		if !strings.Contains(html, `name="repeat_frequency" data-tui-selectbox-hidden-input value="`+frequency+`"`) || strings.Contains(html, `data-calendar-repeat-options hidden`) {
			t.Fatal("repeat settings were not prefilled and visible")
		}
		r.URL.RawQuery = ""
		w = httptest.NewRecorder()
		h.handleEditCalendarEvent(w, r)
		if w.Code != 400 {
			t.Fatal("series editor accepted implicit occurrence scope")
		}
		w = httptest.NewRecorder()
		h.handleCalendarEvent(w, r)
		if !strings.Contains(w.Body.String(), `/edit?scope=series`) || !strings.Contains(w.Body.String(), "Edit series") || strings.Contains(w.Body.String(), `hx-delete=`) {
			t.Fatal("details must advertise whole-series editing, but not series deletion")
		}
	}
}

func TestCalendarSeriesUpdateInvalidatesAllInstancesAndRefreshesViewedWindow(t *testing.T) {
	for _, refreshFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "refreshed", true: "refresh failed"}[refreshFails], func(t *testing.T) {
			h, master := calendarSeriesFixture(t)
			occurrence, _ := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
			untouched, _ := h.db.GetCalendarEvent(t.Context(), "one", "untouched")
			futureStart, futureEnd := occurrence.StartAt.AddDate(5, 0, 0), occurrence.EndAt.AddDate(5, 0, 0)
			far := storage.CalendarEvent{ID: "far-instance", RemoteID: "far-remote", SeriesRemoteID: "master", ETag: `"far-v1"`, StartAt: &futureStart, EndAt: &futureEnd}
			other := far
			other.ID, other.RemoteID, other.SeriesRemoteID = "other-instance", "other-remote", "different-master"
			if err := h.db.ReplaceCalendarEvents(t.Context(), "one", "one-source", []storage.CalendarEvent{far, other}, futureStart.Add(-time.Hour), futureEnd.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			writes, reads := 0, 0
			h.calendarUpdateEvent = func(_ context.Context, _ storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
				writes++
				if existing.RemoteID != "master" || existing.SeriesRemoteID != "" || existing.ETag != master.ETag || existing.StartAt.Year() != 2024 || draft.Recurrence == nil {
					t.Fatal("write did not target the versioned master")
				}
				remote := calendarCreatedRemote(draft)
				remote.RemoteID, remote.ETag = "master", `"master-v2"`
				remote.Recurrence, _ = json.Marshal([]string{"RRULE:" + calendarRecurrenceRule(draft)})
				return remote, nil
			}
			h.calendarFetchEvents = func(_ context.Context, _ storage.CalendarSource, query calendar.EventQuery) (calendar.EventPage, error) {
				reads++
				if query.WindowEnd.Sub(query.WindowStart) > 250*24*time.Hour || !query.WindowStart.After(*master.StartAt) {
					t.Fatal("refresh spans years back to the master rather than the viewed/current windows")
				}
				for _, id := range []string{"edit-event", "far-instance"} {
					if _, err := h.db.GetCalendarEvent(t.Context(), "one", id); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("obsolete occurrence %s remained visible before refresh", id)
					}
				}
				if refreshFails {
					return calendar.EventPage{}, errors.New("offline")
				}
				start, end := *occurrence.StartAt, *occurrence.EndAt
				return calendar.EventPage{Events: []calendar.RemoteEvent{
					{RemoteID: "fresh-instance", SeriesRemoteID: "master", ETag: `"new-instance"`, Summary: "Changed series", StartAt: &start, EndAt: &end},
					{RemoteID: untouched.RemoteID, Summary: untouched.Summary, StartAt: untouched.StartAt, EndAt: untouched.EndAt},
				}}, nil
			}
			form := calendarRecurringForm()
			form.Set("start_date", "2024-10-02")
			form.Set("end_date", "2024-10-02")
			form.Set("repeat_end", "never")
			form.Del("repeat_count")
			form.Set("edit_scope", "series")
			form.Set("version", master.ETag)
			w := httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, calendarUpdateRequest(form, "one"))
			var result struct {
				Saved          bool
				SeriesID       string `json:"series_id"`
				RefreshPending bool   `json:"refresh_pending"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Saved || result.SeriesID != "master" || result.RefreshPending != refreshFails || writes != 1 || reads < 1 {
				t.Fatalf("update: %d %s writes=%d reads=%d %v", w.Code, w.Body.String(), writes, reads, err)
			}
			for _, id := range []string{"untouched", "other-instance"} {
				if _, err := h.db.GetCalendarEvent(t.Context(), "one", id); err != nil {
					t.Fatalf("unrelated event changed: %s %v", id, err)
				}
			}
			if _, err := h.db.GetCalendarEvent(t.Context(), "one", "far-instance"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("old distant instance resurrected")
			}
			w = httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, calendarUpdateRequest(form, "one"))
			if w.Code != 404 || writes != 1 {
				t.Fatal("replayed request wrote the series again")
			}
		})
	}
}

func TestCalendarSeriesUpdateRejectsUnsafeRequestsBeforeWrite(t *testing.T) {
	for _, mode := range []string{"foreign", "stale-master", "occurrence-version", "read-failed", "wrong-master", "read-only", "guests", "remove-repeat", "duplicate-scope", "query-scope"} {
		t.Run(mode, func(t *testing.T) {
			h, master := calendarSeriesFixture(t)
			h.calendarUpdateEvent = func(context.Context, storage.CalendarSource, storage.CalendarEvent, calendar.EventDraft) (calendar.RemoteEvent, error) {
				t.Fatal("unsafe request reached write")
				return calendar.RemoteEvent{}, nil
			}
			form := calendarRecurringForm()
			form.Set("version", master.ETag)
			form.Set("edit_scope", "series")
			user := "one"
			switch mode {
			case "foreign":
				user = "two"
			case "stale-master":
				form.Set("version", `"old"`)
			case "occurrence-version":
				form.Set("version", `"occurrence-v1"`)
			case "read-failed":
				h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
					return calendar.RemoteEvent{}, errors.New("offline")
				}
			case "wrong-master":
				master.RemoteID = "different"
				h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
					return master, nil
				}
			case "read-only":
				if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET access_role='reader' WHERE id='one-source'`); err != nil {
					t.Fatal(err)
				}
			case "guests":
				master.Attendees = json.RawMessage(`[{"email":"guest@example.com"}]`)
				h.calendarReadSeries = func(context.Context, storage.CalendarSource, string) (calendar.RemoteEvent, error) {
					return master, nil
				}
			case "remove-repeat":
				form = calendarCreateForm()
				form.Set("version", master.ETag)
				form.Set("edit_scope", "series")
			case "duplicate-scope":
				form.Add("edit_scope", "series")
			case "query-scope":
				form.Del("edit_scope")
			}
			r := calendarUpdateRequest(form, user)
			if mode == "query-scope" {
				r.URL.RawQuery = "edit_scope=series"
			}
			w := httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, r)
			if w.Code < 400 || strings.Contains(w.Body.String(), `"uncertain":true`) {
				t.Fatalf("unsafe request: %d %s", w.Code, w.Body.String())
			}
			if _, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event"); err != nil {
				t.Fatal("failed request removed cached occurrence")
			}
		})
	}
}

func TestCalendarSeriesRepeatPrefillRoundTrips(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		for _, frequency := range []string{"daily", "weekly", "monthly", "yearly"} {
			for _, ending := range []string{"never", "until", "count"} {
				for _, graph := range []bool{false, true} {
					draft := calendarProviderDraft(t, allDay)
					draft.Recurrence = &calendar.RecurrenceDraft{Frequency: frequency, Interval: 2}
					if ending == "until" {
						draft.Recurrence.Until = "2028-10-02"
					}
					if ending == "count" {
						draft.Recurrence.Count = 12
					}
					event := calendarCreatedRemote(draft)
					if graph {
						event.Recurrence, _ = json.Marshal(calendarOutlookRecurrence(draft))
					} else {
						event.Recurrence, _ = json.Marshal([]string{"RRULE:" + calendarRecurrenceRule(draft)})
					}
					decoded, err := calendarSeriesDraft(event)
					if err != nil || !reflect.DeepEqual(decoded.Recurrence, draft.Recurrence) {
						t.Fatalf("allDay=%v %s %s graph=%v: %#v %v", allDay, frequency, ending, graph, decoded.Recurrence, err)
					}
				}
			}
		}
	}
	event := calendarCreatedRemote(calendarProviderDraft(t, false))
	for _, rule := range []string{"FREQ=WEEKLY;BYDAY=MO,WE", "FREQ=MONTHLY;BYDAY=1MO", "FREQ=DAILY;COUNT=1000", "FREQ=DAILY;INTERVAL=100", "FREQ=DAILY;COUNT=2;COUNT=3", "FREQ=DAILY;BYHOUR=8", "FREQ=WEEKLY;INTERVAL=2;WKST=SU"} {
		event.Recurrence, _ = json.Marshal([]string{"RRULE:" + rule})
		if _, err := calendarSeriesDraft(event); !errors.Is(err, errCalendarUpdateUnsupported) {
			t.Errorf("lossy prefill accepted: %s (%v)", rule, err)
		}
	}
	event.Recurrence = json.RawMessage(`["RRULE:FREQ=DAILY;UNTIL=20261003T070000Z"]`)
	decoded, err := calendarSeriesDraft(event)
	if err != nil || decoded.Recurrence.Until != "2026-10-03" {
		t.Fatalf("inclusive imported UNTIL: %#v %v", decoded.Recurrence, err)
	}
	event.Recurrence = json.RawMessage(`["RRULE:FREQ=DAILY;UNTIL=20261003T065959Z"]`)
	decoded, err = calendarSeriesDraft(event)
	if err != nil || decoded.Recurrence.Until != "2026-10-02" {
		t.Fatalf("UNTIL before start time: %#v %v", decoded.Recurrence, err)
	}
}

func TestCalendarSeriesOutlookKeepsOriginalZone(t *testing.T) {
	for _, wireZone := range []string{"UTC", "GMT Standard Time"} {
		remote := outlookCalendarUpdateEvent{ODataETag: `W/"v1"`, Type: "seriesMaster"}
		remote.ID, remote.ChangeKey = "master", "v1"
		remote.Start = outlookCalendarDateTime{DateTime: "2026-10-02T07:00:00", TimeZone: wireZone}
		remote.End = outlookCalendarDateTime{DateTime: "2026-10-02T08:00:00", TimeZone: wireZone}
		remote.Recurrence = json.RawMessage(`{"pattern":{"type":"daily","interval":1},"range":{"type":"noEnd","startDate":"2026-10-02","recurrenceTimeZone":"Central Europe Standard Time"}}`)
		event, err := calendarOutlookSeriesEvent(remote)
		if err != nil {
			t.Fatal(err)
		}
		draft, err := calendarSeriesDraft(event)
		wantHour := 9
		if wireZone == "GMT Standard Time" {
			wantHour = 8
		}
		if err != nil || draft.TimeZone != "Europe/Budapest" || calendarDraftStart(draft).Hour() != wantHour {
			t.Fatalf("lost original zone: %#v %v", draft, err)
		}
		remote.Start.TimeZone = "Unknown Custom Zone"
		if _, err := calendarOutlookSeriesEvent(remote); !errors.Is(err, errCalendarUpdateUnsupported) {
			t.Fatal("unknown timezone silently fell back to UTC")
		}
		wrongZone := calendarOutlookRecurrence(draft)
		wrongZone["range"].(map[string]any)["recurrenceTimeZone"] = "UTC"
		raw, _ := json.Marshal(wrongZone)
		if calendarRecurrenceMatchesDraft(raw, draft) {
			t.Fatal("confirmation accepted a different recurrence timezone")
		}
	}
}
