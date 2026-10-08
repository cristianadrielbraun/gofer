package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedUserRetentionWorker(t *testing.T, f *ownedAdminDiagnosticFixture, owner string, n int) {
	t.Helper()
	if err := f.h.userStorage.WithUser(t.Context(), owner, func(db *storage.DB) error {
		tx, err := db.Write().BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(`INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,envelope_recipients,status,sent_copy_status,send_after,updated_at) VALUES(?,?,'smtp','test@example.com','[]','sent','not_required',datetime('now','-31 days'),datetime('now','-31 days'))`, fmt.Sprintf("same-send-%d", i), f.accounts[owner].ID); err != nil {
				return err
			}
		}
		return tx.Commit()
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserMailRetentionSweepsPagesBatchesDisabledStoresAndAdminDiagnostics(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	seedUserRetentionWorker(t, f, "alice", 1005)
	seedUserRetentionWorker(t, f, "bob", 3)
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob';
 INSERT INTO accounts(id,user_id,email_address) VALUES('central-sentinel','alice','central@example.com');
 INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,envelope_recipients,status,sent_copy_status,send_after,updated_at) VALUES('same-send-0','central-sentinel','smtp','test@example.com','[]','sent','not_required',datetime('now','-31 days'),datetime('now','-31 days'))`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 70; i++ {
		owner := fmt.Sprintf("unused-%03d", i)
		if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	directory := f.system.Path() + ".users"
	before, err := filepath.Glob(filepath.Join(directory, "*.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f.admin.runMailRetentionAt(t.Context(), now)
	diagnostics := f.admin.mailRetentionDiagnostics()
	if !diagnostics.LastRunAt.Equal(now) || diagnostics.LastPruned.OutgoingSends != 1003 || diagnostics.LastError != "" {
		t.Fatal("bounded sweep totals", diagnostics)
	}
	response := f.request("/api/admin/mail-operations/status")
	var status models.MailOperationsAdminStatus
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Retention.LastPruned.OutgoingSends != 1003 {
		t.Fatal("admin retention diagnostics", response.Code, response.Body.String())
	}
	after, err := filepath.Glob(filepath.Join(directory, "*.db"))
	if err != nil || len(before) != len(after) {
		t.Fatal("unused owners created stores", before, after, err)
	}
	var central int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&central); err != nil || central != 1 {
		t.Fatal("shared sentinel pruned", central, err)
	}
	f.admin.runMailRetentionAt(t.Context(), now)
	if diagnostics := f.admin.mailRetentionDiagnostics(); diagnostics.LastPruned.OutgoingSends != 5 || diagnostics.LastError != "" {
		t.Fatal("next sweep did not finish bounded backlog", diagnostics)
	}
}

func TestUserMailRetentionWorkerJoinsShutdownAndContinuesAfterBusyOwner(t *testing.T) {
	for _, mode := range []string{"shutdown", "busy-owner"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedAdminDiagnosticFixture(t)
			if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'`); err != nil {
				t.Fatal(err)
			}
			seedUserRetentionWorker(t, f, "alice", 1)
			seedUserRetentionWorker(t, f, "bob", 1)
			held := "alice"
			if mode == "busy-owner" {
				held = "bob"
			}
			if err := f.h.userStorage.WithUser(t.Context(), held, func(db *storage.DB) error {
				waits := db.Write().Stats().WaitCount
				if mode == "shutdown" {
					tx, err := db.Write().BeginTx(t.Context(), nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
				}
				if err := f.admin.StartUserMailRetention(t.Context(), UserMailRetentionOptions{Interval: time.Hour, UserTimeout: 250 * time.Millisecond}); err != nil {
					return err
				}
				if err := f.admin.StartUserMailRetention(t.Context(), UserMailRetentionOptions{}); err == nil {
					t.Fatal("duplicate retention admitted")
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					if mode == "shutdown" && db.Write().Stats().WaitCount > waits {
						break
					}
					if mode == "busy-owner" && !f.admin.mailRetentionDiagnostics().LastRunAt.IsZero() {
						state := f.admin.mailRetentionDiagnostics()
						if state.LastPruned.OutgoingSends != 1 || state.LastError == "" {
							return fmt.Errorf("failed Alice capacity wait blocked Bob or hid failure: %+v", state)
						}
						var n int
						if err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&n); err != nil {
							return err
						}
						if n != 0 {
							return fmt.Errorf("Bob was not pruned while Alice deferred")
						}
						break
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("retention did not reach bounded wait/completion")
					}
					time.Sleep(time.Millisecond)
				}
				f.cancel()
				done := make(chan struct{})
				go func() { f.h.userIMAP.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					return fmt.Errorf("retention did not join root shutdown")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f.admin.WaitUserMailRetention()
			if mode == "busy-owner" {
				f.admin.runMailRetentionAt(t.Context(), time.Now().UTC())
				if state := f.admin.mailRetentionDiagnostics(); state.LastPruned.OutgoingSends != 1 || state.LastError != "" {
					t.Fatal("deferred Alice did not recover", state)
				}
			}
		})
	}
}

func TestUserMailRetentionReportsMissingHistoricalStoreWithoutReplacement(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	var path string
	if err := f.h.userStorage.WithUser(t.Context(), "alice", func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	// Opening Bob evicts Alice from cache1 and closes its SQLite handle first.
	if _, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsMail); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f.admin.runMailRetentionAt(t.Context(), time.Now().UTC())
	if f.admin.mailRetentionDiagnostics().LastError == "" {
		t.Fatal("missing historical store reported successful sweep")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("missing database replaced", err)
	}
}
