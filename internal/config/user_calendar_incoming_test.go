package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedIncomingMessageFixture(t *testing.T) (*storage.DB, *UserAccountStore, map[string]storage.CalendarIncomingMessage) {
	t.Helper()
	system, _, accounts, owners := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	candidates := make(map[string]storage.CalendarIncomingMessage)
	for _, owner := range []string{"alice", "bob"} {
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			if _, err := db.Write().Exec(`UPDATE calendar_events SET ical_uid='same-uid',organizer_email=?,attendees_json='[{"email":"guest@example.com","status":"NEEDS-ACTION"}]',response_status='organizer' WHERE id='same-event'`, owners[owner].Email); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,name,role,remote_id,uid_validity) VALUES('incoming',?,'Inbox','inbox','INBOX',7)`, owners[owner].ID); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<24) INSERT INTO messages(id,account_id,from_email,raw_path,created_at,internet_message_id) SELECT i,?,'Guest@Example.com','before',datetime('now','-1 day'),printf('<reply-%d@example.com>',i) FROM n`, owners[owner].ID); err != nil {
				return err
			}
			_, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) SELECT id,'incoming',id FROM messages`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		candidates[owner] = storage.CalendarIncomingMessage{ID: 1, UserID: owner, AccountID: owners[owner].ID, From: "guest@example.com"}
	}
	return system, accounts, candidates
}

func TestUserCalendarIncomingDiscoveryBoundedOwnedAndReadOnly(t *testing.T) {
	system, accounts, candidates := ownedIncomingMessageFixture(t)
	for _, owner := range []string{"alice", "bob", "alice"} {
		c := candidates[owner]
		for _, limit := range []int{0, 17, 1000} {
			if _, err := accounts.ListCalendarIncomingMessages(t.Context(), owner, c.AccountID, limit); !errors.Is(err, storage.ErrCalendarIncomingChanged) {
				t.Fatal("unbounded incoming discovery", limit, err)
			}
		}
		rows, err := accounts.ListCalendarIncomingMessages(t.Context(), owner, c.AccountID, 8)
		if err != nil || len(rows) != 8 {
			t.Fatal("bounded owned incoming page", len(rows), err)
		}
		for i, row := range rows {
			if row.ID != int64(i+1) || row.UserID != owner || row.AccountID != c.AccountID || row.From != "guest@example.com" {
				t.Fatal("incoming page crossed ownership", row)
			}
		}
		other := "alice"
		if owner == other {
			other = "bob"
		}
		if _, err := accounts.ListCalendarIncomingMessages(t.Context(), owner, candidates[other].AccountID, 8); err == nil {
			t.Fatal("foreign account enumerated incoming mail")
		}
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			var count int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_incoming_messages`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("read-only incoming discovery created receipts: %d", count)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var central int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&central); err != nil || central != 0 {
		t.Fatal("incoming content appeared centrally", central, err)
	}
	for _, role := range []string{"sent", "trash", "junk", "spam", "drafts"} {
		if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE folders SET role=?`, role)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if rows, err := accounts.ListCalendarIncomingMessages(t.Context(), "alice", candidates["alice"].AccountID, 8); err != nil || len(rows) != 0 {
			t.Fatal("excluded folder became incoming work", role, rows, err)
		}
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE folders SET role='inbox'; UPDATE calendar_events SET attendees_json='["unusable JSON guest",42]' WHERE id='same-event'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows, err := accounts.ListCalendarIncomingMessages(t.Context(), "alice", candidates["alice"].AccountID, 8); err != nil || len(rows) != 0 {
		t.Fatal("malformed guest became incoming work", rows, err)
	}
}

func TestUserCalendarIncomingMessageReceiptAndRawIdentityAcrossEviction(t *testing.T) {
	_, accounts, candidates := ownedIncomingMessageFixture(t)
	claims := make(map[string]*UserCalendarIncomingMessageSnapshot)
	for _, owner := range []string{"alice", "bob"} {
		var err error
		claims[owner], err = accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates[owner])
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range []string{"alice", "bob", "alice"} {
		if err := accounts.ValidateCalendarIncomingMessage(t.Context(), claims[owner]); err != nil {
			t.Fatal("incoming copy failed across eviction", err)
		}
	}
	other, err := NewUserAccountStore(accounts.routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*UserCalendarIncomingMessageSnapshot{nil, {}, claims["alice"]} {
		if err := other.ValidateCalendarIncomingMessage(t.Context(), c); !errors.Is(err, storage.ErrCalendarIncomingChanged) {
			t.Fatal("unbound incoming claim accepted", err)
		}
	}
	var raw *storage.RawMessageSnapshot
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		if _, err := db.Write().Exec(`UPDATE messages SET raw_path='after' WHERE id=1`); err != nil {
			return err
		}
		var err error
		raw, err = db.SnapshotRawMessage(t.Context(), "alice", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarIncomingMessage(t.Context(), claims["alice"]); err == nil {
		t.Fatal("raw-path publication did not require refresh")
	}
	c, err := accounts.RefreshCalendarIncomingRaw(t.Context(), claims["alice"], raw)
	if err != nil {
		t.Fatal("same retrieval raw-path refresh rejected", err)
	}
	if _, err := accounts.RefreshCalendarIncomingRaw(t.Context(), claims["bob"], raw); !errors.Is(err, storage.ErrCalendarIncomingChanged) {
		t.Fatal("foreign raw identity adopted", err)
	}
	c, err = accounts.FinishCalendarIncomingDelivery(t.Context(), c, "retry", "DNS unavailable", "verification_pending")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarIncomingDelivery(t.Context(), claims["alice"], "ignored", "stale", "not_applied"); err == nil {
		t.Fatal("stale job overwrote newer receipt")
	}
	if _, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"]); !errors.Is(err, storage.ErrCalendarIncomingChanged) {
		t.Fatal("future retry became due", err)
	}
	if err := accounts.ValidateCalendarIncomingMessage(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_incoming_messages SET next_attempt_at=datetime('now','-1 minute')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c, err = accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarIncomingDelivery(t.Context(), c, "ignored", "no authenticated reply", "unverified"); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var attempts int
		var state, reason string
		if err := db.Read().QueryRow(`SELECT state,attempts,reason_code FROM calendar_incoming_messages WHERE message_id=1`).Scan(&state, &attempts, &reason); err != nil {
			return err
		}
		if state != "ignored" || attempts != 2 || reason != "unverified" {
			return fmt.Errorf("bad owned receipt %s/%d/%s", state, attempts, reason)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarIncomingMessage(t.Context(), claims["bob"]); err != nil {
		t.Fatal("receipt changed another owner", err)
	}
}

func TestUserCalendarIncomingMessageWriterWaitRechecksIdentityAndLifecycle(t *testing.T) {
	for _, change := range []string{"sender", "folder", "receipt", "connection", "owner", "account"} {
		t.Run(change, func(t *testing.T) {
			system, accounts, candidates := ownedIncomingMessageFixture(t)
			c, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
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
					_, err := accounts.FinishCalendarIncomingDelivery(ctx, c, "ignored", "stale", "unverified")
					done <- err
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
				case "sender":
					_, err = tx.Exec(`UPDATE messages SET from_email='replacement@example.com' WHERE id=1`)
				case "folder":
					_, err = tx.Exec(`UPDATE message_folder_state SET remote_uid=999 WHERE message_id=1`)
				case "receipt":
					_, err = tx.Exec(`INSERT INTO calendar_incoming_messages(message_id,state,reason_code) VALUES(1,'complete','newer-job')`)
				case "connection":
					_, err = tx.Exec(`UPDATE accounts SET username='reconnected'`)
				case "owner":
					_, err = system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "account":
					_, err = system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, candidates["alice"].AccountID)
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					if err == nil {
						return errors.New("stale incoming receipt committed after writer wait")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var count int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_incoming_messages WHERE reason_code='unverified'`).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					return errors.New("failed incoming receipt left a publication")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarIncomingMessageReceiptTriggerRollback(t *testing.T) {
	for _, fault := range []string{"ignore", "sender", "receipt", "connection"} {
		t.Run(fault, func(t *testing.T) {
			_, accounts, candidates := ownedIncomingMessageFixture(t)
			c, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
			if err != nil {
				t.Fatal(err)
			}
			query := map[string]string{
				"ignore":     `CREATE TRIGGER faulty BEFORE INSERT ON calendar_incoming_messages BEGIN SELECT RAISE(IGNORE); END`,
				"sender":     `CREATE TRIGGER faulty AFTER INSERT ON calendar_incoming_messages BEGIN UPDATE messages SET from_email='changed' WHERE id=NEW.message_id; END`,
				"receipt":    `CREATE TRIGGER faulty AFTER INSERT ON calendar_incoming_messages BEGIN UPDATE calendar_incoming_messages SET attempts=999 WHERE message_id=NEW.message_id; END`,
				"connection": `CREATE TRIGGER faulty AFTER INSERT ON calendar_incoming_messages BEGIN UPDATE accounts SET username='changed'; END`,
			}[fault]
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(query); return err }); err != nil {
				t.Fatal(err)
			}
			if _, err := accounts.FinishCalendarIncomingDelivery(t.Context(), c, "retry", "temporarily offline", "retry"); err == nil {
				t.Fatal("corrupt incoming receipt committed")
			}
			if err := accounts.ValidateCalendarIncomingMessage(t.Context(), c); err != nil {
				t.Fatal("incoming receipt did not roll back atomically", err)
			}
		})
	}
}

