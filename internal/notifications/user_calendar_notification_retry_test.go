package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newOwnedNotificationRetryHTTPFixture(t *testing.T, status string) *userStorageFixture {
	t.Helper()
	f, _ := newOwnedCalendarSyncFixture(t, "caldav")
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			// A retry wakes the real mail worker. Disable receive so this test
			// cannot connect to the placeholder IMAP host. Its native DAV fixture
			// rejects GET, keeping mail pending without any SMTP dispatch.
			if _, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0; UPDATE calendar_events SET ical_uid=?,organizer_email=?,response_status='organizer' WHERE id='same-event'`, owner+"-meeting", owner+"@example.com"); err != nil {
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		source, err := f.accountStore.SnapshotCalendarSource(t.Context(), owner, "same-source")
		if err != nil {
			t.Fatal(err)
		}
		event, err := f.accountStore.SnapshotCalendarEvent(t.Context(), owner, "same-event")
		if err != nil {
			t.Fatal(err)
		}
		ics := fmt.Sprintf("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Fixture//EN\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:%s-meeting\r\nDTSTAMP:20261003T090000Z\r\nDTSTART:20261003T100000Z\r\nDTEND:20261003T110000Z\r\nSUMMARY:%s private meeting\r\nORGANIZER:mailto:%s@example.com\r\nATTENDEE:mailto:%s-guest@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", owner, owner, owner, owner)
		payload, err := json.Marshal(map[string]any{"message_id": "<" + owner + "-original@example.com>", "date": time.Now().UTC(), "from_email": owner + "@example.com", "calendar_notification": map[string]any{"UserID": owner, "SourceID": "same-source", "ResourceID": event.Event().RemoteID, "Method": "REQUEST", "Calendar": ics, "ExpectedCalendar": strings.Replace(ics, "METHOD:REQUEST\r\n", "", 1)}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.accountStore.QueueCalendarNotification(t.Context(), source, storage.QueueOutgoingSendInput{ID: "same-delivery", AccountID: f.accounts[owner].ID, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: owner + "@example.com", EnvelopeRecipients: []string{owner + "-guest@example.com"}, MIMEData: []byte(owner + " immutable MIME secret"), MessageJSON: payload}); err != nil {
			t.Fatal(err)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE outgoing_sends SET status=? WHERE id='same-delivery'`, status)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

const ownedNotificationRetryPath = "/api/calendar/events/same-event/delivery/same-delivery/retry"

func TestUserCalendarNotificationRetryHTTPKeepsOwnerAndKnownFailurePolicy(t *testing.T) {
	for _, status := range []string{storage.OutgoingSendFailed, storage.OutgoingSendPending, storage.OutgoingSendSending, storage.OutgoingSendSent, storage.OutgoingSendAmbiguous, storage.OutgoingSendCanceled} {
		t.Run(status, func(t *testing.T) {
			f := newOwnedNotificationRetryHTTPFixture(t, status)
			if status == storage.OutgoingSendFailed {
				// Explicit retry may repair sending credentials for the same principal.
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET smtp_host='repaired.example.com'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			r := f.request("alice", "POST", ownedNotificationRetryPath, "")
			if r.Code != 200 || r.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(r.Body.String(), "immutable MIME secret") || strings.Contains(r.Body.String(), "bob-guest@example.com") || !strings.Contains(r.Body.String(), "alice-guest@example.com") {
				t.Fatal("owner/render/status", r.Code, r.Body.String())
			}
			if status != storage.OutgoingSendFailed && !strings.Contains(r.Body.String(), "cannot be retried safely") {
				t.Fatal("unsafe status was retried", status, r.Body.String())
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), "same-delivery")
				if err != nil {
					return err
				}
				if string(send.MIMEData) != "alice immutable MIME secret" || send.EnvelopeFrom != "alice@example.com" || send.EnvelopeRecipients[0] != "alice-guest@example.com" {
					t.Fatal("retry retargeted immutable send")
				}
				if status != storage.OutgoingSendFailed && send.Status != status {
					t.Fatal("unsafe status changed", status, send.Status)
				}
				if status == storage.OutgoingSendFailed && (send.Status != storage.OutgoingSendPending && send.Status != storage.OutgoingSendSending) {
					t.Fatal("known failure not queued", send.Status)
				}
				var n int
				if err := db.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&n); err != nil {
					return err
				}
				if n != 1 {
					t.Fatal("retry duplicated notification", n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), "same-delivery")
				if err == nil && (send.Status != status || string(send.MIMEData) != "bob immutable MIME secret" || send.AttemptCount != 0) {
					t.Fatal("colliding Bob send was changed")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if status == storage.OutgoingSendFailed {
				if r := f.request("alice", "POST", ownedNotificationRetryPath, ""); r.Code != 200 || !strings.Contains(r.Body.String(), "cannot be retried safely") {
					t.Fatal("double click", r.Code, r.Body.String())
				}
			}
			var n int
			if err := f.system.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&n); err != nil || n != 0 {
				t.Fatal("central queue fallback", n, err)
			}
		})
	}
}

