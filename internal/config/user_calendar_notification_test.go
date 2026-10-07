package config

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newOwnedNotificationFixture(t *testing.T) (*storage.DB, *UserAccountStore, map[string]*UserCalendarSourceSnapshot) {
	t.Helper()
	system, _, accounts, _ := newCalendarControlFixture(t)
	sources := make(map[string]*UserCalendarSourceSnapshot)
	for _, owner := range []string{"alice", "bob"} {
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE calendar_sources SET remote_id=? WHERE id='same-source'`, "https://"+owner+".test/primary/")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var err error
		sources[owner], err = accounts.SnapshotCalendarSource(t.Context(), owner, "same-source")
		if err != nil {
			t.Fatal(err)
		}
	}
	return system, accounts, sources
}

func notificationInput(t *testing.T, source *UserCalendarSourceSnapshot) storage.QueueOutgoingSendInput {
	t.Helper()
	note := map[string]any{"UserID": source.service.owner, "SourceID": source.Source().ID, "ResourceID": source.Source().RemoteID + "meeting.ics", "Method": "REQUEST", "Calendar": "synthetic invitation", "ExpectedCalendar": "synthetic saved meeting"}
	payload, err := json.Marshal(map[string]any{"message_id": "<original@example.com>", "from_email": source.service.Identity().EmailAddress, "calendar_notification": note})
	if err != nil {
		t.Fatal(err)
	}
	return storage.QueueOutgoingSendInput{ID: "same-notification", AccountID: source.service.id, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: source.service.Identity().EmailAddress, EnvelopeRecipients: []string{"guest@example.com"}, MIMEData: []byte("synthetic immutable MIME"), MessageJSON: payload}
}

func claimOwnedNotification(t *testing.T, accounts *UserAccountStore, source *UserCalendarSourceSnapshot, inputs ...storage.QueueOutgoingSendInput) *UserCalendarNotificationSnapshot {
	t.Helper()
	input := notificationInput(t, source)
	if len(inputs) == 1 {
		input = inputs[0]
	}
	queued, err := accounts.QueueCalendarNotification(t.Context(), source, input)
	if err != nil {
		t.Fatal("queue", err)
	}
	if err := accounts.WithUser(t.Context(), source.service.owner, func(_ *AccountStore, db *storage.DB) error {
		sends, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), source.service.id, time.Now().Add(time.Second), 1)
		if err == nil && (len(sends) != 1 || sends[0].ID != queued.ID) {
			t.Fatal("wrong claim", sends)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := accounts.SnapshotCalendarNotificationDelivery(t.Context(), source.service.owner, queued.ID)
	if err != nil {
		t.Fatal("snapshot", err)
	}
	return snapshot
}

func TestUserCalendarNotificationOwnerCopiesRestartAndAcceptance(t *testing.T) {
	system, accounts, sources := newOwnedNotificationFixture(t)
	alice := claimOwnedNotification(t, accounts, sources["alice"])
	bob := claimOwnedNotification(t, accounts, sources["bob"])
	if alice.send.ID != bob.send.ID || alice.send.AccountID == bob.send.AccountID {
		t.Fatal("fixture did not exercise colliding local IDs")
	}
	copy := alice.Send()
	copy.MIMEData[0] = 'X'
	copy.MessageJSON[0] = 'X'
	copy.EnvelopeRecipients[0] = "wrong@example.com"
	if err := accounts.RestoreCalendarNotification(t.Context(), alice); err != nil {
		t.Fatal("restart/eviction lost authority", err)
	}
	if alice.Source().Source().UserID != "alice" || !strings.Contains(string(alice.Send().MIMEData), "immutable MIME") {
		t.Fatal("detached copy changed authority")
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET smtp_host='repaired'; UPDATE calendar_sources SET is_deleted=1,is_selected=0`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarNotification(t.Context(), alice); err == nil {
		t.Fatal("changed source/config authorized SMTP")
	}
	result := storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: "<original@example.com>"}
	if err := accounts.FinishCalendarNotificationSend(t.Context(), alice, result); err != nil {
		t.Fatal("confirmed SMTP acceptance lost", err)
	}
	if err := accounts.FinishCalendarNotificationSend(t.Context(), alice, result); err == nil {
		t.Fatal("same attempt finished twice")
	}
	if err := accounts.ValidateCalendarNotificationDelivery(t.Context(), bob); err != nil {
		t.Fatal("Alice changed Bob's colliding attempt", err)
	}
	var count int
	if err := system.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&count); err != nil || count != 0 {
		t.Fatal("central content used", count, err)
	}
}

