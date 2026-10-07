package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedReplyFixture struct {
	accounts *UserAccountStore
	system   *storage.DB
	claims   map[string]*UserCalendarResponseClaim
}

func newOwnedReplyFixture(t *testing.T) *ownedReplyFixture {
	t.Helper()
	system, _, accounts, _ := newCalendarControlFixture(t)
	seedCalendarSyncEvents(t, accounts)
	f := &ownedReplyFixture{accounts: accounts, system: system, claims: map[string]*UserCalendarResponseClaim{}}
	for _, owner := range []string{"alice", "bob"} {
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			if _, err := db.Write().Exec(`UPDATE calendar_sources SET remote_id=? WHERE id='same-source'`, "https://"+owner+".test/primary/"); err != nil {
				return err
			}
			_, err := db.Write().Exec(`UPDATE calendar_events SET remote_id=?,response_status='needsAction',organizer_email='host@example.com',ical_uid='private-uid' WHERE id='same-event'`, "https://"+owner+".test/primary/invitation.ics")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := accounts.SnapshotCalendarEvent(t.Context(), owner, "same-event")
		if err != nil {
			t.Fatal(err)
		}
		claim, err := accounts.ReserveCalendarResponse(t.Context(), snapshot, "event", snapshot.Event().ETag, "accepted")
		if err != nil {
			t.Fatal(err)
		}
		f.claims[owner] = claim
	}
	return f
}
func (f *ownedReplyFixture) input(t *testing.T, owner string) (storage.CalendarReplyJob, storage.QueueOutgoingSendInput) {
	t.Helper()
	snapshot := f.claims[owner].event
	event := snapshot.Event()
	payload, err := json.Marshal(map[string]any{"Event": event, "Target": map[string]string{"RemoteID": event.RemoteID, "ETag": event.ETag}, "Scope": "event", "SelfEmail": owner + "@example.com", "Organizer": "host@example.com", "Calendar": "private calendar reply"})
	if err != nil {
		t.Fatal(err)
	}
	return storage.CalendarReplyJob{UserID: "forged", SourceID: "forged", ResourceID: event.RemoteID, RemoteID: event.RemoteID, Version: event.ETag, Response: "accepted", Payload: string(payload)}, storage.QueueOutgoingSendInput{AccountID: snapshot.service.id, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: owner + "@example.com", EnvelopeRecipients: []string{"host@example.com"}, MIMEData: []byte(owner + " immutable MIME"), MessageJSON: []byte(`{"CalendarReply":"immutable reply"}`)}
}
func (f *ownedReplyFixture) queue(t *testing.T, owner string) string {
	t.Helper()
	job, input := f.input(t, owner)
	id, err := f.accounts.QueueCalendarReply(t.Context(), f.claims[owner], job, input)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func TestUserCalendarReplyProducerDurableIsolationAndNoSecrets(t *testing.T) {
	f := newOwnedReplyFixture(t)
	alice, bob := f.queue(t, "alice"), f.queue(t, "bob")
	for owner, id := range map[string]string{"alice": alice, "bob": bob} {
		snapshot, err := f.accounts.RestoreCalendarReply(t.Context(), owner, id, true)
		if err != nil {
			t.Fatal("evicted reply did not restore", owner, err)
		}
		if snapshot.Job().UserID != owner || snapshot.Source().Source().AccountID != f.claims[owner].event.service.id {
			t.Fatal("forged owner/source retained")
		}
		if strings.Contains(snapshot.Job().Payload, "password") || strings.Contains(snapshot.Job().Payload, "-secret") {
			t.Fatal("secret persisted in reply proof")
		}
		if err := f.accounts.ValidateCalendarReply(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
		if _, err := f.accounts.RestoreCalendarReply(t.Context(), owner, id, false); err == nil {
			t.Fatal("unsent email authorized followup")
		}
		job, input := f.input(t, owner)
		if _, err := f.accounts.QueueCalendarReply(t.Context(), f.claims[owner], job, input); err == nil {
			t.Fatal("duplicate queue accepted")
		}
	}
	if _, err := f.accounts.RestoreCalendarReply(t.Context(), "bob", alice, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign queue read", err)
	}
	var n int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM calendar_reply_jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatal("shared queue fallback", n, err)
	}
}

func TestUserCalendarReplyProducerWriterWaitRechecksOriginalAuthority(t *testing.T) {
	for _, mode := range []string{"event", "source", "smtp-tls", "smtp-user", "smtp-password", "disabled", "deleting", "nonce", "input-copy"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			job, input := f.input(t, "alice")
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
				go func() { _, err := f.accounts.QueueCalendarReply(ctx, f.claims["alice"], job, input); done <- err }()
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
				case "event":
					_, err = tx.Exec(`UPDATE calendar_events SET summary='changed' WHERE id='same-event'`)
				case "source":
					_, err = tx.Exec(`UPDATE calendar_sources SET access_role='reader' WHERE id='same-source'`)
				case "smtp-tls":
					_, err = tx.Exec(`UPDATE accounts SET smtp_tls_mode='starttls'`)
				case "smtp-user":
					_, err = tx.Exec(`UPDATE accounts SET smtp_username='changed'`)
				case "smtp-password":
					_, err = tx.Exec(`UPDATE accounts SET encrypted_smtp_password=x'010203'`)
				case "disabled":
					_, err = f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
				case "deleting":
					_, err = f.system.Write().Exec(`UPDATE gofer_account_directory SET state='deleting' WHERE account_id=?`, f.claims["alice"].event.service.id)
				case "nonce":
					_, err = tx.Exec(`UPDATE calendar_response_requests SET claim_id='replacement'`)
				case "input-copy":
					input.MIMEData[0] = 'X'
					input.MessageJSON[0] = 'X'
					input.EnvelopeRecipients[0] = "other@example.com"
				}
				if err != nil {
					return err
				}
				if err := tx.Commit(); err != nil {
					return err
				}
				select {
				case err := <-done:
					if (mode == "input-copy") != (err == nil) {
						t.Fatal("writer wait fence", mode, err)
					}
				case <-ctx.Done():
					return ctx.Err()
				}
				var n int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&n); err != nil {
					return err
				}
				want := 0
				if mode == "input-copy" {
					want = 1
				}
				if n != want {
					t.Fatal("partial queue persisted", mode, n)
				}
				if mode == "input-copy" {
					var raw []byte
					if err := db.Read().QueryRow(`SELECT mime_data FROM outgoing_sends`).Scan(&raw); err != nil {
						return err
					}
					if string(raw) != "alice immutable MIME" {
						t.Fatal("caller changed queued bytes")
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyProducerTriggerFaultsRollbackBothRows(t *testing.T) {
	for _, mode := range []string{"ignore-job", "rewrite-job", "rewrite-envelope", "rewrite-mime", "replace-nonce"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			sqlText := map[string]string{
				"ignore-job":       `CREATE TRIGGER fault BEFORE INSERT ON calendar_reply_jobs BEGIN SELECT RAISE(IGNORE); END`,
				"rewrite-job":      `CREATE TRIGGER fault AFTER INSERT ON calendar_reply_jobs BEGIN UPDATE calendar_reply_jobs SET payload='{}' WHERE id=NEW.id; END`,
				"rewrite-envelope": `CREATE TRIGGER fault AFTER INSERT ON outgoing_sends BEGIN UPDATE outgoing_sends SET envelope_recipients='["foreign@example.com"]' WHERE id=NEW.id; END`,
				"rewrite-mime":     `CREATE TRIGGER fault AFTER INSERT ON outgoing_sends BEGIN UPDATE outgoing_sends SET mime_data=x'00' WHERE id=NEW.id; END`,
				"replace-nonce":    `CREATE TRIGGER fault AFTER INSERT ON calendar_reply_jobs BEGIN UPDATE calendar_response_requests SET claim_id='replacement'; END`,
			}[mode]
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { _, err := db.Write().Exec(sqlText); return err }); err != nil {
				t.Fatal(err)
			}
			job, input := f.input(t, "alice")
			if _, err := f.accounts.QueueCalendarReply(t.Context(), f.claims["alice"], job, input); err == nil {
				t.Fatal("trigger fault accepted")
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var n int
				if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM outgoing_sends)+(SELECT COUNT(*) FROM calendar_reply_jobs)`).Scan(&n); err != nil {
					return err
				}
				if n != 0 {
					return fmt.Errorf("partial pair committed: %d", n)
				}
				var nonce string
				if err := db.Read().QueryRow(`SELECT claim_id FROM calendar_response_requests`).Scan(&nonce); err != nil {
					return err
				}
				if nonce != f.claims["alice"].claim.Identity().ID {
					return fmt.Errorf("reservation modified")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserCalendarReplyRestoredAuthorityRejectsReconfigurationAndTampering(t *testing.T) {
	for _, mode := range []string{"event", "smtp-tls", "source", "nonce", "payload", "mime", "attempt", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedReplyFixture(t)
			id := f.queue(t, "alice")
			snapshot, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", id, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
				var err error
				switch mode {
				case "event":
					_, err = db.Write().Exec(`UPDATE calendar_events SET summary='changed' WHERE id='same-event'`)
				case "smtp-tls":
					_, err = db.Write().Exec(`UPDATE accounts SET smtp_tls_mode='starttls'`)
				case "source":
					_, err = db.Write().Exec(`UPDATE calendar_sources SET remote_id='https://foreign.test/primary/' WHERE id='same-source'`)
				case "nonce":
					_, err = db.Write().Exec(`UPDATE calendar_response_requests SET claim_id='replacement'`)
				case "payload":
					_, err = db.Write().Exec(`UPDATE calendar_reply_jobs SET payload=json_set(payload,'$.Organizer','foreign@example.com') WHERE id=?`, id)
				case "mime":
					_, err = db.Write().Exec(`UPDATE outgoing_sends SET mime_data=x'00' WHERE id=?`, id)
				case "attempt":
					_, err = db.Write().Exec(`UPDATE outgoing_sends SET attempt_count=attempt_count+1 WHERE id=?`, id)
				case "legacy":
					_, err = db.Write().Exec(`UPDATE calendar_reply_jobs SET payload=json_remove(payload,'$.UserAuthority') WHERE id=?`, id)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.accounts.ValidateCalendarReply(t.Context(), snapshot); err == nil {
				t.Fatal("original reply accepted changed state", mode)
			}
			// A new restored attempt may adopt only the delivery counter, never changed
			// invitation/configuration/MIME/payload or a historical missing proof.
			if mode != "attempt" {
				if _, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", id, true); err == nil {
					t.Fatal("new authority adopted tampering", mode)
				}
			}
		})
	}
}

func TestUserCalendarReplyFollowupSurvivesCacheRefreshAndSentCopyCleanup(t *testing.T) {
	f := newOwnedReplyFixture(t)
	id := f.queue(t, "alice")
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE outgoing_sends SET status='sent',mime_data=NULL,message_json='' WHERE id=?`, id)
		if err != nil {
			return err
		}
		_, err = db.Write().Exec(`UPDATE calendar_events SET summary='refreshed',etag='new-version' WHERE id='same-event'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", id, true); err == nil {
		t.Fatal("accepted email prepared for resend")
	}
	followup, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", id, false)
	if err != nil {
		t.Fatal("cache refresh blocked calendar-only work", err)
	}
	if followup.Event() != nil {
		t.Fatal("followup borrowed stale event write authority")
	}
	if err := f.accounts.ValidateCalendarReply(t.Context(), followup); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarReplyProducerRejectsRetargetedInputs(t *testing.T) {
	f := newOwnedReplyFixture(t)
	for _, mode := range []string{"foreign-account", "foreign-sender", "foreign-resource", "version", "response", "scope", "self-email", "organizer", "event", "target", "scheduled", "transport"} {
		t.Run(mode, func(t *testing.T) {
			job, input := f.input(t, "alice")
			var payload map[string]any
			if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "foreign-account":
				input.AccountID = f.claims["bob"].event.service.id
			case "foreign-sender":
				input.EnvelopeFrom = "bob@example.com"
			case "foreign-resource":
				job.ResourceID = "https://bob.test/primary/invitation.ics"
			case "version":
				job.Version = "other-version"
			case "response":
				job.Response = "declined"
			case "scope":
				payload["Scope"] = "series"
			case "self-email":
				payload["SelfEmail"] = "bob@example.com"
			case "organizer":
				payload["Organizer"] = "foreign@example.com"
			case "event":
				payload["Event"].(map[string]any)["UserID"] = "bob"
			case "target":
				payload["Target"].(map[string]any)["RemoteID"] = "https://bob.test/primary/invitation.ics"
			case "scheduled":
				input.IsScheduled = true
			case "transport":
				input.Transport = storage.OutgoingTransportGmail
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			job.Payload = string(encoded)
			if _, err := f.accounts.QueueCalendarReply(t.Context(), f.claims["alice"], job, input); err == nil {
				t.Fatal("retargeted input accepted", mode)
			}
		})
	}
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		var n int
		err := db.Read().QueryRow(`SELECT COUNT(*) FROM outgoing_sends`).Scan(&n)
		if err == nil && n != 0 {
			return fmt.Errorf("rejected input left queue rows")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.queue(t, "alice") // Rejections do not delete the original reservation.
}

func TestUserCalendarReplySMTPPasswordUsesCopiedState(t *testing.T) {
	f := newOwnedReplyFixture(t)
	account := f.claims["alice"].event.service.id
	before, err := f.accounts.SnapshotServices(t.Context(), "alice", account)
	if err != nil {
		t.Fatal(err)
	}
	password, err := before.SMTPPassword()
	if err != nil || password != "alice-mail-password" {
		t.Fatal("mailbox fallback failed")
	}
	if err := f.accounts.WithUser(t.Context(), "alice", func(local *AccountStore, db *storage.DB) error {
		cipher, err := local.encrypt("separate-smtp-secret")
		if err != nil {
			return err
		}
		_, err = db.Write().Exec(`UPDATE accounts SET smtp_username='separate',encrypted_smtp_password=? WHERE id=?`, cipher, account)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	copied, err := f.accounts.SnapshotServices(t.Context(), "alice", account)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET encrypted_smtp_password=x'01' WHERE id=?`, account)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.WithUser(t.Context(), "bob", func(_ *AccountStore, _ *storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	password, err = copied.SMTPPassword()
	if err != nil || password != "separate-smtp-secret" {
		t.Fatal("copied SMTP credential was replaced after eviction")
	}
}