func TestUserCalendarNotificationRetryHTTPRejectsRetargetingAndMalformedRequest(t *testing.T) {
	for _, mode := range []string{"missing-event", "missing-send", "foreign-send", "uid", "sender", "resource", "principal", "readonly", "deselected", "query", "form", "json", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedNotificationRetryHTTPFixture(t, storage.OutgoingSendFailed)
			path, body, want := ownedNotificationRetryPath, "", 404
			switch mode {
			case "missing-event":
				path = strings.Replace(path, "same-event", "missing", 1)
			case "missing-send":
				path = strings.Replace(path, "same-delivery", "missing", 1)
			case "foreign-send":
				if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE outgoing_sends SET id='bob-only'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				path = strings.Replace(path, "same-delivery", "bob-only", 1)
			case "query":
				path += "?confirm_ambiguous=true"
				want = 400
			case "form":
				body = "confirm_ambiguous=true"
				want = 400
			case "json":
				body = `{"action":"retry"}`
				want = 400
			case "oversize":
				body = strings.Repeat("x", 1025)
				want = 400
			default:
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var err error
					switch mode {
					case "uid":
						_, err = db.Write().Exec(`UPDATE calendar_events SET ical_uid='replacement' WHERE id='same-event'`)
					case "sender":
						_, err = db.Write().Exec(`UPDATE accounts SET email_address='replacement@example.com'`)
					case "resource":
						_, err = db.Write().Exec(`UPDATE calendar_events SET remote_id='https://foreign.test/other.ics' WHERE id='same-event'`)
					case "principal":
						_, err = db.Write().Exec(`UPDATE account_caldav_configs SET username='replacement'`)
						want = 200
					case "readonly":
						_, err = db.Write().Exec(`UPDATE calendar_sources SET access_role='reader'`)
						want = 200
					case "deselected":
						_, err = db.Write().Exec(`UPDATE calendar_sources SET is_selected=0`)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			r := f.request("alice", "POST", path, body)
			if r.Code != want {
				t.Fatal("retargeting/malformed policy", mode, r.Code, r.Body.String())
			}
			if want == 200 && !strings.Contains(r.Body.String(), "retry could not be queued") {
				t.Fatal("changed authority silently queued")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), "same-delivery")
				if err == nil && (send.Status != storage.OutgoingSendFailed || send.AttemptCount != 0) {
					t.Fatal("invalid request changed delivery")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarNotificationRetryHTTPRenderingReleasesStore(t *testing.T) {
	f := newOwnedNotificationRetryHTTPFixture(t, storage.OutgoingSendAmbiguous)
	writer := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(writer.release) }) }
	t.Cleanup(release)
	r := httptest.NewRequest("POST", ownedNotificationRetryPath, nil).WithContext(t.Context())
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { f.http.ServeHTTP(writer, r); close(done) }()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("retry render did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	err := f.accountStore.WithUser(ctx, "bob", func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.GetCalendarEvent(ctx, "bob", "same-event")
		return err
	})
	cancel()
	if err != nil {
		t.Fatal("render retained sole cache slot", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry render did not finish")
	}
	if writer.Code != 200 || !strings.Contains(writer.Body.String(), "alice-guest@example.com") || strings.Contains(writer.Body.String(), "bob-guest@example.com") {
		t.Fatal("render owner", writer.Code, writer.Body.String())
	}
}
