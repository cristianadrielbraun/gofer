package config

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarIncomingResponseClaimsRejectForeignAndDiagnosticInputs(t *testing.T) {
	_, accounts, candidates := ownedIncomingMessageFixture(t)
	m, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
	if err != nil {
		t.Fatal(err)
	}
	event, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := accounts.SnapshotCalendarEvent(t.Context(), "bob", "same-event")
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if _, err := accounts.ReserveCalendarIncomingResponse(t.Context(), m, event, 0, stamp, "ACCEPTED"); err == nil {
		t.Fatal("unassociated message reserved native response")
	}
	m, err = accounts.BeginCalendarIncomingDelivery(t.Context(), m, event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.ReserveCalendarIncomingResponse(t.Context(), m, foreign, 0, stamp, "ACCEPTED"); err == nil {
		t.Fatal("foreign same-ID event claimed")
	}
	if err := accounts.ValidateCalendarIncomingResponse(t.Context(), &UserCalendarIncomingResponseClaim{}); err == nil {
		t.Fatal("unsealed response claim accepted")
	}
	claim, err := accounts.ReserveCalendarIncomingResponse(t.Context(), m, event, 0, stamp, "ACCEPTED")
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarIncomingResponse(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	// Same timestamp and another status cannot replace the accepted intent.
	if _, err := accounts.ReserveCalendarIncomingResponse(t.Context(), m, event, 0, stamp, "DECLINED"); !errors.Is(err, storage.ErrCalendarIncomingChanged) {
		t.Fatal("equal-stamp conflict replaced intent", err)
	}
	newer, err := accounts.ReserveCalendarIncomingResponse(t.Context(), m, event, 0, stamp.Add(time.Minute), "DECLINED")
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarIncomingResponse(t.Context(), claim); !errors.Is(err, storage.ErrCalendarIncomingChanged) {
		t.Fatal("superseded response claim remained valid", err)
	}
	if err := accounts.ValidateCalendarIncomingResponse(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithUser(t.Context(), "bob", func(_ *AccountStore, db *storage.DB) error {
		var n int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_incoming_responses`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("foreign response rows %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarIncomingReservationRechecksAfterWriterAndTriggers(t *testing.T) {
	for _, fault := range []string{"ignore", "sender", "receipt", "credentials", "event", "ambiguity", "reservation"} {
		t.Run(fault, func(t *testing.T) {
			_, accounts, candidates := ownedIncomingMessageFixture(t)
			m, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
			if err != nil {
				t.Fatal(err)
			}
			event, err := accounts.SnapshotCalendarEvent(t.Context(), "alice", "same-event")
			if err != nil {
				t.Fatal(err)
			}
			m, err = accounts.BeginCalendarIncomingDelivery(t.Context(), m, event)
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]string{
				"ignore":      `SELECT RAISE(IGNORE);`,
				"sender":      `UPDATE messages SET from_email='replacement@example.com' WHERE id=1;`,
				"receipt":     `UPDATE calendar_incoming_messages SET reason_code='replacement' WHERE message_id=1;`,
				"credentials": `UPDATE account_caldav_configs SET username='replacement';`,
				"event":       `UPDATE calendar_events SET etag='replacement' WHERE id='same-event';`,
				"ambiguity":   `UPDATE calendar_events SET ical_uid='same-uid' WHERE id='outside-event';`,
				"reservation": `UPDATE calendar_incoming_responses SET response='DECLINED';`,
			}[fault]
			when := "AFTER"
			if fault == "ignore" {
				when = "BEFORE"
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER faulty ` + when + ` INSERT ON calendar_incoming_responses BEGIN ` + body + ` END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := accounts.ReserveCalendarIncomingResponse(t.Context(), m, event, 0, time.Now().Add(-time.Hour), "ACCEPTED"); err == nil {
				t.Fatal("faulty reservation committed")
			}
			if err := accounts.ValidateCalendarIncomingAssociation(t.Context(), m, event); err != nil {
				t.Fatal("reservation did not roll back atomically", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_incoming_responses`).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					return fmt.Errorf("partial reservation %d", n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarIncomingDeadlineRechecksCentralGuard(t *testing.T) {
	_, accounts, candidates := ownedIncomingMessageFixture(t)
	c := candidates["alice"]
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		calls := 0
		at, err := db.NextAccountCalendarIncomingAttempt(t.Context(), "alice", c.AccountID, func(_ *sql.Tx, account string) error {
			calls++
			if calls > 1 || account != c.AccountID {
				return storage.ErrAccountRoute
			}
			return nil
		})
		if !errors.Is(err, storage.ErrAccountRoute) || !at.IsZero() {
			return fmt.Errorf("late deadline guard exposed work %v/%v", at, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
