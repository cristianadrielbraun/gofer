package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func claimOwnedReplyDelivery(t *testing.T, f *ownedReplyFixture, owner string) *UserCalendarReplySnapshot {
	t.Helper()
	id := f.queue(t, owner)
	if err := f.accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
		sends, err := db.ClaimDueOutgoingSendsForAccount(t.Context(), f.claims[owner].event.service.id, time.Now().Add(time.Second), 1)
		if err == nil && (len(sends) != 1 || sends[0].ID != id) {
			t.Fatal("wrong claimed reply", sends)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.accounts.SnapshotCalendarReplyDelivery(t.Context(), owner, id)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestUserCalendarReplyDeliveryResultsPreserveExactAttempt(t *testing.T) {
	for _, status := range []string{storage.OutgoingSendSent, storage.OutgoingSendFailed, storage.OutgoingSendPending, storage.OutgoingSendAmbiguous} {
		t.Run(status, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			snapshot := claimOwnedReplyDelivery(t, f, "alice")
			send := snapshot.Send()
			send.MIMEData[0] = 'X'
			send.MessageJSON[0] = 'X'
			send.EnvelopeRecipients[0] = "foreign@example.com"
			if err := f.accounts.ValidateCalendarReplyDelivery(t.Context(), snapshot); err != nil {
				t.Fatal("caller mutated private attempt", err)
			}
			result := storage.CalendarReplySendResult{Status: status, Error: "synthetic rejection"}
			if status == storage.OutgoingSendSent {
				result.Error = ""
				result.InternetID = "<original@example.com>"
			}
			if status == storage.OutgoingSendPending {
				result.NextAttemptAt = time.Now().UTC().Add(time.Hour)
			}
			// A provider may have already accepted mail when settings/cache change.
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE accounts SET smtp_host='new-host',smtp_tls_mode='starttls',smtp_username='new-user',encrypted_smtp_password=x'010203'; UPDATE calendar_events SET summary='refreshed'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.FinishCalendarReplySend(t.Context(), snapshot, result); err != nil {
				t.Fatal("could not record original attempt", err)
			}
			if err := f.accounts.FinishCalendarReplySend(t.Context(), snapshot, result); err == nil {
				t.Fatal("same attempt completed twice")
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				actual, err := db.GetOutgoingSend(t.Context(), snapshot.Job().ID)
				if err != nil {
					return err
				}
				if actual.Status != status || actual.AttemptCount != 1 || !strings.Contains(string(actual.MIMEData), "immutable MIME") {
					t.Fatal("result lost identity", actual)
				}
				if status == storage.OutgoingSendSent && (actual.SentCopyStatus != storage.SentCopyPending || actual.SentMessageID != result.InternetID) {
					t.Fatal("sent receipt lost", actual)
				}
				if status == storage.OutgoingSendPending && !actual.NextAttemptAt.Equal(result.NextAttemptAt) {
					t.Fatal("retry deadline lost")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if status == storage.OutgoingSendSent {
				if _, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", snapshot.Job().ID, false); err != nil {
					t.Fatal("SMTP repair blocked calendar-only followup", err)
				}
				if _, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", snapshot.Job().ID, true); err == nil {
					t.Fatal("accepted email became resendable")
				}
			}
		})
	}
}

func TestUserCalendarReplyDeliveryWriterWaitRejectsSupersededAttempt(t *testing.T) {
	for _, mode := range []string{"attempt", "nonce", "mime", "job", "disabled", "deleting", "input", "trigger-ignore", "trigger-state", "trigger-envelope", "trigger-nonce"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			snapshot := claimOwnedReplyDelivery(t, f, "alice")
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
				result := storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: "<stable@example.com>"}
				go func() { done <- f.accounts.FinishCalendarReplySend(ctx, snapshot, result) }()
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
				case "nonce":
					_, err = tx.Exec(`UPDATE calendar_response_requests SET claim_id='replacement'`)
				case "mime":
					_, err = tx.Exec(`UPDATE outgoing_sends SET mime_data=x'0102'`)
				case "job":
					_, err = tx.Exec(`UPDATE calendar_reply_jobs SET state='canceled'`)
				case "disabled":
					_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deleting":
					_, err = f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, snapshot.send.AccountID)
				case "input":
					send := snapshot.Send()
					send.MIMEData[0] = 'X'
				case "trigger-ignore":
					_, err = tx.Exec(`CREATE TRIGGER reject_reply BEFORE UPDATE OF status ON outgoing_sends BEGIN SELECT RAISE(IGNORE); END`)
				case "trigger-state":
					_, err = tx.Exec(`CREATE TRIGGER reject_reply AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE outgoing_sends SET status='pending' WHERE id=NEW.id; END`)
				case "trigger-envelope":
					_, err = tx.Exec(`CREATE TRIGGER reject_reply AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE outgoing_sends SET envelope_from='foreign@example.com' WHERE id=NEW.id; END`)
				case "trigger-nonce":
					_, err = tx.Exec(`CREATE TRIGGER reject_reply AFTER UPDATE OF status ON outgoing_sends BEGIN UPDATE calendar_response_requests SET claim_id='replacement'; END`)
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					if mode == "input" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("superseded result committed")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				if strings.HasPrefix(mode, "trigger-") {
					send, err := db.GetOutgoingSend(ctx, snapshot.Job().ID)
					if err != nil {
						return err
					}
					if send.Status != storage.OutgoingSendSending || send.AttemptCount != 1 {
						t.Fatal("partial result committed", send)
					}
					var nonce string
					if err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&nonce); err != nil {
						return err
					}
					if nonce != snapshot.proof.Claim.ID {
						t.Fatal("trigger nonce escaped rollback")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyFollowupKeepsOriginalCalendarIdentity(t *testing.T) {
	for _, mode := range []string{"smtp", "calendar-secret", "calendar-user", "calendar-origin", "sender", "mailbox-secret"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			snapshot := claimOwnedReplyDelivery(t, f, "alice")
			if err := f.accounts.FinishCalendarReplySend(t.Context(), snapshot, storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: "<stable@example.com>"}); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				query := map[string]string{"smtp": `UPDATE accounts SET smtp_host='other',smtp_port=2525,smtp_tls_mode='starttls',smtp_username='other',encrypted_smtp_password=x'01'`, "calendar-secret": `UPDATE account_caldav_configs SET encrypted_password=x'01'`, "calendar-user": `UPDATE account_caldav_configs SET username='other'`, "calendar-origin": `UPDATE account_caldav_configs SET base_url='https://other.test/'`, "sender": `UPDATE accounts SET email_address='other@example.com'`, "mailbox-secret": `UPDATE accounts SET encrypted_password=x'01'`}[mode]
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", snapshot.Job().ID, false)
			// This fixture uses separate DAV credentials. A mail-only password repair
			// cannot prevent saving an accepted reply to the unchanged DAV collection.
			if mode == "smtp" || mode == "mailbox-secret" {
				if err != nil {
					t.Fatal("mail settings blocked followup", err)
				}
			} else if err == nil {
				t.Fatal("new calendar identity adopted")
			}
			if errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplySMTPConfigUsesCopiedEndpointAndCentralPolicy(t *testing.T) {
	f := newOwnedReplyFixture(t)
	original := f.claims["alice"].event.Service()
	cfg, err := original.SMTPConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTPHost != original.data.SMTPHost || cfg.SMTPPort != original.data.SMTPPort || cfg.SmtpUsername != original.data.SmtpUsername {
		t.Fatal("copied SMTP endpoint changed")
	}
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET smtp_host='127.0.0.1',smtp_port=2525,smtp_tls_mode='plaintext'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	copied, err := f.accounts.SnapshotServices(t.Context(), "alice", original.AccountID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copied.SMTPConfig(t.Context()); err == nil {
		t.Fatal("plaintext bypassed central policy")
	}
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", "127.0.0.1", 2525, "fixture"); err != nil {
		t.Fatal(err)
	}
	cfg, err = copied.SMTPConfig(t.Context())
	if err != nil || !cfg.SMTPAllowPlaintext || cfg.SMTPHost != "127.0.0.1" {
		t.Fatal("central exception ignored", cfg, err)
	}
	cfg.SMTPHost = "caller-mutated"
	cfg, err = original.SMTPConfig(t.Context())
	if err != nil || cfg.SMTPHost != original.data.SMTPHost {
		t.Fatal("new settings adopted by old snapshot", cfg, err)
	}
}
