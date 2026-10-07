package handler

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Speak real SMTP: authenticate, accept the original envelope, read dot-stuffed
// DATA, then acknowledge/reject/drop the final reply. No mocked send result.
func ownedReplySMTPServer(t *testing.T, final string, beforeFinal func(string), methods ...string) (string, int, *atomic.Int32, <-chan error) {
	t.Helper()
	method := "REPLY"
	if len(methods) == 1 {
		method = methods[0]
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	calls := &atomic.Int32{}
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		r := textproto.NewReader(bufio.NewReader(conn))
		write := func(s string) error { _, err := io.WriteString(conn, s+"\r\n"); return err }
		if err := write("220 Gofer fixture ready"); err != nil {
			done <- err
			return
		}
		for {
			line, err := r.ReadLine()
			if err != nil {
				if errors.Is(err, io.EOF) && calls.Load() > 0 {
					done <- nil // Client.Close need not send QUIT.
					return
				}
				done <- err
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				err = write("250-fixture\r\n250 AUTH PLAIN")
			case strings.HasPrefix(line, "AUTH PLAIN "):
				err = write("235 authenticated")
			case strings.HasPrefix(line, "MAIL FROM:"):
				if line != "MAIL FROM:<alice@example.com>" {
					done <- fmt.Errorf("wrong sender %s", line)
					return
				}
				err = write("250 sender accepted")
			case strings.HasPrefix(line, "RCPT TO:"):
				if line != "RCPT TO:<organizer@example.com>" {
					done <- fmt.Errorf("wrong recipient %s", line)
					return
				}
				err = write("250 recipient accepted")
			case line == "DATA":
				if err = write("354 send data"); err != nil {
					done <- err
					return
				}
				data, readErr := io.ReadAll(r.DotReader())
				if readErr != nil {
					done <- readErr
					return
				}
				calls.Add(1)
				envelope, parseErr := netmail.ReadMessage(strings.NewReader(string(data)))
				if parseErr != nil {
					done <- parseErr
					return
				}
				_, params, parseErr := mime.ParseMediaType(envelope.Header.Get("Content-Type"))
				if parseErr != nil {
					done <- parseErr
					return
				}
				parts := multipart.NewReader(envelope.Body, params["boundary"])
				_, parseErr = parts.NextPart()
				if parseErr != nil {
					done <- parseErr
					return
				}
				calendarPart, parseErr := parts.NextPart()
				if parseErr != nil {
					done <- parseErr
					return
				}
				calendarData, parseErr := io.ReadAll(calendarPart)
				if parseErr != nil {
					done <- parseErr
					return
				}
				contentType, calendarParams, parseErr := mime.ParseMediaType(calendarPart.Header.Get("Content-Type"))
				if parseErr != nil || contentType != "text/calendar" || calendarParams["method"] != method || !strings.Contains(string(calendarData), "METHOD:"+method) || (method == "REPLY" && !strings.Contains(string(calendarData), "PARTSTAT=ACCEPTED")) || !strings.Contains(string(calendarData), "UID:private-uid") {
					done <- fmt.Errorf("native reply MIME lost: %s", calendarData)
					return
				}
				if beforeFinal != nil {
					beforeFinal(string(data))
				}
				if final == "drop" {
					done <- nil
					return
				}
				err = write(final)
			case line == "QUIT":
				_ = write("221 bye")
				done <- nil
				return
			default:
				done <- fmt.Errorf("unexpected SMTP command %s", line)
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	return "127.0.0.1", port, calls, done
}

func ownedReplyDeliveryFixture(t *testing.T, smtpHost string, smtpPort int, beforeDAV func(*userContactPushFixture, *http.Request), provider ...http.Handler) (*userContactPushFixture, string, *atomic.Int32) {
	t.Helper()
	var f *userContactPushFixture
	calls := &atomic.Int32{}
	dav := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, password, ok := r.BasicAuth()
		if !ok || (owner != "alice" && owner != "bob") || password != owner+"-calendar-secret" {
			http.Error(w, "bad authority", 401)
			return
		}
		if len(provider) == 1 {
			calls.Add(1)
			provider[0].ServeHTTP(w, r)
			return
		}
		if r.Method == "OPTIONS" && r.URL.Path == "/"+owner+"/primary/" {
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" || r.URL.Path != "/"+owner+"/primary/invitation.ics" {
			t.Error("wrong DAV resource", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		calls.Add(1)
		if beforeDAV != nil {
			beforeDAV(f, r)
		}
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "text/calendar")
		w.Header().Set("ETag", `"v1"`)
		_, _ = fmt.Fprintf(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Fixture//EN\r\nBEGIN:VEVENT\r\nUID:private-uid\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:Private invitation\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:%s@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", owner)
	}))
	t.Cleanup(dav.Close)
	previous := calDAVHTTPTransport
	calDAVHTTPTransport = dav.Client().Transport
	t.Cleanup(func() { calDAVHTTPTransport = previous })
	f = newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("shared/provider fallback") }))
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", smtpHost, smtpPort, "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		id := f.accounts[owner].ID
		if err := f.h.userAccounts.WithUser(t.Context(), owner, func(local *config.AccountStore, db *storage.DB) error {
			if err := local.SaveCalDAVConfig(t.Context(), owner, id, dav.URL+"/", owner, owner+"-calendar-secret", false); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`UPDATE accounts SET smtp_host=?,smtp_port=?,smtp_tls_mode='plaintext' WHERE id=?`, smtpHost, smtpPort, id); err != nil {
				return err
			}
			if err := db.ReplaceCalendarSources(t.Context(), owner, id, "caldav", []storage.CalendarSource{{ID: "same-source", RemoteID: dav.URL + "/" + owner + "/primary/", AccessRole: "owner", IsSelected: true}}); err != nil {
				return err
			}
			start := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			return db.ReplaceCalendarEvents(t.Context(), owner, "same-source", []storage.CalendarEvent{{ID: "same-event", RemoteID: dav.URL + "/" + owner + "/primary/invitation.ics", ICalUID: "private-uid", ETag: `"v1"`, OrganizerEmail: "organizer@example.com", ResponseStatus: "needsAction", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
	}
	event, err := f.h.userAccounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	p := &userCalendarRequest{h: f.h, event: event}
	var id string
	err = f.h.userIMAP.RunAccountService(t.Context(), "alice", f.accounts["alice"].ID, mail.AccountServiceCalendar, time.Minute, func(ctx context.Context) error {
		target, err := p.readResponseTarget(ctx, "event")
		if err != nil {
			return err
		}
		claim, err := p.reserveResponse(ctx, target, "accepted")
		if err != nil {
			return err
		}
		result, err := p.queueCalDAVReply(ctx, claim, target, "accepted")
		id = result.DeliveryID
		return err
	})
	if err != nil {
		t.Fatal("native reply queue", err)
	}
	return f, id, calls
}

func ownedReplyDeliveryScope(t *testing.T, f *userContactPushFixture) *userMailDeliveryScope {
	t.Helper()
	s := &userMailDeliveryScope{runner: &userMailDelivery{h: f.h}, owner: "alice", id: f.accounts["alice"].ID}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(local *config.AccountStore, _ *storage.DB) error {
		var err error
		s.cfg, err = local.GetConfig(t.Context(), s.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUserCalendarReplyDeliveryNativeDAVAndSMTP(t *testing.T) {
	for _, final := range []string{"250 accepted", "450 temporary", "550 rejected", "drop"} {
		t.Run(final, func(t *testing.T) {
			host, port, smtpCalls, done := ownedReplySMTPServer(t, final, nil)
			f, id, davCalls := ownedReplyDeliveryFixture(t, host, port, nil)
			s := ownedReplyDeliveryScope(t, f)
			more, err := s.send(t.Context())
			want := map[string]string{"250 accepted": storage.OutgoingSendSent, "450 temporary": storage.OutgoingSendPending, "550 rejected": storage.OutgoingSendFailed, "drop": storage.OutgoingSendAmbiguous}[final]
			if !more || (err == nil) != (want == storage.OutgoingSendSent) {
				t.Fatal("delivery outcome", more, err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("SMTP did not close")
			}
			if smtpCalls.Load() != 1 || davCalls.Load() != 2 {
				t.Fatal("dispatch count", smtpCalls.Load(), davCalls.Load())
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), id)
				if err != nil {
					return err
				}
				if send.Status != want || send.AttemptCount != 1 || len(send.MIMEData) == 0 {
					t.Fatal("durable state", send.Status, send.AttemptCount)
				}
				if want == storage.OutgoingSendSent && (send.SentCopyStatus != storage.SentCopyPending || send.SentMessageID == "") {
					t.Fatal("Sent copy lost")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if more, err := s.send(t.Context()); more || err != nil {
				t.Fatal("automatic immediate resend", more, err)
			}
			if smtpCalls.Load() != 1 {
				t.Fatal("resent accepted/ambiguous mail")
			}
			if _, err := f.h.userAccounts.RestoreCalendarReply(t.Context(), "bob", id, true); err == nil {
				t.Fatal("foreign owner acquired reply")
			}
			var central int
			if err := f.system.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&central); err != nil || central != 0 {
				t.Fatal("shared queue fallback", central, err)
			}
		})
	}
}

func TestUserCalendarReplyDeliveryAcceptanceSurvivesSMTPSettingsChange(t *testing.T) {
	ready := make(chan *userContactPushFixture, 1)
	host, port, calls, done := ownedReplySMTPServer(t, "250 accepted", func(_ string) {
		f := <-ready
		// The SMTP DATA wait holds no user-store lease. MaxOpen1 allows Bob to
		// evict Alice, then Alice can change settings before remote acknowledgement.
		if err := f.h.userAccounts.WithUser(t.Context(), "bob", func(_ *config.AccountStore, _ *storage.DB) error { return nil }); err != nil {
			t.Error(err)
		}
		if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET smtp_host='repaired',smtp_port=2525,smtp_username='repaired'; UPDATE calendar_events SET summary='refreshed'`)
			return err
		}); err != nil {
			t.Error(err)
		}
	})
	f, id, _ := ownedReplyDeliveryFixture(t, host, port, nil)
	ready <- f
	more, err := ownedReplyDeliveryScope(t, f).send(t.Context())
	if !more || err != nil {
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
	if calls.Load() != 1 {
		t.Fatal("wrong SMTP count")
	}
	if _, err := f.h.userAccounts.RestoreCalendarReply(t.Context(), "alice", id, false); err != nil {
		t.Fatal("accepted followup lost", err)
	}
}

func TestUserCalendarReplyDeliveryRejectsChangeDuringNativePreflight(t *testing.T) {
	for _, change := range []string{"smtp", "event", "nonce", "mime"} {
		t.Run(change, func(t *testing.T) {
			listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var calls atomic.Int32
			f, id, _ := ownedReplyDeliveryFixture(t, "127.0.0.1", listener.Addr().(*net.TCPAddr).Port, func(f *userContactPushFixture, _ *http.Request) {
				if calls.Add(1) != 2 {
					return
				} // First read queued the native reply.
				if err := f.h.userAccounts.WithUser(t.Context(), "bob", func(_ *config.AccountStore, _ *storage.DB) error { return nil }); err != nil {
					t.Error(err)
				}
				if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
					query := map[string]string{"smtp": `UPDATE accounts SET smtp_username='other'`, "event": `UPDATE calendar_events SET etag='new'`, "nonce": `UPDATE calendar_response_requests SET claim_id='replacement'`, "mime": `UPDATE outgoing_sends SET mime_data=x'010203'`}[change]
					_, err := db.Write().Exec(query)
					return err
				}); err != nil {
					t.Error(err)
				}
			})
			more, err := ownedReplyDeliveryScope(t, f).send(t.Context())
			_ = listener.SetDeadline(time.Now().Add(100 * time.Millisecond))
			conn, acceptErr := listener.Accept()
			if conn != nil {
				_ = conn.Close()
				t.Fatal("stale invitation dialed SMTP")
			}
			var timeout net.Error
			if !errors.As(acceptErr, &timeout) || !timeout.Timeout() {
				t.Fatal("could not verify no SMTP dial", acceptErr)
			}

			if err == nil {
				t.Fatal("stale preflight sent mail")
			}
			if change == "nonce" || change == "mime" {
				if more {
					t.Fatal("unverified result published")
				}
			} else if !more {
				t.Fatal("known unsent rejection lost", err)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), id)
				if err != nil {
					return err
				}
				want := storage.OutgoingSendFailed
				if change == "nonce" || change == "mime" {
					want = storage.OutgoingSendSending
				}
				if send.Status != want {
					t.Fatal("unsafe rejection state", send.Status, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyDeliveryRootCancellationLeavesClaimForRecovery(t *testing.T) {
	var calls atomic.Int32
	f, id, _ := ownedReplyDeliveryFixture(t, "127.0.0.1", 25251, func(f *userContactPushFixture, r *http.Request) {
		if calls.Add(1) != 2 {
			return
		}
		f.cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("native DAV read was not canceled")
		}
	})
	more, err := ownedReplyDeliveryScope(t, f).send(t.Context())
	if more || !errors.Is(err, context.Canceled) {
		t.Fatal("root cancellation outcome", more, err)
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		send, err := db.GetOutgoingSend(t.Context(), id)
		if err != nil {
			return err
		}
		if send.Status != storage.OutgoingSendSending {
			t.Fatal("interrupted claim was recycled", send.Status)
		}
		if err := db.RecoverAccountMailQueue(t.Context(), send.AccountID); err != nil {
			return err
		}
		recovered, err := db.GetOutgoingSend(t.Context(), id)
		if err != nil {
			return err
		}
		if recovered.Status != storage.OutgoingSendAmbiguous {
			t.Fatal("interrupted reply became resendable", recovered.Status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarReplyDeliveryCancellationDuringDATAKeepsUncertainty(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host, port, calls, done := ownedReplySMTPServer(t, "drop", func(_ string) { cancel() })
	f, id, _ := ownedReplyDeliveryFixture(t, host, port, nil)
	more, err := ownedReplyDeliveryScope(t, f).send(ctx)
	if more || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled SMTP outcome", more, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SMTP did not close")
	}
	if calls.Load() != 1 {
		t.Fatal("DATA was not dispatched exactly once")
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		send, err := db.GetOutgoingSend(t.Context(), id)
		if err != nil {
			return err
		}
		if send.Status != storage.OutgoingSendSending {
			t.Fatal("uncertain SMTP was recycled", send.Status)
		}
		if err := db.RecoverAccountMailQueue(t.Context(), send.AccountID); err != nil {
			return err
		}
		send, err = db.GetOutgoingSend(t.Context(), id)
		if err != nil {
			return err
		}
		if send.Status != storage.OutgoingSendAmbiguous {
			t.Fatal("recovery made SMTP resendable", send.Status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
