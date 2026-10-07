package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarReplyRetryNativePreflightAndOriginalSMTPMessage(t *testing.T) {
	host, port, calls, done := ownedReplySMTPServer(t, "250 accepted", nil)
	f, id, davCalls := ownedReplyDeliveryFixture(t, host, port, nil)
	var before storage.OutgoingSend
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(local *config.AccountStore, db *storage.DB) error {
		if _, err := db.Write().Exec(`UPDATE outgoing_sends SET status='failed',attempt_count=1,last_error='SMTP rejected' WHERE id=?`, id); err != nil {
			return err
		}
		var err error
		before, err = db.GetOutgoingSend(t.Context(), id)
		if err != nil {
			return err
		}
		if _, err := db.Write().Exec(`UPDATE accounts SET smtp_username='repaired-smtp-user'`); err != nil {
			return err
		}
		profile, err := f.h.userAccounts.SnapshotServices(t.Context(), "alice", f.accounts["alice"].ID)
		if err != nil {
			return err
		}
		base, username, useAccount := profile.CalDAVSettings()
		return local.SaveCalDAVConfig(t.Context(), "alice", f.accounts["alice"].ID, base, username, "alice-calendar-secret", useAccount)
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/api/calendar/replies/"+id, strings.NewReader("action=retry"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetPathValue("id", id)
	request = request.WithContext(auth.ContextWithUser(t.Context(), &auth.User{ID: "alice"}))
	response := httptest.NewRecorder()
	f.h.handleUserCalendarReplyAction(response, request)
	if response.Code != 200 || response.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(response.Body.String(), "Check its current state") {
		t.Fatal("native retry HTTP action", response.Code, response.Body.String())
	}
	if calls.Load() != 0 || davCalls.Load() != 2 {
		t.Fatal("retry preflight dispatched mail", calls.Load(), davCalls.Load())
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		after, err := db.GetOutgoingSend(t.Context(), id)
		if err == nil && (!bytes.Equal(before.MIMEData, after.MIMEData) || !bytes.Equal(before.MessageJSON, after.MessageJSON) || after.AttemptCount != 1 || after.Status != storage.OutgoingSendPending) {
			t.Fatal("original message changed", after)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if more, err := ownedReplyDeliveryScope(t, f).send(t.Context()); !more || err != nil {
		t.Fatal("renewed reply did not send", more, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SMTP did not close")
	}
	if calls.Load() != 1 || davCalls.Load() != 3 {
		t.Fatal("wrong native dispatch", calls.Load(), davCalls.Load())
	}
	if job := ownedReplyJob(t, f, id); job.SendStatus != storage.OutgoingSendSent {
		t.Fatal(job)
	}
}

func TestUserCalendarReplyRetryConfirmedResendAfterRealLostSMTPAcknowledgement(t *testing.T) {
	var firstMessage, secondMessage string
	host, port, firstCalls, firstDone := ownedReplySMTPServer(t, "drop", func(body string) { firstMessage = body })
	f, id, davCalls := ownedReplyDeliveryFixture(t, host, port, nil)
	scope := ownedReplyDeliveryScope(t, f)
	if more, err := scope.send(t.Context()); !more || err == nil {
		t.Fatal("lost acknowledgement not ambiguous", more, err)
	}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first SMTP did not close")
	}
	if job := ownedReplyJob(t, f, id); job.SendStatus != storage.OutgoingSendAmbiguous {
		t.Fatal(job)
	}
	if more, err := scope.send(t.Context()); more || err != nil {
		t.Fatal("ambiguous send automatically retried", more, err)
	}
	control, err := f.h.userAccounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.h.retryUserCalendarReply(t.Context(), control, false); err == nil {
		t.Fatal("resend did not require confirmation")
	}
	host, port, secondCalls, secondDone := ownedReplySMTPServer(t, "250 accepted", func(body string) { secondMessage = body })
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", host, port, "repaired fixture"); err != nil {
		t.Fatal(err)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET smtp_host=?,smtp_port=? WHERE id=?`, host, port, f.accounts["alice"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.userIMAP.RunUserServiceWork(t.Context(), "alice", func(ctx context.Context) error { return f.h.retryUserCalendarReply(ctx, control, true) }); err != nil {
		t.Fatal(err)
	}
	if more, err := ownedReplyDeliveryScope(t, f).send(t.Context()); !more || err != nil {
		t.Fatal("confirmed resend failed", more, err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second SMTP did not close")
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 || firstMessage == "" || firstMessage != secondMessage || davCalls.Load() != 4 {
		t.Fatal("confirmed resend changed message or dispatch count", firstCalls.Load(), secondCalls.Load(), davCalls.Load())
	}
}

func TestUserCalendarReplyRetryNativeRejectsChangedInvitationAndInFlightAuthority(t *testing.T) {
	for _, mode := range []string{"version", "uid", "organizer", "self", "attempt", "nonce", "calendar", "smtp"} {
		t.Run(mode, func(t *testing.T) {
			api := newOwnedReplyFollowupAPI(t, "success")
			f, id, _ := ownedReplyDeliveryFixture(t, "127.0.0.1", 1, nil, api)
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE outgoing_sends SET status='failed',attempt_count=1 WHERE id=?`, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			control, err := f.h.userAccounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
			if err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			switch mode {
			case "version":
				api.etag = `"v2"`
			case "uid":
				api.body = strings.ReplaceAll(api.body, "UID:private-uid", "UID:other-uid")
			case "organizer":
				api.body = strings.ReplaceAll(api.body, "organizer@example.com", "other@example.com")
			case "self":
				api.body = strings.ReplaceAll(api.body, "alice@example.com", "bob@example.com")
			default:
				api.before = func(*http.Request) {
					if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
						query := map[string]string{"attempt": `UPDATE outgoing_sends SET attempt_count=attempt_count+1`, "nonce": `UPDATE calendar_response_requests SET claim_id='replacement'`, "calendar": `UPDATE account_caldav_configs SET encrypted_password=x'01'`, "smtp": `UPDATE accounts SET smtp_username='changed'`}[mode]
						_, err := db.Write().Exec(query)
						return err
					}); err != nil {
						t.Error(err)
					}
				}
			}
			api.mu.Unlock()
			if err := f.h.userIMAP.RunUserServiceWork(t.Context(), "alice", func(ctx context.Context) error { return f.h.retryUserCalendarReply(ctx, control, false) }); err == nil {
				t.Fatal("unsafe native retry queued", mode)
			}
			if job := ownedReplyJob(t, f, id); job.SendStatus != storage.OutgoingSendFailed || job.Payload != control.Job().Payload {
				t.Fatal("partial native retry", job)
			}
			api.mu.Lock()
			puts := api.puts
			api.mu.Unlock()
			if puts != 0 {
				t.Fatal("retry preflight wrote Calendar", puts)
			}
		})
	}
}
