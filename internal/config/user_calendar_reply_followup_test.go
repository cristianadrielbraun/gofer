package config

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func acceptedOwnedReply(t *testing.T, f *ownedReplyFixture, owner string) *UserCalendarReplySnapshot {
	t.Helper()
	delivery := claimOwnedReplyDelivery(t, f, owner)
	if err := f.accounts.FinishCalendarReplySend(t.Context(), delivery, storage.CalendarReplySendResult{Status: storage.OutgoingSendSent, InternetID: "<" + owner + "@example.com>"}); err != nil {
		t.Fatal(err)
	}
	authority, err := f.accounts.RestoreCalendarReply(t.Context(), owner, delivery.Job().ID, false)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func TestUserCalendarReplyFollowupGenerationsSurviveEvictionAndSupersedeOlderClaims(t *testing.T) {
	f := newOwnedReplyFixture(t)
	authority := acceptedOwnedReply(t, f, "alice")
	future := time.Now().UTC().Add(24 * time.Hour)
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_reply_jobs SET attempted_at=?`, future)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	first, err := f.accounts.StartCalendarReplyFollowup(t.Context(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.WithUser(t.Context(), "bob", func(_ *AccountStore, _ *storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.ValidateCalendarReplyFollowup(t.Context(), first); err != nil {
		t.Fatal("claim lost on eviction", err)
	}
	second, err := f.accounts.StartCalendarReplyFollowup(t.Context(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.FinishCalendarReplyFollowup(t.Context(), first, "complete"); err == nil {
		t.Fatal("superseded followup published")
	}
	// Sent-copy cleanup is independent of saving the accepted reply to Calendar.
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE outgoing_sends SET mime_data=NULL,message_json='',envelope_recipients='[]'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.FinishCalendarReplyFollowup(t.Context(), second, "complete"); err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.FinishCalendarReplyFollowup(t.Context(), second, "complete"); err == nil {
		t.Fatal("completed generation reused")
	}
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		job, err := db.GetCalendarReply(t.Context(), "alice", authority.Job().ID)
		if err != nil {
			return err
		}
		if job.State != "complete" || job.SendStatus != storage.OutgoingSendSent {
			t.Fatal("acceptance lost", job)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarReplyFollowupMetadataRefreshPreservesOriginalCollection(t *testing.T) {
	for _, mode := range []string{"display", "collection", "role", "nonce"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			authority := acceptedOwnedReply(t, f, "alice")
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				query := map[string]string{"display": `UPDATE calendar_sources SET name='renamed',description='new description',color='#123456',is_hidden=1`, "collection": `UPDATE calendar_sources SET remote_id='https://alice.test/other/' WHERE id='same-source'`, "role": `UPDATE calendar_sources SET access_role='reader'`, "nonce": `UPDATE calendar_response_requests SET claim_id='replacement'`}[mode]
				_, err := db.Write().Exec(query)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			refreshed, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", authority.Job().ID, false)
			if mode == "display" {
				if err != nil {
					t.Fatal("display metadata stranded accepted email", err)
				}
				claim, err := f.accounts.StartCalendarReplyFollowup(t.Context(), refreshed)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.accounts.FinishCalendarReplyFollowup(t.Context(), claim, "complete"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("new collection/authority adopted")
				}
				record, err := f.accounts.SnapshotCalendarReplyFollowupRecord(t.Context(), "alice", authority.Job().ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.accounts.MarkCalendarReplyConflict(t.Context(), record); err != nil {
					t.Fatal("could not retire stale accepted followup", err)
				}
				if mode == "nonce" {
					if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
						var nonce string
						err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&nonce)
						if err == nil && nonce != "replacement" {
							t.Fatal("new reservation changed")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestUserCalendarReplyFollowupWriterWaitAndTriggerFaults(t *testing.T) {
	for _, mode := range []string{"new-generation", "source", "calendar-password", "smtp-user", "nonce", "disabled", "deleting", "trigger-ignore", "trigger-state", "trigger-generation", "trigger-nonce"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			authority := acceptedOwnedReply(t, f, "alice")
			claim, err := f.accounts.StartCalendarReplyFollowup(t.Context(), authority)
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
				go func() { done <- f.accounts.FinishCalendarReplyFollowup(ctx, claim, "complete") }()
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
				case "new-generation":
					_, err = tx.Exec(`UPDATE calendar_reply_jobs SET attempted_at=?`, time.Now().Add(time.Hour))
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET remote_id='https://alice.test/other/' WHERE id='same-source'`)
				case "calendar-password":
					_, err = tx.Exec(`UPDATE account_caldav_configs SET encrypted_password=x'01'`)
				case "smtp-user":
					_, err = tx.Exec(`UPDATE accounts SET smtp_username='other'`)
				case "nonce":
					_, err = tx.Exec(`UPDATE calendar_response_requests SET claim_id='replacement'`)
				case "disabled":
					_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deleting":
					_, err = f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, authority.proof.Account)
				case "trigger-ignore":
					_, err = tx.Exec(`CREATE TRIGGER interfere BEFORE UPDATE OF state ON calendar_reply_jobs BEGIN SELECT RAISE(IGNORE); END`)
				case "trigger-state":
					_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF state ON calendar_reply_jobs BEGIN UPDATE calendar_reply_jobs SET state='pending' WHERE id=NEW.id; END`)
				case "trigger-generation":
					_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF state ON calendar_reply_jobs BEGIN UPDATE calendar_reply_jobs SET attempted_at='2000-01-01' WHERE id=NEW.id; END`)
				case "trigger-nonce":
					_, err = tx.Exec(`CREATE TRIGGER interfere AFTER UPDATE OF state ON calendar_reply_jobs BEGIN UPDATE calendar_response_requests SET claim_id='replacement'; END`)
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					// SMTP-only changes can happen during Calendar-only work.
					if mode == "smtp-user" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil {
						t.Fatal("stale result published")
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				if strings.HasPrefix(mode, "trigger-") {
					job, err := db.GetCalendarReply(ctx, "alice", authority.Job().ID)
					if err != nil {
						return err
					}
					if job.State != "pending" || job.SendStatus != storage.OutgoingSendSent {
						t.Fatal("partial followup committed", job)
					}
					var nonce string
					if err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&nonce); err != nil {
						return err
					}
					if nonce != authority.proof.Claim.ID {
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
