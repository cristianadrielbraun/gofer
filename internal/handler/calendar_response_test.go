package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarResponseFixture(t *testing.T, recurring bool) *Handler {
	t.Helper()
	h := calendarUpdateFixture(t)
	h.calendarCreateEvent = func(context.Context, storage.CalendarSource, calendar.EventDraft) (calendar.RemoteEvent, error) {
		t.Fatal("RSVP must not create an event")
		return calendar.RemoteEvent{}, nil
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_events SET response_status = 'needsAction', organizer_email = 'host@example.com', attendees_json = '[{"email":"one@example.com","self":true,"responseStatus":"needsAction"}]' WHERE id = 'edit-event'`); err != nil {
		t.Fatal(err)
	}
	if recurring {
		if _, err := h.db.Write().Exec(`UPDATE calendar_events SET series_remote_id = 'master' WHERE id IN ('edit-event','untouched')`); err != nil {
			t.Fatal(err)
		}
	}
	h.calendarReadResponse = func(_ context.Context, _ storage.CalendarSource, cached storage.CalendarEvent, scope string) (calendarResponseTarget, error) {
		event := calendar.RemoteEvent{RemoteID: cached.RemoteID, ICalUID: cached.ICalUID, SeriesRemoteID: cached.SeriesRemoteID, ETag: cached.ETag, Summary: cached.Summary, Description: cached.Description, StartAt: cached.StartAt, EndAt: cached.EndAt, ResponseStatus: "needsAction", Attendees: json.RawMessage(cached.AttendeesJSON)}
		if scope == "series" {
			event.RemoteID, event.SeriesRemoteID, event.ETag, event.Recurrence = "master", "", `"master-v1"`, json.RawMessage(`["RRULE:FREQ=WEEKLY"]`)
		}
		return calendarResponseTarget{Event: event, Scope: scope}, nil
	}
	h.calendarSendResponse = func(_ context.Context, _ storage.CalendarSource, _ storage.CalendarEvent, target calendarResponseTarget, response string) (calendarResponseResult, error) {
		target.Event.ETag, target.Event.ResponseStatus = `"v2"`, response
		target.Event.Attendees = json.RawMessage(`[{"email":"one@example.com","self":true,"responseStatus":"` + response + `"}]`)
		return calendarResponseResult{Event: target.Event}, nil
	}
	// A failed series refresh must retain its previously cached occurrences.
	h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
		return calendar.EventPage{}, errors.New("offline fixture")
	}
	return h
}

func calendarResponseRequest(scope, version, response, user string) *http.Request {
	form := url.Values{"scope": {scope}, "version": {version}, "response": {response}}
	r := httptest.NewRequest(http.MethodPost, "/api/calendar/events/edit-event/response", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", "edit-event")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
}

func TestCalendarResponseHandlerScopesAndCache(t *testing.T) {
	for _, scope := range []string{"event", "occurrence", "series"} {
		t.Run(scope, func(t *testing.T) {
			h := calendarResponseFixture(t, scope != "event")
			version := `"v1"`
			if scope == "series" {
				version = `"master-v1"`
			}
			calls := 0
			writer := h.calendarSendResponse
			h.calendarSendResponse = func(ctx context.Context, source storage.CalendarSource, event storage.CalendarEvent, target calendarResponseTarget, response string) (calendarResponseResult, error) {
				calls++
				if source.UserID != "one" || event.ID != "edit-event" || target.Scope != scope || target.Event.ETag != version {
					t.Fatal("wrong response target")
				}
				return writer(ctx, source, event, target, response)
			}
			subscriber := h.syncer.Events().Subscribe()
			defer h.syncer.Events().Unsubscribe(subscriber)
			w := httptest.NewRecorder()
			h.handleCalendarResponse(w, calendarResponseRequest(scope, version, "accepted", "one"))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"responded":true`) || !strings.Contains(w.Body.String(), `"scope":"`+scope+`"`) || calls != 1 {
				t.Fatalf("status=%d response=%s calls=%d", w.Code, w.Body.String(), calls)
			}
			saved, err := h.db.GetCalendarEvent(t.Context(), "one", "edit-event")
			if err != nil {
				t.Fatal(err)
			}
			if scope == "series" {
				if saved.ETag != `"v1"` || saved.ResponseStatus != "needsAction" || !strings.Contains(w.Body.String(), `"refresh_pending":true`) {
					t.Fatal("series master was copied over an occurrence")
				}
			} else if saved.ETag != `"v2"` || saved.ResponseStatus != "accepted" || !strings.Contains(saved.AttendeesJSON, "accepted") || saved.Summary != "Original" {
				t.Fatalf("bad cache update: %#v", saved)
			}
			other, _ := h.db.GetCalendarEvent(t.Context(), "one", "untouched")
			if other.Summary != "Keep me" || other.ResponseStatus != "" {
				t.Fatal("reply changed another occurrence")
			}
			w = httptest.NewRecorder()
			h.handleCalendarResponse(w, calendarResponseRequest(scope, version, "accepted", "one"))
			if w.Code != 409 || calls != 1 {
				t.Fatal("response notification was sent twice")
			}
		})
	}
}

