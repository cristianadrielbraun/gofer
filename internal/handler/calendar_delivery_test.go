package handler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
)

func calendarDeliveryRequest(h *Handler, method, path, user string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/calendar/events/{id}", h.handleCalendarEvent)
	mux.HandleFunc("GET /api/calendar/events/{id}/delivery", h.handleCalendarDeliveryStatus)
	mux.HandleFunc("POST /api/calendar/events/{id}/delivery/{sendID}/retry", h.handleCalendarDeliveryRetry)
	r := httptest.NewRequest(method, path, nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func queueDeliveryTestNotification(t *testing.T, h *Handler, f *incomingDAVFixture, key string) string {
	t.Helper()
	sources, err := h.db.ListSelectedCalendarSources(t.Context(), "one")
	if err != nil {
		t.Fatal(err)
	}
	cal, err := calendarUpdateDecodeICS([]byte(f.body))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.queueCalendarNotification(t.Context(), sources[0], sources[0].RemoteID+"event.ics", key, "REQUEST", cal, cal, []calendar.GuestDraft{{Email: "guest@example.com"}}, false); err != nil {
		t.Fatal(err)
	}
	deliveries, err := h.db.ListCalendarNotificationDeliveries(t.Context(), "one", "incoming-meeting")
	if err != nil || len(deliveries) == 0 {
		t.Fatalf("queued delivery=%+v %v", deliveries, err)
	}
	return deliveries[0].ID
}

func TestCalendarDeliveryStatusAndSafeRetry(t *testing.T) {
	h, f := calendarIncomingFixture(t)
	id := queueDeliveryTestNotification(t, h, f, "v0")
	endpoint := "/api/calendar/events/incoming-meeting/delivery"
	request := func(method, suffix, user string) *httptest.ResponseRecorder {
		return calendarDeliveryRequest(h, method, endpoint+suffix, user)
	}
	if w := request("GET", "", "two"); w.Code != 404 {
		t.Fatal("foreign delivery visible", w.Code)
	}
	if w := request("GET", "", "one"); w.Code != 200 || !strings.Contains(w.Body.String(), "Invitation/update queued") || !strings.Contains(w.Body.String(), "every 5s") || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("pending status %d %s", w.Code, w.Body.String())
	}
	for _, state := range []string{"sending", "sent", "ambiguous", "canceled", "failed"} {
		if _, err := h.db.Write().Exec(`UPDATE outgoing_sends SET status=? WHERE id=?`, state, id); err != nil {
			t.Fatal(err)
		}
		w := request("GET", "", "one")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `data-calendar-delivery-state="`+state+`"`) {
			t.Fatalf("missing state %s: %s", state, w.Body.String())
		}
		hasRetry := strings.Contains(w.Body.String(), "/retry")
		if hasRetry != (state == "failed") {
			t.Fatalf("unsafe retry for %s", state)
		}
		if state == "ambiguous" {
			w = request("POST", "/"+id+"/retry", "one")
			send, err := h.db.GetOutgoingSend(t.Context(), id)
			if err != nil || send.Status != "ambiguous" || !strings.Contains(w.Body.String(), "cannot be retried safely") {
				t.Fatal("ambiguous resend allowed")
			}
		}
	}
	if w := request("POST", "/"+id+"/retry", "two"); w.Code != 404 {
		t.Fatal("foreign retry allowed")
	}
	w := request("POST", "/"+id+"/retry", "one")
	send, err := h.db.GetOutgoingSend(t.Context(), id)
	if err != nil || send.Status != "pending" || w.Code != 200 || !strings.Contains(w.Body.String(), "queued") {
		t.Fatalf("retry not queued %d %+v %v", w.Code, send, err)
	}
	w = request("POST", "/"+id+"/retry", "one")
	if !strings.Contains(w.Body.String(), "cannot be retried safely") {
		t.Fatal("double-click not guarded")
	}
	var count int
	if err := h.db.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&count); err != nil || count != 1 {
		t.Fatal("retry created duplicate", count, err)
	}
	if w := request("POST", "/missing/retry", "one"); w.Code != 404 {
		t.Fatal("unknown delivery retried")
	}
	if w := calendarDeliveryRequest(h, "GET", "/api/calendar/events/incoming-meeting", "one"); w.Code != 200 || !strings.Contains(w.Body.String(), `id="calendar-event-delivery"`) {
		t.Fatal("details missing compact status", w.Code)
	}
	if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='one-source'`); err != nil {
		t.Fatal(err)
	}
	if w := request("POST", "/"+id+"/retry", "one"); w.Code != 404 {
		t.Fatal("deselected delivery retried")
	}
}

func TestCalendarDeliveryCannotExposeOrRetryUnrelatedMail(t *testing.T) {
	h, f := calendarIncomingFixture(t)
	id := queueDeliveryTestNotification(t, h, f, "v0")
	endpoint := "/api/calendar/events/incoming-meeting/delivery"
	for _, mode := range []string{"ordinary mail", "foreign snapshot", "reused resource UID"} {
		t.Run(mode, func(t *testing.T) {
			send, err := h.db.GetOutgoingSend(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, err := h.db.Write().Exec(`UPDATE outgoing_sends SET message_json=? WHERE id=?`, send.MessageJSON, id)
				if err != nil {
					t.Error(err)
				}
			}()
			var snapshot outgoingMessageSnapshot
			if err := json.Unmarshal(send.MessageJSON, &snapshot); err != nil {
				t.Fatal(err)
			}
			msg := snapshot.outgoingMessage()
			switch mode {
			case "ordinary mail":
				msg.CalendarNotification = nil
			case "foreign snapshot":
				msg.CalendarNotification.UserID = "two"
			case "reused resource UID":
				msg.CalendarNotification.Calendar = strings.ReplaceAll(msg.CalendarNotification.Calendar, f.uid, "another-meeting")
			}
			body, err := json.Marshal(snapshotOutgoingMessage(msg))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.Write().Exec(`UPDATE outgoing_sends SET message_json=?,status='failed' WHERE id=?`, body, id); err != nil {
				t.Fatal(err)
			}
			w := calendarDeliveryRequest(h, "GET", endpoint, "one")
			if w.Code != 200 || strings.Contains(w.Body.String(), "Invitation/update failed") {
				t.Fatal("unrelated send exposed")
			}
			w = calendarDeliveryRequest(h, "POST", endpoint+"/"+id+"/retry", "one")
			if w.Code != 404 {
				t.Fatal("unrelated send retried", w.Code)
			}
		})
	}
}

func TestCalendarDeliveryIncomingFeedback(t *testing.T) {
	for _, mode := range []string{"unsigned", "verification unavailable", "calendar unavailable", "success"} {
		t.Run(mode, func(t *testing.T) {
			h, f := calendarIncomingFixture(t)
			raw := incomingReplyMail(t, h, f, "ACCEPTED", time.Now().Add(-time.Minute), mode != "unsigned")
			if mode == "verification unavailable" {
				h.calendarIncomingLookupTXT = func(context.Context, string) ([]string, error) {
					return nil, &net.DNSError{IsTemporary: true, Err: "offline"}
				}
			}
			if mode == "calendar unavailable" {
				f.dropConfirmation = true
			}
			id := storeIncomingTestMail(t, h, raw)
			h.runCalendarIncomingTick(t.Context())
			w := calendarDeliveryRequest(h, "GET", "/api/calendar/events/incoming-meeting/delivery", "one")
			want := map[string]string{"unsigned": "could not be verified", "verification unavailable": "verification waiting to retry", "calendar unavailable": "reply waiting to retry", "success": "reply processed"}[mode]
			if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("feedback %s: %d %s", mode, w.Code, w.Body.String())
			}
			if mode != "success" && strings.Contains(w.Body.String(), "reply processed") {
				t.Fatal("unconfirmed reply shown as processed")
			}
			if mode == "success" && !strings.Contains(w.Body.String(), `title="Accepted"`) {
				t.Fatal("live guest badge was not refreshed")
			}
			if mode == "unsigned" {
				e, err := h.db.GetCalendarEvent(t.Context(), "one", "incoming-meeting")
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(e.AttendeesJSON, "ACCEPTED") {
					t.Fatal("unverified feedback changed RSVP")
				}
				// A newer valid reply replaces the warning for this guest.
				storeIncomingTestMail(t, h, incomingReplyMail(t, h, f, "ACCEPTED", time.Now(), true))
				h.runCalendarIncomingTick(t.Context())
				w = calendarDeliveryRequest(h, "GET", "/api/calendar/events/incoming-meeting/delivery", "one")
				if strings.Contains(w.Body.String(), "could not be verified") || !strings.Contains(w.Body.String(), "reply processed") {
					t.Fatal("old warning obscures newer result")
				}
			}
			if mode == "calendar unavailable" {
				if _, err := h.db.Write().Exec(`UPDATE calendar_incoming_messages SET next_attempt_at=datetime('now','-1 minute') WHERE message_id=?`, id); err != nil {
					t.Fatal(err)
				}
				h.runCalendarIncomingTick(t.Context())
				w = calendarDeliveryRequest(h, "GET", "/api/calendar/events/incoming-meeting/delivery", "one")
				if !strings.Contains(w.Body.String(), "reply processed") || f.writes != 1 {
					t.Fatal("recovery repeated write or lost confirmation")
				}
			}
		})
	}
}

func TestCalendarDeliveryProviderManaged(t *testing.T) {
	h, _ := calendarIncomingFixture(t)
	for _, tc := range []struct{ provider, label string }{{"gmail", "Google Calendar"}, {"outlook", "Microsoft Calendar"}} {
		if _, err := h.db.Write().Exec(`UPDATE calendar_sources SET provider=? WHERE id='one-source'`, tc.provider); err != nil {
			t.Fatal(err)
		}
		w := calendarDeliveryRequest(h, "GET", "/api/calendar/events/incoming-meeting/delivery", "one")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "managed by "+tc.label) || strings.Contains(w.Body.String(), "/retry") || strings.Contains(w.Body.String(), "Invitation/update sent") {
			t.Fatalf("provider-managed feedback: %d %s", w.Code, w.Body.String())
		}
	}
}
