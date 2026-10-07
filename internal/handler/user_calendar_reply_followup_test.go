package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedReplyFollowupAPI struct {
	t                *testing.T
	mu               sync.Mutex
	body, etag, mode string
	gets, puts       int
	before           func(*http.Request)
}

func newOwnedReplyFollowupAPI(t *testing.T, mode string) *ownedReplyFollowupAPI {
	return &ownedReplyFollowupAPI{t: t, mode: mode, etag: `"v1"`, body: "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Fixture//EN\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:Native invitation\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"}
}
func (a *ownedReplyFollowupAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" && r.URL.Path == "/alice/primary/" {
		w.WriteHeader(204)
		return
	}
	if r.Method == "REPORT" && r.URL.Path == "/alice/primary/" {
		a.mu.Lock()
		body, etag := a.body, a.etag
		a.mu.Unlock()
		var escaped bytes.Buffer
		if err := xml.EscapeText(&escaped, []byte(body)); err != nil {
			a.t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(207)
		_, _ = fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>/alice/primary/invitation.ics</d:href><d:propstat><d:prop><d:getetag>%s</d:getetag><c:calendar-data>%s</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, etag, escaped.String())
		return
	}
	if r.URL.Path != "/alice/primary/invitation.ics" {
		a.t.Error("foreign followup resource", r.URL.Path)
		http.Error(w, "foreign", 400)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch r.Method {
	case "GET":
		a.gets++
		if a.before != nil {
			a.before(r)
		}
		if r.Context().Err() != nil {
			return
		}
		if a.puts > 0 && a.mode == "readback-failure" {
			a.mode = "success"
			http.Error(w, "temporary readback", 503)
			return
		}
		if a.puts > 0 && a.mode == "malformed-readback" {
			a.mode = "success"
			_, _ = io.WriteString(w, "invalid ICS")
			return
		}
		w.Header().Set("Content-Type", "text/calendar")
		w.Header().Set("ETag", a.etag)
		w.Header().Set("Schedule-Tag", `"s1"`)
		_, _ = io.WriteString(w, a.body)
	case "PUT":
		a.puts++
		if r.Header.Get("If-Match") != `"v1"` || r.Header.Get("If-Schedule-Tag-Match") != `"s1"` {
			a.t.Error("missing original conditional write", r.Header)
			http.Error(w, "bad precondition", 400)
			return
		}
		if a.mode == "rejected" {
			http.Error(w, "concurrent change", 412)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			a.t.Error(err)
			return
		}
		if !strings.Contains(string(data), "PARTSTAT=ACCEPTED") || !strings.Contains(string(data), "SCHEDULE-AGENT=CLIENT") {
			a.t.Error("accepted reply or duplicate-scheduling prevention lost", string(data))
			http.Error(w, "wrong body", 400)
			return
		}
		a.body, a.etag = string(data), `"v2"`
		if a.mode == "lost-put-ack" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				a.t.Error(err)
				return
			}
			_ = conn.Close()
			a.mode = "success"
			return
		}
		w.WriteHeader(204)
	default:
		a.t.Error("unexpected followup method", r.Method)
		http.Error(w, "unexpected", 400)
	}
}
func markOwnedReplyAccepted(t *testing.T, f *userContactPushFixture, id string) {
	t.Helper()
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		sends, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), f.accounts["alice"].ID, time.Now().Add(time.Second), 1)
		if err == nil && (len(sends) != 1 || sends[0].ID != id) {
			t.Fatal("wrong accepted fixture attempt")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.h.userAccounts.SnapshotCalendarReplyDelivery(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	var message outgoingMessageSnapshot
	if err := json.Unmarshal(snapshot.Send().MessageJSON, &message); err != nil {
		t.Fatal(err)
	}
	if err := f.h.userAccounts.FinishCalendarReplySend(t.Context(), snapshot, storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: message.MessageID}); err != nil {
		t.Fatal(err)
	}
}
func ownedReplyJob(t *testing.T, f *userContactPushFixture, id string) storage.CalendarReplyJob {
	t.Helper()
	var job storage.CalendarReplyJob
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		job, err = db.GetCalendarReply(t.Context(), "alice", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestUserCalendarReplyFollowupNativeReadConditionalPUTAndRecovery(t *testing.T) {
	for _, mode := range []string{"success", "already-saved", "version-conflict", "rejected", "lost-put-ack", "readback-failure", "malformed-readback"} {
		t.Run(mode, func(t *testing.T) {
			api := newOwnedReplyFollowupAPI(t, mode)
			f, id, _ := ownedReplyDeliveryFixture(t, "127.0.0.1", 25251, nil, api)
			markOwnedReplyAccepted(t, f, id)
			if mode == "already-saved" || mode == "version-conflict" {
				job := ownedReplyJob(t, f, id)
				var payload calendarReplyPayload
				if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				api.etag = `"v2"`
				if mode == "already-saved" {
					api.body = payload.Calendar
				}
				api.mu.Unlock()
			}
			// Independent Sent-copy cleanup and event cache refresh cannot turn this
			// accepted email back into send work, or strand its Calendar-only save.
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE outgoing_sends SET mime_data=NULL,message_json='',envelope_recipients='[]'; UPDATE calendar_events SET summary='refreshed'; UPDATE calendar_sources SET name='renamed',color='#123456'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			changed, err := f.h.finishUserCalendarReply(t.Context(), "alice", f.accounts["alice"].ID, id)
			uncertain := mode == "lost-put-ack" || mode == "readback-failure" || mode == "malformed-readback"
			if uncertain {
				if changed || err == nil {
					t.Fatal("uncertain save was finalized", changed, err)
				}
				if job := ownedReplyJob(t, f, id); job.State != "pending" || job.SendStatus != storage.OutgoingSendSent {
					t.Fatal("accepted email was recycled", job)
				}
				changed, err = f.h.finishUserCalendarReply(t.Context(), "alice", f.accounts["alice"].ID, id)
			}
			if !changed || err != nil {
				t.Fatal("followup outcome", changed, err)
			}
			want := "complete"
			if mode == "version-conflict" || mode == "rejected" {
				want = "conflict"
			}
			if job := ownedReplyJob(t, f, id); job.State != want || job.SendStatus != storage.OutgoingSendSent {
				t.Fatal("durable followup result", job)
			}
			api.mu.Lock()
			puts := api.puts
			api.mu.Unlock()
			if (mode == "already-saved" || mode == "version-conflict") && puts != 0 {
				t.Fatal("unnecessary PUT", puts)
			}
			if mode != "already-saved" && mode != "version-conflict" && puts != 1 {
				t.Fatal("duplicate conditional PUT", puts)
			}
			if more, err := ownedReplyDeliveryScope(t, f).send(t.Context()); more || err != nil {
				t.Fatal("followup sent another email", more, err)
			}
			if _, err := f.h.finishUserCalendarReply(t.Context(), "bob", f.accounts["bob"].ID, id); err == nil {
				t.Fatal("foreign owner followed up")
			}
		})
	}
}

func TestUserCalendarReplyFollowupRealSMTPAcceptanceThenNativeSave(t *testing.T) {
	host, port, smtpCalls, done := ownedReplySMTPServer(t, "250 accepted", nil)
	api := newOwnedReplyFollowupAPI(t, "success")
	f, id, _ := ownedReplyDeliveryFixture(t, host, port, nil, api)
	if more, err := ownedReplyDeliveryScope(t, f).send(t.Context()); !more || err != nil {
		t.Fatal("SMTP acceptance", more, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SMTP did not finish")
	}
	if changed, err := f.h.finishUserCalendarReply(t.Context(), "alice", f.accounts["alice"].ID, id); !changed || err != nil {
		t.Fatal("accepted reply save", changed, err)
	}
	if smtpCalls.Load() != 1 || ownedReplyJob(t, f, id).State != "complete" {
		t.Fatal("SMTP/followup outcome")
	}
}

func TestUserCalendarReplyFollowupRechecksGenerationAndConfigurationDuringHTTP(t *testing.T) {
	for _, mode := range []string{"generation", "smtp", "calendar", "nonce", "eviction"} {
		t.Run(mode, func(t *testing.T) {
			api := newOwnedReplyFollowupAPI(t, "success")
			f, id, _ := ownedReplyDeliveryFixture(t, "127.0.0.1", 25251, nil, api)
			markOwnedReplyAccepted(t, f, id)
			var calls atomic.Int32
			api.mu.Lock()
			api.before = func(_ *http.Request) {
				if calls.Add(1) != 1 {
					return
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "bob", func(_ *config.AccountStore, _ *storage.DB) error { return nil }); err != nil {
					t.Error(err)
				}
				if mode == "eviction" {
					return
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					query := map[string]string{"generation": `UPDATE calendar_reply_jobs SET attempted_at=?`, "smtp": `UPDATE accounts SET smtp_host='repaired'`, "calendar": `UPDATE account_caldav_configs SET encrypted_password=x'0102'`, "nonce": `UPDATE calendar_response_requests SET claim_id='replacement'`}[mode]
					var err error
					if mode == "generation" {
						_, err = db.Write().Exec(query, time.Now().Add(time.Hour))
					} else {
						_, err = db.Write().Exec(query)
					}
					return err
				}); err != nil {
					t.Error(err)
				}
			}
			api.mu.Unlock()
			changed, err := f.h.finishUserCalendarReply(t.Context(), "alice", f.accounts["alice"].ID, id)
			if mode == "eviction" {
				if !changed || err != nil {
					t.Fatal("eviction broke followup", changed, err)
				}
				return
			}
			if changed || err == nil {
				t.Fatal("stale HTTP result published", changed, err)
			}
			if job := ownedReplyJob(t, f, id); job.State != "pending" || job.SendStatus != storage.OutgoingSendSent {
				t.Fatal("failed guard recycled accepted mail", job)
			}
			changed, err = f.h.finishUserCalendarReply(t.Context(), "alice", f.accounts["alice"].ID, id)
			if mode == "calendar" || mode == "nonce" {
				if !changed || err == nil || ownedReplyJob(t, f, id).State != "conflict" {
					t.Fatal("stale authority was not retired", changed, err)
				}
			} else if !changed || err != nil || ownedReplyJob(t, f, id).State != "complete" {
				t.Fatal("safe fresh attempt did not recover", changed, err)
			}
		})
	}
}

func TestUserCalendarReplyFollowupRootCancellationKeepsAcceptedMailPending(t *testing.T) {
	api := newOwnedReplyFollowupAPI(t, "success")
	f, id, _ := ownedReplyDeliveryFixture(t, "127.0.0.1", 25251, nil, api)
	markOwnedReplyAccepted(t, f, id)
	api.mu.Lock()
	api.before = func(r *http.Request) {
		f.cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("Calendar HTTP was not canceled")
		}
	}
	api.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	changed, err := f.h.finishUserCalendarReply(ctx, "alice", f.accounts["alice"].ID, id)
	if changed || !errors.Is(err, context.Canceled) {
		t.Fatal("root cancellation outcome", changed, err)
	}
	job := ownedReplyJob(t, f, id)
	if job.State != "pending" || job.SendStatus != storage.OutgoingSendSent {
		t.Fatal("cancellation recycled accepted email", job)
	}
}

func TestUserCalendarReplyFollowupBackgroundWorkerRecoversWithoutSecondEmail(t *testing.T) {
	host, port, smtpCalls, done := ownedReplySMTPServer(t, "250 accepted", nil)
	api := newOwnedReplyFollowupAPI(t, "readback-failure")
	f, id, _ := ownedReplyDeliveryFixture(t, host, port, nil, api)
	if err := f.h.userAccounts.SetCalendarServiceEnabled(t.Context(), "bob", f.accounts["bob"].ID, false); err != nil {
		t.Fatal(err)
	}
	if more, err := ownedReplyDeliveryScope(t, f).send(t.Context()); !more || err != nil {
		t.Fatal(more, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SMTP did not finish")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := f.h.StartUserCalendarSync(ctx, UserCalendarSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		job := ownedReplyJob(t, f, id)
		if job.State == "complete" {
			break
		}
		if job.State != "pending" || job.SendStatus != storage.OutgoingSendSent {
			t.Fatal("worker lost acceptance", job)
		}
		if time.Now().After(deadline) {
			t.Fatal("background followup did not recover", job)
		}
		time.Sleep(20 * time.Millisecond)
	}
	api.mu.Lock()
	puts := api.puts
	api.mu.Unlock()
	if puts != 1 || smtpCalls.Load() != 1 {
		t.Fatal("background recovery repeated a write/email", puts, smtpCalls.Load())
	}
	cancel()
	f.cancel()
	f.h.userIMAP.Wait()
}
