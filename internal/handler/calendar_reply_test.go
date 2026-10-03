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
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarReplyHandlerFixture(t *testing.T) (*Handler, *replyDAVFixture) {
	t.Helper()
	f := newReplyDAVFixture(t, "fallback", "event", "timed")
	h := calendarWorkerFixture(t)
	_, err := h.db.Write().Exec(`UPDATE accounts SET provider='imap',email_address='me@example.com',smtp_host='smtp.example.com',smtp_port=587 WHERE id='one-account'; UPDATE calendar_sources SET provider='caldav',remote_id=?,access_role='owner' WHERE id='one-source'`, f.source.RemoteID)
	if err != nil {
		t.Fatal(err)
	}
	h.accountStore, err = config.NewAccountStore(h.db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.accountStore.SaveCalDAVConfig(t.Context(), "one", "one-account", f.source.RemoteID, "user", "pass", false); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReplaceCalendarEvents(t.Context(), "one", "one-source", []storage.CalendarEvent{f.event}, *f.event.StartAt, f.event.EndAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
		cal, err := calendarUpdateDecodeICS([]byte(f.body))
		if err != nil {
			return calendar.EventPage{}, err
		}
		etag := `"v1"`
		if f.writes > 0 {
			etag = `"v2"`
		}
		event, err := calDAVResponseEvent(cal.Events()[0].Component, calendarReplyResource(f.event), etag)
		return calendar.EventPage{Events: []calendar.RemoteEvent{event}}, err
	}
	return h, f
}

func calendarReplyHTTPRequest(method, id, user string, values url.Values) *http.Request {
	r := httptest.NewRequest(method, "/api/calendar/replies/"+id, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", id)
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
}

func queueReplyThroughHandler(t *testing.T, h *Handler) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleCalendarResponse(w, calendarResponseRequest("event", `"v1"`, "accepted", "one"))
	var result struct {
		Pending, Responded bool
		Delivery           string
		ID                 string `json:"delivery_id"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Pending || result.Responded || result.Delivery != "email" || result.ID == "" {
		t.Fatalf("email reply was not queued honestly: %d %s", w.Code, w.Body.String())
	}
	return result.ID
}

func TestCalendarReplyHandlerAndRecovery(t *testing.T) {
	for _, mode := range []string{"success", "calendar-race", "calendar-unconfirmed", "identity-changed", "invitation-changed", "server-changed", "deselected", "cancel", "ambiguous", "resend", "failed-retry"} {
		t.Run(mode, func(t *testing.T) {
			h, f := calendarReplyHandlerFixture(t)
			id := queueReplyThroughHandler(t, h)
			if f.writes != 0 {
				t.Fatal("calendar changed before email delivery")
			}
			duplicate := httptest.NewRecorder()
			h.handleCalendarResponse(duplicate, calendarResponseRequest("event", `"v1"`, "declined", "one"))
			if duplicate.Code != 409 {
				t.Fatal("queued reply did not block competing response")
			}
			send, err := h.db.GetOutgoingSend(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot outgoingMessageSnapshot
			if json.Unmarshal(send.MessageJSON, &snapshot) != nil {
				t.Fatal("bad mail snapshot")
			}
			msg := snapshot.outgoingMessage()
			if msg.CalendarReply == "" || snapshotOutgoingMessage(msg).CalendarReply != snapshot.CalendarReply || len(send.EnvelopeRecipients) != 1 || send.EnvelopeRecipients[0] != "host@example.com" {
				t.Fatal("reply MIME or organizer lost from durable queue")
			}
			foreign := httptest.NewRecorder()
			h.handleCalendarReplyStatus(foreign, calendarReplyHTTPRequest("GET", id, "two", nil))
			if foreign.Code != 404 {
				t.Fatal("foreign reply exposed")
			}
			if mode == "cancel" {
				w := httptest.NewRecorder()
				h.handleCalendarReplyAction(w, calendarReplyHTTPRequest("POST", id, "one", url.Values{"action": {"cancel"}}))
				job, _ := h.db.GetCalendarReply(t.Context(), "one", id)
				if w.Code != 200 || job.State != "canceled" || job.SendStatus != "canceled" {
					t.Fatal("reply not canceled atomically")
				}
				if err := h.db.BeginCalendarResponse(t.Context(), "one", "one-source", f.event.RemoteID, `"v1"`, "declined"); err != nil {
					t.Fatal("cancel did not release unused reservation", err)
				}
				return
			}
			if mode == "identity-changed" {
				_, _ = h.db.Write().Exec(`UPDATE accounts SET email_address='different@example.com' WHERE id='one-account'`)
			}
			if mode == "invitation-changed" {
				f.writes = 1
			}
			if mode == "deselected" {
				_, _ = h.db.Write().Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='one-source'`)
			}
			if mode == "server-changed" {
				if err := h.accountStore.SaveCalDAVConfig(t.Context(), "one", "one-account", "https://different.invalid/cal/", "new-user", "new-password", false); err != nil {
					t.Fatal(err)
				}
			}
			unlock, err := h.beforeCalendarReplySend(t.Context(), send, msg)
			unlock()
			if mode == "identity-changed" || mode == "invitation-changed" || mode == "deselected" || mode == "server-changed" {
				if err == nil {
					t.Fatal("unsafe email preflight accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := h.db.ClaimDueOutgoingSends(t.Context(), time.Now().Add(time.Second), 1)
			if err != nil || len(claimed) != 1 {
				t.Fatal("reply not claimable by mail worker", err)
			}
			if mode == "ambiguous" || mode == "resend" || mode == "failed-retry" {
				status := "ambiguous"
				action := "confirm-sent"
				if mode == "failed-retry" {
					status = "failed"
					action = "retry"
				}
				if mode == "resend" {
					action = "resend-confirmed"
				}
				if err := h.db.FinishOutgoingSendWithError(t.Context(), id, status, "fixture"); err != nil {
					t.Fatal(err)
				}
				h.runCalendarReplyFollowups(t.Context())
				if f.writes != 0 {
					t.Fatal("unconfirmed email changed calendar")
				}
				if status == "ambiguous" {
					w := httptest.NewRecorder()
					h.handleCalendarReplyAction(w, calendarReplyHTTPRequest("POST", id, "one", url.Values{"action": {"retry"}}))
					if w.Code != 409 {
						t.Fatal("ambiguous send retried without explicit warning")
					}
				}
				w := httptest.NewRecorder()
				h.handleCalendarReplyAction(w, calendarReplyHTTPRequest("POST", id, "one", url.Values{"action": {action}}))
				if w.Code != 200 {
					t.Fatalf("explicit recovery failed: %d %s", w.Code, w.Body.String())
				}
				if mode != "ambiguous" {
					job, _ := h.db.GetCalendarReply(t.Context(), "one", id)
					if job.SendStatus != "pending" || f.writes != 0 {
						t.Fatal("retry sent prematurely")
					}
					return
				}
			} else if err := h.db.CompleteOutgoingSend(t.Context(), id, msg.MessageID, false); err != nil {
				t.Fatal(err)
			}
			if mode == "calendar-race" {
				f.writes = 1
				f.body = strings.Replace(f.body, "SUMMARY:Meeting", "SUMMARY:Changed", 1)
			}
			if mode == "calendar-unconfirmed" {
				// Simulate a successful PUT followed by process interruption before
				// its acknowledgement was committed. Recovery must only read back.
				job, _ := h.db.GetCalendarReply(t.Context(), "one", id)
				var payload calendarReplyPayload
				_ = json.Unmarshal([]byte(job.Payload), &payload)
				f.body = payload.Calendar
				f.writes = 1
			}
			h.runCalendarReplyFollowups(t.Context())
			job, err := h.db.GetCalendarReply(t.Context(), "one", id)
			if err != nil {
				t.Fatal(err)
			}
			want := "complete"
			if mode == "calendar-race" {
				want = "conflict"
			}
			if job.State != want || f.writes != 1 {
				t.Fatalf("unsafe follow-up: %+v writes=%d", job, f.writes)
			}
			h.runCalendarReplyFollowups(t.Context())
			if f.writes != 1 {
				t.Fatal("recovery repeated a finished write")
			}
			var count int
			_ = h.db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&count)
			if count != 1 {
				t.Fatal("calendar save queued another email")
			}
			if mode == "calendar-race" {
				notice, err := h.db.UnresolvedCalendarReply(t.Context(), "one", "one-source", f.event.RemoteID)
				if err != nil || notice.State != "conflict" {
					t.Fatal("save failure lost after closing dialog")
				}
			}
		})
	}
}