func TestUserCalendarIncomingDeliveryAssociationIsExactAndDiagnosticOnly(t *testing.T) {
	for _, mode := range []string{"confirmed", "foreign-event", "not-invited", "ambiguous", "trigger-ambiguity"} {
		t.Run(mode, func(t *testing.T) {
			_, accounts, candidates := ownedIncomingMessageFixture(t)
			c, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
			if err != nil {
				t.Fatal(err)
			}
			eventOwner := "alice"
			if mode == "foreign-event" {
				eventOwner = "bob"
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				switch mode {
				case "not-invited":
					_, err := db.Write().Exec(`UPDATE calendar_events SET attendees_json='[{"email":"other@example.com"}]' WHERE id='same-event'`)
					return err
				case "ambiguous":
					_, err := db.Write().Exec(`UPDATE calendar_events SET ical_uid='same-uid' WHERE id='outside-event'`)
					return err
				case "trigger-ambiguity":
					_, err := db.Write().Exec(`CREATE TRIGGER faulty AFTER INSERT ON calendar_incoming_messages BEGIN UPDATE calendar_events SET ical_uid='same-uid' WHERE id='outside-event'; END`)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			event, err := accounts.SnapshotCalendarEvent(t.Context(), eventOwner, "same-event")
			if err != nil {
				t.Fatal(err)
			}
			next, err := accounts.BeginCalendarIncomingDelivery(t.Context(), c, event)
			if mode != "confirmed" {
				if err == nil {
					t.Fatal("unrelated or ambiguous event diagnostic accepted", mode)
				}
				if err := accounts.ValidateCalendarIncomingMessage(t.Context(), c); err != nil {
					t.Fatal("failed association left partial receipt", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := accounts.ValidateCalendarEvent(t.Context(), event); err != nil {
				t.Fatal("diagnostic changed calendar event", err)
			}
			if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var state, reason, attendee, uid, eventID string
				var attempts int
				if err := db.Read().QueryRow(`SELECT state,reason_code,attendee,ical_uid,event_id,attempts FROM calendar_incoming_messages WHERE message_id=1`).Scan(&state, &reason, &attendee, &uid, &eventID, &attempts); err != nil {
					return err
				}
				if state != "retry" || reason != "processing" || attendee != "guest@example.com" || uid != "same-uid" || eventID != "same-event" || attempts != 0 {
					return fmt.Errorf("bad processing diagnostic %s/%s/%s/%s/%s/%d", state, reason, attendee, uid, eventID, attempts)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := accounts.FinishCalendarIncomingDelivery(t.Context(), next, "ignored", "sender authentication failed", "unverified"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarIncomingDiscoveryExcludesAnotherOwnedAccount(t *testing.T) {
	_, accounts, candidates := ownedIncomingMessageFixture(t)
	extra, err := accounts.CreateAccount(t.Context(), "alice", secureAccountStoreTestRequest("alice-other@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		if err := db.ReplaceCalendarSources(t.Context(), "alice", extra.ID, "caldav", []storage.CalendarSource{{ID: "other-source", RemoteID: "/other/", IsSelected: true}}); err != nil {
			return err
		}
		if _, err := db.Write().Exec(`INSERT INTO calendar_events(id,user_id,source_id,remote_id,ical_uid,organizer_email,attendees_json,all_day,start_at,end_at,start_date,end_date) SELECT 'other-event','alice','other-source','other-native','same-uid',?,attendees_json,all_day,start_at,end_at,start_date,end_date FROM calendar_events WHERE id='same-event'`, extra.Email); err != nil {
			return err
		}
		if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,name,role) VALUES('other-inbox',?,'Inbox','inbox')`, extra.ID); err != nil {
			return err
		}
		if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,from_email,created_at) VALUES(25,?,'guest@example.com',datetime('now','-2 days'))`, extra.ID); err != nil {
			return err
		}
		_, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(25,'other-inbox',77)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := accounts.ListCalendarIncomingMessages(t.Context(), "alice", candidates["alice"].AccountID, 8)
	if err != nil || len(rows) != 8 || rows[0].ID != 1 {
		t.Fatal("older incoming work from another owned account entered the page", rows, err)
	}
	rows, err = accounts.ListCalendarIncomingMessages(t.Context(), "alice", extra.ID, 8)
	if err != nil || len(rows) != 1 || rows[0].ID != 25 {
		t.Fatal("extra account could not discover its own work", rows, err)
	}
}

func TestUserCalendarIncomingReceiptLongErrorsRemainRetryable(t *testing.T) {
	_, accounts, candidates := ownedIncomingMessageFixture(t)
	c, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarIncomingDelivery(t.Context(), c, "retry", strings.Repeat("provider diagnostic ", 300), "retry"); err != nil {
		t.Fatal("large provider error prevented recording retry", err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var n int
		var state string
		if err := db.Read().QueryRow(`SELECT length(last_error),state FROM calendar_incoming_messages WHERE message_id=1`).Scan(&n, &state); err != nil {
			return err
		}
		if n > 4096 || state != "retry" {
			return fmt.Errorf("diagnostic limit lost retry: %d/%s", n, state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarIncomingMessageConcurrentReceiptAndUnrelatedMailChanges(t *testing.T) {
	_, accounts, candidates := ownedIncomingMessageFixture(t)
	first, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
	if err != nil {
		t.Fatal(err)
	}
	second, err := accounts.SnapshotCalendarIncomingMessage(t.Context(), candidates["alice"])
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE messages SET snippet='body cache refreshed',updated_at=CURRENT_TIMESTAMP WHERE id=1; UPDATE message_folder_state SET is_read=1 WHERE message_id=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.ValidateCalendarIncomingMessage(t.Context(), first); err != nil {
		t.Fatal("unrelated mail activity invalidated reply identity", err)
	}
	next, err := accounts.FinishCalendarIncomingDelivery(t.Context(), first, "retry", "first result", "verification_pending")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.FinishCalendarIncomingDelivery(t.Context(), second, "ignored", "second result", "unverified"); !errors.Is(err, storage.ErrCalendarIncomingChanged) {
		t.Fatal("concurrent stale receipt overwrote newer result", err)
	}
	if err := accounts.ValidateCalendarIncomingMessage(t.Context(), next); err != nil {
		t.Fatal("concurrent loser changed winning receipt", err)
	}
}

func TestUserCalendarIncomingDiscoveryRechecksLateGuard(t *testing.T) {
	_, accounts, candidates := ownedIncomingMessageFixture(t)
	if err := accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var calls int
		rows, err := db.ListAccountCalendarIncomingMessages(t.Context(), "alice", candidates["alice"].AccountID, 8, func(_ *sql.Tx, account string) error {
			calls++
			if account != candidates["alice"].AccountID || calls > 1 {
				return storage.ErrAccountRoute
			}
			return nil
		})
		if !errors.Is(err, storage.ErrAccountRoute) || len(rows) != 0 {
			return fmt.Errorf("late guard exposed incoming work: %d/%v", len(rows), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
