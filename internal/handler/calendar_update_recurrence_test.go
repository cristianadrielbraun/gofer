package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestCalendarUpdateConvertsSingleEventAndRefreshesInstances(t *testing.T) {
	for _, refreshFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "refreshed", true: "refresh failed"}[refreshFails], func(t *testing.T) {
			h := calendarUpdateFixture(t)
			writes, reads := 0, 0
			form := calendarRecurringForm()
			draft, _ := parseCalendarEventDraft(calendarCreateHTTPRequest(form, "one"))
			untouched, _ := h.db.GetCalendarEvent(t.Context(), "one", "untouched")
			h.calendarUpdateEvent = func(_ context.Context, source storage.CalendarSource, existing storage.CalendarEvent, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
				writes++
				if existing.RemoteID != "remote-event" || existing.ETag != `"v1"` || source.ID != "one-source" || draft.Recurrence == nil {
					t.Fatal("conversion lost its original target/version or repeat settings")
				}
				remote := calendarCreatedRemote(draft)
				remote.RemoteID, remote.ETag, remote.ICalUID = existing.RemoteID, `"v2"`, existing.ICalUID
				remote.Recurrence, _ = json.Marshal([]string{"RRULE:" + calendarRecurrenceRule(draft)})
				return remote, nil
			}
			h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
				t.Fatal("conversion must not create a second event")
				return calendar.RemoteEvent{}, nil
			}
			h.calendarFetchEvents = func(_ context.Context, source storage.CalendarSource, query calendar.EventQuery) (calendar.EventPage, error) {
				reads++
				if source.ID != "one-source" || source.UserID != "one" || !query.WindowStart.Before(*draft.StartAt) || !query.WindowEnd.After(*draft.EndAt) {
					t.Fatal("conversion refreshed the wrong source/window")
				}
				if _, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("converted master remained visible while loading occurrences")
				}
				if refreshFails {
					return calendar.EventPage{}, errors.New("temporary read failure")
				}
				instance := calendarCreatedRemote(draft)
				instance.RemoteID, instance.SeriesRemoteID, instance.ETag = "provider-instance", "remote-event", `"i1"`
				keep := calendar.RemoteEvent{RemoteID: untouched.RemoteID, Summary: untouched.Summary, StartAt: untouched.StartAt, EndAt: untouched.EndAt}
				return calendar.EventPage{Events: []calendar.RemoteEvent{instance, keep}}, nil
			}
			w := httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, calendarUpdateRequest(form, "one"))
			var result struct {
				Saved          bool   `json:"saved"`
				SeriesID       string `json:"series_id"`
				EventID        string `json:"event_id"`
				RefreshPending bool   `json:"refresh_pending"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || !result.Saved || result.SeriesID != "remote-event" || result.EventID != "edit-event" || result.RefreshPending != refreshFails {
				t.Fatalf("conversion: %d %s (%v)", w.Code, w.Body.String(), err)
			}
			if writes != 1 || reads != 1 {
				t.Fatalf("writes=%d reads=%d", writes, reads)
			}
			events, err := h.db.ListCalendarEvents(t.Context(), "one", draft.StartAt.Add(-time.Hour), draft.EndAt.Add(time.Hour))
			want := 2
			if refreshFails {
				want = 1
			}
			if err != nil || len(events) != want {
				t.Fatalf("duplicate master or lost unrelated event: %v %v", events, err)
			}
			for _, event := range events {
				if event.RemoteID == "remote-event" {
					t.Fatal("master rendered as a duplicate appointment")
				}
			}
			var version, recurrence, uid string
			var deleted int
			if err := h.db.Read().QueryRow(`SELECT etag,recurrence_json,ical_uid,is_deleted FROM calendar_events WHERE id='edit-event'`).Scan(&version, &recurrence, &uid, &deleted); err != nil || version != `"v2"` || !calendarUpdateHasDetails(json.RawMessage(recurrence)) || uid != "keep-uid" || deleted != 1 {
				t.Fatalf("conversion not durably recorded: %s %s %s %d %v", version, recurrence, uid, deleted, err)
			}
			w = httptest.NewRecorder()
			h.handleUpdateCalendarEvent(w, calendarUpdateRequest(form, "one"))
			if w.Code != 404 || writes != 1 {
				t.Fatal("replayed conversion made another provider write")
			}
		})
	}
}

func TestCalendarUpdateConversionRequiresConfirmedRecurrence(t *testing.T) {
	h := calendarUpdateFixture(t) // This writer returns a single event, no recurrence.
	w := httptest.NewRecorder()
	h.handleUpdateCalendarEvent(w, calendarUpdateRequest(calendarRecurringForm(), "one"))
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"uncertain":true`) {
		t.Fatalf("ignored recurrence reported as saved: %d %s", w.Code, w.Body.String())
	}
	event, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
	if err != nil || event.ETag != `"v1"` || event.IsDeleted {
		t.Fatal("unconfirmed conversion changed the cache")
	}
}

func TestCalendarUpdateRecurrenceConfirmationNormalizesRules(t *testing.T) {
	draft, _ := parseCalendarEventDraft(calendarCreateHTTPRequest(calendarRecurringForm(), "one"))
	for _, rule := range []string{"FREQ=WEEKLY;INTERVAL=1;COUNT=4;WKST=MO;BYDAY=FR", "COUNT=4;FREQ=WEEKLY", "BYDAY=FR;COUNT=4;FREQ=WEEKLY"} {
		if !calendarRecurrenceRuleMatches(rule, draft) {
			t.Errorf("reordered/defaulted rule rejected: %s", rule)
		}
	}
	for _, rule := range []string{"FREQ=DAILY;COUNT=4", "FREQ=WEEKLY;COUNT=3", "FREQ=WEEKLY;COUNT=4;COUNT=99", "FREQ=WEEKLY;COUNT=4;BYDAY=MO", "FREQ=WEEKLY;COUNT=4;BYHOUR=10"} {
		if calendarRecurrenceRuleMatches(rule, draft) {
			t.Errorf("different/unsafe rule accepted: %s", rule)
		}
	}
	raw, _ := json.Marshal(calendarOutlookRecurrence(draft))
	if !calendarRecurrenceMatchesDraft(raw, draft) {
		t.Fatal("Graph rule did not match its draft")
	}
	if calendarRecurrenceMatchesDraft(json.RawMessage(`["RRULE:FREQ=WEEKLY;COUNT=4","EXDATE:20261009"]`), draft) {
		t.Fatal("unrequested recurrence exception accepted")
	}
}
