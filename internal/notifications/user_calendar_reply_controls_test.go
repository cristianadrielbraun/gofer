package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedOwnedReplyStatus(t *testing.T, f *userStorageFixture) map[string]string {
	t.Helper()
	seedUserCalendarViews(t, f)
	ids := map[string]string{}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			payload, err := json.Marshal(map[string]any{"Event": storage.CalendarEvent{ID: owner + "-private-event"}, "Scope": "event", "Calendar": owner + "-private-ICS"})
			if err != nil {
				return err
			}
			ids[owner], err = db.QueueCalendarReply(t.Context(), storage.CalendarReplyJob{UserID: owner, SourceID: "same-calendar-id", ResourceID: "/primary/invitation.ics", RemoteID: "invitation.ics", Version: `"v1"`, Response: "accepted", Payload: string(payload)}, storage.QueueOutgoingSendInput{AccountID: f.accounts[owner].ID, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: owner + "@example.com", EnvelopeRecipients: []string{"organizer@example.com"}, MIMEData: []byte(owner + "-private-MIME"), MessageJSON: []byte(`{"CalendarReply":"private reply"}`)})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func TestUserCalendarReplyStatusHTTPOwnerIsolationAndReconfiguredHistory(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	ids := seedOwnedReplyStatus(t, f)
	for _, owner := range []string{"alice", "bob"} {
		other := "alice"
		if owner == "alice" {
			other = "bob"
		}
		for _, pair := range []struct{ state, status string }{{"pending", "pending"}, {"pending", "failed"}, {"pending", "ambiguous"}, {"pending", "sent"}, {"conflict", "sent"}, {"complete", "sent"}, {"canceled", "canceled"}, {"dismissed", "sent"}} {
			if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE outgoing_sends SET status=? WHERE id=?`, pair.status, ids[owner]); err != nil {
					return err
				}
				_, err := db.Write().Exec(`UPDATE calendar_reply_jobs SET state=? WHERE id=?`, pair.state, ids[owner])
				return err
			}); err != nil {
				t.Fatal(err)
			}
			r := f.request(owner, "GET", "/api/calendar/replies/"+ids[owner], "")
			if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(r.Header().Get("Content-Type"), "text/html") || strings.Contains(r.Body.String(), "private-MIME") || strings.Contains(r.Body.String(), "private-ICS") || strings.Contains(r.Body.String(), other+"-private-event") {
				t.Fatal("reply status isolation", owner, pair, r.Code, r.Body.String())
			}
		}
		if r := f.request(other, "GET", "/api/calendar/replies/"+ids[owner], ""); r.Code != 404 {
			t.Fatal("foreign reply read", r.Code)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			if _, err := db.Write().Exec(`UPDATE accounts SET smtp_host='broken'`); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`UPDATE calendar_sources SET is_selected=0,is_deleted=1 WHERE id='same-calendar-id'`); err != nil {
				return err
			}
			_, err := db.Write().Exec(`UPDATE calendar_reply_jobs SET state='pending' WHERE id=?`, ids[owner])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := f.request(owner, "GET", "/api/calendar/replies/"+ids[owner], ""); r.Code != 200 {
			t.Fatal("reconfiguration hid history", r.Code, r.Body.String())
		}
	}
	for _, table := range []string{"calendar_reply_jobs", "outgoing_sends"} {
		var n int
		if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("shared reply fallback", table, n, err)
		}
	}
	if r := f.request("alice", "GET", "/api/calendar/replies/missing", ""); r.Code != 404 {
		t.Fatal(r.Code)
	}
	request := httptest.NewRequest("GET", "/api/calendar/replies/"+ids["alice"], nil)
	response := httptest.NewRecorder()
	f.http.ServeHTTP(response, request)
	if response.Code == 200 || strings.Contains(response.Body.String(), "alice-private-event") {
		t.Fatal("unauthenticated history", response.Code)
	}
}

func TestUserCalendarReplyStatusHTTPRenderReleasesDatabaseLease(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	ids := seedOwnedReplyStatus(t, f)
	writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(writer.release) }) }
	defer release()
	request := httptest.NewRequest("GET", "/api/calendar/replies/"+ids["alice"], nil).WithContext(t.Context())
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { f.http.ServeHTTP(writer, request); close(done) }()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not begin")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetCalendarReply(ctx, "bob", ids["bob"]); return err }); err != nil {
		t.Fatal("reply render retained sole store slot", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not end")
	}
	if writer.Code != 200 || strings.Contains(writer.Body.String(), "bob-private-event") {
		t.Fatal(writer.Code, writer.Body.String())
	}
}

func TestUserCalendarReplyStatusHTTPRejectsStoppedRoot(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	ids := seedOwnedReplyStatus(t, f)
	f.stopIMAP()
	f.imap.Wait()
	if r := f.request("alice", "GET", "/api/calendar/replies/"+ids["alice"], ""); r.Code != 404 || strings.Contains(r.Body.String(), "alice-private-event") {
		t.Fatal("stopped root served private reply", r.Code, r.Body.String())
	}
}

func TestUserCalendarReplyActionHTTPLocalRecoveryAndOwnership(t *testing.T) {
	for _, action := range []string{"cancel", "confirm-sent", "dismiss"} {
		t.Run(action, func(t *testing.T) {
			f := newUserStorageFixtureMode(t, true)
			ids := seedOwnedReplyStatus(t, f)
			state, status := "pending", "pending"
			if action == "confirm-sent" {
				status = "ambiguous"
			}
			if action == "dismiss" {
				state, status = "conflict", "sent"
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE outgoing_sends SET status=? WHERE id=?`, status, ids["alice"]); err != nil {
					return err
				}
				if _, err := db.Write().Exec(`UPDATE calendar_reply_jobs SET state=? WHERE id=?`, state, ids["alice"]); err != nil {
					return err
				}
				_, err := db.Write().Exec(`UPDATE calendar_sources SET is_selected=0,is_deleted=1 WHERE id='same-calendar-id'; UPDATE accounts SET smtp_host='broken'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if r := f.request("bob", "POST", "/api/calendar/replies/"+ids["alice"], "action="+action); r.Code != 404 {
				t.Fatal("foreign recovery", r.Code, r.Body.String())
			}
			r := f.request("alice", "POST", "/api/calendar/replies/"+ids["alice"], "action="+action)
			if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(r.Body.String(), "Check its current state") || strings.Contains(r.Body.String(), "bob-private-event") {
				t.Fatal("local recovery HTTP", action, r.Code, r.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				job, err := db.GetCalendarReply(t.Context(), "alice", ids["alice"])
				if err != nil {
					return err
				}
				wantState, wantStatus := map[string]string{"cancel": "canceled", "confirm-sent": "pending", "dismiss": "dismissed"}[action], map[string]string{"cancel": "canceled", "confirm-sent": "sent", "dismiss": "sent"}[action]
				if job.State != wantState || job.SendStatus != wantStatus {
					t.Fatal("local action changed result", job)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyActionHTTPStrictFormsAndRetryConfirmation(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	ids := seedOwnedReplyStatus(t, f)
	path := "/api/calendar/replies/" + ids["alice"]
	for _, body := range []string{"", "action=", "action=unknown", "action=cancel&action=cancel", "action=cancel&extra=1", "action=" + strings.Repeat("x", 2000)} {
		if r := f.request("alice", "POST", path, body); r.Code != 400 {
			t.Fatal("malformed reply action", body[:min(len(body), 80)], r.Code, r.Body.String())
		}
	}
	if r := f.request("alice", "POST", path+"?action=cancel", "action=cancel"); r.Code != 400 {
		t.Fatal("query action accepted", r.Code)
	}
	for _, action := range []string{"retry", "resend-confirmed", "dismiss"} {
		if r := f.request("alice", "POST", path, "action="+action); r.Code != 409 {
			t.Fatal("invalid state accepted", action, r.Code)
		}
	}
	for _, status := range []string{"failed", "ambiguous"} {
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE outgoing_sends SET status=? WHERE id=?`, status, ids["alice"])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		wrong, right := "resend-confirmed", "retry"
		if status == "ambiguous" {
			wrong, right = "retry", "resend-confirmed"
		}
		if r := f.request("alice", "POST", path, "action="+wrong); r.Code != 409 {
			t.Fatal("wrong confirmation accepted", status, r.Code)
		}
		// Missing legacy authority is visible and cancelable, but cannot silently
		// become a new provider grant through the retry endpoint.
		if r := f.request("alice", "POST", path, "action="+right); r.Code != 200 || !strings.Contains(r.Body.String(), "Check its current state") {
			t.Fatal("historical proof adopted", status, r.Code, r.Body.String())
		}
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			job, err := db.GetCalendarReply(t.Context(), "alice", ids["alice"])
			if err == nil && job.SendStatus != status {
				t.Fatal("legacy delivery requeued", job)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
