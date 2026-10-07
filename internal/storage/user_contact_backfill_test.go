package storage

import (
	"context"
	"errors"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"os"
	"reflect"
	"testing"
	"time"
)

func seedBackfillMessages(t *testing.T, r *AccountRouting, owner string, n int) {
	t.Helper()
	a := createRoutingTestAccount(t, r, owner)
	if err := r.WithUser(t.Context(), owner, func(db *DB) error {
		tx, err := db.Write().BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(`INSERT INTO messages(account_id,subject,from_email,from_name,date_received) VALUES (?,'backfill',?,?, '2026-10-07 10:00:00')`, a.AccountID, fmt.Sprintf("%03d@test.invalid", i), fmt.Sprintf("Sender %d", i)); err != nil {
				return err
			}
		}
		return tx.Commit()
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserContactBackfillReleasesCacheAndPublishesMarker(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	seedBackfillMessages(t, r, "alice", 260)
	createRoutingTestAccount(t, r, "bob")
	total, err := r.CountUserContactBackfill(t.Context(), "alice", nil)
	if err != nil || total != 260 {
		t.Fatal(total, err)
	}
	var progress []int
	assertReleased := func() {
		stores.mu.Lock()
		entry := stores.entries["alice"]
		pinned := entry != nil && entry.refs != 0
		stores.mu.Unlock()
		if pinned {
			t.Fatal("backfill callback retained Alice's lease")
		}
	}
	activityCount := 0
	system.SetContactActivityHook(func(event ContactActivityNotification) {
		if event.UserID != "alice" {
			t.Error("activity owner", event)
		}
		activityCount++
		assertReleased()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := r.WithUser(ctx, "bob", func(*DB) error { return nil }); err != nil {
			t.Fatal("hook held store", err)
		}
	})
	if err := r.BackfillUserContacts(t.Context(), "alice", nil, "senders,recipients", func(n int) {
		progress = append(progress, n)
		if n == 128 {
			if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
				_, err := db.Write().Exec(`INSERT INTO messages(account_id,from_email) SELECT id,'zzz-later@test.invalid' FROM accounts WHERE user_id='alice'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		// This acquisition would block until cancellation if the callback held Alice.
		assertReleased()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := r.WithUser(ctx, "bob", func(db *DB) error { return db.SetSetting(ctx, "bob", "backfill-progress", fmt.Sprint(n)) }); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if activityCount != 262 {
		t.Fatal("activity events", activityCount)
	}
	if fmt.Sprint(progress) != "[128 256 260]" {
		t.Fatal(progress)
	}
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		var n int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles WHERE user_id='alice'`).Scan(&n); err != nil {
			return err
		}
		if n != 260 {
			t.Errorf("profiles %d", n)
		}
		marker, err := db.GetSetting(t.Context(), "alice", "contacts_observed_backfilled_v1")
		if marker != "senders,recipients" {
			t.Error("marker", marker)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles`).Scan(&n); err != nil || n != 0 {
		t.Fatal("central write", n, err)
	}
}

func TestUserContactBackfillStopsBetweenBatches(t *testing.T) {
	for _, reason := range []string{"settings", "disabled", "admin-version", "cancel"} {
		t.Run(reason, func(t *testing.T) {
			system, _, r := newAccountRoutingTest(t, 1)
			seedBackfillMessages(t, r, "alice", 130)
			var actor *DiagnosticsActor
			if reason == "admin-version" {
				actor = &DiagnosticsActor{ID: "admin", AuthVersion: 1}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := r.BackfillUserContacts(ctx, "alice", actor, "senders,recipients", func(n int) {
				if n != 128 {
					t.Fatal(n)
				}
				switch reason {
				case "settings":
					if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
						return db.SetUISettings(t.Context(), "alice", map[string]string{"contacts_observed_sources": "recipients"})
					}); err != nil {
						t.Fatal(err)
					}
				case "disabled":
					if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
						t.Fatal(err)
					}
				case "admin-version":
					if _, err := system.Write().Exec(`UPDATE users SET auth_version=auth_version+1 WHERE id='admin'`); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					cancel()
				}
			})
			expected := map[string]error{"settings": ErrContactBackfillChanged, "disabled": ErrUserStoreOwner, "admin-version": ErrUserDiagnosticsAccess, "cancel": context.Canceled}[reason]
			if !errors.Is(err, expected) {
				t.Fatal("stale job", err)
			}
			// Restore central admission to inspect the retained file, without retrying.
			if _, err := system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
				t.Fatal(err)
			}
			if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
				marker, err := db.GetSetting(t.Context(), "alice", "contacts_observed_backfilled_v1")
				if marker != "" {
					t.Error("interrupted marker", marker)
				}
				var n int
				if e := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles`).Scan(&n); e != nil {
					return e
				}
				if n != 128 {
					t.Error("partial count", n)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserContactBackfillAdminDisabledMissingAndAtomicFailure(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	seedBackfillMessages(t, r, "alice", 2)
	actor := DiagnosticsActor{ID: "admin", AuthVersion: 1}
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_backfill BEFORE INSERT ON contact_observations BEGIN SELECT RAISE(ABORT,'forced rollback'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.BackfillUserContacts(t.Context(), "alice", nil, "senders,recipients", nil); err == nil {
		t.Fatal("injected error succeeded")
	}
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		var n int
		if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM contact_profiles)+(SELECT COUNT(*) FROM contact_activity_events)`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Error("partial transaction", n)
		}
		_, err := db.Write().Exec(`DROP TRIGGER reject_backfill`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := r.BackfillUserContacts(t.Context(), "alice", nil, "", nil); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal(err)
	}
	if err := r.BackfillUserContacts(t.Context(), "alice", &actor, "", nil); err != nil {
		t.Fatal(err)
	}
	createRoutingTestAccount(t, r, "bob") // evict Alice before removing her file
	if err := os.Remove(stores.userPath("alice")); err != nil {
		t.Fatal(err)
	}
	if err := r.BackfillUserContacts(t.Context(), "alice", &actor, "", nil); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := os.Stat(stores.userPath("alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recreated", err)
	}
}

func TestUserContactBackfillMatchesLegacyContactRules(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	for _, owner := range []string{"alice", "bob"} {
		seedBackfillMessages(t, r, owner, 3)
		if err := r.WithUser(t.Context(), owner, func(db *DB) error {
			if _, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "manual", Name: "Manual chosen name", Email: "000@test.invalid"}); err != nil {
				return err
			}
			if err := db.UpsertObservedContact(t.Context(), owner, "Suppressed", "001@test.invalid", time.Now()); err != nil {
				return err
			}
			contacts, err := db.SearchContacts(t.Context(), owner, "001@test.invalid", 10)
			if err != nil {
				return err
			}
			if len(contacts) != 1 {
				t.Fatal(contacts)
			}
			if err := db.DeleteContact(t.Context(), owner, contacts[0].ID, true); err != nil {
				return err
			}
			_, err = db.Write().Exec(`INSERT INTO message_recipients(message_id,kind,name,email) SELECT MIN(id),'to','Recipient','recipient@test.invalid' FROM messages`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Compare independently populated databases using the existing shared-store
	// algorithm and the new paged owned algorithm, including repeated manual jobs.
	for attempt := 0; attempt < 2; attempt++ {
		if err := r.WithUser(t.Context(), "alice", func(db *DB) error { return db.BackfillObservedContacts(t.Context(), "alice") }); err != nil {
			t.Fatal(err)
		}
		if err := r.BackfillUserContacts(t.Context(), "bob", nil, "", nil); err != nil {
			t.Fatal(err)
		}
		type row struct {
			email, name   string
			n, suppressed int
		}
		got := map[string][]row{}
		for _, owner := range []string{"alice", "bob"} {
			if err := r.WithUser(t.Context(), owner, func(db *DB) error {
				rows, err := db.Read().Query(`SELECT co.normalized_email,cp.display_name,co.message_count,co.is_suppressed FROM contact_observations co JOIN contact_profiles cp ON cp.id=co.profile_id AND cp.user_id=co.user_id WHERE co.user_id=? ORDER BY co.normalized_email`, owner)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var v row
					if err := rows.Scan(&v.email, &v.name, &v.n, &v.suppressed); err != nil {
						return err
					}
					got[owner] = append(got[owner], v)
				}
				return rows.Err()
			}); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got["alice"], got["bob"]) {
			t.Fatal("legacy parity", got)
		}
		if len(got["bob"]) != 4 || got["bob"][0].name != "Manual chosen name" || got["bob"][1].suppressed != 1 {
			t.Fatal("contact rules", got)
		}
	}
}