func TestCalendarResponseHandlerGuards(t *testing.T) {
	for _, mode := range []string{"foreign", "deselected", "readonly", "no-authorization", "unsupported-provider", "cancelled", "wrong-scope", "missing-scope", "missing-version", "stale", "unknown-response", "duplicate", "extra-field", "query-fields", "changed-occurrence", "not-invitation", "uncertain", "pending", "definite-failure", "wrong-result", "already-answered"} {
		t.Run(mode, func(t *testing.T) {
			h := calendarResponseFixture(t, true)
			scope, version, response, user := "occurrence", `"v1"`, "tentative", "one"
			status, writes := 409, 0
			var statement string
			switch mode {
			case "foreign":
				user, status = "two", 404
			case "deselected":
				statement, status = `UPDATE calendar_sources SET is_selected=0 WHERE id='one-source'`, 404
			case "readonly":
				statement, status = `UPDATE calendar_sources SET access_role='reader' WHERE id='one-source'`, 403
			case "no-authorization":
				h.calendarCreateEvent, status = nil, 403
			case "unsupported-provider":
				statement, status = `UPDATE calendar_sources SET provider='caldav' WHERE id='one-source'`, 403
				h.calendarCreateEvent = nil // CalDAV now supports replies, but still requires configured credentials.
			case "cancelled":
				statement, status = `UPDATE calendar_events SET status='cancelled' WHERE id='edit-event'`, 403
			case "wrong-scope":
				scope, status = "event", 400
			case "missing-scope":
				scope, status = "", 400
			case "missing-version":
				version, status = "", 400
			case "stale":
				version = `"old"`
			case "unknown-response":
				response, status = "organizer", 400
			case "duplicate", "extra-field", "query-fields":
				status = 400
			case "uncertain", "definite-failure", "wrong-result":
				status = 502
			case "pending", "already-answered":
				status = 200
			}
			if statement != "" {
				if _, err := h.db.Write().Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			reader := h.calendarReadResponse
			h.calendarReadResponse = func(ctx context.Context, source storage.CalendarSource, event storage.CalendarEvent, scope string) (calendarResponseTarget, error) {
				target, err := reader(ctx, source, event, scope)
				if mode == "not-invitation" {
					return target, errCalendarNotInvitation
				}
				if mode == "changed-occurrence" {
					target.OccurrenceVersion = `"new"`
				}
				if mode == "already-answered" {
					target.Event.ResponseStatus = "tentative"
				}
				return target, err
			}
			h.calendarSendResponse = func(_ context.Context, _ storage.CalendarSource, _ storage.CalendarEvent, target calendarResponseTarget, response string) (calendarResponseResult, error) {
				writes++
				switch mode {
				case "uncertain":
					return calendarResponseResult{}, errors.New("timeout")
				case "definite-failure":
					return calendarResponseResult{}, calendarCreateProviderError{403}
				case "pending":
					return calendarResponseResult{Pending: true}, nil
				case "wrong-result":
					target.Event.RemoteID, target.Event.ResponseStatus = "different", response
					return calendarResponseResult{Event: target.Event}, nil
				default:
					t.Fatal("unsafe write")
					return calendarResponseResult{}, nil
				}
			}
			r := calendarResponseRequest(scope, version, response, user)
			if mode == "duplicate" || mode == "extra-field" {
				_ = r.ParseForm()
				if mode == "duplicate" {
					r.PostForm["scope"] = []string{"occurrence", "series"}
				} else {
					r.PostForm.Set("attendee", "someone@example.com")
				}
			}
			if mode == "query-fields" {
				r.URL.RawQuery = "response=declined"
			}
			w := httptest.NewRecorder()
			h.handleCalendarResponse(w, r)
			if w.Code != status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
			}
			if mode == "uncertain" || mode == "pending" || mode == "wrong-result" || mode == "definite-failure" {
				if writes != 1 {
					t.Fatal("missing provider request")
				}
				w = httptest.NewRecorder()
				h.handleCalendarResponse(w, calendarResponseRequest(scope, version, response, user))
				if mode == "definite-failure" {
					if writes != 2 {
						t.Fatal("definite failure cannot retry")
					}
				} else if writes != 1 || w.Code != 409 {
					t.Fatal("ambiguous or queued response was resent")
				}
			} else if writes != 0 {
				t.Fatal("unexpected write")
			}
		})
	}
}

func TestCalendarResponseFormCacheFirstAndSeriesScope(t *testing.T) {
	h := calendarResponseFixture(t, true)
	request := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.SetPathValue("id", "edit-event")
		return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	}
	reads := 0
	reader := h.calendarReadResponse
	h.calendarReadResponse = func(ctx context.Context, source storage.CalendarSource, event storage.CalendarEvent, scope string) (calendarResponseTarget, error) {
		reads++
		return reader(ctx, source, event, scope)
	}
	w := httptest.NewRecorder()
	h.handleCalendarEvent(w, request("/api/calendar/events/edit-event"))
	if w.Code != 200 || reads != 0 || !strings.Contains(w.Body.String(), "Your response") || !strings.Contains(w.Body.String(), `data-calendar-response-ready="true"`) {
		t.Fatal("cached invitation did not render immediately")
	}
	w = httptest.NewRecorder()
	h.handleCalendarResponseForm(w, request("/api/calendar/events/edit-event/response?scope=series"))
	if w.Code != 200 || reads != 1 || !strings.Contains(w.Body.String(), "master-v1") || !strings.Contains(w.Body.String(), `name="scope" value="series"`) {
		t.Fatalf("series form: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_events SET response_status='' WHERE id='edit-event'`); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.handleCalendarEvent(w, request("/api/calendar/events/edit-event"))
	if !strings.Contains(w.Body.String(), `hx-trigger="load, click"`) || !strings.Contains(w.Body.String(), `data-calendar-response-ready="false"`) {
		t.Fatal("legacy cache must load the current response")
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_events SET response_status='organizer' WHERE id='edit-event'`); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.handleCalendarEvent(w, request("/api/calendar/events/edit-event"))
	if strings.Contains(w.Body.String(), "data-calendar-response-form") {
		t.Fatal("organizers must not see RSVP controls or a flashing response loader")
	}
}
