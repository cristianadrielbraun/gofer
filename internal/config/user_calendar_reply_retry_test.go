package config

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedReplyRetryFixture(t *testing.T, confirmed bool) (*ownedReplyFixture, *UserCalendarReplyControlSnapshot) {
	t.Helper()
	f, id := replyControlFixture(t, "cancel")
	status := storage.OutgoingSendFailed
	if confirmed {
		status = storage.OutgoingSendAmbiguous
	}
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE outgoing_sends SET status=?,attempt_count=1,last_error='previous attempt' WHERE id=?`, status, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	control, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	return f, control
}

func TestUserCalendarReplyRetryExplicitRepairRetainsOriginalMessageAndNonce(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "confirmed-ambiguous"}[confirmed], func(t *testing.T) {
			f, control := ownedReplyRetryFixture(t, confirmed)
			old, err := f.accounts.snapshotCalendarReply(t.Context(), "alice", control.Job().ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(local *AccountStore, db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE accounts SET smtp_host='repaired.example',smtp_port=587,smtp_username='repaired'`); err != nil {
					return err
				}
				return local.SaveCalDAVConfig(t.Context(), "alice", control.AccountID(), "https://alice.test/cal", "alice", "repaired-calendar-password", false)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", control.Job().ID, true); err == nil {
				t.Fatal("automatic retry adopted repaired settings")
			}
			prepared, err := f.accounts.PrepareCalendarReplyRetry(t.Context(), control, confirmed)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.WithUser(t.Context(), "bob", func(_ *AccountStore, _ *storage.DB) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.RequeueCalendarReply(t.Context(), control, prepared, confirmed); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.RequeueCalendarReply(t.Context(), control, prepared, confirmed); err == nil {
				t.Fatal("old retry reused")
			}
			current, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", control.Job().ID, true)
			if err != nil {
				t.Fatal("renewed private authority did not restore", err)
			}
			if current.send.Status != storage.OutgoingSendPending || current.send.AttemptCount != 1 || !bytes.Equal(current.send.MIMEData, old.send.MIMEData) || !bytes.Equal(current.send.MessageJSON, old.send.MessageJSON) || current.proof.Claim != old.proof.Claim || current.proof.SendState != old.proof.SendState || current.proof.PayloadState != old.proof.PayloadState || current.send.SentCopyStatus != old.send.SentCopyStatus {
				t.Fatal("retry replaced original content/nonce", current.send)
			}
			if current.proof.CalendarPrincipal != old.proof.CalendarPrincipal || current.proof.CalendarIdentity == old.proof.CalendarIdentity || current.proof.SMTP == old.proof.SMTP {
				t.Fatal("credential repair was not bound", current.proof)
			}
		})
	}
}

func TestUserCalendarReplyRetryRejectsRetargetingAndHistoricalAdoption(t *testing.T) {
	for _, mode := range []string{"principal", "base", "source", "sender", "uid", "resource", "nonce", "mime", "no-proof", "older-principal-unchanged", "older-principal-password", "wrong-confirmation"} {
		t.Run(mode, func(t *testing.T) {
			f, control := ownedReplyRetryFixture(t, false)
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var query string
				switch mode {
				case "principal":
					query = `UPDATE account_caldav_configs SET username='another-principal'`
				case "base":
					query = `UPDATE account_caldav_configs SET base_url='https://another.test/cal'`
				case "source":
					query = `UPDATE calendar_sources SET remote_id='https://alice.test/other/' WHERE id='same-source'`
				case "sender":
					query = `UPDATE accounts SET email_address='another@example.com'`
				case "uid":
					query = `UPDATE calendar_events SET ical_uid='different-uid' WHERE id='same-event'`
				case "resource":
					query = `UPDATE calendar_events SET remote_id='https://alice.test/primary/other.ics' WHERE id='same-event'`
				case "nonce":
					query = `UPDATE calendar_response_requests SET claim_id='replacement'`
				case "mime":
					query = `UPDATE outgoing_sends SET mime_data=x'00'`
				case "no-proof", "older-principal-unchanged", "older-principal-password":
					var payload map[string]json.RawMessage
					if err := json.Unmarshal([]byte(control.Job().Payload), &payload); err != nil {
						return err
					}
					if mode == "no-proof" {
						delete(payload, "UserAuthority")
					} else {
						var proof userCalendarReplyAuthority
						if err := json.Unmarshal(payload["UserAuthority"], &proof); err != nil {
							return err
						}
						proof.CalendarPrincipal = [32]byte{}
						body, err := json.Marshal(proof)
						if err != nil {
							return err
						}
						payload["UserAuthority"] = body
					}
					body, err := json.Marshal(payload)
					if err != nil {
						return err
					}
					if _, err := db.Write().Exec(`UPDATE calendar_reply_jobs SET payload=? WHERE id=?`, string(body), control.Job().ID); err != nil {
						return err
					}
					if mode == "older-principal-password" {
						_, err := db.Write().Exec(`UPDATE account_caldav_configs SET encrypted_password=x'00'`)
						return err
					}
					return nil
				case "wrong-confirmation":
					return nil
				}
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			fresh, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", control.Job().ID)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := f.accounts.PrepareCalendarReplyRetry(t.Context(), fresh, mode == "wrong-confirmation")
			if mode == "older-principal-unchanged" {
				if err != nil {
					t.Fatal("unchanged older proof stranded", err)
				}
				if err := f.accounts.RequeueCalendarReply(t.Context(), fresh, prepared, false); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("retargeted retry prepared", mode)
			}
		})
	}
}

