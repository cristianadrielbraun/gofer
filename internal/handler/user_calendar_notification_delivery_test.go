package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedNotificationDeliveryFixture(t *testing.T, host string, port int, kind, remote string, beforeRead func(*userContactPushFixture)) (*userContactPushFixture, string, *atomic.Int32) {
	t.Helper()
	var f *userContactPushFixture
	calls := &atomic.Int32{}
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Fixture//EN\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTAMP:20261003T090000Z\r\nSEQUENCE:1\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:Private meeting\r\nORGANIZER;SCHEDULE-AGENT=CLIENT:mailto:alice@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:organizer@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, err := calendarUpdateDecodeICS([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	expected := raw
	if kind == "removed" {
		expected = strings.ReplaceAll(expected, "ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:organizer@example.com\r\n", "")
	}
	dav := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, password, ok := r.BasicAuth()
		if !ok || owner != "alice" || password != "alice-calendar-secret" || r.Method != "GET" || r.URL.Path != "/alice/primary/meeting.ics" {
			t.Error("wrong native DAV authority", owner, r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		calls.Add(1)
		if beforeRead != nil {
			beforeRead(f)
		}
		if remote == "missing" {
			http.NotFound(w, r)
			return
		}
		if remote == "unauthorized" {
			http.Error(w, "denied", 401)
			return
		}
		current := expected
		if remote == "mismatch" {
			current = strings.Replace(current, "SUMMARY:Private meeting", "SUMMARY:Changed meeting", 1)
		}
		if remote == "readded" {
			current = raw
		}
		if remote == "server-agent" {
			current = strings.Replace(current, "SCHEDULE-AGENT=CLIENT", "SCHEDULE-AGENT=SERVER", 1)
		}
		w.Header().Set("Content-Type", "text/calendar")
		w.Header().Set("ETag", `"v2"`)
		_, _ = fmt.Fprint(w, current)
	}))
	t.Cleanup(dav.Close)
	previous := calDAVHTTPTransport
	calDAVHTTPTransport = dav.Client().Transport
	t.Cleanup(func() { calDAVHTTPTransport = previous })
	f = newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("shared/provider fallback") }))
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", host, port, "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		id := f.accounts[owner].ID
		if err := f.h.userAccounts.WithUser(t.Context(), owner, func(local *config.AccountStore, db *storage.DB) error {
			if err := local.SaveCalDAVConfig(t.Context(), owner, id, dav.URL+"/", owner, owner+"-calendar-secret", false); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`UPDATE accounts SET smtp_host=?,smtp_port=?,smtp_tls_mode='plaintext' WHERE id=?`, host, port, id); err != nil {
				return err
			}
			return db.ReplaceCalendarSources(t.Context(), owner, id, "caldav", []storage.CalendarSource{{ID: "same-source", RemoteID: dav.URL + "/" + owner + "/primary/", AccessRole: "owner", IsSelected: true}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	source, err := f.h.userAccounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
	if err != nil {
		t.Fatal(err)
	}
	method := "REQUEST"
	if kind != "request" {
		method = "CANCEL"
	}
	desired, err := calendarUpdateDecodeICS([]byte(expected))
	if err != nil {
		t.Fatal(err)
	}
	p := &userCalendarRequest{h: f.h, source: source}
	err = f.h.userIMAP.RunAccountService(t.Context(), "alice", source.Service().AccountID(), mail.AccountServiceCalendar, time.Minute, func(ctx context.Context) error {
		operation, err := p.actionContext(ctx, true)
		if err != nil {
			return err
		}
		return f.h.queueCalendarNotification(operation, source.Source(), source.Source().RemoteID+"meeting.ics", "v1", method, cal, desired, []calendar.GuestDraft{{Email: "organizer@example.com"}}, kind == "deleted")
	})
	if err != nil {
		t.Fatal("owned native notification producer", err)
	}
	var id string
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		return db.Read().QueryRow(`SELECT id FROM outgoing_sends`).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return f, id, calls
}

func TestUserCalendarNotificationDeliveryNativeDAVAndSMTP(t *testing.T) {
	for _, kind := range []string{"request", "removed", "deleted"} {
		for _, final := range []string{"250 accepted", "450 temporary", "550 rejected", "drop"} {
			t.Run(kind+"/"+final, func(t *testing.T) {
				method := "REQUEST"
				remote := "matching"
				if kind != "request" {
					method = "CANCEL"
				}
				if kind == "deleted" {
					remote = "missing"
				}
				host, port, smtpCalls, done := ownedReplySMTPServer(t, final, func(raw string) {
					if strings.Contains(raw, "UserAuthority") || strings.Contains(raw, "calendar-secret") || strings.Contains(raw, "SCHEDULE-AGENT") {
						t.Error("private authority leaked into native MIME")
					}
				}, method)
				f, id, davCalls := ownedNotificationDeliveryFixture(t, host, port, kind, remote, nil)
				s := ownedReplyDeliveryScope(t, f)
				more, err := s.send(t.Context())
				want := map[string]string{"250 accepted": storage.OutgoingSendSent, "450 temporary": storage.OutgoingSendPending, "550 rejected": storage.OutgoingSendFailed, "drop": storage.OutgoingSendAmbiguous}[final]
				if !more || (err == nil) != (want == storage.OutgoingSendSent) {
					t.Fatal("native result", more, err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("SMTP did not close")
				}
				if smtpCalls.Load() != 1 || davCalls.Load() != 1 {
					t.Fatal("dispatch count", smtpCalls.Load(), davCalls.Load())
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					send, err := db.GetOutgoingSend(t.Context(), id)
					if err == nil && (send.Status != want || send.AttemptCount != 1 || len(send.MIMEData) == 0) {
						t.Fatal("durable result lost", send)
					}
					if err == nil && want == storage.OutgoingSendSent && (send.SentCopyStatus != storage.SentCopyPending || send.SentMessageID == "") {
						t.Fatal("accepted send lost Sent copy")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if more, err := s.send(t.Context()); more || err != nil {
					t.Fatal("automatic resend", more, err)
				}
				if smtpCalls.Load() != 1 {
					t.Fatal("resent original notification")
				}
				var original storage.OutgoingSend
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					var err error
					original, err = db.GetOutgoingSend(t.Context(), id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var saved outgoingMessageSnapshot
				if err := json.Unmarshal(original.MessageJSON, &saved); err != nil {
					t.Fatal(err)
				}
				source, err := f.h.userAccounts.SnapshotCalendarSource(t.Context(), "alice", "same-source")
				if err != nil {
					t.Fatal(err)
				}
				desired, err := calendarUpdateDecodeICS([]byte(saved.CalendarNotification.ExpectedCalendar))
				if err != nil {
					t.Fatal(err)
				}
				p := &userCalendarRequest{h: f.h, source: source}
				operation, err := p.actionContext(t.Context(), true)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.h.queueCalendarNotification(operation, source.Source(), saved.CalendarNotification.ResourceID, "v1", saved.CalendarNotification.Method, desired, desired, []calendar.GuestDraft{{Email: "organizer@example.com"}}, saved.CalendarNotification.Deleted); err != nil {
					t.Fatal("repeated save", err)
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					actual, err := db.GetOutgoingSend(t.Context(), id)
					wantStatus := want
					if want == storage.OutgoingSendFailed {
						wantStatus = storage.OutgoingSendPending
					}
					if err == nil && (actual.Status != wantStatus || string(actual.MIMEData) != string(original.MIMEData) || actual.EnvelopeFrom != original.EnvelopeFrom || actual.EnvelopeRecipients[0] != original.EnvelopeRecipients[0] || actual.AttemptCount != original.AttemptCount) {
						t.Fatal("repeated save replayed/retargeted original notification", actual)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.h.userAccounts.SnapshotCalendarNotificationDelivery(t.Context(), "bob", id); err == nil {
					t.Fatal("foreign owner acquired notification")
				}
				var count int
				if err := f.system.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&count); err != nil || count != 0 {
					t.Fatal("shared content fallback", count, err)
				}
			})
		}
	}
}

func TestUserCalendarNotificationDeliveryRejectsChangedAuthorityDuringDAV(t *testing.T) {
	for _, change := range []string{"smtp", "source", "owner"} {
		t.Run(change, func(t *testing.T) {
			f, id, calls := ownedNotificationDeliveryFixture(t, "127.0.0.1", 1, "request", "matching", func(f *userContactPushFixture) {
				var err error
				if change == "owner" {
					_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				} else {
					err = f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
						var err error
						if change == "smtp" {
							_, err = db.Write().Exec(`UPDATE accounts SET smtp_host='changed'`)
						} else {
							_, err = db.Write().Exec(`UPDATE calendar_sources SET is_selected=0`)
						}
						return err
					})
				}
				if err != nil {
					t.Error(err)
				}
			})
			if _, err := ownedReplyDeliveryScope(t, f).send(t.Context()); err == nil {
				t.Fatal("changed authority permitted SMTP")
			}
			if calls.Load() != 1 {
				t.Fatal("wrong native request count", calls.Load())
			}
			// Disabled owners cannot even publish a retry. Their claimed attempt
			// remains uncertain for lifecycle recovery rather than auto-resending.
			if change == "owner" {
				return
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), id)
				if err == nil && (send.Status != storage.OutgoingSendPending || send.SentMessageID != "") {
					t.Fatal("rejected preflight published success", send)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarNotificationDeliveryFreshDAVPreflight(t *testing.T) {
	for _, tc := range []struct{ kind, remote, status string }{{"request", "mismatch", storage.OutgoingSendPending}, {"request", "missing", storage.OutgoingSendPending}, {"request", "server-agent", storage.OutgoingSendFailed}, {"removed", "readded", storage.OutgoingSendPending}, {"deleted", "matching", storage.OutgoingSendPending}, {"deleted", "unauthorized", storage.OutgoingSendPending}} {
		t.Run(tc.kind+"/"+tc.remote, func(t *testing.T) {
			f, id, calls := ownedNotificationDeliveryFixture(t, "127.0.0.1", 1, tc.kind, tc.remote, nil)
			if more, err := ownedReplyDeliveryScope(t, f).send(t.Context()); !more || err == nil {
				t.Fatal("invalid saved meeting permitted", more, err)
			}
			if calls.Load() != 1 {
				t.Fatal("fresh read count", calls.Load())
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), id)
				if err == nil && send.Status != tc.status {
					t.Fatal("preflight state", send.Status, send.LastError)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarNotificationDeliveryAcceptanceSurvivesSettingsAndCacheChanges(t *testing.T) {
	var f *userContactPushFixture
	var id string
	host, port, _, done := ownedReplySMTPServer(t, "250 accepted", func(string) {
		if err := f.h.userAccounts.WithUser(context.Background(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET smtp_host='repaired'; UPDATE calendar_sources SET is_selected=0,is_deleted=1`)
			return err
		}); err != nil {
			t.Error(err)
		}
	}, "REQUEST")
	f, id, _ = ownedNotificationDeliveryFixture(t, host, port, "request", "matching", nil)
	if more, err := ownedReplyDeliveryScope(t, f).send(t.Context()); !more || err != nil {
		t.Fatal("confirmed acceptance erased", more, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SMTP did not close")
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		send, err := db.GetOutgoingSend(t.Context(), id)
		if err == nil && send.Status != storage.OutgoingSendSent {
			t.Fatal("confirmed acceptance not durable")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