func TestUserCalendarNotificationWriterWaitAndTriggerRollback(t *testing.T) {
	for _, change := range []string{"attempt", "mime", "json", "sender", "recipients", "disabled", "deleting", "trigger-ignore", "trigger-status", "trigger-envelope", "trigger-copy", "input-copy"} {
		t.Run(change, func(t *testing.T) {
			system, accounts, sources := newOwnedNotificationFixture(t)
			snapshot := claimOwnedNotification(t, accounts, sources["alice"])
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				done := make(chan error, 1)
				go func() {
					done <- accounts.FinishCalendarNotificationSend(ctx, snapshot, storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: "<original@example.com>"})
				}()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				switch change {
				case "attempt":
					_, err = tx.Exec(`UPDATE outgoing_sends SET attempt_count=attempt_count+1`)
				case "mime":
					_, err = tx.Exec(`UPDATE outgoing_sends SET mime_data=x'0102'`)
				case "json":
					_, err = tx.Exec(`UPDATE outgoing_sends SET message_json='{}'`)
				case "sender":
					_, err = tx.Exec(`UPDATE outgoing_sends SET envelope_from='foreign@example.com'`)
				case "recipients":
					_, err = tx.Exec(`UPDATE outgoing_sends SET envelope_recipients='["foreign@example.com"]'`)
				case "disabled":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deleting":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, snapshot.send.AccountID)
				case "trigger-ignore":
					_, err = tx.Exec(`CREATE TRIGGER reject_notification BEFORE UPDATE OF status ON outgoing_sends BEGIN SELECT RAISE(IGNORE); END`)
				case "trigger-status":
					_, err = tx.Exec(`CREATE TRIGGER reject_notification AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE outgoing_sends SET status='pending' WHERE id=NEW.id; END`)
				case "trigger-envelope":
					_, err = tx.Exec(`CREATE TRIGGER reject_notification AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE outgoing_sends SET envelope_from='foreign@example.com' WHERE id=NEW.id; END`)
				case "trigger-copy":
					_, err = tx.Exec(`CREATE TRIGGER reject_notification AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE outgoing_sends SET sent_copy_status='complete' WHERE id=NEW.id; END`)
				case "input-copy":
					copy := snapshot.Send()
					copy.MIMEData[0] = 'X'
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					if change == "input-copy" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("superseded attempt committed")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				if strings.HasPrefix(change, "trigger-") {
					send, err := db.GetOutgoingSend(ctx, snapshot.send.ID)
					if err != nil {
						return err
					}
					if send.Status != storage.OutgoingSendSending || send.SentMessageID != "" || send.SentCopyStatus != storage.SentCopyNotRequired {
						t.Fatal("failed publication was not rolled back", send)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarNotificationQueueRejectsRetargetingAndChangedServices(t *testing.T) {
	for _, change := range []string{"source", "owner", "resource", "smtp", "source-trigger", "mime-trigger", "status-trigger", "event-trigger"} {
		t.Run(change, func(t *testing.T) {
			_, accounts, sources := newOwnedNotificationFixture(t)
			source := sources["alice"]
			input := notificationInput(t, source)
			var events []*UserCalendarEventSnapshot
			if change == "event-trigger" {
				seedCalendarSyncEvents(t, accounts)
				event, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, event)
			}
			var payload map[string]any
			_ = json.Unmarshal(input.MessageJSON, &payload)
			note := payload["calendar_notification"].(map[string]any)
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var err error
				switch change {
				case "source":
					note["SourceID"] = "other-source"
				case "owner":
					note["UserID"] = "bob"
				case "resource":
					note["ResourceID"] = "https://bob.test/primary/meeting.ics"
				case "smtp":
					_, err = db.Write().Exec(`UPDATE accounts SET smtp_host='changed'`)
				case "source-trigger":
					_, err = db.Write().Exec(`CREATE TRIGGER reject_notification AFTER INSERT ON outgoing_sends BEGIN UPDATE calendar_sources SET is_selected=0; END`)
				case "mime-trigger":
					_, err = db.Write().Exec(`CREATE TRIGGER reject_notification AFTER INSERT ON outgoing_sends BEGIN UPDATE outgoing_sends SET mime_data=x'0102' WHERE id=NEW.id; END`)
				case "status-trigger":
					_, err = db.Write().Exec(`CREATE TRIGGER reject_notification AFTER INSERT ON outgoing_sends BEGIN UPDATE outgoing_sends SET status='sent' WHERE id=NEW.id; END`)
				case "event-trigger":
					_, err = db.Write().Exec(`CREATE TRIGGER reject_notification AFTER INSERT ON outgoing_sends BEGIN UPDATE calendar_events SET summary='changed during queue'; END`)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			input.MessageJSON, _ = json.Marshal(payload)
			if _, err := accounts.QueueCalendarNotification(t.Context(), source, input, events...); err == nil {
				t.Fatal("invalid queue accepted")
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				err := db.Read().QueryRow(`SELECT count(*) FROM outgoing_sends`).Scan(&n)
				if err == nil && n != 0 {
					t.Fatal("partial notification retained")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarNotificationRetryRepairsOriginalPrincipalOnly(t *testing.T) {
	for _, mode := range []string{"failed", "ambiguous-unconfirmed", "ambiguous-confirmed", "dav-principal", "sender", "source", "sent"} {
		t.Run(mode, func(t *testing.T) {
			_, accounts, sources := newOwnedNotificationFixture(t)
			snapshot := claimOwnedNotification(t, accounts, sources["alice"])
			status := storage.OutgoingSendFailed
			if strings.HasPrefix(mode, "ambiguous") {
				status = storage.OutgoingSendAmbiguous
			}
			result := storage.CalendarReplySendResult{Status: status, Error: "synthetic failure"}
			if mode == "sent" {
				result = storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: "<original@example.com>"}
			}
			if err := accounts.FinishCalendarNotificationSend(t.Context(), snapshot, result); err != nil {
				t.Fatal(err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE accounts SET smtp_host='repaired',smtp_username='repaired-user'`); err != nil {
					return err
				}
				var err error
				switch mode {
				case "dav-principal":
					_, err = db.Write().Exec(`UPDATE account_caldav_configs SET username='different-principal'`)
				case "sender":
					_, err = db.Write().Exec(`UPDATE accounts SET email_address='other@example.com'`)
				case "source":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET remote_id='https://alice.test/other/' WHERE id='same-source'`)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			retried, err := accounts.RetryCalendarNotification(t.Context(), "alice", snapshot.send.ID, mode == "ambiguous-confirmed")
			allowed := mode == "failed" || mode == "ambiguous-confirmed"
			if (err == nil) != allowed {
				t.Fatal("retry authority", mode, err)
			}
			if !allowed {
				return
			}
			if string(retried.MIMEData) != string(snapshot.send.MIMEData) || retried.EnvelopeFrom != snapshot.send.EnvelopeFrom || retried.EnvelopeRecipients[0] != snapshot.send.EnvelopeRecipients[0] || retried.AttemptCount != 1 || retried.Status != storage.OutgoingSendPending {
				t.Fatal("retry retargeted original send", retried)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), snapshot.send.AccountID, time.Now().Add(time.Second), 1)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			next, err := accounts.SnapshotCalendarNotificationDelivery(t.Context(), "alice", snapshot.send.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.RestoreCalendarNotification(t.Context(), next); err != nil {
				t.Fatal("retry could not resume after repair", err)
			}
			if err := accounts.FinishCalendarNotificationSend(t.Context(), snapshot, storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: "<stale@example.com>"}); err == nil {
				t.Fatal("old attempt overwrote repaired attempt")
			}
		})
	}
}

func TestUserCalendarNotificationRetryEventWriterWaitAndRollback(t *testing.T) {
	for _, mode := range []string{"event-uid", "event-resource", "event-summary", "attempt", "mime", "status", "disabled", "deleting", "trigger-ignore", "trigger-event", "trigger-message", "input-copy"} {
		t.Run(mode, func(t *testing.T) {
			system, accounts, sources := newOwnedNotificationFixture(t)
			input := notificationInput(t, sources["alice"])
			var payload map[string]any
			_ = json.Unmarshal(input.MessageJSON, &payload)
			payload["calendar_notification"].(map[string]any)["Calendar"] = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:original-uid\r\nORGANIZER:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			input.MessageJSON, _ = json.Marshal(payload)
			attempt := claimOwnedNotification(t, accounts, sources["alice"], input)
			if err := accounts.FinishCalendarNotificationSend(t.Context(), attempt, storage.CalendarReplySendResult{Status: storage.OutgoingSendFailed, Error: "known rejection"}); err != nil {
				t.Fatal(err)
			}
			seedCalendarSyncEvents(t, accounts)
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE calendar_events SET remote_id=?,ical_uid='original-uid' WHERE id='same-event'`, sources["alice"].Source().RemoteID+"meeting.ics")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			event, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			control, err := accounts.SnapshotCalendarNotificationControl(t.Context(), "alice", attempt.send.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			if err := accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				done := make(chan error, 1)
				go func() { _, err := accounts.RetryCalendarNotificationForEvent(ctx, control, event); done <- err }()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for db.Write().Stats().WaitCount == before {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-tick.C:
					}
				}
				switch mode {
				case "event-uid":
					_, err = tx.Exec(`UPDATE calendar_events SET ical_uid='replacement' WHERE id='same-event'`)
				case "event-resource":
					_, err = tx.Exec(`UPDATE calendar_events SET remote_id='replacement' WHERE id='same-event'`)
				case "event-summary":
					_, err = tx.Exec(`UPDATE calendar_events SET summary='replacement' WHERE id='same-event'`)
				case "attempt":
					_, err = tx.Exec(`UPDATE outgoing_sends SET attempt_count=attempt_count+1`)
				case "mime":
					_, err = tx.Exec(`UPDATE outgoing_sends SET mime_data=x'0102'`)
				case "status":
					_, err = tx.Exec(`UPDATE outgoing_sends SET status='ambiguous'`)
				case "disabled":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deleting":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, control.send.AccountID)
				case "trigger-ignore":
					_, err = tx.Exec(`CREATE TRIGGER reject_retry BEFORE UPDATE OF status ON outgoing_sends BEGIN SELECT RAISE(IGNORE); END`)
				case "trigger-event":
					_, err = tx.Exec(`CREATE TRIGGER reject_retry AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE calendar_events SET ical_uid='replacement' WHERE id='same-event'; END`)
				case "trigger-message":
					_, err = tx.Exec(`CREATE TRIGGER reject_retry AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE outgoing_sends SET message_json='{}' WHERE id=NEW.id; END`)
				case "input-copy":
					copy := control.Send()
					copy.MIMEData[0] = 'X'
					copy.MessageJSON[0] = 'X'
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					if mode == "input-copy" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("superseded event/attempt retried")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				if strings.HasPrefix(mode, "trigger-") {
					send, err := db.GetOutgoingSend(ctx, control.send.ID)
					if err != nil {
						return err
					}
					if send.Status != storage.OutgoingSendFailed || send.LastError != "known rejection" || string(send.MessageJSON) != string(control.send.MessageJSON) {
						t.Fatal("retry transaction was not rolled back")
					}
					var uid string
					if err := db.Read().QueryRow(`SELECT ical_uid FROM calendar_events WHERE id='same-event'`).Scan(&uid); err != nil {
						return err
					}
					if uid != "original-uid" {
						t.Fatal("event mutation was not rolled back")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarNotificationRetryEventContentBinding(t *testing.T) {
	for _, mode := range []string{"matching", "uid", "organizer", "method", "malformed", "additional-calendar", "foreign-event"} {
		t.Run(mode, func(t *testing.T) {
			_, accounts, sources := newOwnedNotificationFixture(t)
			input := notificationInput(t, sources["alice"])
			var payload map[string]any
			_ = json.Unmarshal(input.MessageJSON, &payload)
			raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:original-uid\r\nORGANIZER:mailto:alice@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			switch mode {
			case "uid":
				raw = strings.Replace(raw, "UID:original-uid", "UID:another-meeting", 1)
			case "organizer":
				raw = strings.Replace(raw, "mailto:alice@example.com", "mailto:bob@example.com", 1)
			case "method":
				raw = strings.Replace(raw, "METHOD:REQUEST", "METHOD:REPLY", 1)
			case "malformed":
				raw = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nORGANIZER;CN=\""
			case "additional-calendar":
				raw += raw
			}
			payload["calendar_notification"].(map[string]any)["Calendar"] = raw
			input.MessageJSON, _ = json.Marshal(payload)
			attempt := claimOwnedNotification(t, accounts, sources["alice"], input)
			if err := accounts.FinishCalendarNotificationSend(t.Context(), attempt, storage.CalendarReplySendResult{Status: storage.OutgoingSendFailed, Error: "known rejection"}); err != nil {
				t.Fatal(err)
			}
			seedCalendarSyncEvents(t, accounts)
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE calendar_events SET remote_id=?,ical_uid='original-uid' WHERE id='same-event'`, sources["alice"].Source().RemoteID+"meeting.ics")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			owner := "alice"
			if mode == "foreign-event" {
				owner = "bob"
			}
			event, err := accounts.SnapshotCalendarEvent(t.Context(), owner, "same-event")
			if err != nil {
				t.Fatal(err)
			}
			control, err := accounts.SnapshotCalendarNotificationControl(t.Context(), "alice", attempt.send.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = accounts.RetryCalendarNotificationForEvent(t.Context(), control, event)
			if (err == nil) != (mode == "matching") {
				t.Fatal("notification was rebound to wrong event/content", mode, err)
			}
			if mode != "matching" {
				if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
					send, err := db.GetOutgoingSend(t.Context(), attempt.send.ID)
					if err == nil && (send.Status != storage.OutgoingSendFailed || send.AttemptCount != 1 || string(send.MIMEData) != string(attempt.send.MIMEData)) {
						t.Fatal("rejected binding mutated send")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