func TestUserCalendarReplyRetryWriterWaitAndTriggerRollback(t *testing.T) {
	for _, mode := range []string{"attempt", "event", "source", "smtp", "calendar", "nonce", "disabled", "deleting", "trigger-ignore", "trigger-payload", "trigger-mime", "trigger-nonce", "trigger-copy"} {
		t.Run(mode, func(t *testing.T) {
			f, control := ownedReplyRetryFixture(t, false)
			prepared, err := f.accounts.PrepareCalendarReplyRetry(t.Context(), control, false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			if err := f.accounts.WithUser(ctx, "alice", func(_ *AccountStore, db *storage.DB) error {
				tx, err := db.Write().BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				before := db.Write().Stats().WaitCount
				done := make(chan error, 1)
				go func() { done <- f.accounts.RequeueCalendarReply(ctx, control, prepared, false) }()
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
				case "attempt":
					_, err = tx.Exec(`UPDATE outgoing_sends SET attempt_count=attempt_count+1`)
				case "event":
					_, err = tx.Exec(`UPDATE calendar_events SET summary='changed' WHERE id='same-event'`)
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET is_selected=0 WHERE id='same-source'`)
				case "smtp":
					_, err = tx.Exec(`UPDATE accounts SET smtp_username='changed'`)
				case "calendar":
					_, err = tx.Exec(`UPDATE account_caldav_configs SET encrypted_password=x'0102'`)
				case "nonce":
					_, err = tx.Exec(`UPDATE calendar_response_requests SET claim_id='replacement'`)
				case "disabled":
					_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deleting":
					_, err = f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, control.AccountID())
				case "trigger-ignore":
					_, err = tx.Exec(`CREATE TRIGGER interfere BEFORE UPDATE OF payload ON calendar_reply_jobs BEGIN SELECT RAISE(IGNORE); END`)
				case "trigger-payload":
					_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF payload ON calendar_reply_jobs BEGIN UPDATE calendar_reply_jobs SET payload='{}' WHERE id=NEW.id; END`)
				case "trigger-mime":
					_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF payload ON calendar_reply_jobs BEGIN UPDATE outgoing_sends SET mime_data=x'00' WHERE id=NEW.id; END`)
				case "trigger-nonce":
					_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF payload ON calendar_reply_jobs BEGIN UPDATE calendar_response_requests SET claim_id='replacement'; END`)
				case "trigger-copy":
					_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF payload ON calendar_reply_jobs BEGIN UPDATE outgoing_sends SET sent_copy_status='pending' WHERE id=NEW.id; END`)
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
						t.Fatal("late retry published", mode)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				if strings.HasPrefix(mode, "trigger-") {
					job, err := db.GetCalendarReply(ctx, "alice", control.Job().ID)
					if err != nil {
						return err
					}
					if job != control.Job() {
						t.Fatal("partial retry escaped", job)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
