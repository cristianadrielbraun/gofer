package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func seedStartupThreading(t *testing.T, r *AccountRouting, owner string, n int) {
	t.Helper()
	a := createRoutingTestAccount(t, r, owner)
	if err := r.WithUser(t.Context(), owner, func(db *DB) error {
		tx, err := db.Write().BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for id := 1; id <= n; id++ {
			var date any = "2026-10-07 10:00:00"
			if id == 1 {
				date = nil
			}
			if id == 2 {
				date = ""
			}
			var internetID any = fmt.Sprintf("<m%d@test.invalid>", id)
			reply, refs := "", ""
			subject := fmt.Sprintf("Project %d", id)
			if id == 3 {
				reply = "  <m1@test.invalid>  "
				refs = "<missing@test.invalid> <m1@test.invalid>"
				subject = "Re: Project 1"
			}
			if id == 4 {
				internetID = ""
			}
			if id == 5 {
				internetID = nil // The column is nullable; repair treats NULL as empty.
			}
			if _, err := tx.Exec(`INSERT INTO messages(id,account_id,internet_message_id,subject,in_reply_to,"references",date_received) VALUES(?,?,?,?,?,?,?)`, id, a.AccountID, internetID, subject, reply, refs, date); err != nil {
				return err
			}
		}
		return tx.Commit()
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserStartupThreadingMatchesLegacyOrderAndGrouping(t *testing.T) {
	_, _, r := newAccountRoutingTest(t, 1)
	for _, owner := range []string{"alice", "bob"} {
		seedStartupThreading(t, r, owner, 12)
	}
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error { return db.EnsureThreading(t.Context()) }); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureUserThreadingForStartup(t.Context(), "bob", nil); err != nil {
		t.Fatal(err)
	}
	type message struct {
		id, parent, root, count, unread int64
		normalized, subject, refs       string
	}
	results := map[string][]message{}
	for _, owner := range []string{"alice", "bob"} {
		if err := r.WithUser(t.Context(), owner, func(db *DB) error {
			rows, err := db.Read().Query(`SELECT m.id,COALESCE(m.thread_parent_id,0),t.root_message_id,t.message_count,t.unread_count,m.message_id_normalized,m.normalized_subject,m."references" FROM messages m JOIN threads t ON t.id=m.thread_id ORDER BY m.id`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var m message
				if err := rows.Scan(&m.id, &m.parent, &m.root, &m.count, &m.unread, &m.normalized, &m.subject, &m.refs); err != nil {
					return err
				}
				results[owner] = append(results[owner], m)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(results["bob"]) != 12 || !reflect.DeepEqual(results["alice"], results["bob"]) {
		t.Fatal("legacy threading parity", results)
	}
	if results["bob"][2].parent != 1 || results["bob"][3].normalized != "local-4@gofer.local" {
		t.Fatal(results)
	}
	var idle ThreadingState
	if err := r.EnsureUserThreadingForStartup(t.Context(), "bob", func(s ThreadingState) { idle = s }); err != nil {
		t.Fatal(err)
	}
	if idle != (ThreadingState{}) {
		t.Fatal("completed repair repeated", idle)
	}
}

func TestUserStartupThreadingReleasesCacheAndKeepsIDSnapshot(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	// This fixture writes hundreds of threads. Allow its actual SQLite close
	// and final WAL checkpoint to drain under race instrumentation before the
	// common small-fixture cleanup runs; outstanding leases still fail the wait.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error("threading fixture drain", err)
		}
	})
	seedStartupThreading(t, r, "alice", 501)
	createRoutingTestAccount(t, r, "bob")
	var progress []int
	if err := r.EnsureUserThreadingForStartup(t.Context(), "alice", func(s ThreadingState) {
		progress = append(progress, s.Processed)
		stores.mu.Lock()
		entry := stores.entries["alice"]
		pinned := entry != nil && entry.refs > 0
		stores.mu.Unlock()
		if pinned {
			t.Fatal("progress retained Alice's lease")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		if err := r.WithUser(ctx, "bob", func(*DB) error { return nil }); err != nil {
			t.Fatal("cache admission", err)
		}
		if s.Processed == 0 {
			if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
				_, err := db.Write().Exec(`DELETE FROM messages WHERE id=499;
 UPDATE messages SET internet_message_id='<edited501@test.invalid>',date_received=NULL WHERE id=501;
 INSERT INTO messages(id,account_id,internet_message_id,subject) SELECT 502,id,'<later@test.invalid>','Later arrival' FROM accounts WHERE user_id='alice'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(progress, []int{0, 500, 501}) {
		t.Fatal(progress)
	}
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		var edited, later string
		if err := db.Read().QueryRow(`SELECT message_id_normalized FROM messages WHERE id=501`).Scan(&edited); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT message_id_normalized FROM messages WHERE id=502`).Scan(&later); err != nil {
			return err
		}
		if edited != "edited501@test.invalid" || later != "" {
			t.Error("snapshot/current row", edited, later)
		}
		if db.GetThreadingState() != (ThreadingState{}) {
			t.Error("progress stored on cached DB")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 0 {
		t.Fatal("central fallback", n, err)
	}
	if err := r.EnsureUserThreadingForStartup(t.Context(), "alice", nil); err != nil {
		t.Fatal("restart repair", err)
	}
	if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
		var n string
		err := db.Read().QueryRow(`SELECT message_id_normalized FROM messages WHERE id=502`).Scan(&n)
		if n != "later@test.invalid" {
			t.Error("restart did not repair remaining message", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserStartupThreadingGuardsWriterWaitAndRollsBack(t *testing.T) {
	for _, fault := range []string{"owner", "transaction"} {
		t.Run(fault, func(t *testing.T) {
			system, stores, r := newAccountRoutingTest(t, 1)
			seedStartupThreading(t, r, "alice", 4)
			lease := acquireUserStore(t, stores, "alice")
			db := lease.DB()
			if fault == "transaction" {
				if _, err := db.Write().Exec(`CREATE TRIGGER reject_thread_repair BEFORE UPDATE OF message_id_normalized ON messages WHEN NEW.id=3 BEGIN SELECT RAISE(ABORT,'injected repair failure'); END`); err != nil {
					t.Fatal(err)
				}
				if err := r.EnsureUserThreadingForStartup(t.Context(), "alice", nil); err == nil {
					t.Fatal("injected failure succeeded")
				}
			} else {
				tx, err := db.Write().BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				waits := db.Write().Stats().WaitCount
				done := make(chan error, 1)
				go func() { done <- r.EnsureUserThreadingForStartup(t.Context(), "alice", nil) }()
				waitCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				for db.Write().Stats().WaitCount == waits {
					select {
					case err := <-done:
						t.Fatal("repair did not wait for writer", err)
					case <-waitCtx.Done():
						t.Fatal(waitCtx.Err())
					case <-time.After(10 * time.Millisecond):
					}
				}
				if _, err := system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if !errors.Is(err, ErrUserStoreOwner) {
						t.Fatal(err)
					}
				case <-waitCtx.Done():
					t.Fatal(waitCtx.Err())
				}
			}
			var n int
			if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE message_id_normalized!='')+(SELECT COUNT(*) FROM threads)+(SELECT COUNT(*) FROM message_references)`).Scan(&n); err != nil || n != 0 {
				t.Fatal("partial repair committed", n, err)
			}
		})
	}
}

func TestUserStartupThreadingDisabledUnusedAndMissingStores(t *testing.T) {
	system, stores, r := newAccountRoutingTest(t, 1)
	seedStartupThreading(t, r, "alice", 4)
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureUserThreadingForStartup(t.Context(), "alice", nil); err != nil {
		t.Fatal("disabled retained repair", err)
	}
	unused := "../../outside"
	if err := r.EnsureUserThreadingForStartup(t.Context(), unused, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stores.userPath(unused)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unused file created", err)
	}
	createRoutingTestAccount(t, r, "bob")
	if err := os.Remove(stores.userPath("alice")); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureUserThreadingForStartup(t.Context(), "alice", nil); !errors.Is(err, ErrAccountRoute) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing known store", err)
	}
	if _, err := os.Stat(stores.userPath("alice")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing store replaced", err)
	}
	if _, err := system.Write().Exec(`UPDATE users SET status='disabled',deletion_pending=1 WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	owners, err := r.ListStartupStoreOwners(t.Context(), "", 1)
	if err != nil || len(owners) > 1 {
		t.Fatal(owners, err)
	}
	after := ""
	seen := map[string]bool{}
	for {
		owners, err := r.ListStartupStoreOwners(t.Context(), after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(owners) == 0 {
			break
		}
		seen[owners[0]] = true
		after = owners[0]
	}
	if !seen["alice"] || seen["admin"] || seen["bob"] {
		t.Fatal("startup identity inventory", seen)
	}
	if err := r.EnsureUserThreadingForStartup(t.Context(), "admin", nil); !errors.Is(err, ErrUserStoreOwner) {
		t.Fatal("management data admitted", err)
	}
}

func TestUserStartupThreadingHonorsAccountDirectoryTransitions(t *testing.T) {
	for _, change := range []string{"deleting", "untracked"} {
		t.Run(change, func(t *testing.T) {
			system, _, r := newAccountRoutingTest(t, 1)
			seedStartupThreading(t, r, "alice", 4)
			if change == "deleting" {
				if _, err := system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE user_id='alice'`); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
					_, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES('untracked','alice','untracked@test.invalid'); INSERT INTO messages(id,account_id,internet_message_id) VALUES(5,'untracked','<untracked@test.invalid>')`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			err := r.EnsureUserThreadingForStartup(t.Context(), "alice", nil)
			if (change == "untracked" && !errors.Is(err, ErrAccountRoute)) || (change == "deleting" && err != nil) {
				t.Fatal(change, err)
			}
			if err := r.WithUser(t.Context(), "alice", func(db *DB) error {
				var n int
				err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM messages WHERE message_id_normalized!='')+(SELECT COUNT(*) FROM threads)`).Scan(&n)
				if n != 0 {
					t.Error("unavailable account was repaired or partial batch committed", n)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
