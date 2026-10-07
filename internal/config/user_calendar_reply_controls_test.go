package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func replyControlFixture(t *testing.T, action string) (*ownedReplyFixture, string) {
	t.Helper()
	f := newOwnedReplyFixture(t)
	id := f.queue(t, "alice")
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		if action == "confirm-sent" {
			_, err := db.Write().Exec(`UPDATE outgoing_sends SET status='ambiguous',attempt_count=1,last_error='lost acknowledgement',locked_at=CURRENT_TIMESTAMP WHERE id=?`, id)
			return err
		}
		if action == "dismiss" {
			if _, err := db.Write().Exec(`UPDATE outgoing_sends SET status='sent',attempt_count=1,sent_message_id='<original@test>' WHERE id=?`, id); err != nil {
				return err
			}
			_, err := db.Write().Exec(`UPDATE calendar_reply_jobs SET state='conflict' WHERE id=?`, id)
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f, id
}

func TestUserCalendarReplyControlsIsolationEvictionAndSettingsRepair(t *testing.T) {
	for _, action := range []string{"cancel", "confirm-sent", "dismiss"} {
		t.Run(action, func(t *testing.T) {
			f, id := replyControlFixture(t, action)
			snapshot, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "bob", id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("foreign status", err)
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE accounts SET smtp_host='changed',smtp_username='changed',encrypted_smtp_password=x'01'`); err != nil {
					return err
				}
				if _, err := db.Write().Exec(`UPDATE account_caldav_configs SET encrypted_password=x'02'`); err != nil {
					return err
				}
				_, err := db.Write().Exec(`UPDATE calendar_sources SET is_deleted=1,is_selected=0 WHERE id='same-source'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.WithUser(t.Context(), "bob", func(_ *AccountStore, _ *storage.DB) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.ApplyCalendarReplyControl(t.Context(), snapshot, action); err != nil {
				t.Fatal("settings stranded local recovery", err)
			}
			if err := f.accounts.ApplyCalendarReplyControl(t.Context(), snapshot, action); err == nil {
				t.Fatal("stale control reused")
			}
			current, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
			if err != nil {
				t.Fatal(err)
			}
			state, status := map[string]string{"cancel": "canceled", "confirm-sent": "pending", "dismiss": "dismissed"}[action], map[string]string{"cancel": "canceled", "confirm-sent": "sent", "dismiss": "sent"}[action]
			if current.Job().State != state || current.Job().SendStatus != status {
				t.Fatal(current.Job())
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests`).Scan(&n); err != nil {
					return err
				}
				if (action == "cancel" && n != 0) || (action != "cancel" && n != 1) {
					t.Fatal("wrong reservation release", n)
				}
				send, err := db.GetOutgoingSend(t.Context(), id)
				if err == nil && (send.SentCopyStatus != storage.SentCopyNotRequired || send.SentCopyAttempts != 0) {
					t.Fatal("confirmation created a Sent copy", send)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyControlsPreserveUnknownAndReplacementReservations(t *testing.T) {
	for _, mode := range []string{"legacy", "tampered-proof", "replacement", "missing", "failed"} {
		t.Run(mode, func(t *testing.T) {
			f, id := replyControlFixture(t, "cancel")
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				switch mode {
				case "legacy", "tampered-proof":
					job, err := db.GetCalendarReply(t.Context(), "alice", id)
					if err != nil {
						return err
					}
					var payload map[string]json.RawMessage
					if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
						return err
					}
					if mode == "legacy" {
						delete(payload, "UserAuthority")
					} else {
						payload["Scope"] = json.RawMessage(`"series"`)
					}
					body, err := json.Marshal(payload)
					if err != nil {
						return err
					}
					_, err = db.Write().Exec(`UPDATE calendar_reply_jobs SET payload=? WHERE id=?`, string(body), id)
					return err
				case "replacement":
					_, err := db.Write().Exec(`UPDATE calendar_response_requests SET claim_id='replacement'`)
					return err
				case "missing":
					_, err := db.Write().Exec(`DELETE FROM calendar_response_requests`)
					return err
				case "failed":
					_, err := db.Write().Exec(`UPDATE outgoing_sends SET status='failed',attempt_count=2,last_error='SMTP rejected' WHERE id=?`, id)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.ApplyCalendarReplyControl(t.Context(), snapshot, "cancel"); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM calendar_response_requests`).Scan(&n); err != nil {
					return err
				}
				if ((mode == "failed" || mode == "missing") && n != 0) || ((mode != "failed" && mode != "missing") && n != 1) {
					t.Fatal("inferred reservation ownership", n)
				}
				if mode == "replacement" {
					var nonce string
					if err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&nonce); err != nil {
						return err
					}
					if nonce != "replacement" {
						t.Fatal(nonce)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyControlsWriterWaitAndTriggerRollback(t *testing.T) {
	for _, action := range []string{"cancel", "confirm-sent", "dismiss"} {
		for _, mode := range []string{"attempt", "generation", "payload", "disabled", "account-deleting", "local-deleting", "trigger-ignore", "trigger-state", "trigger-nonce", "trigger-send"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				f, id := replyControlFixture(t, action)
				snapshot, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
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
					go func() { done <- f.accounts.ApplyCalendarReplyControl(ctx, snapshot, action) }()
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
						_, err = tx.Exec(`UPDATE outgoing_sends SET attempt_count=attempt_count+1 WHERE id=?`, id)
					case "generation":
						_, err = tx.Exec(`UPDATE calendar_reply_jobs SET attempted_at=? WHERE id=?`, time.Now(), id)
					case "payload":
						_, err = tx.Exec(`UPDATE calendar_reply_jobs SET payload='{}' WHERE id=?`, id)
					case "disabled":
						_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
					case "account-deleting":
						_, err = f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, snapshot.AccountID())
					case "local-deleting":
						_, err = tx.Exec(`UPDATE accounts SET is_deleting=1`)
					case "trigger-ignore":
						if action == "confirm-sent" {
							_, err = tx.Exec(`CREATE TRIGGER interfere BEFORE UPDATE OF status ON outgoing_sends BEGIN SELECT RAISE(IGNORE); END`)
						} else {
							_, err = tx.Exec(`CREATE TRIGGER interfere BEFORE UPDATE OF state ON calendar_reply_jobs BEGIN SELECT RAISE(IGNORE); END`)
						}
					case "trigger-state":
						if action == "confirm-sent" {
							_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE calendar_reply_jobs SET state='conflict' WHERE id=NEW.id; END`)
						} else {
							_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF state ON calendar_reply_jobs BEGIN UPDATE calendar_reply_jobs SET state='pending' WHERE id=NEW.id; END`)
						}
					case "trigger-nonce":
						if action == "cancel" {
							_, err = tx.Exec(`CREATE TRIGGER interfere BEFORE DELETE ON calendar_response_requests BEGIN SELECT RAISE(IGNORE); END`)
						} else if action == "confirm-sent" {
							_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE calendar_response_requests SET claim_id='replacement'; END`)
						} else {
							_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF state ON calendar_reply_jobs BEGIN UPDATE calendar_response_requests SET claim_id='replacement'; END`)
						}
					case "trigger-send":
						if action == "confirm-sent" {
							_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE outgoing_sends SET mime_data=x'00',sent_copy_status='pending' WHERE id=NEW.id; END`)
						} else {
							_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF state ON calendar_reply_jobs BEGIN UPDATE outgoing_sends SET attempt_count=attempt_count+1 WHERE id=NEW.id; END`)
						}
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
							t.Fatal("late control published", mode)
						}
					case <-ctx.Done():
						return ctx.Err()
					}
					if strings.HasPrefix(mode, "trigger-") {
						job, err := db.GetCalendarReply(ctx, "alice", id)
						if err != nil {
							return err
						}
						if job != snapshot.Job() {
							t.Fatal("partial control escaped rollback", job)
						}
						var nonce string
						if err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&nonce); err != nil {
							return err
						}
						if nonce != snapshot.original.ID {
							t.Fatal("reservation escaped rollback", nonce)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserCalendarReplyControlsRejectInFlightAndAmbiguousCancellation(t *testing.T) {
	for _, status := range []string{storage.OutgoingSendSending, storage.OutgoingSendAmbiguous, storage.OutgoingSendSent} {
		t.Run(status, func(t *testing.T) {
			f, id := replyControlFixture(t, "cancel")
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE outgoing_sends SET status=? WHERE id=?`, status, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.ApplyCalendarReplyControl(t.Context(), snapshot, "cancel"); !errors.Is(err, storage.ErrOutgoingSendNotCancelable) {
				t.Fatal(err)
			}
			if err := f.accounts.ApplyCalendarReplyControl(t.Context(), snapshot, "retry"); err == nil {
				t.Fatal("local control authorized retry")
			}
		})
	}
}

func TestUserCalendarReplyControlsConfirmationOnlyAuthorizesCalendarFollowup(t *testing.T) {
	for _, mode := range []string{"unchanged", "reconfigured"} {
		t.Run(mode, func(t *testing.T) {
			f, id := replyControlFixture(t, "confirm-sent")
			snapshot, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "reconfigured" {
				if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE account_caldav_configs SET encrypted_password=x'0102'`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.accounts.ApplyCalendarReplyControl(t.Context(), snapshot, "confirm-sent"); err != nil {
				t.Fatal(err)
			}
			authority, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", id, false)
			if mode == "reconfigured" {
				if err == nil {
					t.Fatal("confirmation adopted changed Calendar")
				}
				record, err := f.accounts.SnapshotCalendarReplyFollowupRecord(t.Context(), "alice", id)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.accounts.MarkCalendarReplyConflict(t.Context(), record); err != nil {
					t.Fatal(err)
				}
			} else {
				if err != nil {
					t.Fatal("confirmation stranded original Calendar", err)
				}
				claim, err := f.accounts.StartCalendarReplyFollowup(t.Context(), authority)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.accounts.FinishCalendarReplyFollowup(t.Context(), claim, "complete"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", id, true); err == nil {
				t.Fatal("confirmation reauthorized SMTP")
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), id)
				if err == nil && (send.AttemptCount != 1 || send.Status != storage.OutgoingSendSent || send.SentCopyStatus != storage.SentCopyNotRequired || send.SentMessageID != "") {
					t.Fatal("confirmation resent or created Sent copy", send)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyControlsDismissAllowsSentPayloadCleanup(t *testing.T) {
	f, id := replyControlFixture(t, "dismiss")
	snapshot, err := f.accounts.SnapshotCalendarReplyControl(t.Context(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE outgoing_sends SET mime_data=NULL,message_json='',envelope_recipients='[]',sent_copy_status='complete',sent_copy_attempt_count=1 WHERE id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.ApplyCalendarReplyControl(t.Context(), snapshot, "dismiss"); err != nil {
		t.Fatal("Sent-copy completion stranded notice", err)
	}
}
